// SPDX-License-Identifier: GPL-2.0-only
// fxpresets.go -- A2': the **four parameter presets** of the POLY family (three AnalogX tiers + VSE)
//
// Why this file exists: the three AnalogX tiers and VSE have long worked on their own
// (`analogx.go` / `vse.go`), but the notion of a "tier" is told differently in the two files --
// a panel/documentation/script that needs to know "which numbers this tier actually is" had to
// assemble them by hand. Here the **four parameter sets** are registered as one table: one preset
// = one complete set of hardware parameters (including the harmonic table), with its **primary
// source**. The table itself **repeats no numbers**: the three AnalogX entries come from
// `analogxTiers` and the VSE entry from `vseExciterParams(vseGearMin)` -- change a constant in one
// place and the table follows.
//
// -- The four presets and their primary sources ------------------------------
//
//	(1) analogx-slight   AnalogX model 0: gain 0.6  / LPF 19650 Hz
//	(2) analogx-moderate AnalogX model 1: gain 1.2  / LPF 18233 Hz
//	(3) analogx-extreme  AnalogX model 2: gain 2.4  / LPF 16307 Hz
//	   + shared by all three tiers: harmonic table `ANALOGX_HARMONICS`, HP 240 Hz Q0.717, Peak 633 Hz (+0.58 dB)
//	     the community ViPER4Android rewrite (viperfx-re): src/viper/effects/AnalogX.cpp:5-16 (harmonic table)
//	     src/viper/effects/AnalogX.cpp:62-93 (three-tier switch: gain and LPF)
//	     src/viper/effects/AnalogX.cpp:53-57 (HP/Peak, identical for all three tiers)
//	     where the tier index comes from (core): src/viper/ViPER.cpp:410-412
//	       `PARAM_ANALOGX_MODE -> analogX.SetProcessingModel(val1)`
//	     tier names (official client panel):
//	       the original ViPER4Android app (viper4android_fx, android_4.x): res/values/arrays.xml:266-274 (Slight/Moderate/Extreme)
//	       the same app's zh-rCN localization: res/values-zh-rCN/arrays.xml:103-107 (the Chinese tier names)
//
//	(4) vse             V4A `SpectrumExtend` factory shape + **panel default gear 0.1**
//	   + harmonic table `SPECTRUM_HARMONICS` = 0.02 for each odd harmonic
//	     the community ViPER4Android rewrite (viperfx-re): src/viper/effects/SpectrumExtend.cpp:4-15
//	   + refFreq (= the highpass cutoff of the first stage) 7600 Hz, second-stage lowpass fs/2-2000
//	     src/viper/effects/SpectrumExtend.cpp:19 (referenceFreq = 7600)
//	     src/viper/effects/SpectrumExtend.cpp:44-52 (HP = refFreq, LP = fs/2-2000)
//	   + exciter wet gain: client `round(UI*5.6*100)` -> core `/100`
//	     the original ViPER4Android app (viper4android_fx, android_4.x-5.x): src/com/vipercn/viper4android_v2/service/ViPER4AndroidService.java:1708-1710
//	     the community ViPER4Android rewrite (viperfx-re): src/viper/ViPER.cpp:242-245 (SetExciter(val1/100))
//	     used in the core as: SpectrumExtend.cpp:33,38 (`tmp * this->exciter`)
//	   + default gear 0.1: the original ViPER4Android app: res/xml/headset_preferences_l2.xml:85-92
//	     (`android:defaultValue="0.1"`) => exciter = 0.56 in the core
//
// -- Same persistence/interface convention as the "other effects" (no new state fields) -------
//
// The four presets **introduce no new runtime state**: they land on two existing fields --
//   · `analogx-*` => `currentAnalogX` (`analogx` in `state.go:60-61`, already persisted)
//   · `vse`       => `currentExciter` (`exciter` in `state.go:49`, already persisted; see the head of `vse.go`)
// So "the preset is still there after a restart" is guaranteed by the existing state mechanism,
// and `state.go`'s structs need no change.
//
// ⚠️ One convention this device **cannot express** (reported truthfully, not silently truncated):
// the top VSE panel gear 1.0 => 5.6 in the core, and it lands on a MIX2 Q3.15 coefficient (a
// single coefficient is limited to about 4.0). This device folds it into range by "scaling the
// whole polynomial + one gain stage" (`harmonic.go:56-89`), and **setExciter reports an explicit
// error when it does not fit** (rather than just clamping it). The criteria are
// `TestVSETopGearsReachable` + `validateExciterMix`.

