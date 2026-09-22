package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ============================================================================
// Runtime state persistence: chain / dynamic bass / loudness / limiter / headroom --
// carrying on from the previous run after a restart
// ============================================================================
// Why this file exists: those values used to live only in memory, so **a restart (including
// a restart after flashing a new bitstream) went back to flat** and the user had to go back
// to the panel and pick a type again. It was measured once on the board: after flashing the
// bitstream and restarting, `bands_active` went from 7 to 0 and what the user heard was
// "the effects are gone".
//
// Division of labour with "types / presets" (do not mix the two):
//   - Types (/var/lib/zybo-audio/types/) = the **sets of settings the user has put
//     together**, long-term assets;
//   - Presets (presets/) = genre templates, applied once and done;
//   - This file = **the one running right now** (the selected type name in the panel + the
//     chain + cascade/chain-level settings).
//
// What is stored is in **the user's terms**, not the derived result: the loudness
// compensation sections computed from the current volume are not stored (they must be
// recomputed whenever the volume changes, see reapplyCurrentChain); what is stored is
// `chain` (without loudness).
//
// Failure policy: **never fail to start because of the state file**. A corrupt file, an
// unknown version or a failed write only logs and runs with the defaults -- this is a
// stereo, and it is better to lose one effect than to have no sound.
// ============================================================================

const stateFormat = 1

// statePath can be overridden by an environment variable (used by tests and local runs; on
// the board the default path is used).
var statePath = envOr("ZYBO_STATE_FILE", "/var/lib/zybo-audio/state.json")

// runtimeState is the snapshot that is persisted. Every field carries omitempty, which
// makes it easier to read by hand.
type runtimeState struct {
	Format    int              `json:"format"`
	Version   string           `json:"version,omitempty"`
	SavedAt   string           `json:"saved_at,omitempty"`
	Type      string           `json:"type,omitempty"` // the "type" name selected in the panel (select it again on the way back)
	Chain     []ChainItem      `json:"chain"`
	PreampDB  float64          `json:"preamp_db"`
	GainDB    float64          `json:"gain_db"`
	DynBass   *dynParams       `json:"dyn_bass,omitempty"`
	Crossfeed *crossfeedParams `json:"crossfeed,omitempty"`
	Surround  *surroundParams  `json:"surround,omitempty"`
	Exciter   *exciterParams   `json:"exciter,omitempty"`
	// ViPERBass (bass enhancement). It **must be registered here** to be persisted (the same
	// lesson as Tube/Clarity below):
	// VSE does not take a new field -- its runtime state is Exciter (the tier can be derived
	// back from the parameters, see vse.go).
	ViPERBass *viperBassParams `json:"viperbass,omitempty"`
	// Tube / Clarity / SpeakerCorrection are runtime effects with only an on/off switch (or
	// a few parameters).
	// WARNING: they **must be registered here** to actually be persisted: setters such as
	// `setTube` only call markStateDirty(), and the snapshot writes only the fields of this
	// struct -- found on 2026-09-20 that tube flavor was missing exactly this entry: the
	// dirty flag was set but the field was not => it silently disappeared after a restart
	// (while the API still showed it as "on").
	Tube              *tubeParams              `json:"tube,omitempty"`
	Clarity           *clarityParams           `json:"clarity,omitempty"`
	SpeakerCorrection *speakerCorrectionParams `json:"speaker_correction,omitempty"`
	// AnalogX (A2): one of the three tiers.
	AnalogX *analogxParams `json:"analogx,omitempty"`
	// DynamicBass (V4A `DynamicBass` short branch): like DynBass it **must be registered
	// here** to be persisted.
	DynamicBass *dynamicBassParams `json:"dynamic_bass,omitempty"`
	// ColorfulMusic (V4A field surround): **must be registered here** to be persisted (the
	// same lesson as Tube/Clarity: setColorfulMusic only calls markStateDirty(), and the
	// snapshot writes only the fields of this struct).
	ColorfulMusic *colorfulParams `json:"colorfulmusic,omitempty"`
	Loudness      loudnessPersist `json:"loudness"`
	Limiter       limiterPersist  `json:"limiter"`
	Headroom      bool            `json:"headroom"`
	// Volume is the panel slider's percentage (0..100). It **used not to be stored**, so every
	// restart fell back to the sound card's own default (measured 90%) -- a user-changed
	// volume could not survive one restart.
	// Absent (an old state file without this field / a value of 0) = use the code default
	// (90).
	Volume int `json:"volume,omitempty"`
}

