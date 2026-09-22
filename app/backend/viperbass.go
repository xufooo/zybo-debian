// SPDX-License-Identifier: GPL-2.0-only
// viperbass.go — V4A `ViPERBass` (bass enhancement): **the NATURAL_BASS profile + the PURE_BASS_PLUS profile**
//
// ── The three modes and this round's scope ───────────────────────────
//
//	Enum source: the community ViPER4Android rewrite (viperfx-re): src/viper/effects/ViPERBass.h:13-17
//	    0 NATURAL_BASS / 1 PURE_BASS_PLUS / 2 SUBWOOFER
//	Panel source: the client sources (viper4android_fx): android_4.x-5.x/res/values/arrays.xml:535 (`vbass_mode`)
//	    Natural Bass / Pure Bass+ / Subwoofer, three entries with values 0/1/2.
//
//	· profile 0 NATURAL_BASS   — implemented in this file (both structure and gain were measured against the official core, see below).
//	· profile 1 PURE_BASS_PLUS — **our implementation has been able to do it since 2026-09-21**: the engine gained a **second small FIR path**
//	    (OP_SFIR, 8 MACs/beat, 64 taps), so the dry path's 63-tap polyphase kernel now has its own
//	    coefficients/history/multipliers and **coexists with the user-exclusive 8192-tap convolution stage without interfering**.
//	    When the hardware has not built this path (CAP1 bit7 clear / CAP4 reports 0) it is still **refused outright**, never silently degraded.
//	    The structure and the origin of the three measured constants are in the "PURE_BASS_PLUS" section at the end of the file.
//	· profile 2 SUBWOOFER — not done (`utils/Subwoofer.cpp` is a different pole filter, outside this round's scope).
//
// ── Primary sources (code) ───────────────────────────────────────────
//
//	viperfx-re (community ViPER4Android rewrite): src/viper/effects/ViPERBass.cpp:47-48 (the NATURAL_BASS branch)
//	    samples[i]   += biquad[0].ProcessSample(samples[i])   * bassFactor;
//	    samples[i+1] += biquad[1].ProcessSample(samples[i+1]) * bassFactor;
//	viperfx-re: src/viper/effects/ViPERBass.cpp:93-94 / :110-113 (low-pass parameters)
//	    biquad[i].SetLowPassParameter((float)speaker, samplingRate, 0.53);
//	viperfx-re: src/viper/utils/Biquad.cpp:91-106 (SetLowPassParameter = **the standard RBJ second-order low-pass**)
//	    alpha = sin(ω)/(2Q) ⇒ Q = 0.53, fc = `speaker`.
//	Panel entries (`arrays.xml:545,565`):
//	    vbass_freq  = 30/40/50/60/66/78/80/100 Hz (= the `speaker` argument above)
//	    vbass_boost = 50/100/…/600 (step 50), labels 3.5/6.0/8.0/…/17.0 dB
//
// ── oracle measurements (official core `libviperfx_x64_linux.so`; the harness is the ViPERFX oracle, viperfx_oracle) ──
//
//	1) The low-pass is exactly RBJ(fc=speaker, Q=0.53): sweeping 20…300 Hz (`bassgain.c` low-frequency sine + phase lock),
//	   with speaker=40 the model ↔ measurement difference at 20/30/40/50/60/80/100/200 Hz is **0.04…0.18 dB**.
//	2) `bassFactor = gain/100` (**no +1**): at gain=0 the measurement is **bit-identical to bypass** (the wet signal is exactly 0);
//	   for gain=50…600 the wet amplitude is strictly proportional to gain (50→100 is exactly ×2, →600 exactly ×12).
//	   ⚠️ The panel's dB labels (50 → "3.5 dB") describe the total boost for a **mono source**:
//	      out = x + (g/100)·LP((L+R)/2), which for L=R is 1+g/100 ⇒ 20log10(1.5)=3.52 dB ✓ that matches.
//	3) **The wet signal uses the mono sum**: (L+R)/2 passes the low-pass and is then added to both channels **at the same time**.
//	   Measured: driving the left channel alone makes **half** the wet signal appear on the right (0.1666 on the right at gain=50, 1.9991 at gain=600,
//	   strictly proportional to gain); driving the right channel alone mirrors it. This is not a crosstalk bug but deliberate "bass mono-isation".
//	4) ViPERBass **only takes effect in headphone FX**: with `PARAM_FX_TYPE_SWITCH=2` (speaker) the measurement is bit-identical to bypass
//	   (the same rule as ColorfulMusic: the `PARAM_HPFX_*` family runs only in the headphone chain).
//
// ── One **known deviation** of our implementation from the official core (which must be stated) ──
//
//	3) That "mono sum" is **impossible here**: the engine's cross-channel register (bus 14) is **crossfeed's exclusively**
//	(write = this channel's lo, read = the other channel's lo, latched at the end of the pass), so ViPERBass writing it would tread on crossfeed.
//	So it lands as **per channel** `out_i = x_i + (g/100)·LP(x_i)`:
//
//	  · bass where L == R (the overwhelming majority of real music, and the case the panel's dB labels describe):
//	    the two expressions are **bit-identical** — the official core's (LP(L)+LP(R))/2 is just LP(L). ✓
//	  · bass where left and right are out of phase (rare): the core cancels it, our implementation does not. Up to 6 dB of wet-signal difference.
//
//	This is a deliberate approximation, not "we did not measure it": the measured conclusion is in 3), and the implementation trade-off is in the note on the nodes function below.
//	To be fully faithful the engine would need a **channel-sum bus** (or several copies of the cross-channel register).
//
// ── Fixed-point accuracy (why the code contains a 1/32 scaling section) ────
//
//	For 40 Hz / Q0.53 the low-pass numerator is b0 = 6.8e-6, while the coefficient format is 18-bit Q3.15 (1 LSB = 3.05e-5)
//	⇒ b0/b1/b2 **all round to 0** and the low-pass output is always 0 (measured: without scaling the 40 Hz low-pass response is
//	0 across the whole band). The remedy: multiply the numerator as a whole by K (K=32 here, a power of two) and insert a `×1/K`
//	scaling biquad before it (b0=1/32 is exactly 1024 in Q3.15). That way the low-pass **internal** level is still on the order of LP(x)
//	(so it does not blow up the 24-bit state), while the numerator coefficients are 32× larger ⇒ the quantisation error is 32× smaller.
//
//	Checked against the oracle measurement table (the calibration case in `viperbass_test.go`, gain 50/200/600 × 20…200 Hz):
//	the maximum deviation from the official core is **0.34 dB** (under the same scaling K=16 gives 0.49 dB and K=64 gives 0.40 dB).
//
//	⚠️ When the panel gain > 400 (wet > 4.0, i.e. the Q3.15 ceiling of MIX2), the excess factor is folded into the low-pass numerator
//	   ⇒ the low-pass path's internal level reaches up to 1.5× full scale. With in-chain headroom on (HEADROOM=3) that is safe;
//	   with the headroom off, a full-scale low frequency gets flattened by the saturator (it sounds harder, not silent).