package main

import (
	"fmt"
	"log"
)

// fxPreset is **one complete set of effect parameters** (not a "name + reference": the numbers
// themselves are carried along, so the panel/documentation/regression tests can compare directly).
type fxPreset struct {
	Name string `json:"name"` // stable ID (the API uses it to select this set)
	Cn   string `json:"cn"`   // panel-facing name (same labels as the official client panel)
	FX   string `json:"fx"`   // which effect it lands on: "analogx" / "vse"

	// -- AnalogX (FX == "analogx") --
	Model int `json:"model,omitempty"` // processing model 0/1/2 (ViPER.cpp:410-412)

	// -- shape shared by both groups --
	// Harmonics: relative amplitude of the 1st..10th harmonic (AnalogX uses its own table, VSE uses 0.02 on odd harmonics)
	Harmonics [10]float64 `json:"harmonics"`
	// WetGain: AnalogX = the **linear** gain of the wet path in the dry/wet mix (the source's `this->gain`);
	//          VSE = the in-core exciter (= reconstruct/100). **Not dB**; see the comparison below.
	WetGain float64 `json:"wet_gain"`
	// HPFHz / LPFHz: the bandpass before and after the harmonic generator (both Q = 0.717)
	HPFHz float64 `json:"hpf_hz"`
	LPFHz float64 `json:"lpf_hz"`

	// -- VSE (FX == "vse") --
	Gear        float64 `json:"gear,omitempty"`        // panel gear (0.1...1.0)
	Reconstruct int     `json:"reconstruct,omitempty"` // the integer the client sends down = round(gear*5.6*100)

	Source string `json:"source"` // primary source (`file:line`)
}

// fxPresetNames is the stable ID order of the four presets (both the API and the tests follow it).
//
// The IDs use the English tier names (matching the `analogx_mode` array at `arrays.xml:266-274`
// label for label); the panel name is carried separately in the `Cn` field (matching
// `values-zh-rCN/arrays.xml:103-107`, the zh-rCN localization of those same three labels).
var fxPresetNames = [4]string{"analogx-slight", "analogx-moderate", "analogx-extreme", "vse"}

// fxAnalogXPresetIDSuffix aligns the tier index with the ID suffix (the order is the source's model 0/1/2).
var fxAnalogXPresetIDSuffix = [3]string{"slight", "moderate", "extreme"}

// fxAnalogXCn holds the panel names of the three AnalogX tiers (`values-zh-rCN/arrays.xml:103-107`).
var fxAnalogXCn = [3]string{"Slight", "Moderate", "Extreme"}

// fxPresetTable returns those four presets (computed fresh each time, so it stays derived from analogxTiers / the exciter factory shape).
//
// The order matches `fxPresetNames`.
func fxPresetTable() []fxPreset {
	out := make([]fxPreset, 0, len(fxPresetNames))
	for i, t := range analogxTiers {
		out = append(out, fxPreset{
			Name:      "analogx-" + fxAnalogXPresetIDSuffix[i],
			Cn:        fxAnalogXCn[i],
			FX:        "analogx",
			Model:     i,
			Harmonics: analogxHarmonics,
			WetGain:   t.Gain,
			HPFHz:     240.0, // AnalogX.cpp:53-54 (identical for all three tiers)
			LPFHz:     t.LPHz,
			Source: "the community ViPER4Android rewrite (viperfx-re): src/viper/effects/AnalogX.cpp:5-16 (harmonic table), " +
				":62-93 (three-tier gain/LPF), :53-57 (HP240/Peak633); " +
				"tier names from the original ViPER4Android app (viper4android_fx, android_4.x): res/values/arrays.xml:266-274",
		})
	}
	// VSE: factory shape + panel default gear 0.1 (=> exciter 0.56 in the core).
	// ⚠️ Use vseExciterParams(vseGearMin) rather than copying exciterDefaultParams() outright:
	//    the two hold the same numbers today (0.56 / 7600 / 22000 / 0.02 on odd harmonics), but
	//    VSE's exact definition is "the set for panel gear 0.1", so going through the vse path is
	//    what keeps them from diverging later.
	//    If it truly cannot be resolved (impossible: vseGearMin is a constant of this package),
	//    fall back to the factory shape and log it -- **never panic**: this is an audio device, and
	//    a broken caps endpoint is harder to diagnose than one missing preset.
	gear := vseGearMin
	p, err := vseExciterParams(gear)
	if err != nil {
		log.Printf("fxpresets: cannot resolve VSE default gear %g (%v), falling back to the factory shape", gear, err)
		p = exciterDefaultParams()
	}
	rec, err := vseReconstruct(gear)
	if err != nil {
		log.Printf("fxpresets: cannot resolve reconstruct for VSE default gear %g (%v), deriving it from mix*100", gear, err)
		rec = int(p.Mix*100 + 0.5)
	}
	out = append(out, fxPreset{
		Name:        "vse",
		Cn:          "VSE exciter (default gear)",
		FX:          "vse",
		Harmonics:   p.Harmonics,
		WetGain:     p.Mix,
		HPFHz:       p.HPFHz,
		LPFHz:       p.LPFHz,
		Gear:        gear,
		Reconstruct: rec,
		Source: "the community ViPER4Android rewrite (viperfx-re): src/viper/effects/SpectrumExtend.cpp:4-15 (0.02 on odd harmonics), " +
			":19（refFreq 7600）,:44-52（HP=refFreq / LP=fs/2−2000）; " +
			"gear -> core from the original ViPER4Android app (viper4android_fx, android_4.x-5.x): src/com/vipercn/" +
			"viper4android_v2/service/ViPER4AndroidService.java:1708-1710; " +
			"default gear 0.1 from the original ViPER4Android app: res/xml/headset_preferences_l2.xml:85-92",
	})
	return out
}

