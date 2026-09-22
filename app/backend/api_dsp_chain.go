// SPDX-License-Identifier: GPL-2.0-only
// api_dsp_chain.go -- the 0.2 DSP interface: capability self-description / effect chain / presets
//
// Design intent:
//   · The UI no longer guesses what the hardware supports -- it reads /api/dsp/capabilities first
//     and greys out unsupported effects and presets accordingly;
//   · A preset is not a black box -- the list returns the chain contents and applying it means
//     "put this chain onto the hardware";
//   · Import goes through the EqualizerAPO subset parser plus curve fitting, **and returns the
//     warnings to the user truthfully** ("which lines were not applied, how large the fitting
//     error is"), never dropping anything silently.

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
)

// ── Capability self-description ───────────────────────────────────────

func handleDSPCapabilities(w http.ResponseWriter, r *http.Request) {
	types := make([]string, 0, len(chainTypeToSlot))
	seen := map[string]bool{}
	for _, slotType := range chainTypeToSlot {
		// Normalise to the canonical name (several aliases map to one slot type, reported once)
		name := canonicalChainType(slotType)
		if !seen[name] {
			seen[name] = true
			types = append(types, name)
		}
	}
	sort.Strings(types)
	planned := map[string]string{}
	for t, why := range chainPlannedType {
		planned[t] = why
	}
	// Engine capability follows the **hardware's own report** (CAP0's magic decides old vs new bitstream); the software model is only a fallback
	engineName := "biquad-hardwired" // 0.1: the hardware really is 6 fixed biquads
	// ⚠️ `bands` reports **how many bands the software chain can really take right now** -- it must
	//    come from the **same source** as the actual capacity of `chainToSlots()` and the band
	//    count `dspWriteAllBands()` can push down: `bandLimit()`.
	//    Historically this reported maxSections(16) while the chain only took 6 bands (the UI then
	//    let the user build a chain that was bound to 400 = lying), and it also reported 6 while the
	//    engine could really hold 16 (the UI blocking what the user could do = unfair); both times
	//    "the reported number and the real capacity had parted ways". Now bandLimit() decides both
	//    together, so they cannot split:
	//      · 0.1 bitstream: 6 (the 6-band cascade hard-coded in the RTL);
	//      · 0.2 slot-table engine: maxSections (clamped jointly by the coefficient/state RAM and
	//        the slot count).
	hwCaps := dspEngineCaps{}
	if dspEngineGen == 1 {
		engineName = "slot-table" // 0.2: slot-table engine
		hwCaps = dspReadEngineCaps()
	}
	caps := map[string]any{
		"engine":        engineName,
		"engine_gen":    dspEngineGen, // 0 = old bitstream (no slot table), 1 = 0.2 slot-table engine
		"bands":         bandLimit(),
		"types":         types,
		"planned":       planned,
		"min_freq_hz":   minBandFreq,
		"max_freq_hz":   maxBandFreq,
		"sample_rate":   int(sampleRate),
		"dsp_available": dspAvailable,
		"limiter": map[string]any{
			"available": true,
			"bypass":    !dspLimiterEnabled,
			// Two modes: feedback = the 0.1 feedback type (no lookahead); truepeak = the 0.2 lookahead version
			"mode": limiterModeName(),
			// Report only what the **hardware really supports**: look straight at CAP1 bit9 (whether
			// the limiter is actually compiled into this bitstream) instead of using "engine
			// generation == 1" as a proxy -- proxies drift away from the facts
			"truepeak_available": hwCaps.TruepeakAvailable(),
			// Truthfully report the time constant **actually achieved**: Q1.15 can express at most
			// ~341 ms, and longer requests are clamped (see limMaxMsTP in dsp_limiter_tp.go)
			"att_ms":     limMsFromKTP(limKFromMsTP(dspLimiterAttMs)),
			"rel_ms":     limMsFromKTP(limKFromMsTP(dspLimiterRelMs)),
			"rel_ms_max": limMaxMsTP,
			"note":       "the feedback mode has no lookahead and produces waveform discontinuities on 0 dBFS material (about 1770 per second); the truepeak mode looks ahead 2 ms and smooths in the log domain, and that number measures 0",
		},
	}
	// Dynamic bass: availability **is reported by the hardware** (CAP1 bit11), not proxied by
	// "engine generation == 1".
	// When the board still runs an old bitstream without DYN, the UI must grey this item out
	// truthfully, otherwise the user drags it and gets a 400.
	db := map[string]any{"available": dynBassAvailable(), "on": currentDynBass != nil}
	if cur := currentDynBass; cur != nil {
		db["gain_db"] = cur.GainDB
		db["cut_db"] = cur.CutDB
		db["ref_db"] = cur.RefDB
		db["ks"] = cur.KS
		db["att_ms"] = cur.AttMs
		db["rel_ms"] = cur.RelMs
	}
	caps["dyn_bass"] = db
	// Same for crossfeed: availability comes from CAP1 bit3 (MIX2)
	caps["crossfeed"] = map[string]any{
		"available": crossfeedAvailable(),
		"on":        currentCrossfeed != nil,
		"view":      crossfeedView(),
		// A6 item 1: the three tiers' panel names + values + which tier is this device's default (= Moderate), with sources alongside the table.
		"tiers": crossfeedTiersView(),
	}
	// Convolver (FIR slot): availability comes from CAP1 bit5 (see fir_bank.v).
	// Reporting the ceiling and the coefficient base address keeps the UI/documentation from
	// hard-coding the number "4096".
	caps["fir"] = map[string]any{
		"available": firAvailable(),
		"taps":      firTaps,
		"coef_base": hwCoefFIRBase,
	}
	// Tube flavour (V4A TubeSimulator): BIQUAD alone is enough, no extra hardware requirement.
	caps["tube"] = map[string]any{
		"available": tubeAvailable(),
		"on":        currentTube != nil,
		"view":      tubeView(),
	}
	// ViPERClarity (A4 upper/lower half): NATURAL / OZONE take 1 band each; **XHIFI is implemented
	// too** (three parallel branches plus two branch delays, see clarityModeXHIFI in clarity.go and
	// xhifi.go).
	// ⚠️ This used to say `"modes": ["natural","ozone"]` + `"unsupported": ["xhifi"]` -- a leftover
	//    from early 0.2 (before XHIFI landed) that had parted ways with the implementation: the
	//    baseline chain itself uses `clarity=xhifi`.
	//    It now reports truthfully, following the implementation. XHIFI's **hardware prerequisite**
	//    is two delay slots (CAP3[23:16] >= 2), and xhifiNode reports that explicitly during planning
	//    instead of proxying it with "is the engine there" here.
	caps["clarity"] = map[string]any{
		"available":   clarityAvailable(),
		"on":          currentClarity != nil,
		"view":        clarityView(),
		"modes":       []string{"natural", "ozone", "xhifi"},
		"level_range": []float64{0, 100}, // V4A's raw panel value (ViPER.cpp:362-364 SetClarity(val1/100))
		"xhifi_slots": 2,                 // XHIFI needs two non-interfering delay slots (fs/400 and fs/200)
		"delay_slots": hwDelaySlots,
		"unsupported": []string{},
	}
	// AnalogX (A2 upper half): three tiers (0.6/1.2/2.4 with 19650/18233/16307), needs a POLY slot.
	// The tier table is **derived from analogxTiers** (analogxTiersView) and no numbers are copied
	// here -- this used to hard-code a second copy of the three-tier table, so changing a source
	// constant could miss it here ("the number the interface reports parts ways with the
	// implementation's").
	caps["analogx"] = map[string]any{
		"available": analogxAvailable(),
		"on":        currentAnalogX != nil,
		"view":      analogxView(),
		"tiers":     analogxTiersView(),
	}
	// SpeakerCorrection (A4 lower half): 3 bands, no adjustable parameters (the three frequencies/Q are hard-coded in V4A).
	caps["speaker_correction"] = map[string]any{
		"available": speakerCorrectionAvailable(),
		"on":        currentSpeakerCorrection != nil,
		"view":      speakerCorrectionView(),
		"sections":  3,
	}
	// The four A2' parameter presets (three AnalogX tiers + VSE). See fxpresets.go: they
	// **introduce no new state fields**, they merely register the two existing parameter sets as one
	// table with primary sources and provide a one-line entry point
	// (PUT /api/dsp/chain {"fx_preset":"analogx-moderate"}).
	caps["fx_presets"] = fxPresetsView()
	// Harmonic exciter (nonlinear harmonic stage): availability comes from CAP1 bit6 (POLY).
	// It is the **only nonlinear** opcode in the engine, and V4A's SpectrumExtend / AnalogX both
	// rely on it.
	caps["exciter"] = map[string]any{
		"available": exciterAvailable(),
		"on":        currentExciter != nil,
		"view":      exciterView(),
	}
	// VSE (V4A SpectrumExtend): structurally the same thing as the "harmonic exciter" (see vse.go),
	// so its availability criterion is POLY too (CAP1 bit6). It is reported separately because it is
	// its own card in the panel (gears 0.1...1.0), while the `exciter` entry is a custom shape.
	caps["vse"] = map[string]any{
		"available": vseAvailable(),
		"on":        vseView() != nil,
		"view":      vseView(),
		// The 10 panel gears plus the in-core parameters each one converts to (sources in the head of vse.go)
		"gears": vseGearTable(),
	}
	// ViPERBass (bass enhancement): availability comes from MIX2 (CAP1 bit3).
	// ⚠️ **The modes differ in capability**: NATURAL needs only MIX2, while PURE_BASS_PLUS also
	//    needs a **second small FIR** (CAP1 bit7 + the capacity CAP4 reports). The UI must grey them
	//    out separately -- proxying with "is the engine there" makes the user pick PBP and then get
	//    an "apply rejected" error (or worse: an unfaithful approximation).
	caps["viperbass"] = map[string]any{
		"available":     viperBassAvailable(),
		"pbp_available": sfirAvailableNow(),
		"on":            currentViPERBass != nil,
		"view":          viperBassView(),
		"modes":         []string{"natural", "pure_bass_plus", "subwoofer"},
		// Panel steps (vbass_freq / vbass_boost from arrays.xml) -- the UI builds its dropdowns from these
		"cutoff_hz": []float64{30, 40, 50, 60, 66, 78, 80, 100},
		"gain":      []float64{50, 100, 150, 200, 250, 300, 350, 400, 450, 500, 550, 600},
	}
	// Same for surround: availability comes from CAP1 bit4 (DELAY)
	caps["surround"] = map[string]any{
		"available": surroundAvailable(),
		"on":        currentSurround != nil,
		"view":      surroundView(),
	}
	// ColorfulMusic (V4A field surround): availability comes from **CAP1 bit12 (joint-stereo frame
	// pass)**.
	// Without that bit it is explicitly greyed out / rejected -- never silently degraded (see
	// setColorfulMusic in colorful.go).
	caps["colorfulmusic"] = map[string]any{
		"available": colorfulAvailable(),
		"on":        currentColorful != nil,
		"view":      colorfulView(),
		// The client's value ranges (arrays.xml); the UI builds its sliders from these
		"client_widening_range": []float64{120, 200},
		"client_depth_range":    []float64{200, 800},
		"client_midimage_range": []float64{100, 200},
	}
	if dspEngineGen == 1 {
		caps["hardware"] = map[string]any{
			"version":      hwCaps.Version,
			"opcodes":      hwCaps.Opcodes, // bitmap: bit n = opcode n supported (NOP/BIQUAD/SAT...)
			"num_slots":    hwCaps.NumSlots,
			"fir_max_log2": hwCaps.FirMaxLog2,
			"delay_log2":   hwCaps.DelayLog2,
			// How many **non-interfering** delay slots each channel gets (CAP3[23:16]). 0 = old bitstream
			// => counted as 1: the old bitstream's delay ring has a single write pointer, so two delay
			// slots step on each other within one frame (XHIFI needs two).
			"delay_slots":  hwDelaySlots,
			"max_sections": hwMaxSections,
			// In-chain headroom: whether this bitstream compiles it (CAP1 bit10), whether it is currently
			// on, and how many dB. Only with it can "turning effects on does not change the overall
			// loudness" hold (see dspHeadroomUsable in dsp.go).
			"headroom_available": hwCaps.HeadroomAvailable(),
			"headroom_on":        dspHeadroomOn,
			"headroom_db":        headroomDB,
			"status":             dspReadStatus(),
			// Dropped-sample count (STATUS[31:24], saturating). **Non-zero = the engine once dropped
			// samples because the FSM could not keep up** => that is one "pop". This is the number that
			// turns "an occasional small noise" from a listening impression into a countable event:
			// sample it every few seconds and watch the increment to line up "the pop I heard" with "one
			// sample was dropped".
			// As a counter-check: if this number stays 0 while the ear still hears pops, the problem is
			// not in the engine (look downstream at i2s_tx/codec or upstream at the FIFO/network) and
			// there is no need to guess further.
			"overrun_count": (dspReadStatus() >> 24) & 0xFF,
		}
	}
	if lib, err := loadPresetLibrary(); err == nil {
		user, _ := loadUserPresets()
		caps["presets"] = map[string]int{"builtin": len(lib), "user": len(user)}
	} else {
		caps["presets"] = map[string]int{"builtin": 0, "user": 0}
		caps["preset_library_error"] = err.Error()
	}
	writeJSON(w, caps)
}

