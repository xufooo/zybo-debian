// SPDX-License-Identifier: GPL-2.0-only
// chain.go -- the effect chain model (the basis of 0.2's "adjustable + preset import")
//
// 0.1's in-memory model was a fixed 6 slots + 6 preset tables compiled into the backend, an
// import of more than 6 sections was **silently truncated** (ApplyToDSP in config.go), and
// AutoEQ's LSC/HSC lines were silently skipped as well.
// 0.2 switches to an **ordered effect chain**: preset files, the UI and import/export are all
// based on it; when it is mapped to the hardware it maps onto biquad slots, and **if it does
// not fit it reports an explicit error**, never quietly dropping data. Capacity comes from
// bandLimit(): 6 sections for a 0.1 bitstream (the cascade hardcoded in the RTL), maxSections
// for the 0.2 slot-table engine (determined by RAM).
//
// Only the biquad family (peq / shelf / high-low pass / notch / bandpass / allpass) can reach
// the hardware today; the remaining types (dynamic bass, equal loudness, crossfeed, reverb
// ...) are **registered as "planned"** here, which is used to grey them out in the UI and to
// give an explicit message on import -- see chainPlannedType.

package main

import (
	"fmt"
	"log"
	"sort"
	"strings"
)

// ChainItem is one node on the effect chain. P0 uses only the biquad family's first four
// fields; the remaining types will use more parameters later (strength / mix ratio etc.), so
// Params is left for them.
type ChainItem struct {
	Type    string             `json:"type"`
	Enabled bool               `json:"enabled"`
	Freq    float64            `json:"freq,omitempty"`
	GainDB  float64            `json:"gain_db,omitempty"`
	Q       float64            `json:"q,omitempty"`
	Params  map[string]float64 `json:"params,omitempty"`
	// Name is only for chain items that "reference an external resource" (convolution
	// referencing an IR, DDC referencing a .vdc): after a restart the chain is restored from
	// state.json, but the coefficients / file contents are not in the chain, so this name is
	// what reads them back.
	// WARNING: Params is map[string]float64 (numbers only), so a name cannot go in there --
	//    stuffing a string into params["name"] hit a wall at compile time.
	Name string `json:"name,omitempty"`
}

// chainTypeToSlot maps a chain type name onto a hardware slot type (the ones designBiquad
// knows).
// It also accepts EqualizerAPO / REW / AutoEQ spellings (pk / lsc / hsc / peq ...), because
// presets and import files come from those ecosystems.
var chainTypeToSlot = map[string]string{
	"peq": "PK", "pk": "PK", "parametric": "PK", "modal": "PK",
	"lowshelf": "LS", "ls": "LS", "lsc": "LS",
	"highshelf": "HS", "hs": "HS", "hsc": "HS",
	"highpass": "HP", "hp": "HP", "hpq": "HPQ",
	"lowpass": "LP", "lp": "LP", "lpq": "LPQ",
	"notch": "NO", "no": "NO",
	"bandpass": "BP", "bp": "BP",
	"allpass": "AP", "ap": "AP",
	// convolution / DDC: goes through the FIR slot (parallel split cores, see fir_bank.v and
	// planKindFIR in dsp_slot.go).
	// The coefficients are not in the chain; they are loaded by /api/dsp/ir, and the chain item
	// only expresses "this stage convolves, and with how many taps".
	"fir": "FIR", "convolver": "FIR", "ir": "FIR",
}

// chainPlannedType registers the types that "need a new engine" together with their stage.
// Purpose: (1) the UI greys out presets/cards that use these types and states why; (2) on
// import it reports an explicit error instead of dropping them silently.
var chainPlannedType = map[string]string{
	// Note: dynamic_bass is already **implemented** (DYN slot, see dyn.go and
	// buildSlotPlanNodes in dsp_slot.go), so it is not in this "planned" table -- otherwise the
	// UI would grey it out and an import would report "needs a new engine".
	"loudness":   "equal loudness (P2)",
	"compressor": "compression (P2)",
	"limiter":    "true-peak limiting (P2)",
	// Note: crossfeed / surround are already **implemented** (MIX2 / DELAY slots) and are
	// likewise not in this table.
	"delay":  "delay (P2)",
	"copy":   "channel matrix (P2)",
	"reverb": "reverb (P3)",
	// Note: convolver is already **implemented** (FIR slot, fir_bank.v), so it was moved out of
	// this table.
	"analog": "tube flavor / even harmonics (P3)",
}