// fxPresetByName returns one preset by its stable ID (an unknown name is reported as an error, with no "nearest match").
func fxPresetByName(name string) (fxPreset, error) {
	for _, p := range fxPresetTable() {
		if p.Name == name {
			return p, nil
		}
	}
	return fxPreset{}, fmt.Errorf("no such parameter preset %q; this device has only four: %v"+
		" (the three AnalogX tiers are in AnalogX.cpp:62-93, VSE in SpectrumExtend.cpp:4-15)",
		name, fxPresetNames[:])
}

// applyFXPreset applies one preset to an **existing** runtime effect (no new state fields).
//
//   - analogx-* => setAnalogX (lands on currentAnalogX, `analogx` in state.go)
//   - vse       => setVSE (lands on currentExciter, `exciter` in state.go)
//
// Failures (e.g. the hardware has no POLY slot) are **reported explicitly** by the callee and re-thrown as-is here, with no silent degradation.
func applyFXPreset(name string) error {
	p, err := fxPresetByName(name)
	if err != nil {
		return err
	}
	switch p.FX {
	case "analogx":
		params := analogxParams{Model: p.Model}
		return setAnalogX(&params)
	case "vse":
		gear := p.Gear
		return setVSE(&gear)
	}
	return fmt.Errorf("parameter preset %q has unknown effect type %q (this device has only analogx / vse)", name, p.FX)
}

// fxPresetCurrent back-derives "which presets are currently hit" (empty = none).
//
// Same idea as `vseGearOf`: **do not store a second copy of the tier**, derive it from the runtime
// truth instead, so that "the stored tier" and "the parameters actually applied" cannot each be
// half-changed and contradict each other.
func fxPresetCurrent() []string {
	out := make([]string, 0, 2)
	if currentAnalogX != nil {
		m := currentAnalogX.Model
		if m >= 0 && m < len(fxAnalogXPresetIDSuffix) {
			out = append(out, "analogx-"+fxAnalogXPresetIDSuffix[m])
		}
	}
	// VSE's runtime truth is currentExciter: it counts only when it is exactly "some tier of the VSE
	// shape".
	// ⚠️ Only the gear-0.1 (default) set is a preset; the other 9 tiers are "custom VSE gears",
	//    expressed by the gear field of `vseView()`, and do not count as hitting fx_preset.
	if currentExciter != nil {
		if gear, ok := vseGearOf(*currentExciter); ok && gear == vseGearMin {
			out = append(out, "vse")
		}
	}
	return out
}

// fxPresetsView is for the API/panel: the four presets plus which ones are currently hit.
func fxPresetsView() map[string]any {
	on := fxPresetCurrent()
	return map[string]any{
		"presets": fxPresetTable(),
		"on":      on,
		"note": "The four A2' parameter presets (three AnalogX tiers + the VSE default gear). " +
			"Apply in one line: PUT /api/dsp/chain {\"fx_preset\":\"analogx-moderate\"}. " +
			"They land on the existing analogx / exciter fields (the persistence convention is unchanged). " +
			"Turning them off still uses their own off fields (analogx.off / vse.off).",
	}
}
