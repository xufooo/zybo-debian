// SPDX-License-Identifier: GPL-2.0-only
// dynbass.go — replication of V4A `DynamicBass` (the bass half of `DynamicSystem`), **simple branch**
//
// Primary source (checked line by line): the community ViPER4Android rewrite (viperfx-re): src/viper/utils/DynamicBass.cpp
//
//	:11-20  SetSamplingRate/constructor: lowFreqX=120, highFreqX=80, lowFreqY=40,
//	        highFreqY=fs/4; `lowPass.SetLowPassParameter(55, fs, qPeak/666 + 0.5)`
//	:21-28  **simple branch** (lowFreqX ≤ 120):
//	          avg = lowPass55.ProcessSample(L + R);  L += avg;  R += avg;
//	:29-41  **full branch** (lowFreqX > 120): two chains of 6-tap PolesFilter × 2 channels + two side gains
//	        ⇒ our engine has no such opcode and **refuses it outright** (see below)
//	:50-60  SetBassGain(gain): qPeak = clamp((gain-1)/20·1600, 0, 1600),
//	        then the low-pass is reset (Q = qPeak/666 + 0.5)
//
// Where the parameters come from (client → core):
//
//	coeffs = "x1;x2;x3;x4;x5;x6" ⇒ XCOEFFS(x1,x2) / YCOEFFS(x3,x4) / SIDEGAIN(x5,x6)
//	  the official ViPER4Android client (viper4android_fx): android_4.x/src/com/vipercn/viper4android_v2/service/ViPER4AndroidService.java:1825-1834
//	  (default string "100;5600;40;80;50;50"; `dynamicsystem_outputs_values[4]` in the official panel's
//	   `res/values/arrays.xml:525-534` is the same value = "Common Earphone(v2)")
//	bass (a percentage) ⇒ dsBass = bass·20 + 100 ⇒ BASSGAIN ⇒ core `SetBassGain(dsBass/100)`
//	  (same file :1836-1839 and the community ViPER4Android rewrite (viperfx-re): src/viper/ViPER.cpp:335-337)
//
// ── How our implementation maps it (hardware frozen, existing opcodes only) ────
//
// The simple branch's formula needs "L + R in the same beat", but the engine is **time-multiplexed per channel** (one channel per pass),
// and the only cross-channel path is bus 22 plus the `xf_lo` register pair. Its timing was
// **measured pass by pass** in the cross-phase engine testbench tb_engine_xphase.v (not inferred):
//
//	on the **R pass** of frame n, the other channel reads **the same frame's** L[n]      (0 samples apart)
//	on the **L pass** of frame n, the other channel reads **the previous frame's** R[n−1]  (1 sample apart)
//
// ⇒ our implementation can only do "**one 55 Hz low-pass per channel, side signal = own channel + other channel (L pass 1 sample late)**".
//
//	The R channel's filter input sequence is sample for sample V4A's `L[n]+R[n]` ⇒ **its output is exactly V4A's
//	`avg[n]` (bit for bit)**; the L channel's input is `L[n]+R[n−1]`, with only the R component 1 sample late
//	(0.4125° at 55 Hz = 360°·55/48000).
//
//	Building it as "two filters" rather than "one shared avg" was forced by the hardware: BIQUAD state is partitioned per channel
//	(`st_r = sidx + stb`, L uses 0..47 and R uses 48..95, `dsp_engine.v:1165`/`:1264`),
//	and a 16-bit address cannot yield the same index; while "compute in one pass, reuse in the other" would need an intermediate value
//	held **across passes** on some bus, and the bypass action of an L-only slot on the other pass is `bus[out] <= bus[in]` (`dsp_engine.v:1217`),
//	which is bound to overwrite that value. Both facts are pinned in dynbass_test.go.
//
// Slot shape (4 slots / 3 sections / 15 coefficients):
//
//  1. send slot n=0 (bypass copy, **takes no section and no MAC**): this channel's dry signal → cross-channel bus 22
//  2. MIX2  1/K · dry + 1/K · (other channel)     → side signal/K = (L+R)/K (see fixed-point scaling)
//  3. BIQUAD 55 Hz low-pass (numerator ×K)            → avg
//  4. MIX2  1.0 · dry + 1.0 · avg            → output
//
// Fixed-point scaling (K=256, a power of two): the 55 Hz low-pass numerator **does not fit** in Q3.15
// (b0 = 1.28e-5 while 1 LSB is 3.05e-5 ⇒ quantising it directly turns it all into 0 and the low-pass output stays 0).
// The remedy is the same as ViPERBass's (`viperbass.go:59-68`): multiply the numerator as a whole by K while
// **dividing by K** the signal entering the filter. No extra "scaling section" is added here — **1/K is folded into the weight of that MIX2 in 2)** (128 LSB, exact),
// so the whole block is still 4 slots and 3 sections. The cost: the branch entering the low-pass has only 24−log2(256) = 16 bits of effective precision
// (the bass it adds sits at ~96 dB SNR, and the dry path is unaffected).
//
// Measured deviation (Go side after Q3.15 quantisation, compared point by point against V4A's float response; see TestDynamicBassLowPassGain):
// 20 Hz −1.06 dB, 30 Hz −0.69 dB, 55 Hz −0.02 dB, 80 Hz +0.19 dB, 200 Hz +0.11 dB.
// The main cause is the quantisation of the **denominator**: under Q3.15 a1/a2 have only 15 fractional bits, while at 55 Hz/48 kHz
// `1+a1+a2` is itself only 1.686 LSB (2.000 after quantisation) — an inherent limit of the frozen hardware,
// not something that can be tuned away. That 1 dB error in the low band is recorded here honestly.
package main