// canonicalChainType turns a hardware slot type (PK/LS/HS/HP/LP/NO/BP/AP) back into the canonical chain name.
func canonicalChainType(slotType string) string {
	switch slotType {
	case "PK":
		return "peq"
	case "LS":
		return "lowshelf"
	case "HS":
		return "highshelf"
	case "HP", "HPQ":
		return "highpass"
	case "LP", "LPQ":
		return "lowpass"
	case "NO":
		return "notch"
	case "BP":
		return "bandpass"
	case "AP":
		return "allpass"
	}
	return strings.ToLower(slotType)
}

// ── Effect chain read/write ───────────────────────────────────────────

type chainRequest struct {
	Chain    []ChainItem `json:"chain"`
	PreampDB float64     `json:"preamp_db"`
	// GainDB is the user-facing "overall gain" (0..+12 dB): it takes back the volume the automatic
	// headroom gave away, at the cost of peaks being clipped by the limiter. The loudness/distortion
	// trade-off is the user's to make (see chainGainDB in dsp.go).
	GainDB float64 `json:"gain_db"`
	// A non-nil DynBass enables dynamic bass (the DYN stage); an explicit null turns it off.
	// Absent (the field does not appear) means "leave the current setting alone" -- consistent with
	// chain's semantics and keeping a forgotten field from being read as "off".
	DynBass *dynBassRequest `json:"dyn_bass"`
	// A non-nil DynamicBass enables **the simple branch of V4A `DynamicBass`** (see dynbass.go);
	// off=true = off. Absent (the field does not appear) = "leave the current setting alone",
	// consistent with the other effect fields.
	//
	// ⚠️ It and `dyn_bass` are **two different effects**: `dyn_bass` is this device's DYN stage
	// (side-chain detection + slow gain, ~ V4A PlaybackGain), while `dynamic_bass` is V4A's
	// DynamicSystem.
	DynamicBass *dynamicBassRequest `json:"dynamic_bass"`
	// Type is the **name of the "type" currently selected in the panel** (pure software state, the
	// hardware does not know it). It takes no part in coefficient computation and is only stored in
	// the state file together with the chain (see state.go): after a restart the panel selects the
	// same card from it.
	Type string `json:"type"`
	// A non-nil Surround enables the sound stage (Haas: the right channel is delayed by a few ms); off=true = off.
	Surround *surroundRequest `json:"surround"`
	// A non-nil Crossfeed enables headphone crossfeed (the MIX2 stage); off=true or an entirely empty
	// object = off.
	// Absent (the field does not appear) = "leave the current setting alone", consistent with the
	// chain / dyn_bass semantics.
	Crossfeed *crossfeedRequest `json:"crossfeed"`
	// A non-nil Exciter enables the harmonic exciter (the POLY stage); off=true = off.
	Exciter *exciterRequest `json:"exciter"`
	// A non-nil Tube enables the tube flavour (V4A TubeSimulator: a first-order moving average); off=true = off.
	Tube *tubeRequest `json:"tube"`
	// A non-nil Clarity enables V4A ViPERClarity (NATURAL/OZONE); off=true = off.
	Clarity *clarityRequest `json:"clarity"`
	// A non-nil SpeakerCorrection enables V4A SpeakerCorrection (3 bands, no adjustable parameters); off=true = off.
	SpeakerCorrection *speakerCorrectionRequest `json:"speaker_correction"`
	// A non-nil AnalogX enables the three V4A AnalogX tiers (HP240+POLY+MIX2+LP+Peak633); off=true = off.
	AnalogX *analogxRequest `json:"analogx"`
	// A non-nil VSE enables V4A SpectrumExtend by gear (see vse.go); off=true = off.
	// ⚠️ It lands on the **same** POLY stage as `Exciter`, so the two are "two entrances to one
	// card": whichever comes later in the request wins.
	VSE *vseRequest `json:"vse"`
	// A non-nil ViPERBass enables bass enhancement (see viperbass.go); off=true = off.
	ViPERBass *viperBassRequest `json:"viperbass"`
	// A non-nil ColorfulMusic enables V4A field surround (joint-stereo frame pass); off=true = off.
	// Absent (the field does not appear) = "leave the current setting alone", consistent with the
	// rest of the fields.
	ColorfulMusic *colorfulRequest `json:"colorfulmusic"`
	// FXPreset is the name of one of the four A2' parameter presets (`analogx-slight` /
	// `analogx-moderate` / `analogx-extreme` / `vse`, see fxpresets.go): one name **pushes a whole
	// parameter set down in one line**.
	//
	// Semantics: it takes effect **before the other effect fields**, so an explicit `analogx` / `vse`
	// in the same request overrides it ("apply the preset first, then fine-tune"). It writes only the
	// analogx and exciter cards and touches no other effect; turning off still uses their own `off`
	// fields (`analogx.off` / `vse.off`). An unknown name is an **error**, not rounded to the nearest
	// match (a gear the panel does not have is not ours to invent -- same reason as vseReconstruct).
	FXPreset *string `json:"fx_preset"`
}