type loudnessPersist struct {
	On        bool    `json:"on"`
	Strength  float64 `json:"strength"`
	RefOffset float64 `json:"ref_offset_db"`
}

type limiterPersist struct {
	Enabled  bool    `json:"enabled"`
	TruePeak bool    `json:"truepeak"`
	ThrDB    float64 `json:"thr_db"`
	AttMs    float64 `json:"att_ms"`
	RelMs    float64 `json:"rel_ms"`
}

var (
	stateMu        sync.Mutex
	stateDirty     bool
	stateTimer     *time.Timer
	stateRestoring bool // suppresses saving during restore: otherwise restore -> applyChain -> save, self-exciting
	// stateDebounce is "how long after a change it is persisted". Dragging the panel slider
	// pushes a change every 250 ms, and writing synchronously = dozens of stuttering writes
	// per drag; 2 seconds merges them into one, so a power loss loses at most the last
	// 2 seconds of settings.
	stateDebounce = 2 * time.Second
)

// selectedTypeName is **the type currently selected in the panel** (pure software state; the
// hardware does not know about it).
// The chain itself already expresses its contents; the name is kept only so that the panel
// can select the same card after a restart.
var selectedTypeName string

// snapshotRuntimeState takes a snapshot of the current in-memory state.
func snapshotRuntimeState() runtimeState {
	st := runtimeState{
		Format:   stateFormat,
		Version:  appVersion,
		SavedAt:  time.Now().Format(time.RFC3339),
		Type:     selectedTypeName,
		Chain:    currentUserChain,
		PreampDB: currentPreampDB,
		GainDB:   chainGainDB,
		Headroom: dspHeadroomOn,
		Volume:   currentVolume,
		Loudness: loudnessPersist{On: loudnessOn, Strength: loudnessStrength, RefOffset: loudnessRefOffsetDB},
		Limiter: limiterPersist{
			Enabled: dspLimiterEnabled, TruePeak: dspLimiterTP, ThrDB: dspLimiterThrDB,
			AttMs: dspLimiterAttMs, RelMs: dspLimiterRelMs,
		},
	}
	if currentDynBass != nil {
		p := *currentDynBass
		st.DynBass = &p
	}
	if currentDynamicBass != nil {
		p := *currentDynamicBass
		st.DynamicBass = &p
	}
	if currentCrossfeed != nil {
		p := *currentCrossfeed
		st.Crossfeed = &p
	}
	if currentSurround != nil {
		p := *currentSurround
		st.Surround = &p
	}
	if currentExciter != nil {
		p := *currentExciter
		st.Exciter = &p
	}
	if currentViPERBass != nil {
		p := *currentViPERBass
		st.ViPERBass = &p
	}
	if currentColorful != nil {
		p := *currentColorful
		st.ColorfulMusic = &p
	}
	if currentTube != nil {
		p := *currentTube
		st.Tube = &p
	}
	if currentClarity != nil {
		p := *currentClarity
		st.Clarity = &p
	}
	if currentSpeakerCorrection != nil {
		p := *currentSpeakerCorrection
		st.SpeakerCorrection = &p
	}
	if currentAnalogX != nil {
		p := *currentAnalogX
		st.AnalogX = &p
	}
	return st
}