import (
	"fmt"
	"strconv"
	"strings"
)

const (
	// dynamicBassLPHz is the 55 Hz from `DynamicBass.cpp:17`/`:47`/`:59`/`:84`.
	dynamicBassLPHz = 55.0
	// dynamicBassQDivisor / base come from `qPeak/666 + 0.5` (`DynamicBass.cpp:17`).
	dynamicBassQDivisor = 666.0
	dynamicBassQBase    = 0.5
	// dynamicBassQPeakMax is `SetBassGain`'s upper clamp (`DynamicBass.cpp:54-56`: x>1600 ⇒ 1600).
	dynamicBassQPeakMax = 1600.0
	// dynamicBassBassMaxPct is the client's upper bound for `dynamicsystem_bass_values` (arrays.xml:470-520).
	dynamicBassBassMaxPct = 100.0
	// dynamicBassPreScale is the fixed-point scaling K for the low-pass numerator (a power of two ⇒ both 1/K and K are exact).
	dynamicBassPreScale = 256.0
)

// dynamicBassParams carries this stage's user-facing parameters, **exactly as the client defines them**:
//   - Coeffs: the "x1;x2;x3;x4;x5;x6" string from the client's `dynamicsystem.coeffs`;
//   - Bass: the percentage from the client's `dynamicsystem.bass` (0..100).
//
// The simple branch uses only x1 (branch selection) and Bass (Q); x2..x6 matter only to the full branch,
// but they are **stored anyway** (same parameter set, so a mode switch loses nothing), and the API notes that they are inert.
type dynamicBassParams struct {
	Coeffs string  `json:"coeffs"`
	Bass   float64 `json:"bass"`
}

// dynamicBassDefaultParams is V4A's **official default profile**: coeffs "100;5600;40;80;50;50"
// (`headset_preferences_l2.xml:274` / `arrays.xml:529` = "Common Earphone(v2)"),
// bass = 0 (the `android:defaultValue="0"` at :284-288 of the same xml).
func dynamicBassDefaultParams() dynamicBassParams {
	return dynamicBassParams{Coeffs: "100;5600;40;80;50;50", Bass: 0}
}

// dynamicBassCoeffsOf parses "x1;x2;x3;x4;x5;x6" (6 non-negative integers; the client uses Integer.valueOf).
func dynamicBassCoeffsOf(p dynamicBassParams) ([6]float64, error) {
	var c [6]float64
	parts := strings.Split(strings.TrimSpace(p.Coeffs), ";")
	if len(parts) != 6 {
		return c, fmt.Errorf("DynamicBass coeffs must be 6 numbers (\"x1;x2;x3;x4;x5;x6\"), got %q", p.Coeffs)
	}
	for i, s := range parts {
		v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil {
			return c, fmt.Errorf("DynamicBass coeffs field %d (%q) is not a number", i+1, s)
		}
		if v < 0 {
			return c, fmt.Errorf("DynamicBass coeffs field %d = %g, the client only sends non-negative integers", i+1, v)
		}
		c[i] = v
	}
	return c, nil
}

