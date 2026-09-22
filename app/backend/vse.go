// SPDX-License-Identifier: GPL-2.0-only
// vse.go — the **tier mapping** for V4A `VSE` (Spectrum Extension) (A3)
//
// This file **adds no new DSP structure**: VSE is just "the factory-shaped harmonic
// exciter", and `harmonic.go`'s `exciterDefaultParams()` is already exactly that (odd
// harmonics only, 0.02 each; HPF 7600 Q0.717; LPF fs/2−2000 Q0.717). All that is added
// here is the **panel tier → kernel parameter** segment, pinned down by measuring what
// the official core actually does.
//
// ── First-hand sources (client side) ─────────────────────────────────
//
//	the decompiled ViPER4Android 4.x/5.x client, src/com/vipercn/viper4android_v2/service/ViPER4AndroidService.java:1708-1710
//	    int nSEValue = (int)(Math.round(Float.valueOf(
//	        preferences.getString("viper4android.headphonefx.vse.value", "0.1")) * 5.6f * 100.0f));
//	    v4aModule.setParameter_px4_vx4x1(PARAM_HPFX_VSE_BARK_RECONSTRUCT, nSEValue);
//	    v4aModule.setParameter_px4_vx4x1(PARAM_HPFX_VSE_REFERENCE_BARK, 7600);
//	the decompiled ViPER4Android 4.x/5.x client, res/values/arrays.xml:31 (`vse_strength_values`)
//	    the panel is 0.1 / 0.2 / … / 1.0, **10 tiers** in total, default "0.1".
//	⇒ the tier alone determines `BARK_RECONSTRUCT = round(UI×5.6×100)` = 56…560 (+56 per tier);
//	  the client **always sends 7600** for `REFERENCE_BARK`, independent of the tier.
//
// ── The **real semantics** of the two parameters (oracle measurements, not name-guessing) ──
//
//	· `PARAM_HPFX_VSE_REFERENCE_BARK` = **the cutoff frequency (Hz) of the pre-stage high-pass**.
//	  The "bark" in the name is misleading: it is neither a Bark scale value nor a band number.
//	  Measured (the V4A reference oracle, vse.c: 3000/4000 Hz sine, reconstruct=560): changing
//	  REFERENCE_BARK to 5000 / 7600 / 10000 matches the 3rd-harmonic prediction of a
//	  `HPF(fc=that value, Q0.717)` model to within +0.04 / +0.05 / +0.10 dB.
//	· `PARAM_HPFX_VSE_BARK_RECONSTRUCT` = **wet gain ×100** (the kernel divides by 100 ⇒ 0.56…5.6).
//	  Measured: reconstruct 56→560 takes the 3rd harmonic from −78.35 to −58.66 dBFS
//	  (+19.64 dB, exactly a factor of 10).
//
// ── Point-by-point comparison against the official core (frozen into a regression by `vse_test.go`) ──
//
//	3000 Hz sine, amplitude 20000 (−4.3 dBFS), REFERENCE_BARK=7600:
//
//	tier      reconstruct   model h1 / measured h1   model h3 / measured h3
//	0.1          56         19883.8 / 19791.3      3.8368 / 3.9619   (−0.28 dB)
//	1.0         560         18851.0 / 18763.4      38.3681 / 38.2120  (+0.04 dB)
//
//	⇒ this unit's harmonic exciter structure (polynomial + 0.999 leaky integrator + RBJ Q0.717
//	  band-pass before and after) **is** the official core's VSE; agreement is on the order of
//	  0.05 dB, an order of magnitude tighter than the 0.6 dB tolerance of `xhifi_calib_test.go`.
//
// ── State: why state.go has no new field ─────────────────────────────
//
//	VSE's runtime state **is** `currentExciter` (`state.go` already persists `exciter`), and the
//	tier can be derived back from it (`vseGearOf`). So no second copy of the "tier" is kept here,
//	which avoids two states fighting each other: after a restart, restoring `exciter` from
//	state.json restores the VSE tier.

package main

import (
	"fmt"
	"math"
)

// vseReferenceBarkHz is the **actual argument** for `PARAM_HPFX_VSE_REFERENCE_BARK`: the V4A
// client always sends 7600 (see ViPER4AndroidService.java:1710 at the top of this file), and
// the oracle measurements show it is the cutoff frequency (Hz) of the pre-stage high-pass.
// The "bark" in the name is misleading; do not use it as a Bark value.
const vseReferenceBarkHz = 7600.0

// vseGearMin / vseGearMax are the endpoints of the panel tiers (the 10 tiers of arrays.xml).
const (
	vseGearMin = 0.1
	vseGearMax = 1.0
	// vseGearStep is the tier spacing: the panel has **10 discrete tiers**, not a continuous slider.
	vseGearStep = 0.1
	// vseReconstructPerGear is the slope of that client line: UI×5.6×100 (+56 per tier).
	vseReconstructPerGear = 5.6 * 100.0
)