package main

import (
	"fmt"
	"math"
)

// ViPERBass modes (the enum from `ViPERBass.h:13-17`; the values are the panel values).
const (
	viperBassModeNatural   = 0 // NATURAL_BASS — implemented
	viperBassModePBP       = 1 // PURE_BASS_PLUS — see the end of the file; supported since 2026-09-21 (needs the second small FIR)
	viperBassModeSubwoofer = 2 // SUBWOOFER — not done
)

// viperBassQ is the low-pass Q (0.53 from `ViPERBass.cpp:93`, matching the oracle measurement to within 0.18 dB).
const viperBassQ = 0.53

// viperBassPreScale is the fixed-point scaling factor for the numerator (K=32, a power of two ⇒ 1/K is exact in Q3.15).
// Why 32: the maximum deviation from the oracle measurement table is 0.34 dB (K=16 → 0.49, K=64 → 0.40).
const viperBassPreScale = 32.0

// viperBassParams holds ViPERBass's user-facing parameters (the raw panel values).
type viperBassParams struct {
	// Mode: 0/1/2 = Natural Bass / Pure Bass+ / Subwoofer (the panel's vbass_mode).
	Mode int `json:"mode"`
	// CutoffHz: the panel's `vbass_freq` (30/40/50/60/66/78/80/100 Hz), i.e. the low-pass cutoff frequency.
	CutoffHz float64 `json:"cutoff_hz"`
	// Gain: the panel's `vbass_boost` (50…600, step 50). Wet coefficient = Gain/100 (0.5…6.0).
	Gain float64 `json:"gain"`
}