// normalizeChainType lowercases and trims whitespace, so that the spellings of various tools
// are accepted.
func normalizeChainType(t string) string {
	return strings.ToLower(strings.TrimSpace(t))
}

// chainSlotType returns the hardware slot type for a chain type; ok=false when it is not
// supported.
// The two **non-EQ** effects in "chain item form" (they do not enter an EQ slot; they
// configure the matching runtime feature):
//
//   - crossfeed: entry 14 of the preset library, "big stage (wide soundstage)", is written
//     that way (`type:"crossfeed"`), so the chain item form must be recognized -- otherwise a
//     preset / import file would report "unknown effect type" as soon as it arrives.
//   - surround: the V4A DiffSurround one (the delay-type soundstage). Common aliases are
//     accepted too.
//
// They do not take EQ sections; applyChain turns them into currentCrossfeed / currentSurround.
var chainEffectType = map[string]string{
	"crossfeed": "crossfeed", "cure": "crossfeed", "bs2b": "crossfeed",
	// tube flavor (V4A `TubeSimulator`, see tube.go): one first-order section, zero hardware
	// cost.
	"tube": "tube", "tubesimulator": "tube", "tube_simulator": "tube", "6n1j": "tube",
	"surround": "surround", "diffsurround": "surround", "widener": "surround",
	"haas": "surround",
	// convolution / DDC: fir / convolver / ir are all recognized in a chain item (the
	// coefficients are loaded separately by /api/dsp/ir)
	"fir": "fir", "convolver": "fir", "ir": "fir",
	// device correction (DDC, .vdc)
	"ddc": "ddc", "vdc": "ddc",
	// ViPERClarity (A4): NATURAL / OZONE are 1 section each; XHIFI uses 8 slots (3+3+3+1
	// sections + 2 branch delays + 2 MIX2, see xhifi.go). XHIFI needs **two delay slots** =>
	// it can only be pushed out when the bitstream's CAP3[23:16] >= 2, otherwise the compiler
	// refuses at the planning stage (no silent downgrade to another tier).
	"clarity": "clarity", "viperclarity": "clarity", "viper_clarity": "clarity",
	// AnalogX (first half of A2): five slots HP240 + POLY + MIX2 + LP + Peak633 (4 sections +
	// 1 MIX2).
	"analogx": "analogx", "viperanalogx": "analogx", "analog_x": "analogx",
	// ViPERBass (bass enhancement, V4A `ViPERBass`): the NATURAL_BASS tier is implemented
	// (2 slots, 3 sections, see viperbass.go); the Pure Bass+ / Subwoofer tiers are
	// **explicitly rejected at compile time** (the reasons are in viperBassValidate in
	// viperbass.go).
	"viperbass": "viperbass", "viper_bass": "viperbass", "bassboost": "viperbass",
	// ColorfulMusic (V4A field surround = DepthSurround + Stereo3DSurround): a **joint stereo**
	// stage whose hardware precondition is CAP1 bit12 (see colorful.go and the "joint stereo
	// frame pass" in dsp_engine.v).
	"colorfulmusic": "colorfulmusic", "colorful_music": "colorfulmusic",
	"colorful": "colorfulmusic", "headphone360": "colorfulmusic",
	// WARNING: a chain item's `level` parameter is **not interpreted**: the V4A client sends
	//    two integers, coeffs "widening;depth" and midimage (arrays.xml), while a chain item
	//    has only one numeric field, so forcing it in would be guesswork.
	//    To use non-default parameters go through the `colorfulmusic` field of /api/dsp/chain
	//    (or the panel).
	// SpeakerCorrection (second half of A4): 3 sections (LP13500 -> HP80 -> (1+BP420)/2).
	"speakercorrection": "speaker", "speaker_correction": "speaker",
	"speakercorr": "speaker", "speaker": "speaker",
}

func chainSlotType(t string) (string, bool) {
	st, ok := chainTypeToSlot[normalizeChainType(t)]
	return st, ok
}