// dynamicBassBassGain turns the client percentage into the argument of the core `SetBassGain`:
// `dsBass = bass*20 + 100` from `ViPER4AndroidService.java:1836-1838`, then `/100`.
func dynamicBassBassGain(pct float64) float64 {
	return (pct*20.0 + 100.0) / 100.0
}

// dynamicBassQPeak follows `DynamicBass.cpp:53-57`: x = (gain−1)/20·1600, clamped above 1600.
func dynamicBassQPeak(bassGain float64) float64 {
	x := (bassGain - 1.0) / 20.0 * 1600.0
	if x > dynamicBassQPeakMax {
		x = dynamicBassQPeakMax
	}
	if x < 0 {
		x = 0
	}
	return x
}

// dynamicBassQ is the low-pass Q: `qPeak/666 + 0.5` (`DynamicBass.cpp:17`).
// bass=0 (default) ⇒ qPeak=0 ⇒ Q=0.5; bass=100 ⇒ qPeak=1600 ⇒ Q≈2.902.
func dynamicBassQ(qPeak float64) float64 {
	return qPeak/dynamicBassQDivisor + dynamicBassQBase
}

// dynamicBassQForBass is the one-step substitution "panel percentage → Q" (for the API echo and the criteria).
func dynamicBassQForBass(pct float64) float64 {
	return dynamicBassQ(dynamicBassQPeak(dynamicBassBassGain(pct)))
}

// dynamicBassPreScaleWeight is the Q3.15 weight used by the two MIX2 slots 2)/4).
// 1/K = 128 LSB (exact: K is a power of two).
func dynamicBassPreScaleWeight() int32 { return int32(qOne / dynamicBassPreScale) }

// dynamicBassLowPassCoefs returns the 5 Q3.15 coefficients of the 55 Hz low-pass, **with the numerator already multiplied by K**.
//
// The low-pass itself follows V4A's `Biquad::SetLowPassParameter` (the community ViPER4Android rewrite (viperfx-re): src/viper/utils/Biquad.cpp:91-107)
// — it is the RBJ low-pass, `alpha = sin/(2Q)`; our `rbjLowPassFloat` has exactly the same shape.
func dynamicBassLowPassCoefs(pct float64) ([5]int32, error) {
	q := dynamicBassQForBass(pct)
	b0, b1, b2, a1, a2 := rbjLowPassFloat(dynamicBassLPHz, q, sampleRate)
	k := dynamicBassPreScale
	c := [5]int32{
		floatToQ315(b0 * k), floatToQ315(b1 * k), floatToQ315(b2 * k),
		floatToQ315(a1), floatToQ315(a2),
	}
	for i, v := range c {
		if v > coefMax || v < coefMin {
			return c, fmt.Errorf("DynamicBass 55 Hz low-pass coefficient %d = %d is outside the Q3.15 range"+
				" (bass=%g ⇒ Q=%g)", i, v, pct, q)
		}
	}
	if c[0] == 0 && c[1] == 0 && c[2] == 0 {
		return c, fmt.Errorf("DynamicBass 55 Hz low-pass numerator quantises to all zeros (bass=%g) —"+
			" the scaling K=%g did not take effect, so the low-pass would always output 0", pct, k)
	}
	return c, nil
}