// vseReconstruct converts a panel tier into the kernel parameter `PARAM_HPFX_VSE_BARK_RECONSTRUCT`.
// It copies the client line verbatim: `round(UI × 5.6 × 100)`.
//
// Only the 10 tiers that really exist on the panel are accepted (integer multiples of 0.1): the
// kernel could of course be given a value like 137, but that would be **a tier we invented** —
// not on the panel, not in any source — so it is rejected rather than rounded to the nearest.
func vseReconstruct(gear float64) (int, error) {
	if math.IsNaN(gear) || math.IsInf(gear, 0) {
		return 0, fmt.Errorf("VSE tier is not a finite number")
	}
	if gear < vseGearMin-1e-9 || gear > vseGearMax+1e-9 {
		return 0, fmt.Errorf("VSE tier %g is outside the panel range %g…%g (the 10 tiers of arrays.xml)",
			gear, vseGearMin, vseGearMax)
	}
	// must be an integer multiple of 0.1 (floating-point tolerance is half a tier)
	steps := gear / vseGearStep
	if math.Abs(steps-math.Round(steps)) > 1e-6 {
		return 0, fmt.Errorf("VSE tier %g is not a panel tier: the panel only has "+
			"the ten discrete tiers 0.1/0.2/…/1.0 (+56 per tier)", gear)
	}
	return int(math.Round(gear * vseReconstructPerGear)), nil
}

// vseExciterParams expands a tier into harmonic exciter parameters (factory shape + wet gain determined by the tier).
//
//	Mix = reconstruct/100 = UI×5.6      (kernel convention, see the /100 in ViPER4AndroidService.java:1708)
//	Everything else (the 10 harmonic amplitudes / HPF 7600 / LPF fs/2−2000) is SpectrumExtend's
//	factory value and does not depend on the tier.
func vseExciterParams(gear float64) (exciterParams, error) {
	r, err := vseReconstruct(gear)
	if err != nil {
		return exciterParams{}, err
	}
	p := exciterDefaultParams()
	p.Mix = float64(r) / 100.0
	return p, nil
}

// vseGearOf **derives** the tier back from the current harmonic exciter parameters: it only counts when the parameters are exactly "the VSE shape of some tier".
//
// Why derive it instead of storing a second tier: `currentExciter` is the single runtime
// truth (state.go only stores that too). If a `gear` were stored as well, the two paths (the
// `exciter` field and the `vse` field) could each be half-updated and contradict each other.
// When the panel cannot get a tier, ok=false is returned (for example when the user has
// customised the harmonics through the exciter API) and the UI shows "custom".
func vseGearOf(p exciterParams) (float64, bool) {
	def := exciterDefaultParams()
	f := func(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
	if len(p.Harmonics) != len(def.Harmonics) {
		return 0, false
	}
	for i := range p.Harmonics {
		if !f(p.Harmonics[i], def.Harmonics[i]) {
			return 0, false
		}
	}
	if !f(p.HPFHz, def.HPFHz) || !f(p.LPFHz, def.LPFHz) {
		return 0, false
	}
	for steps := 1; steps <= 10; steps++ {
		gear := float64(steps) * vseGearStep
		r, err := vseReconstruct(gear)
		if err != nil {
			continue
		}
		if f(p.Mix, float64(r)/100.0) {
			return gear, true
		}
	}
	return 0, false
}

// setVSE sets VSE by tier (gear == nil means off).
//
// It lands on the **same** `currentExciter`: VSE and "custom harmonic exciter" share those
// same 4 hardware slots, and whichever is set last wins (consistent with VSE simply being
// SpectrumExtend in V4A).
func setVSE(gear *float64) error {
	if gear == nil {
		return setExciter(nil)
	}
	p, err := vseExciterParams(*gear)
	if err != nil {
		return err
	}
	return setExciter(&p)
}

// vseView is what goes back to the UI: the current tier (nil when the shape is not VSE).
func vseView() any {
	if currentExciter == nil {
		return nil
	}
	gear, ok := vseGearOf(*currentExciter)
	if !ok {
		return nil
	}
	r, _ := vseReconstruct(gear)
	return map[string]any{
		"gear":        gear,
		"reconstruct": r,
		"reference":   vseReferenceBarkHz,
		"mix":         currentExciter.Mix,
		"harmonics":   currentExciter.Harmonics,
	}
}

// vseAvailable uses the same criterion as the harmonic exciter: a POLY slot must exist.
func vseAvailable() bool { return exciterAvailable() }

// vseGearTable is what goes back to the UI: the 10 panel tiers → kernel parameters (the
// **complete table** of the tier mapping; sources at the top of the file). The UI uses it to
// show "what is actually sent down for this tier", avoiding the black box of "one number on
// the panel, another one in the kernel".
func vseGearTable() []map[string]any {
	out := make([]map[string]any, 0, 10)
	for steps := 1; steps <= 10; steps++ {
		gear := float64(steps) * vseGearStep
		r, err := vseReconstruct(gear)
		if err != nil {
			continue
		}
		out = append(out, map[string]any{
			"gear":        gear,
			"reconstruct": r,
			"mix":         float64(r) / 100.0,
		})
	}
	return out
}