// viperBassDefaultParams is the V4A client's default: mode "0", freq "40", gain "50"
// (the three default strings of the getString calls at `ViPER4AndroidService.java:1846-1852`).
func viperBassDefaultParams() viperBassParams {
	return viperBassParams{Mode: viperBassModeNatural, CutoffHz: 40, Gain: 50}
}

var currentViPERBass *viperBassParams

// viperBassAvailable: the low-pass + dry/wet sum = one BIQUAD slot + one MIX2 slot,
// so the criterion belongs to the same family as crossfeed/exciter (the 0.2 slot-table engine + MIX2 capability).
func viperBassAvailable() bool {
	return dspEngineGen == 1 && dspReadEngineCaps().Mix2Available()
}

// viperBassValidate checks the panel profile/range. It is factored out so it can be unit-tested offline.
func viperBassValidate(p viperBassParams) error {
	switch p.Mode {
	case viperBassModeNatural:
	case viperBassModePBP:
		// ⚠️ **No hardware check here** (the same rule as mode 0): hardware capability is held by two gates —
		//   1) on the API side `setViPERBass` checks CAP1 bit7 + CAP4 (sfirAvailableNow);
		//   2) the chain compiler checks hwSFIRMaxTaps (the tap ceiling CAP4 reports) ⇒ if it does not fit, it errors out explicitly.
		//   Putting a hardware check into validate would make "compile a PBP chain offline" impossible
		//   (with no hardware every CAP bit is 0), and offline is exactly the planner's unit-test environment.
	case viperBassModeSubwoofer:
		return fmt.Errorf("ViPERBass's Subwoofer profile is not implemented here (it uses the pole filter in utils/Subwoofer.cpp, " +
			"which is not the same structure as Natural Bass)")
	default:
		return fmt.Errorf("ViPERBass mode accepts only 0/1/2 (Natural Bass / Pure Bass+ / Subwoofer), got %d", p.Mode)
	}
	if math.IsNaN(p.CutoffHz) || p.CutoffHz < 30 || p.CutoffHz > 100 {
		return fmt.Errorf("ViPERBass cutoff %g is outside the panel range 30…100 Hz (vbass_freq)", p.CutoffHz)
	}
	if math.IsNaN(p.Gain) || p.Gain < 0 || p.Gain > 600 {
		return fmt.Errorf("ViPERBass gain %g is outside the panel range 0…600 (vbass_boost)", p.Gain)
	}
	return nil
}