// markStateDirty records that "the state changed", merged into a single save (see
// stateDebounce).
func markStateDirty() {
	stateMu.Lock()
	defer stateMu.Unlock()
	if stateRestoring {
		return
	}
	if stateTimer != nil {
		stateTimer.Reset(stateDebounce)
		return
	}
	stateTimer = time.AfterFunc(stateDebounce, func() {
		stateMu.Lock()
		stateTimer = nil
		stateDirty = true
		stateMu.Unlock()
		saveRuntimeState()
	})
}

// saveRuntimeState saves immediately (atomic write: temp file -> fsync -> rename).
func saveRuntimeState() error {
	stateMu.Lock()
	stateDirty = false
	stateMu.Unlock()

	st := snapshotRuntimeState()
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(statePath), ".tmp-state-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, statePath)
}

// loadRuntimeState reads the file and does the most basic sanity checks.
// Returns (state, exists, error). A missing file is not an error (the first boot has none).
func loadRuntimeState() (runtimeState, bool, error) {
	var st runtimeState
	data, err := os.ReadFile(statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return st, false, nil
		}
		return st, false, err
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return st, true, fmt.Errorf("the state file is not valid JSON: %w", err)
	}
	if st.Format != stateFormat {
		return st, true, fmt.Errorf("state file format %d, this version only accepts %d", st.Format, stateFormat)
	}
	return st, true, nil
}

// restoreRuntimeState **applies the persisted state back to the hardware** (called once at
// boot).
//
// Order: first put the on/off switches and parameters back into memory **in the user's
// terms**, then push them to the hardware in one go with applyChain() (the chain, loudness,
// headroom and limiter are all derived inside it from the current volume/headroom settings).
//
// Any step that fails only logs and execution continues: better to lose one setting than to
// have no sound.
func restoreRuntimeState() {
	st, ok, err := loadRuntimeState()
	if err != nil {
		log.Printf("there is a problem with the state file; running with defaults this time: %v", err)
		return
	}
	if !ok {
		log.Printf("no state file (%s), running with defaults", statePath)
		return
	}

	stateRestoring = true
	defer func() { stateRestoring = false }()

	applyRuntimeState(st, dynBassAvailable(), crossfeedAvailable(), surroundAvailable(),
		exciterAvailable(), colorfulAvailable())

	// Volume: **actually written into the mixer**. Without that, "default 90%" is only a
	// number in memory and the actual output still uses the sound card's power-on default
	// (which happens to be 90 as well, but that is a coincidence, not a guarantee).
	if err := setSystemVolume(currentVolume); err != nil {
		log.Printf("failed to restore volume %d%%: %v", currentVolume, err)
	}
	if err := dspSetLimiter(dspLimiterEnabled, dspLimiterThrDB); err != nil {
		log.Printf("failed to restore the limiter: %v", err)
	}
	if err := dspSetLimiterTimes(dspLimiterAttMs, dspLimiterRelMs); err != nil {
		log.Printf("failed to restore the limiter time constants: %v", err)
	}
	if err := applyChain(st.Chain, st.PreampDB, st.GainDB); err != nil {
		log.Printf("failed to restore the chain (running flat): %v", err)
		if err := applyChain(nil, 0, 0); err != nil {
			log.Printf("even pushing out the flat chain failed: %v", err)
		}
		return
	}
	log.Printf("restored the previous settings: volume=%d%%, type=%q, %d-section chain, dynamic bass=%s, V4A dynamic bass=%s, crossfeed=%s, surround=%s, loudness=%s, limiter=%s@%.1fdBFS",
		currentVolume,
		nonEmpty(st.Type, "(not recorded)"), len(st.Chain), onOff(currentDynBass != nil),
		onOff(currentDynamicBass != nil),
		onOff(currentCrossfeed != nil), onOff(currentSurround != nil),
		onOff(loudnessOn), limiterModeName(), dspLimiterThrDB)
}