// vseRequest is the wire format of VSE (SpectrumExtend).
// Gear takes one of the 10 discrete panel gears 0.1/0.2/.../1.0 (vse_strength_values in arrays.xml);
// the in-core parameter = round(Gear*5.6*100) (the client's line, see vse.go).
type vseRequest struct {
	Gear *float64 `json:"gear"`
	Off  *bool    `json:"off"`
}

// viperBassRequest is the wire format of ViPERBass (see viperbass.go).
// Mode 0/1/2 = Natural Bass / Pure Bass+ / Subwoofer; CutoffHz = the panel low-freq (30...100);
// Gain = the panel boost (50...600).
// ⚠️ mode=1 (PBP) requires a small FIR in the hardware (CAP1 bit7 + CAP4); without it an explicit
// error is returned,
//
//	never a silent degradation to NATURAL (see setViPERBass).
type viperBassRequest struct {
	Mode     *int     `json:"mode"`
	CutoffHz *float64 `json:"cutoff_hz"`
	Gain     *float64 `json:"gain"`
	Off      *bool    `json:"off"`
}

// analogxRequest is the wire format of AnalogX (see analogx.go): only the tier 0/1/2.
type analogxRequest struct {
	Model *int  `json:"model"`
	Off   *bool `json:"off"`
}