// setViPERBass sets (or clears, by passing nil) ViPERBass. Unsupported hardware is **reported as an error**, never silently dropped.
func setViPERBass(p *viperBassParams) error {
	if p == nil {
		currentViPERBass = nil
		markStateDirty()
		return nil
	}
	if !viperBassAvailable() {
		return fmt.Errorf("this hardware has no MIX2 slot (CAP bit not set), so ViPERBass's dry/wet sum cannot be downloaded")
	}
	if err := viperBassValidate(*p); err != nil {
		return err
	}
	// PBP additionally requires the **second small FIR** (the 63-tap polyphase kernel on the dry path).
	// ⚠️ The criterion is the capability bits the hardware reports (CAP1 bit7 + the CAP4 capacity), not the engine generation:
	//    whether a small FIR is built into a bitstream of the same generation is a build decision (saving 8 DSPs for a cut-down version is perfectly reasonable),
	//    and using the version number as a proxy would diverge from the facts. If it is not satisfied the request is **refused outright**, never silently degraded to NATURAL —
	//    that would give the user "a different effect" from what the panel says.
	if p.Mode == viperBassModePBP && !sfirAvailableNow() {
		return fmt.Errorf("ViPERBass's Pure Bass+ needs the **second small FIR** (CAP1 bit7 = OP_SFIR, "+
			"CAP4 reports its capacity), which this bitstream does not report (offline / no hardware lands here too): the official core's dry path "+
			"is not a bypass but a **%d-tap FIR** (the taps are character-for-character the same as "+
			"POLYPHASE_COEFFICIENTS_2 in the community ViPER4Android rewrite (viperfx-re), with the main tap at index 31 and DC −13.85 dB), "+
			"plus wet signal delayed by **%d samples** and multiplied by bassFactor. Approximating it with biquads takes 31 sections; "+
			"and the alternative \"bypass + delayed wet\" is already 8.4 dB off at 30 Hz ⇒ better not to do it at all",
			viperBassPBPKernelLen, viperBassPBPWetDelay)
	}
	q := *p
	currentViPERBass = &q
	markStateDirty()
	return nil
}

// viperBassView is what is handed back to the UI: the current parameters (nil = off).
func viperBassView() any {
	if currentViPERBass == nil {
		return nil
	}
	return currentViPERBass
}

// viperBassNodes compiles ViPERBass into a node sequence. Dispatch is by profile:
//
//	profile 0 NATURAL_BASS   — 2 slots (scaling+low-pass packed together, MIX2): bypassed dry + low-passed wet;
//	profile 1 PURE_BASS_PLUS — 4 slots (delay 64, scaling+low-pass, **small FIR dry path**, MIX2).
func viperBassNodes(p viperBassParams) ([]planNode, error) {
	if err := viperBassValidate(p); err != nil {
		return nil, err
	}
	if p.Mode == viperBassModePBP {
		return viperBassPBPNodes(p)
	}
	wet := p.Gain / 100.0 // = bassFactor in ViPERBass.cpp
	w, k := wet, viperBassPreScale
	if wet > maxMixQ315 {
		w = maxMixQ315
		k = viperBassPreScale * wet / w
	}
	b0, b1, b2, a1, a2 := rbjLowPassFloat(p.CutoffHz, viperBassQ, sampleRate)
	// Scaling section: y = x/K (1/K is a power of two ⇒ exact in Q3.15; b1/b2/a1/a2 are all 0, so no state is used)
	pre := [5]int32{floatToQ315(1.0 / viperBassPreScale), 0, 0, 0, 0}
	// Low-pass section: numerator ×k (k is a real number, no longer a power of two — it has to express that wet/4 factor exactly)
	//
	// ⚠️ This uses `floatToQ315` (round to nearest) rather than `q315Round`: the latter **adds another half LSB**
	//    to the scaled value before rounding, which for a low-frequency numerator with only 7~15 LSBs is a systematic overshoot of the order of +5% (measured: it drags the
	//    40 Hz/gain200 error from 0.66 dB to … see below). The `rbj*` family uses floatToQ315 anyway, so this stays consistent with it.
	//    The range (±4.0) is caught by the explicit check right after, not by q315Round's saturation.
	lp := [5]int32{
		floatToQ315(b0 * k), floatToQ315(b1 * k), floatToQ315(b2 * k),
		floatToQ315(a1), floatToQ315(a2),
	}
	for i, v := range lp {
		if v > coefMax || v < coefMin {
			return nil, fmt.Errorf("ViPERBass low-pass coefficient %d is outside the Q3.15 range (%d) —"+
				" cutoff %g Hz / gain %g cannot be expressed here", i, v, p.CutoffHz, p.Gain)
		}
	}
	// ⚠️ With wet=0 the weight must be **exactly 0**: `q315Round` would turn it into 1 LSB (it adds half an LSB before rounding),
	//    while "gain=0 is bit-identical to bypass" is an oracle-measured fact (see measurement 2 in the file header) — leaving 1 LSB of
	//    wet signal would break it. Only gain>0 goes through q315Round (consistent with every other MIX2 weight in the project).
	wq := int32(0)
	if w > 0 {
		wq = q315Round(w)
	}
	return []planNode{{
		Kind:  planKindViPERBass,
		Extra: pre, // scaling section
		Coefs: lp,  // low-pass section
		Mix:   [2]int32{q315Round(1.0), wq},
	}}, nil
}