// applyRuntimeState does only **the software part**: put the values from the state back into
// memory (without touching the hardware).
//
// It is split out so it can be tested: on the real board the "back into memory" step can
// only be verified indirectly through hardware behaviour, and it is the first scene of every
// "the settings are lost after a restart" problem.
//
// dynOK / mixOK are passed in by the caller (on the real board dynBassAvailable() /
// crossfeedAvailable()): **when the bitstream does not include DYN or MIX2 the entry is
// quietly dropped** rather than erroring -- the state file may come from a machine with a
// newer bitstream, and falling back to an older bitstream must not stop the boot.
func applyRuntimeState(st runtimeState, dynOK, mixOK, dlyOK, polyOK, jsOK bool) {
	if st.Type != "" {
		selectedTypeName = st.Type
	}
	// The chain itself is also "software state": put it back into memory first (the hardware
	// step is done by the caller through applyChain, the same push path, so that the
	// derivation rules during restore are not different from normal operation). Clamping
	// (gain 0..12 etc.) is still left to applyChain.
	currentUserChain = st.Chain
	currentPreampDB = st.PreampDB
	chainGainDB = st.GainDB
	loudnessOn = st.Loudness.On
	if st.Loudness.Strength > 0 {
		loudnessStrength = st.Loudness.Strength
	}
	if st.Loudness.RefOffset >= 0 {
		loudnessRefOffsetDB = st.Loudness.RefOffset
	}
	dspLimiterEnabled = st.Limiter.Enabled
	dspLimiterTP = st.Limiter.TruePeak
	if st.Limiter.ThrDB != 0 || st.Limiter.Enabled {
		dspLimiterThrDB = st.Limiter.ThrDB
	}
	if st.Limiter.AttMs > 0 {
		dspLimiterAttMs = st.Limiter.AttMs
	}
	if st.Limiter.RelMs > 0 {
		dspLimiterRelMs = st.Limiter.RelMs
	}
	dspHeadroomOn = st.Headroom
	// Volume: if the state has one, use it (1..100), otherwise keep the default (90).
	// 0 is the real state "muted", but at volume 0 nothing can be heard => 0 is not treated
	// as a "user choice", so that a corrupt state file cannot make the board mute.
	if st.Volume > 0 && st.Volume <= 100 {
		currentVolume = st.Volume
	}

	if st.DynBass != nil {
		if dynOK {
			p := *st.DynBass
			currentDynBass = &p
		} else {
			log.Printf("the state stores dynamic bass, but this bitstream has no DYN slot => not enabling it this time")
			currentDynBass = nil
		}
	} else {
		currentDynBass = nil
	}

	// DynamicBass (V4A short branch): needs BIQUAD + MIX2. When the capability is missing the
	// entry is **quietly dropped** (the state file may come from a machine with a newer
	// bitstream, and falling back to an older bitstream must not stop the boot), and a log
	// line is left behind.
	if st.DynamicBass != nil {
		if dynamicBassAvailable() {
			p := *st.DynamicBass
			if _, err := dynamicBassValidate(p); err != nil {
				log.Printf("the DynamicBass parameters in the state are invalid (%v) => not enabling it this time", err)
				currentDynamicBass = nil
			} else {
				currentDynamicBass = &p
			}
		} else {
			log.Printf("the state stores V4A DynamicBass, but this bitstream has no BIQUAD+MIX2 slot => not enabling it this time")
			currentDynamicBass = nil
		}
	} else {
		currentDynamicBass = nil
	}

	if st.Crossfeed != nil {
		if mixOK {
			p := *st.Crossfeed
			currentCrossfeed = &p
		} else {
			log.Printf("the state stores crossfeed, but this bitstream has no MIX2 slot => not enabling it this time")
			currentCrossfeed = nil
		}
	} else {
		currentCrossfeed = nil
	}

	if st.Surround != nil {
		if dlyOK {
			p := *st.Surround
			currentSurround = &p
		} else {
			log.Printf("the state stores surround, but this bitstream has no DELAY slot => not enabling it this time")
			currentSurround = nil
		}
	} else {
		currentSurround = nil
	}

	// ColorfulMusic: needs the **joint stereo frame pass** (CAP1 bit12). An old bitstream does
	// not have it => the entry is quietly dropped (the state file may come from a machine with
	// a newer bitstream, and falling back to an older bitstream must not stop the boot).
	// The parameters still go through colorfulValidate for a range check: a corrupt state file
	// must not configure the engine into an unknown state.
	if st.ColorfulMusic != nil {
		if jsOK {
			if p, err := colorfulValidate(*st.ColorfulMusic); err == nil {
				currentColorful = &p
			} else {
				log.Printf("the ColorfulMusic parameters in the state are invalid (%v) => not enabling it this time", err)
				currentColorful = nil
			}
		} else {
			log.Printf("the state stores ColorfulMusic, but this bitstream has no joint stereo frame pass => not enabling it this time")
			currentColorful = nil
		}
	} else {
		currentColorful = nil
	}

	// Harmonic exciter (POLY slot). Same again: when the bitstream has no POLY the entry is
	// quietly dropped rather than refusing to boot.
	if st.Exciter != nil {
		if polyOK {
			p := *st.Exciter
			currentExciter = &p
		} else {
			log.Printf("the state stores the harmonic exciter, but this bitstream has no POLY slot => not enabling it this time")
			currentExciter = nil
		}
	} else {
		currentExciter = nil
	}

	// ViPERBass: the same criterion as crossfeed (needs the MIX2 slot). During restore it goes
	// through setViPERBass for a range check, since a corrupt state file (an out-of-range
	// tier) must not configure the engine into an unknown state.
	// WARNING: PBP (mode=1) has been a **legal tier** since 2026-09-21, but it additionally
	//     requires the small FIR (CAP1 bit7 + CAP4): when moved to an old bitstream without
	//     the small FIR, setViPERBass refuses because of the CAP self-report => it is quietly
	//     switched off here (rather than configuring the engine into a chain that cannot run a
	//     dry path).
	if st.ViPERBass != nil && viperBassAvailable() {
		p := *st.ViPERBass
		if err := setViPERBass(&p); err != nil {
			log.Printf("the ViPERBass in the state is invalid (%v) => not enabling it this time", err)
			currentViPERBass = nil
		}
	} else {
		currentViPERBass = nil
	}

	// Tube flavor / Clarity / SpeakerCorrection: all of them only need the BIQUAD capability,
	// so the criterion is "is the engine there".
	// When the bitstream is 0.1 (no slot-table engine) the entry is quietly dropped,
	// consistent with the entries above.
	if st.Tube != nil && tubeAvailable() {
		currentTube = &tubeParams{}
	} else {
		currentTube = nil
	}
	if st.Clarity != nil && clarityAvailable() {
		p := *st.Clarity
		// use setClarity for a range check: a corrupt state file (an out-of-range tier) must
		// not configure the engine into an unknown state
		if err := setClarity(&p); err != nil {
			log.Printf("the Clarity in the state is invalid (%v) => not enabling it this time", err)
			currentClarity = nil
		}
	} else {
		currentClarity = nil
	}
	if st.SpeakerCorrection != nil && speakerCorrectionAvailable() {
		currentSpeakerCorrection = &speakerCorrectionParams{}
	} else {
		currentSpeakerCorrection = nil
	}
	if st.AnalogX != nil && analogxAvailable() {
		p := *st.AnalogX
		if err := setAnalogX(&p); err != nil {
			log.Printf("the AnalogX in the state is invalid (%v) => not enabling it this time", err)
			currentAnalogX = nil
		}
	} else {
		currentAnalogX = nil
	}
}

func nonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