// clarityRequest is the wire format of ViPERClarity (see clarity.go).
// Mode takes `"natural"` / `"ozone"` / `"xhifi"` (all three are implemented); `off=true` turns it off.
// ⚠️ XHIFI needs **two delay slots** => it can only be pushed down when the bitstream has
// CAP3[23:16] >= 2 (an old bitstream is rejected at compile time).
type clarityRequest struct {
	Mode  string   `json:"mode"`
	Level *float64 `json:"level"` // V4A's raw panel value 0..100
	Off   *bool    `json:"off"`
}

// speakerCorrectionRequest has only a switch (the three frequencies/Q are hard-coded in V4A).
type speakerCorrectionRequest struct {
	Off *bool `json:"off"`
}

// exciterRequest is the wire format of the harmonic exciter (see exciterParams in harmonic.go).
// Harmonics holds 10 harmonic amplitudes (the 1st..10th); the default is V4A SpectrumExtend's
// factory shape.
type exciterRequest struct {
	Harmonics []float64 `json:"harmonics"`
	Mix       *float64  `json:"mix"`
	HPFHz     *float64  `json:"hpf_hz"`
	LPFHz     *float64  `json:"lpf_hz"`
	Off       *bool     `json:"off"`
}

// colorfulRequest is the wire format of ColorfulMusic (see colorfulParams in colorful.go).
// ⚠️ All three fields are **core conventions** (not the client panel's integers):
//
//	depth     = the second number in the client's coeffs (200..800, core 0..1000)
//	widening  = the first number in the client's coeffs / 100 (client 120..200 => 1.2..2.0)
//	mid_image = the client's midimage / 100 (client 100..200 => 1.0..2.0)
type colorfulRequest struct {
	Depth    *int     `json:"depth"`
	Widening *float64 `json:"widening"`
	MidImage *float64 `json:"mid_image"`
	Off      *bool    `json:"off"`
}

// surroundRequest is the wire format of the sound stage (see surroundParams in surround.go).
type surroundRequest struct {
	DelayMs *float64 `json:"delay_ms"`
	Off     *bool    `json:"off"`
}

// crossfeedRequest is the wire format of crossfeed (see crossfeedParams in crossfeed.go).
type crossfeedRequest struct {
	FcutHz *float64 `json:"fcut_hz"`
	Feed   *float64 `json:"feed"`
	// PassFilter only adds V4A `Cure`'s final stage when explicitly given as true (the default keeps pure bs2b = the JamesDSP shape).
	PassFilter *bool `json:"pass_filter"`
	Off        *bool `json:"off"`
}

// tubeRequest is the wire format of the tube flavour (see tube.go: V4A TubeSimulator has only a switch, no parameters).
type tubeRequest struct {
	Off *bool `json:"off"`
}

// dynamicBassRequest is the wire format of V4A DynamicBass (**following the client's convention**,
// see dynbass.go):
//   - coeffs: "x1;x2;x3;x4;x5;x6" from the client's `dynamicsystem.coeffs`;
//   - bass: the percentage (0..100) from the client's `dynamicsystem.bass`.
//
// x1 > 120 (the full branch) is **explicitly rejected**, not passed off as an approximation.
type dynamicBassRequest struct {
	Coeffs *string  `json:"coeffs"`
	Bass   *float64 `json:"bass"`
	Off    *bool    `json:"off"`
}