// ── PURE_BASS_PLUS (PBP) ────────────────────────────────────────────
//
// The official core's structure (the PURE_BASS_PLUS branch at `ViPERBass.cpp:60-79`):
//
//	dry path: samples ──► Polyphase (63-tap FIR, POLYPHASE_COEFFICIENTS_2) ──► FIR63(x)[n]
//	wet path: samples ──► biquad[0/1] (RBJ low-pass fc/Q0.53) ──► waveBuffer (delay) ──► LP(x)[n−64]
//	output:   out[n] = FIR63(x)[n] + bassFactor · LP(x)[n−64]          bassFactor = gain/100
//
// All three numbers were **measured with the oracle** (viperbass_test.go's
// TestViPERBassPBPMeasuredSpec records how):
//
//	· 63 taps, main tap @31, DC Σk = 0.2027 = −13.85 dB;
//	· the wet signal trails the dry path by **64** samples (integer-aligned with Natural's wet signal: the RMS at 63/65 is both 0.59, and only 64 gives 0.0);
//	· bassFactor = gain/100 (**no +1**).
//
// How our implementation lands it (4 slots):
//
//	slot0 DELAY  64        : bus   → bus+1
//	slot1 BIQUAD n=2       : bus+1 → bus+2   (scaling ×1/K → low-pass, the same fixed-point trick as NATURAL)
//	slot2 SFIR   n=3       : bus   → bus+3   (dry path; the taps use their own address segment coefSFIRBase)
//	slot3 MIX2             : dry bus+3 + wet bus+2 → bus+2
//
// As with NATURAL there is one **known deviation** (already stated in the file header): the wet signal's low-pass is **per channel**,
// not the official core's (LP(L)+LP(R))/2 — the engine's cross-channel register (bus 22) is crossfeed's exclusively.
// For L==R bass (the case the panel's dB labels describe) the two expressions are bit-identical; out-of-phase left/right differs by up to 6 dB of wet signal.
//
// Fixed-point accuracy: the same scheme as NATURAL — the 40 Hz low-pass numerator b0 = 6.8e-6 is 0 in Q3.15,
// so a ×1/K (K=32) scaling section is inserted and the low-pass numerator is multiplied by k. For gain > 400 (wet > 4.0,
// the MIX2 weight ceiling) the excess factor is folded into the low-pass numerator, exactly as in NATURAL.
func viperBassPBPNodes(p viperBassParams) ([]planNode, error) {
	wet := p.Gain / 100.0
	w, k := wet, viperBassPreScale
	if wet > maxMixQ315 {
		w = maxMixQ315
		k = viperBassPreScale * wet / w
	}
	b0, b1, b2, a1, a2 := rbjLowPassFloat(p.CutoffHz, viperBassQ, sampleRate)
	pre := [5]int32{floatToQ315(1.0 / viperBassPreScale), 0, 0, 0, 0}
	lp := [5]int32{
		floatToQ315(b0 * k), floatToQ315(b1 * k), floatToQ315(b2 * k),
		floatToQ315(a1), floatToQ315(a2),
	}
	for i, v := range lp {
		if v > coefMax || v < coefMin {
			return nil, fmt.Errorf("ViPERBass PBP low-pass coefficient %d is outside the Q3.15 range (%d) —"+
				" cutoff %g Hz / gain %g cannot be expressed here", i, v, p.CutoffHz, p.Gain)
		}
	}
	// The wet weight follows the same rule as NATURAL: with wet=0 it is **exactly 0** (the oracle measured "gain=0 is bit-identical to bypass").
	wq := int32(0)
	if w > 0 {
		wq = q315Round(w)
	}
	// Dry-path taps: the 63 polyphase kernel values + 1 zero (rounding up to sfirTaps = 64, matching fir_bank's
	// two constraints "taps is a multiple of MACS, block count is a power of two"; one extra zero tap is bit-equivalent).
	sfir := make([]int32, sfirTaps)
	for i, v := range viperBassPBPKernel {
		sfir[i] = floatToQ315(v)
	}
	return []planNode{{
		Kind:  planKindViPERBassPBP,
		Len:   viperBassPBPWetDelay, // wet-signal delay (samples)
		Extra: pre,                  // scaling section
		Coefs: lp,                   // low-pass section
		Mix:   [2]int32{q315Round(1.0), wq},
		SFir:  sfir, // dry-path taps (own address segment)
	}}, nil
}