// dynamicBassValidate checks the parameters and returns an error the user can make sense of.
//
// ⚠️ x1 > 120 takes the **full branch** of `DynamicBass.cpp:29-41`, which our implementation **currently refuses outright**.
// ⚠️ Correction (2026-09-22, the author checked the source): **it needs no new hardware**.
// `PolesFilter` (`utils/PolesFilter.cpp:29-47`) is two cascaded **4-stage first-order low-passes per channel**
//   - one 3-sample input delay, and the three outputs out1=x[3], out2=x[n−3]−y[3], out3=y[3]−x[3] are all quantities of this channel,
//     so the full branch `L' = x2+y3+sideX·y2+sideY·y1+x3` **has no cross-channel term at all**.
//     Two identical first-order sections in cascade = one second-order section (b0=a², b1=b2=0, a1=−2(1−a), a2=(1−a)²)
//     ⇒ it can be assembled from the existing `OP_BIQUAD`/`OP_DELAY`/`OP_MIX2` (about 11~12 slots).
//
// **The real limit is slots**: the engine caps at NSLOT=24 while the baseline chain already uses 22 ⇒ the full branch does not fit alongside the existing chain.
// So the reason for refusing here is "not enough slots / not implemented yet", not "missing hardware". Deferred to 0.3 (effect pairing + UI).
func dynamicBassValidate(p dynamicBassParams) ([6]float64, error) {
	c, err := dynamicBassCoeffsOf(p)
	if err != nil {
		return c, err
	}
	if p.Bass < 0 || p.Bass > dynamicBassBassMaxPct {
		return c, fmt.Errorf("DynamicBass bass percentage must be within 0..%g, got %g"+
			" (the client's `dynamicsystem_bass_values` caps out at %g)",
			dynamicBassBassMaxPct, p.Bass, dynamicBassBassMaxPct)
	}
	if c[0] > dynamicBassSimpleBranchMaxX1 {
		return c, fmt.Errorf("V4A DynamicBass's **full branch** cannot be done here: x1 = %g > %g"+
			" (`DynamicBass.cpp:29-41`) — our implementation **has not implemented** it yet (the existing opcodes can assemble it, about 11~12 slots,"+
			" but the engine caps at 24 slots and the baseline chain already uses 22 ⇒ it does not fit alongside the existing chain; **this is not missing hardware**)."+
			" Pick a profile with x1 ≤ %g (V4A's official default \"100;5600;40;80;50;50\" is one);"+
			" the full branch is deferred to 0.3 (effect pairing + UI)",
			c[0], dynamicBassSimpleBranchMaxX1, dynamicBassSimpleBranchMaxX1)
	}
	if _, err := dynamicBassLowPassCoefs(p.Bass); err != nil {
		return c, err
	}
	return c, nil
}

// ── V4A DynamicBass on the chain ─────────────────────────────────────

var currentDynamicBass *dynamicBassParams

// dynamicBassAvailable: whether this hardware can run the simple branch — it needs BIQUAD (CAP1 bit1) + MIX2 (CAP1 bit3).
// The cross-channel bus (number 22) is built into the engine and has no capability bit.
func dynamicBassAvailable() bool {
	if dspEngineGen != 1 {
		return false
	}
	c := dspReadEngineCaps()
	return c.BiquadAvailable() && c.Mix2Available()
}

// setDynamicBass sets (or clears, by passing nil). Invalid parameters / a missing opcode on the hardware are **reported as errors**.
func setDynamicBass(p *dynamicBassParams) error {
	if p == nil {
		currentDynamicBass = nil
		markStateDirty()
		return nil
	}
	if _, err := dynamicBassValidate(*p); err != nil {
		return err
	}
	if !dynamicBassAvailable() {
		return fmt.Errorf("this hardware cannot run V4A DynamicBass's simple branch: it needs the BIQUAD (CAP1 bit1)"+
			" and MIX2 (CAP1 bit3) slots, while the current bitstream reports opcode map %#x", dspReadEngineCaps().Opcodes)
	}
	q := *p
	currentDynamicBass = &q
	markStateDirty()
	return nil
}

// dynamicBassNodes compiles the parameters into chain nodes (1 node = 4 slots in the slot planner).
func dynamicBassNodes(p dynamicBassParams) ([]planNode, error) {
	if _, err := dynamicBassValidate(p); err != nil {
		return nil, err
	}
	lp, err := dynamicBassLowPassCoefs(p.Bass)
	if err != nil {
		return nil, err
	}
	return []planNode{{Kind: planKindDynBass, Coefs: lp}}, nil
}