// dynBassRequest is the wire format of dynamic bass (all dB / ms, see dynParams in dyn.go).
type dynBassRequest struct {
	GainDB *float64 `json:"gain_db"`
	CutDB  *float64 `json:"cut_db"`
	RefDB  *float64 `json:"ref_db"`
	KS     *float64 `json:"ks"`
	AttMs  *float64 `json:"att_ms"`
	RelMs  *float64 `json:"rel_ms"`
	Off    *bool    `json:"off"`
}

func handleDSPChain(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, map[string]any{
			"chain":              chainFromSlots(),
			"type":               selectedTypeName,
			"preamp_db":          currentPreampAppliedDB,
			"bands":              bandLimit(),
			"dsp_available":      dspAvailable,
			"unsupported":        chainUnsupported(chainFromSlots()),
			"dyn_bass":           dynBassView(),
			"dynamic_bass":       dynamicBassView(),
			"crossfeed":          crossfeedView(),
			"surround":           surroundView(),
			"colorfulmusic":      colorfulView(),
			"exciter":            exciterView(),
			"vse":                vseView(),
			"viperbass":          viperBassView(),
			"tube":               tubeView(),
			"clarity":            clarityView(),
			"speaker_correction": speakerCorrectionView(),
			"analogx":            analogxView(),
			// A2': which parameter presets are currently hit (empty = none).
			// Like vseView / analogxView this is **back-derived**, with no second copy of the tier stored
			// (so two states cannot fight each other).
			// The complete parameter table of the four presets is in `fx_presets` under
			// /api/dsp/capabilities.
			"fx_preset": fxPresetCurrent(),
		})
	case http.MethodPut:
		var req chainRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
			httpError(w, http.StatusBadRequest, "request body is not valid JSON: "+err.Error())
			return
		}
		if len(req.Chain) == 0 {
			// An empty chain = fully flat (legal), but [] must be passed explicitly, so a forgotten
			if req.Chain == nil {
				httpError(w, http.StatusBadRequest, "chain field is missing (pass [] explicitly for an empty chain)")
				return
			}
		}
		// ⚠️ Take a snapshot **before changing any effect state**: the `setXxx` calls in this handler
		//    are driven by explicit fields and live **outside** `applyChain`'s snapshot -- relying on
		//    applyChain's rollback alone would miss them
		//    (measured: a rejected request left crossfeed behind in memory).
		reqSnap := captureChainState()

		// Dynamic bass: update when the field is given explicitly (off=true or an entirely empty object = off)
		if req.Type != "" {
			selectedTypeName = req.Type
		}
		// A2' parameter presets: pushed down **first**, so more specific fields in the same request can
		// override them ("apply the preset first, then fine-tune"). They write only the analogx /
		// exciter cards and do not interact with other fields.
		if req.FXPreset != nil {
			if err := applyFXPreset(*req.FXPreset); err != nil {
				reqSnap.restore()
				httpError(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		if req.DynBass != nil {
			p := dynDefaultParams()
			if cur := currentDynBass; cur != nil {
				p = *cur
			}
			r := req.DynBass
			if r.GainDB != nil {
				p.GainDB = *r.GainDB
			}
			if r.CutDB != nil {
				p.CutDB = *r.CutDB
			}
			if r.RefDB != nil {
				p.RefDB = *r.RefDB
			}
			if r.KS != nil {
				p.KS = *r.KS
			}
			if r.AttMs != nil {
				p.AttMs = *r.AttMs
			}
			if r.RelMs != nil {
				p.RelMs = *r.RelMs
			}
			if r.Off != nil && *r.Off {
				if err := setDynBass(nil); err != nil {
					httpError(w, http.StatusBadRequest, err.Error())
					return
				}
			} else if err := setDynBass(&p); err != nil {
				httpError(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		// V4A DynamicBass (simple branch): update when the field is given explicitly (off=true or an entirely empty object = off)
		if req.DynamicBass != nil {
			p := dynamicBassDefaultParams()
			if cur := currentDynamicBass; cur != nil {
				p = *cur
			}
			r := req.DynamicBass
			if r.Coeffs != nil {
				p.Coeffs = *r.Coeffs
			}
			if r.Bass != nil {
				p.Bass = *r.Bass
			}
			if r.Off != nil && *r.Off {
				if err := setDynamicBass(nil); err != nil {
					reqSnap.restore()
					httpError(w, http.StatusBadRequest, err.Error())
					return
				}
			} else if err := setDynamicBass(&p); err != nil {
				reqSnap.restore()
				httpError(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		// Sound stage: update when the field is given explicitly (off=true or an entirely empty object = off)
		if req.Surround != nil {
			p := surroundDefaultParams()
			if cur := currentSurround; cur != nil {
				p = *cur
			}
			r := req.Surround
			if r.DelayMs != nil {
				p.DelayMs = *r.DelayMs
			}
			if r.Off != nil && *r.Off {
				if err := setSurround(nil); err != nil {
					httpError(w, http.StatusBadRequest, err.Error())
					return
				}
			} else if err := setSurround(&p); err != nil {
				httpError(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		// ColorfulMusic: update when the field is given explicitly (off=true or off when depth=0)
		if req.ColorfulMusic != nil {
			p := colorfulDefaultParams()
			if cur := currentColorful; cur != nil {
				p = *cur
			}
			r := req.ColorfulMusic
			if r.Depth != nil {
				p.Depth = *r.Depth
			}
			if r.Widening != nil {
				p.Widening = *r.Widening
			}
			if r.MidImage != nil {
				p.MidImage = *r.MidImage
			}
			if (r.Off != nil && *r.Off) || p.Depth == 0 {
				if err := setColorfulMusic(nil); err != nil {
					httpError(w, http.StatusBadRequest, err.Error())
					return
				}
			} else if err := setColorfulMusic(&p); err != nil {
				httpError(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		// crossfeed: update when the field is given explicitly (off=true or an entirely empty object = off)
		if req.Crossfeed != nil {
			p := crossfeedDefaultParams()
			if cur := currentCrossfeed; cur != nil {
				p = *cur
			}
			r := req.Crossfeed
			if r.FcutHz != nil {
				p.FcutHz = *r.FcutHz
			}
			if r.Feed != nil {
				p.Feed = *r.Feed
			}
			// PassFilter is only added when explicitly true; not given = keep it (so the default stays pure bs2b)
			if r.PassFilter != nil {
				p.PassFilter = *r.PassFilter
			}
			if r.Off != nil && *r.Off {
				if err := setCrossfeed(nil); err != nil {
					httpError(w, http.StatusBadRequest, err.Error())
					return
				}
			} else if err := setCrossfeed(&p); err != nil {
				httpError(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		// Tube flavour: update when the field is given explicitly (off=true = off)
		if req.Tube != nil {
			if req.Tube.Off != nil && *req.Tube.Off {
				if err := setTube(nil); err != nil {
					httpError(w, http.StatusBadRequest, err.Error())
					return
				}
			} else if err := setTube(&tubeParams{}); err != nil {
				httpError(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		// Harmonic exciter: update when the field is given explicitly (off=true or an entirely empty object = off)
		if req.Exciter != nil {
			p := exciterDefaultParams()
			if cur := currentExciter; cur != nil {
				p = *cur
			}
			r := req.Exciter
			if len(r.Harmonics) > 0 {
				if len(r.Harmonics) != 10 {
					httpError(w, http.StatusBadRequest, "harmonics must be 10 numbers (the 1st..10th harmonic amplitudes)")
					return
				}
				copy(p.Harmonics[:], r.Harmonics)
			}
			if r.Mix != nil {
				p.Mix = *r.Mix
			}
			if r.HPFHz != nil {
				p.HPFHz = *r.HPFHz
			}
			if r.LPFHz != nil {
				p.LPFHz = *r.LPFHz
			}
			if r.Off != nil && *r.Off {
				if err := setExciter(nil); err != nil {
					httpError(w, http.StatusBadRequest, err.Error())
					return
				}
			} else if err := setExciter(&p); err != nil {
				httpError(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		// VSE (V4A SpectrumExtend): update when the field is given explicitly (off=true = off)
		if req.VSE != nil {
			if req.VSE.Off != nil && *req.VSE.Off {
				if err := setVSE(nil); err != nil {
					httpError(w, http.StatusBadRequest, err.Error())
					return
				}
			} else {
				g := vseGearMin
				if req.VSE.Gear != nil {
					g = *req.VSE.Gear
				}
				if err := setVSE(&g); err != nil {
					httpError(w, http.StatusBadRequest, err.Error())
					return
				}
			}
		}
		// ViPERBass: update when the field is given explicitly (off=true = off); an absent field = leave the current setting alone
		if req.ViPERBass != nil {
			if req.ViPERBass.Off != nil && *req.ViPERBass.Off {
				if err := setViPERBass(nil); err != nil {
					httpError(w, http.StatusBadRequest, err.Error())
					return
				}
			} else {
				p := viperBassDefaultParams()
				if cur := currentViPERBass; cur != nil {
					p = *cur
				}
				if req.ViPERBass.Mode != nil {
					p.Mode = *req.ViPERBass.Mode
				}
				if req.ViPERBass.CutoffHz != nil {
					p.CutoffHz = *req.ViPERBass.CutoffHz
				}
				if req.ViPERBass.Gain != nil {
					p.Gain = *req.ViPERBass.Gain
				}
				if err := setViPERBass(&p); err != nil {
					httpError(w, http.StatusBadRequest, err.Error())
					return
				}
			}
		}
		// ViPERClarity (A4 upper half): update when the field is given explicitly (off=true = off)
		if req.Clarity != nil {
			if req.Clarity.Off != nil && *req.Clarity.Off {
				if err := setClarity(nil); err != nil {
					httpError(w, http.StatusBadRequest, err.Error())
					return
				}
			} else {
				p := clarityDefaultParams()
				if cur := currentClarity; cur != nil {
					p = *cur
				}
				if req.Clarity.Mode != "" {
					m, ok := clarityModeFromString(req.Clarity.Mode)
					if !ok {
						httpError(w, http.StatusBadRequest,
							"clarity.mode accepts only natural / ozone / xhifi")
						return
					}
					p.Mode = m
				}
				if req.Clarity.Level != nil {
					p.Level = *req.Clarity.Level
				}
				if err := setClarity(&p); err != nil {
					httpError(w, http.StatusBadRequest, err.Error())
					return
				}
			}
		}
		// SpeakerCorrection (A4 lower half): only a switch
		if req.SpeakerCorrection != nil {
			if req.SpeakerCorrection.Off != nil && *req.SpeakerCorrection.Off {
				if err := setSpeakerCorrection(nil); err != nil {
					httpError(w, http.StatusBadRequest, err.Error())
					return
				}
			} else if err := setSpeakerCorrection(&speakerCorrectionParams{}); err != nil {
				httpError(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		// AnalogX (A2): update when the field is given explicitly (off=true = off)
		if req.AnalogX != nil {
			if req.AnalogX.Off != nil && *req.AnalogX.Off {
				if err := setAnalogX(nil); err != nil {
					httpError(w, http.StatusBadRequest, err.Error())
					return
				}
			} else {
				p := analogxDefaultParams()
				if cur := currentAnalogX; cur != nil {
					p = *cur
				}
				if req.AnalogX.Model != nil {
					p.Model = *req.AnalogX.Model
				}
				if err := setAnalogX(&p); err != nil {
					httpError(w, http.StatusBadRequest, err.Error())
					return
				}
			}
		}
		// ⚠️ First restore the resource-type chain items that have no request field (convolution / DDC):
		//    GET's chain returns only the EQ bands, while PUT's chain replaces everything => without
		//    restoring them the user's convolution and DDC would be silently deleted (every single EQ
		//    edit in the WebUI would hit it). See preserveResourceChainItems in chain.go.
		effChain := preserveResourceChainItems(req.Chain, currentUserChain)
		_, active, err := chainToSlots(effChain)
		if err != nil {
			// Tell the user explicitly why it does not fit / is not supported, instead of just truncating
			httpError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := applyChain(effChain, req.PreampDB, req.GainDB); err != nil {
			// The whole request **either takes effect entirely or not at all**: put back the part the handler
			reqSnap.restore()
			log.Printf("chain apply failed; effect state change(s) from this request were rolled back too: %v", err)
			httpError(w, http.StatusInternalServerError, "writing to the hardware failed: "+err.Error())
			return
		}
		writeJSON(w, map[string]any{
			"ok":                 true,
			"applied":            active,
			"preamp_db":          currentPreampAppliedDB,
			"dyn_bass":           dynBassView(),
			"dynamic_bass":       dynamicBassView(),
			"crossfeed":          crossfeedView(),
			"surround":           surroundView(),
			"colorfulmusic":      colorfulView(),
			"exciter":            exciterView(),
			"tube":               tubeView(),
			"clarity":            clarityView(),
			"speaker_correction": speakerCorrectionView(),
			"analogx":            analogxView(),
		})
	default:
		httpError(w, http.StatusMethodNotAllowed, "only GET / PUT are supported")
	}
}

// ── Presets ───────────────────────────────────────────────────────────

// presetView is the preset summary returned to the UI (for the list, without the whole chain).
type presetView struct {
	Name     string   `json:"name"`
	Note     string   `json:"note,omitempty"`
	Target   string   `json:"target,omitempty"`
	Stage    string   `json:"stage,omitempty"`
	Requires []string `json:"requires,omitempty"`
	Builtin  bool     `json:"builtin"`
	Bands    int      `json:"bands"`
	Usable   bool     `json:"usable"`
	Why      []string `json:"why,omitempty"`
	// Chain is **what this preset actually does** (frequencies/gains/Q). Carrying it in the list
	// serves two purposes:
	//   (1) the UI can compare it against the "current chain" and thus highlight truthfully "which
	//       preset is in use now" -- otherwise it can only guess from "which one the user clicked",
	//       which points at the wrong one after apply/import/manual chain edits;
	//   (2) the contents are visible before applying (a preset is not a black box).
	// The JSON for 16 presets x 6 bands is only a few KB, not worth saving.
	Chain []ChainItem `json:"chain,omitempty"`
}

func presetToView(p Preset) presetView {
	v := presetView{
		Name:     p.Name,
		Note:     p.Note,
		Target:   p.Target,
		Stage:    p.Stage,
		Requires: p.Requires,
		Builtin:  p.Builtin,
		Bands:    len(p.Chain),
		Why:      presetUnsupported(p),
		Chain:    p.Chain,
	}
	v.Usable = len(v.Why) == 0
	return v
}

func handleDSPPresets(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		lib, err := loadPresetLibrary()
		if err != nil {
			httpError(w, http.StatusInternalServerError, "failed to read the built-in library: "+err.Error())
			return
		}
		user, _ := loadUserPresets()
		views := make([]presetView, 0, len(lib)+len(user))
		for _, p := range lib {
			views = append(views, presetToView(p))
		}
		for _, p := range user {
			views = append(views, presetToView(p))
		}
		writeJSON(w, map[string]any{"presets": views, "band_limit": bandLimit()})
	case http.MethodPost:
		// Save the current chain as a user preset
		var body struct {
			Name string `json:"name"`
			Note string `json:"note"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
			httpError(w, http.StatusBadRequest, "request body is not valid JSON: "+err.Error())
			return
		}
		chain := chainFromSlots()
		if len(chain) == 0 {
			httpError(w, http.StatusBadRequest, "the current chain is empty (fully flat); there is nothing to save")
			return
		}
		p := Preset{
			Format:   presetFormat,
			Version:  presetFormatVersion,
			Name:     body.Name,
			Note:     body.Note,
			Target:   "any",
			Requires: chainRequires(chain),
			Chain:    chain,
			PreampDB: currentPreampDB,
		}
		if err := saveUserPreset(p); err != nil {
			httpError(w, http.StatusBadRequest, "save failed: "+err.Error())
			return
		}
		log.Printf("preset: saved user preset %q (%d bands)", p.Name, len(p.Chain))
		writeJSON(w, map[string]any{"ok": true, "name": p.Name})
	default:
		httpError(w, http.StatusMethodNotAllowed, "only GET / POST are supported")
	}
}

func handleDSPPresetApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpError(w, http.StatusMethodNotAllowed, "only POST is supported")
		return
	}
	var body struct{ Name string }
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		httpError(w, http.StatusBadRequest, "request body is not valid JSON: "+err.Error())
		return
	}
	p, err := findPreset(body.Name)
	if err != nil {
		httpError(w, http.StatusNotFound, err.Error())
		return
	}
	if why := presetUnsupported(p); len(why) > 0 {
		httpError(w, http.StatusConflict,
			fmt.Sprintf("preset %q needs a stronger engine; this device cannot do it yet: %s", p.Name, strings.Join(why, "; ")))
		return
	}
	if err := applyPreset(p); err != nil {
		httpError(w, http.StatusInternalServerError, "apply failed: "+err.Error())
		return
	}
	log.Printf("preset: applied %q (%d bands, preamp %.1f dB)", p.Name, len(p.Chain), currentPreampAppliedDB)
	writeJSON(w, map[string]any{"ok": true, "name": p.Name, "preamp_db": currentPreampAppliedDB})
}

func handleDSPPresetDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpError(w, http.StatusMethodNotAllowed, "only POST is supported")
		return
	}
	var body struct{ Name string }
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		httpError(w, http.StatusBadRequest, "request body is not valid JSON: "+err.Error())
		return
	}
	if err := deleteUserPreset(body.Name); err != nil {
		httpError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleDSPPresetRename renames a **user preset** (built-in ones cannot be renamed; see the reasoning at renameUserPreset).
func handleDSPPresetRename(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpError(w, http.StatusMethodNotAllowed, "only POST is supported")
		return
	}
	var body struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		httpError(w, http.StatusBadRequest, "request body is not valid JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(body.From) == "" {
		httpError(w, http.StatusBadRequest, "from is missing (which preset to rename)")
		return
	}
	to, err := renameUserPreset(body.From, body.To)
	if err != nil {
		// not found / built-in => 404; illegal or colliding name => 400.
		// The two status codes exist so the UI can tell "this one cannot be renamed" apart from "this
		//    name cannot be used".
		if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "built-in preset") {
			httpError(w, http.StatusNotFound, err.Error())
			return
		}
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	log.Printf("preset: renamed user preset %q -> %q", body.From, to)
	writeJSON(w, map[string]any{"ok": true, "name": to})
}

// handleDSPPresetExport exports a preset as a shareable JSON file.
func handleDSPPresetExport(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		httpError(w, http.StatusBadRequest, "name parameter is missing")
		return
	}
	p, err := findPreset(name)
	if err != nil {
		httpError(w, http.StatusNotFound, err.Error())
		return
	}
	p.Builtin = false
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", "attachment; filename=\"preset.json\"")
	w.Write(append(data, '\n'))
}

// handleDSPPresetImport imports an external configuration: EqualizerAPO / REW text, or a JSON
// preset from this device. It returns warnings (which lines were not applied, the fitting error)
// for the UI to show -- this is where "no silent data loss" lands.
func handleDSPPresetImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpError(w, http.StatusMethodNotAllowed, "only POST is supported")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		httpError(w, http.StatusBadRequest, "reading request body failed: "+err.Error())
		return
	}
	text := strings.TrimSpace(string(raw))
	if text == "" {
		httpError(w, http.StatusBadRequest, "request body is empty")
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))

	// (1) A JSON preset from this device (a single preset or a library file)
	if strings.HasPrefix(text, "{") {
		var single Preset
		if err := json.Unmarshal(raw, &single); err == nil && single.Name != "" {
			if err := validatePreset(&single); err != nil {
				httpError(w, http.StatusBadRequest, "invalid preset: "+err.Error())
				return
			}
			if name != "" {
				single.Name = name
				if err := validatePreset(&single); err != nil {
					httpError(w, http.StatusBadRequest, "invalid preset: "+err.Error())
					return
				}
			}
			if why := presetUnsupported(single); len(why) > 0 {
				writeJSON(w, map[string]any{
					"ok":       true,
					"preset":   presetToView(single),
					"warnings": []string{"this preset uses effects this device does not have yet: " + strings.Join(why, "; ")},
				})
				return
			}
			applied, applyErr := importAndApply(single)
			resp := map[string]any{"ok": true, "preset": presetToView(single), "warnings": []string{}, "applied": applied}
			if applyErr != "" {
				resp["apply_error"] = applyErr
			}
			writeJSON(w, resp)
			return
		}
	}
	// (2) EqualizerAPO / REW text
	pe, perr := parseEqualizerAPO(text)
	if perr != nil && pe == nil {
		httpError(w, http.StatusBadRequest, perr.Error())
		return
	}
	if name == "" {
		name = "imported configuration"
	}
	// The fitted band count = what this hardware can really hold (on 0.2 a 10-band curve no longer
	// has to be squeezed into 6 bands).
	p, warns, err := presetFromAPO(pe, name, bandLimit())
	if perr != nil {
		warns = append(warns, perr.Error())
	}
	if err != nil {
		httpError(w, http.StatusBadRequest, "import failed: "+err.Error())
		return
	}
	applied, applyErr := importAndApply(p)
	log.Printf("preset: imported %q (%d bands, %d warnings, applied=%v)", p.Name, len(p.Chain), len(warns), applied)
	resp := map[string]any{
		"ok":        true,
		"preset":    presetToView(p),
		"warnings":  warns,
		"applied":   applied,
		"preamp_db": currentPreampAppliedDB,
	}
	if applyErr != "" {
		resp["apply_error"] = applyErr
	}
	writeJSON(w, resp)
}

// importAndApply **saves the import result as a user preset first**, then tries to apply it to the
// hardware.
// Why the split: a successful parse/fit is a successful import and must not fail as a whole because
// the hardware side failed to write (a development machine has no DSP, yet import and fitting
// still have to be verifiable).
func importAndApply(p Preset) (bool, string) {
	if err := saveUserPreset(p); err != nil {
		log.Printf("preset: failed to save the import result: %v", err)
	}
	if err := applyPreset(p); err != nil {
		return false, err.Error()
	}
	return true, ""
}

// ── Small helpers ─────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write response failed: %v", err)
	}
}

func httpError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