// viperBassPBPKernelLen / viperBassPBPWetDelay: the two measured PBP constants (see above).
const (
	viperBassPBPKernelLen = 63
	viperBassPBPWetDelay  = 64
)

// viperBassPBPKernel is the 63-tap FIR of the official core's **Pure Bass+ dry path**.
//
// Source: `POLYPHASE_COEFFICIENTS_2` in the community ViPER4Android rewrite (viperfx-re),
// src/viper/utils/Polyphase.cpp. **Checked tap by tap against the official core** (2026-09-21): with an impulse of 20000 in,
// frames 255…317 of the output are these 63 numbers ×20000 (e.g. frame 286 = 12441 = 0.6221×20000, matching kernel[31];
// frame 255 = −47 ≈ kernel[0]×20000), and **the dry path's bypass spike no longer exists**.
//
// It pushes the dry signal down by 13.85 dB at low frequencies (Σk = 0.2027) — this is precisely what fundamentally separates PBP from NATURAL,
// and why the alternative "bypass + delayed wet" does not hold up (8.4 dB off at 30 Hz).
//
// ⚠️ This table is also the **source of the golden vectors**: the golden-vector model pbp_model.py copies the same floats character for character,
//
//	and generates from them the Q3.15 values and impulse response used by the sfir engine testbench. Any change must be made on both sides;
//	TestViPERBassPBPKernelMatchesModel in viperbass_test.go watches for that.
var viperBassPBPKernel = [viperBassPBPKernelLen]float64{
	-0.002339, -0.002073, -0.001940, -0.001675, -0.001515, -0.001329,
	-0.001223, -0.001037, -0.000904, -0.000851, -0.000532, -0.000851,
	-0.000106, -0.001010, 0.000558, -0.001435, 0.001302, -0.001967,
	0.002259, -0.002605, 0.003216, -0.003562, 0.004784, -0.005475,
	0.007655, -0.008506, 0.017622, -0.024639, 0.028679, -0.017303,
	-0.032507, 0.623321, 0.184702, -0.166867, 0.025729, -0.078490,
	-0.015735, -0.041199, -0.023151, -0.031524, -0.020121, -0.024985,
	-0.017303, -0.019616, -0.015018, -0.015204, -0.012838, -0.011881,
	-0.010951, -0.009516, -0.009090, -0.007788, -0.007442, -0.006353,
	-0.006087, -0.005183, -0.004970, -0.004253, -0.003987, -0.003482,
	-0.003216, -0.002871, -0.002578,
}