// dynamicBassSlotCost is this stage's hardware cost (for the API echo and the docs, and the number pinned in the criteria).
//
// 4 slots: send (n=0 bypass copy) + side-signal MIX2 + 55 Hz low-pass BIQUAD + add-back MIX2.
// 3 sections: the send slot takes S_DISP's bypass branch and **writes no state** (`dsp_engine.v:1216-1220`),
// while the two MIX2s and the low-pass each count as one section (the same accounting as AnalogX/ViPERBass).
// 15 coefficients: three 5-word blocks.
const (
	dynamicBassSlots      = 4
	dynamicBassSections   = 3
	dynamicBassCoefWords  = 15
	dynamicBassSideLagSmp = 1 // in the L pass's side signal, how many samples the other channel lags (measured in the testbench)
)

// dynamicBassView is the current state handed back to the UI/API (nil = off).
func dynamicBassView() any {
	if currentDynamicBass == nil {
		return nil
	}
	p := *currentDynamicBass
	c, _ := dynamicBassCoeffsOf(p)
	q := dynamicBassQForBass(p.Bass)
	lp, _ := dynamicBassLowPassCoefs(p.Bass)
	return map[string]any{
		"coeffs": p.Coeffs, "bass": p.Bass,
		"x1": c[0], "branch": dynamicBassBranchOf(c[0]),
		"lowpass_hz": dynamicBassLPHz, "q": q,
		"q_peak":             dynamicBassQPeak(dynamicBassBassGain(p.Bass)),
		"bass_gain":          dynamicBassBassGain(p.Bass),
		"lowpass_coefs_q315": []int32{lp[0], lp[1], lp[2], lp[3], lp[4]},
		"pre_scale_k":        dynamicBassPreScale,
		"available":          dynamicBassAvailable(),
		"slots":              dynamicBassSlots, "sections": dynamicBassSections,
		"coef_words": dynamicBassCoefWords,
		"inert_coeffs": "x2..x6 only matter to the full branch (x1>120) and are unused by the simple branch; " +
			"they are stored anyway so a mode switch loses nothing",
		"side_channel_lag_samples": dynamicBassSideLagSmp,
		"side_channel_lag_note": "on frame n's R pass the other channel reads the same frame's L[n] (0 samples apart) ⇒ the right channel equals V4A bit for bit; " +
			"on the L pass it reads the previous frame's R[n−1] (1 sample apart = 0.4125° at 55 Hz) ⇒ in the left channel's side signal R lags by 1 sample. " +
			"Evidence = measured pass by pass in the cross-phase engine testbench tb_engine_xphase.v",
		"source": "the community ViPER4Android rewrite (viperfx-re): src/viper/utils/DynamicBass.cpp:11-28 (simple branch); " +
			"Biquad.cpp:91-107 (55 Hz low-pass); " +
			"ViPER4AndroidService.java:1825-1839 (parameter definitions)",
	}
}

// dynBassSideLagDegrees is the phase error (in degrees) of that 1 sample at 55 Hz: 360·f/fs.
// A pure function, so the criteria and the API can recompute it.
func dynBassSideLagDegrees() float64 {
	return 360.0 * dynamicBassLPHz / sampleRate
}

// dynamicBassFullBranchBlocker reports the **current blocker** for the full branch (x1 > 120) (so docs and the API can reference it,
// so it does not stay scattered across comments). ⚠️ 2026-09-22 correction: the blocker is **slots**, not hardware —
// `PolesFilter` is two 4-stage first-order low-passes per channel plus a 3-sample delay, with no cross-channel term,
// so it can be assembled from the existing opcodes (two identical first-order sections = one second-order section), about 11~12 slots;
// while the engine caps at NSLOT=24 and the baseline chain already uses 22 ⇒ it does not fit alongside the existing chain. Deferred to 0.3 (effect pairing + UI).
func dynamicBassFullBranchBlocker() string {
	return "the full branch (x1 > 120) is not implemented yet: the existing opcodes can assemble it (about 11~12 slots), " +
		"but the engine caps at 24 slots and the baseline chain already uses 22 ⇒ it does not fit alongside the existing chain. **No new hardware is needed**; deferred to 0.3."
}