// resourceChainKinds are the chain item kinds that have **no request field** and can only be
// switched through their own endpoints.
//
// The criterion is "does it have its own field in chainRequest":
//
//   - crossfeed / surround / tube / dyn / exciter all do (crossfeed=... etc.), and the
//     semantics of their switches is handled by those fields (absent = unchanged);
//   - **fir (convolution) and ddc do not** -- they are switched through the two separate
//     endpoints /api/dsp/ir and /api/dsp/ddc, and there is not even a field for them in the
//     request body.
//
// So only these two kinds need "carrying over from the old chain", see
// preserveResourceChainItems.
var resourceChainKinds = map[string]bool{"fir": true, "ddc": true}

// preserveResourceChainItems carries resource-type chain items (convolution / DDC) that
// incoming does **not cover** over from existing, unchanged.
//
// WARNING: why this is required (hit in practice on 2026-09-20):
//
//		The `chain` field of GET /api/dsp/chain returns only the **EQ bands**
//		(chainFromSlots()); convolution and DDC are neither in that array nor covered by
//		**any request field**; and PUT's `chain` is a **wholesale replacement** -- one line in
//		applyChainInner, `currentUserChain = chain` (chain.go:324).
//		=> The **most ordinary** path, "read the chain -> edit the EQ -> write it back",
//		silently deletes the user's convolution and DDC.
//		The WebUI's applyNow() goes through it on every EQ change (webui/index.html:1360
//		sends only chain + the per-effect fields).
//
//	  - It cannot be heard at the time: for runtime state applyChainEffects is "not in the
//	    chain = unchanged", so the IR is still sounding at that moment; it is only **on the
//	    next restart** (restoring from state.json) that the lost configuration is noticed.
//	  - Actual consequence: the reference `thepbone-clear_bass.irs` and `mh750.vdc` were
//	    lost together once.
//
// The rule matches the other fields: **absent = unchanged**. To delete them use `?off=1` on
// their own endpoints; to give them explicitly in the chain (which is how preset / import
// files write them), include them in chain -- then the request wins.
func preserveResourceChainItems(incoming, existing []ChainItem) []ChainItem {
	if len(existing) == 0 {
		return incoming
	}
	covered := map[string]bool{}
	for _, it := range incoming {
		if kind, ok := chainEffectType[normalizeChainType(it.Type)]; ok && resourceChainKinds[kind] {
			covered[kind] = true
		}
	}
	out := append([]ChainItem(nil), incoming...)
	for _, it := range existing {
		kind, ok := chainEffectType[normalizeChainType(it.Type)]
		if !ok || !resourceChainKinds[kind] || covered[kind] {
			continue
		}
		out = append(out, it)
		covered[kind] = true
	}
	return out
}

// chainItemCheck explains whether a chain node can reach the hardware at the current stage:
// nil means it can (the biquad family); otherwise it returns the reason shown to the user.
func chainItemCheck(it ChainItem) error {
	nt := normalizeChainType(it.Type)
	if _, ok := chainTypeToSlot[nt]; ok {
		return nil
	}
	// chain-item crossfeed / surround: turned into runtime features (no EQ sections taken), but
	// **the hardware must have that slot** -- if it does not, "not supported" must be reported
	// honestly so the UI can grey it out (missing MIX2/DELAY on an old bitstream is normal).
	if kind, ok := chainEffectType[nt]; ok {
		switch kind {
		case "crossfeed":
			if !crossfeedAvailable() {
				return fmt.Errorf("effect %q needs the MIX2 slot (P2 capability: requires a 0.2 bitstream)", it.Type)
			}
		case "surround":
			if !surroundAvailable() {
				return fmt.Errorf("effect %q needs the DELAY slot (P2 capability: requires a 0.2 bitstream)", it.Type)
			}
		case "clarity":
			if !clarityAvailable() {
				return fmt.Errorf("effect %q needs the slot-table engine's BIQUAD (requires a 0.2 bitstream)", it.Type)
			}
		case "speaker":
			if !speakerCorrectionAvailable() {
				return fmt.Errorf("effect %q needs the slot-table engine's BIQUAD (requires a 0.2 bitstream)", it.Type)
			}
		case "viperbass":
			if !viperBassAvailable() {
				return fmt.Errorf("effect %q needs the MIX2 slot (P2 capability: requires a 0.2 bitstream)", it.Type)
			}
		case "colorfulmusic":
			if !colorfulAvailable() {
				return fmt.Errorf("effect %q needs the **joint stereo frame pass** (CAP1 bit12): "+
					"its DepthSurround is a joint stereo processor (it needs L and R in the same beat), which a per-channel engine cannot do", it.Type)
			}
		}
		return nil
	}
	if stage, ok := chainPlannedType[nt]; ok {
		return fmt.Errorf("effect %q needs a new engine (%s); the current hardware cannot do it", it.Type, stage)
	}
	return fmt.Errorf("unknown effect type %q", it.Type)
}

// chainToSlots maps the chain onto hardware slots.
//
// WARNING: the key difference from 0.1: **if it does not fit, it reports an error**. 0.1's
// ApplyToDSP was `if n > 6 { n = 6 }`, so an AutoEQ 10-section curve applied only the first
// 6 sections (31Hz...1kHz) and 2k/4k/8k/16k were silently dropped.
//
// Capacity is asked of `bandLimit()` (6 sections for the fixed cascade of a 0.1 bitstream /
// maxSections for the 0.2 slot-table engine), **never a hardcoded number**: hardcoding leads
// to the user being wronged by "the UI blocks at 6 sections while the hardware could actually
// hold 16".
func chainToSlots(chain []ChainItem) ([maxBands]slotConfig, int, error) {
	var slots [maxBands]slotConfig

	// first collect every node that is "enabled and able to reach the hardware"
	active := make([]slotConfig, 0, len(chain))
	for i, it := range chain {
		if !it.Enabled {
			continue
		}
		if err := chainItemCheck(it); err != nil {
			return slots, 0, fmt.Errorf("chain item %d: %w", i+1, err)
		}
		if _, isFx := chainEffectType[normalizeChainType(it.Type)]; isFx {
			continue // crossfeed / surround: not EQ sections, configured separately by applyChain
		}
		st, _ := chainSlotType(it.Type)
		active = append(active, slotConfig{
			Type:   st,
			Freq:   it.Freq,
			Q:      it.Q,
			GainDB: it.GainDB,
		})
	}

	if limit := bandLimit(); len(active) > limit {
		// tell the user explicitly "how many sections were dropped and which ones", instead of
		// silently truncating
		kept := make([]string, 0, limit)
		for _, sc := range active[:limit] {
			kept = append(kept, fmt.Sprintf("%s@%.0fHz", sc.Type, sc.Freq))
		}
		dropped := make([]string, 0, len(active)-limit)
		for _, sc := range active[limit:] {
			dropped = append(dropped, fmt.Sprintf("%s@%.0fHz", sc.Type, sc.Freq))
		}
		return slots, 0, fmt.Errorf("the effect chain has %d sections but the current hardware has only %d: the first %d can be applied (%s), "+
			"the following %d are **not applied** (turn off a card first, or enable fewer sections): %s",
			len(active), limit, limit, strings.Join(kept, ", "),
			len(dropped), strings.Join(dropped, ", "))
	}

	for i := 0; i < maxBands; i++ {
		if i < len(active) {
			slots[i] = sanitizeBand(active[i])
			continue
		}
		slots[i] = slotConfig{Type: "off"}
	}
	return slots, len(active), nil
}

// chainRequires returns the effect types used on the chain (deduplicated, stably sorted).
// The `requires` field in preset files corresponds to it: if the engine does not support one
// of them => the UI greys out that preset.
func chainRequires(chain []ChainItem) []string {
	set := map[string]bool{}
	for _, it := range chain {
		nt := normalizeChainType(it.Type)
		if nt == "" {
			continue
		}
		set[nt] = true
	}
	out := make([]string, 0, len(set))
	for t := range set {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// chainUnsupported returns the types on the chain that "cannot reach the hardware at the
// current stage" together with why (used for greying out / error reporting).
func chainUnsupported(chain []ChainItem) map[string]string {
	out := map[string]string{}
	for _, it := range chain {
		if err := chainItemCheck(it); err != nil {
			out[normalizeChainType(it.Type)] = err.Error()
		}
	}
	return out
}

// chainFromSlots reads the current hardware slots back into a chain (used by
// GET /api/dsp/chain).
// Only non-off sections appear in the chain, keeping the semantics of "the chain is what is
// in effect".
func chainFromSlots() []ChainItem {
	out := make([]ChainItem, 0, maxBands)
	for i := 0; i < maxBands; i++ {
		sc := currentSlots[i]
		if isBandOff(sc.Type) {
			continue
		}
		it := ChainItem{
			Type:    strings.ToLower(sc.Type),
			Enabled: true,
			Freq:    sc.Freq,
			GainDB:  sc.GainDB,
			Q:       sc.Q,
		}
		// hardware slot type -> canonical name on the chain (PK->peq / LS->lowshelf ...)
		switch sc.Type {
		case "PK":
			it.Type = "peq"
		case "LS":
			it.Type = "lowshelf"
		case "HS":
			it.Type = "highshelf"
		case "HP", "HPQ":
			it.Type = "highpass"
		case "LP", "LPQ":
			it.Type = "lowpass"
		case "NO":
			it.Type = "notch"
		case "BP":
			it.Type = "bandpass"
		case "AP":
			it.Type = "allpass"
		}
		out = append(out, it)
	}
	return out
}

// applyChain puts the chain onto the hardware: update the in-memory slots -> go through the
// existing wholesale write path.
// userPreampDB <= 0, where 0 means "let the automatic headroom decide" (see dsp_headroom.go).
// chainStateSnapshot is **all the in-memory state** that "pushing out one chain" can change.
// It is used for transactional rollback.
//
// Why rollback is required (measured on 2026-09-20): the handler's order is "change the
// in-memory state first -> then compile and push out".
// Once the push-out is rejected (too many sections, hardware write failure ...), memory
// already holds the **new** state while the hardware is still on the **old** chain: the API
// reported an error but the effects are "half in effect", and the next arbitrary push-out
// **carries** that half-applied state along.
// Hit in practice: a rejected request left the exciter and tube flavor in memory, and they
// stayed on the chain from then on.
//
// WARNING: 2026-09-22 gap filled: this snapshot originally missed **Clarity /
// SpeakerCorrection / AnalogX** (the three runtime effects added when A4/A2 landed), yet
// those three are exactly on a par with dyn/cross/tube -- the same pit with the same shape:
// "a rejected request left these three in memory".
// The guard is `TestChainSnapshotCoversEveryRuntimeEffect` (adding a new effect and
// forgetting to register it turns it red).
type chainStateSnapshot struct {
	dyn     *dynParams
	dynbass *dynamicBassParams
	cross   *crossfeedParams
	sur     *surroundParams
	exc     *exciterParams
	bass    *viperBassParams
	tube    *tubeParams
	col     *colorfulParams
	clarity *clarityParams
	spk     *speakerCorrectionParams
	analogx *analogxParams
	chain   []ChainItem
	slots   [maxBands]slotConfig
	preamp  float64
	gain    float64
}

func captureChainState() chainStateSnapshot {
	return chainStateSnapshot{
		dyn: currentDynBass, dynbass: currentDynamicBass,
		cross: currentCrossfeed, sur: currentSurround,
		exc: currentExciter, bass: currentViPERBass, tube: currentTube, col: currentColorful,
		clarity: currentClarity, spk: currentSpeakerCorrection, analogx: currentAnalogX,
		chain: currentUserChain, slots: currentSlots,
		preamp: currentPreampDB, gain: chainGainDB,
	}
}

func (s chainStateSnapshot) restore() {
	currentDynBass, currentDynamicBass = s.dyn, s.dynbass
	currentCrossfeed, currentSurround = s.cross, s.sur
	currentExciter, currentTube = s.exc, s.tube
	currentViPERBass = s.bass
	currentColorful = s.col
	currentClarity = s.clarity
	currentSpeakerCorrection = s.spk
	currentAnalogX = s.analogx
	currentUserChain, currentSlots = s.chain, s.slots
	currentPreampDB, chainGainDB = s.preamp, s.gain
}

// applyChain is **transactional**: on success it takes effect, on failure the snapshot above
// is put back exactly as it was.
// If the hardware step did not go through => after the rollback "memory == hardware" and the
// two still agree (no half-applied state).
//
// WARNING: it does not cover loading the IR/DDC **coefficients** (`loadIRNamed` reads the
// convolution kernel into irBank):
//
//	that part is large, and their own endpoints report errors separately; all this guarantees
//	is that "effect state + chain + slots + gain" agree.
func applyChain(chain []ChainItem, userPreampDB, userGainDB float64) error {
	snap := captureChainState()
	err := applyChainInner(chain, userPreampDB, userGainDB)
	if err != nil {
		snap.restore()
		log.Printf("chain push-out failed; the in-memory state has been rolled back (the hardware keeps the old chain): %v", err)
	}
	return err
}

func applyChainInner(chain []ChainItem, userPreampDB, userGainDB float64) error {
	// chain-item crossfeed / surround: **turned into runtime features here** (they are not EQ
	// sections).
	// The semantics match the API's separate fields: the item is in the chain => set/clear it
	// according to enabled; it is not in the chain => unchanged (the panel sends them in
	// separate fields while the preset library writes them in the chain -- both paths have to
	// work).
	applyChainEffects(chain)

	// The chain carries an IR name but the in-memory one is a different one => **read it back
	// by name**.
	// This step is the safety net for "after a restart the chain is still there and the
	// coefficients are only in memory": without reading them back the FIR would run with zero
	// coefficients (it sounds like silence while the API looks perfectly normal). If the
	// read-back fails, report the error honestly and never continue silently.
	if currentFIR != nil && currentFIR.Name != "" && currentFIR.Name != irLoadedName() {
		if err := loadIRNamed(currentFIR.Name); err != nil {
			return err
		}
	}

	// The loudness compensation (equal loudness) is merged in here: it is an overall
	// compensation that **follows the volume automatically** and belongs to no effect card, so
	// it does not enter the user's chain data and is merged only at the moment of reaching the
	// hardware (added at the same frequency point).
	currentUserChain = chain
	slots, _, err := chainToSlots(withLoudness(chain))
	if err != nil {
		return err
	}
	currentSlots = slots
	currentPreampDB = userPreampDB
	if userGainDB < 0 { // only ever in the "louder" direction; for a deeper cut use preamp_db
		userGainDB = 0
	}
	if userGainDB > 12 {
		userGainDB = 12
	}
	chainGainDB = userGainDB
	if err := dspWriteAllBands(); err != nil {
		return err
	}
	// "should be saved" is only recorded after the hardware has been changed (see state.go):
	// a failed write must not make these settings count as "the previous ones"
	markStateDirty()
	return nil
}

// applyChainEffects handles the "non-EQ effect items" in the chain (crossfeed / surround).
//
// Why this is needed: in the preset library (presets/library.json) and in import files these
// two effects are in **chain item** form
// (`{"type":"crossfeed","enabled":true,"level":3}`), while the panel sends them as **separate
// fields**. Supporting only the latter would report "unknown effect type" as soon as a preset
// is applied.
//
// WARNING: the parameter mapping stays honest: the chain item's `level` (V4A's tier) has **no
// reliable primary-source mapping**, so only "on/off + the default tier" is used here and the
// concrete value of `level` is not interpreted as fcut/feed -- that would be guesswork.
func applyChainEffects(chain []ChainItem) {
	for _, it := range chain {
		kind, ok := chainEffectType[normalizeChainType(it.Type)]
		if !ok {
			continue
		}
		if !it.Enabled {
			switch kind {
			case "crossfeed":
				currentCrossfeed = nil
			case "surround":
				currentSurround = nil
			case "fir":
				currentFIR = nil
			case "ddc":
				ddcClear()
			case "tube":
				currentTube = nil
			case "clarity":
				currentClarity = nil
			case "speaker":
				currentSpeakerCorrection = nil
			case "analogx":
				currentAnalogX = nil
			case "viperbass":
				currentViPERBass = nil
			case "colorfulmusic":
				currentColorful = nil
			}
			continue
		}
		switch kind {
		case "crossfeed":
			if currentCrossfeed == nil && crossfeedAvailable() {
				p := crossfeedDefaultParams() // bs2b Light tier (700 Hz / 6.0 dB)
				currentCrossfeed = &p
			}
		case "viperbass":
			// chain item form: params["mode"] / params["cutoff_hz"] / params["gain"] can
			// override the panel values; by default the V4A client's default tier is used
			// (Natural Bass / 40 Hz / 50).
			if currentViPERBass == nil && viperBassAvailable() {
				p := viperBassDefaultParams()
				if v, ok := it.Params["mode"]; ok {
					p.Mode = int(v)
				}
				if v, ok := it.Params["cutoff_hz"]; ok {
					p.CutoffHz = v
				} else if v, ok := it.Params["freq"]; ok {
					p.CutoffHz = v
				}
				if v, ok := it.Params["gain"]; ok {
					p.Gain = v
				}
				if err := setViPERBass(&p); err != nil {
					log.Printf("the ViPERBass in the chain item is invalid (%v) => skipping this stage", err)
				}
			}
		case "colorfulmusic":
			// chain item form: only expresses "enable this stage"; the parameters use the
			// **official client defaults** (coeffs "120;200" + midimage "150"). For non-default
			// parameters use the separate fields.
			if currentColorful == nil && colorfulAvailable() {
				p := colorfulDefaultParams()
				if err := setColorfulMusic(&p); err != nil {
					log.Printf("the ColorfulMusic in the chain item is invalid (%v) => skipping this stage", err)
				}
			}
		case "tube":
			if currentTube == nil && tubeAvailable() {
				currentTube = &tubeParams{}
			}
		case "analogx":
			if currentAnalogX == nil && analogxAvailable() {
				p := analogxDefaultParams()
				if v, ok := it.Params["model"]; ok {
					p.Model = int(v)
				}
				if err := setAnalogX(&p); err != nil {
					log.Printf("the AnalogX in the chain item is invalid (%v) => skipping this stage", err)
				}
			}
		case "clarity":
			// first half of A4: one parameter per tier (tier + amount). A chain item can
			// override them through params; the tier can also be written by naming it directly
			// as `"type": "ozone"` (more intuitive in preset files).
			if currentClarity == nil && clarityAvailable() {
				p := clarityDefaultParams()
				if m, ok := clarityModeFromString(it.Type); ok {
					p.Mode = m
				}
				if v, ok := it.Params["mode"]; ok {
					p.Mode = int(v)
				}
				if v, ok := it.Params["level"]; ok {
					p.Level = v
				}
				if err := setClarity(&p); err != nil {
					log.Printf("the Clarity in the chain item is invalid (%v) => skipping this stage", err)
				}
			}
		case "speaker":
			// second half of A4: V4A SpeakerCorrection, with no adjustable parameters (the
			// three frequencies/Q are hardcoded).
			if currentSpeakerCorrection == nil && speakerCorrectionAvailable() {
				currentSpeakerCorrection = &speakerCorrectionParams{}
			}
		case "fir":
			// convolution stage: the chain item only expresses "this stage convolves, with how
			// many taps"; the coefficients (IR) are loaded by /api/dsp/ir into their own address
			// space (starting at coefFIRBase). When the hardware has no FIR slot nothing is set
			// => the chain genuinely has no such stage (no silent downgrade into something
			// else).
			if firAvailable() {
				p := firParams{Taps: firTaps}
				if v, ok := it.Params["taps"]; ok && v > 0 {
					p.Taps = int(v)
				}
				if nm := it.Name; nm != "" {
					// the chain carries an IR name: read the coefficients back by name (after a
					// restart the chain is still there and the coefficients are only in memory;
					// without the read-back the FIR would run with zero coefficients = silence,
					// while the API looks perfectly normal).
					// Only the **name is remembered** here; the actual read-back is done in one
					// place further down (to avoid putting IO inside the lock).
					p.Name = nm
				}
				currentFIR = &p
			}
		case "ddc":
			// the chain carries a name => read it back from disk by name and fit it (after a
			// restart the chain is still there and so is the file)
			if it.Name != "" {
				// the section count is taken from the chain item (chosen when loading), default
				// 4 -- hardcoding 4 would lose the steepness of a 16-section fit
				sec := int(chainItemParam("ddc", "sections", 4))
				// native=1 means it was originally "loaded as-is with the file's own sections"
				// => the read-back must also go as-is, otherwise a 20-section source file would
				// be refitted to 20 sections (same count, different shape, silently changed
				// sound).
				nat := chainItemParam("ddc", "native", 0) != 0
				// WARNING: the upper bound is maxSections, **not a hardcoded 16**: since the
				//    section capacity grew to 36, hardcoding 16 would silently downgrade a
				//    20/36-section fitted DDC to 4 sections (the shape changes, the API reports
				//    nothing). This number does not take part in native loading (it loads the
				//    file's own sections), so it only affects the fitting path.
				if sec < 1 || sec > hwMaxSections {
					sec = 4
				}
				if err := loadDDCNamed(it.Name, sec, nat); err != nil {
					log.Printf("failed to read back DDC %q: %v", it.Name, err)
				}
			}
		case "surround":
			if currentSurround == nil && surroundAvailable() {
				p := surroundDefaultParams()
				// if the chain item gives delay_ms explicitly, use it (this field's meaning is
				// unambiguous)
				if v, ok := it.Params["delay_ms"]; ok && v > 0 {
					p.DelayMs = v
				}
				currentSurround = &p
			}
		}
	}
}
