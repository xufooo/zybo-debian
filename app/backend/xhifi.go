// xhifi.go -- second half of A4': the **XHIFI preset** of V4A `ViPERClarity` (`utils/HiFi.cpp`, 78 lines, read in full)
//
// First-hand evidence (checked line by line, including the **argument-order trap** in the parameters):
//
//	ctor: `filters[i].lowpass = IIR_NOrder_BW_LH(1)`, `highpass = IIR_NOrder_BW_LH(3)`,
//	      `bandpass = IIR_NOrder_BW_BP(3)`; two `WaveBuffer(2, 0x800)`.
//	Reset: `lowpass.setLPF(120)`, `highpass.setHPF(1200)`, `bandpass.setBPF(120.f, 1200.f, fs)`;
//	      `buffers[0].PushZeros(fs/400)`, `buffers[1].PushZeros(fs/200)` (48k => 120 / 240 samples).
//	Process (per channel):
//	      out1 = LP1(x)  → lpBuf (through buffers[1], delayed by fs/200)
//	      out2 = HP3(x)  → **directly** written back to samples (no delay)
//	      out3 = BP3(x)  → bpBuf (through buffers[0], delayed by fs/400)
//	      samples = out2·gain·**1.2** + bpOut·gain + lpOut
//
//	⚠️ In `IIR_NOrder_BW_BP::setBPF` the **parameter names are the opposite of the caller's intent**: the implementation is
//	   `lowpass[x].setLPF_BW(lowCut)` + `highpass[x].setHPF_BW(highCut)`, while `HiFi.cpp`
//	   passes `(120.f, 1200.f)` => **the LP is at 1200 Hz and the HP at 120 Hz** => the band-pass is **120…1200 Hz**
//	   (reading it by the parameter names as "HP 1200 / LP 120" is exactly backwards). The signal order is **LP half first, HP half second**.
//	⚠️ `IIR_NOrder_BW_LH(n)` is **n identical first-order sections in cascade** (not a true Butterworth) -- the same convention as Cure's
//	   PassFilter, so `iir1LPF_BW`/`iir1HPF_BW` are reused directly (already in cure.go).
//
// The three branch gains (`1.2·gain` / `gain` / `1`) are **folded into the MIX2 weights** rather than multiplied into the filter numerators:
// that way the whole chain has a single saturation point, consistent with V4A's "sum in floating point, then place once", and nothing is flattened inside the filters.
//
// Cost: **10 sections** (HP3 + the BP's LP3/HP3 + LP1) + **2 DELAYs** (120/240 samples) + 2 MIX2
// => **8 slots**, 12 state sections, 70 coefficient words, about 100 cycles per frame.
// It fits: after the three-axis expansion on 2026-09-21 (NSLOT=24 / NSEC=48 / NCOEF=240),
// the real chain (VSE + convolution + DDC's 18 sections + ViPERBass + this stage's 8 slots/12 sections/70 words) fits --
// the criterion is the real-chain capacity test (realchain_capacity_test.go); before the expansion it reported "not enough slots: 17 needed".
// ⚠️ The two DELAY slots require the hardware to have "an independent write pointer per slot" (CAP3[23:16] ≥ 2, since 2026-09-21) --
// older bitstreams have only one write pointer, so two delay slots would share it and step on each other, which is why this is rejected at compile time (see allocDelay).

package main

import (
	"fmt"
	"math"
)

// xhifiSections returns XHIFI's four coefficient groups (**no scaling** -- the branch gains are multiplied at the mixing stage):
//
//	hp   = HP(1200 Hz) × 3
//	bpL  = LP(1200 Hz) × 3 (the first half of the BP)
//	bpH  = HP(120 Hz)  × 3 (the second half of the BP)
//	lp   = LP(120 Hz)  × 1 (only [0] is meaningful)
func xhifiSections(fs float64) (hp, bpL, bpH, lp [3][5]int32) {
	h0, h1, ha := iir1HPF_BW(1200.0, fs)
	lh0, lh1, lha := iir1HPF_BW(120.0, fs)
	ll0, ll1, lla := iir1LPF_BW(1200.0, fs)
	pl0, pl1, pla := iir1LPF_BW(120.0, fs)
	for i := 0; i < 3; i++ {
		hp[i] = biquadFrom1st(h0, h1, ha)
		bpL[i] = biquadFrom1st(ll0, ll1, lla)
		bpH[i] = biquadFrom1st(lh0, lh1, lha)
	}
	lp[0] = biquadFrom1st(pl0, pl1, pla)
	return
}

// xhifiNode returns **one** node for XHIFI (the compiler expands it into 8 slots, see dsp_slot.go).
//
// `level` is V4A's raw panel value 0..100; `gain = level/100 + 1.0` (`ViPERClarity::SetClarityToFilter`)
// => range 1.0..2.0 (0 dB .. +6.02 dB), and the HP branch is multiplied by 1.2 again.
func xhifiNode(level, fs float64) (planNode, error) {
	if level < 0 || level > 100 {
		return planNode{}, fmt.Errorf("Clarity range is 0..100 (V4A's raw panel value), got %g", level)
	}
	gain := level/100.0 + 1.0
	hp, bpL, bpH, lp := xhifiSections(fs)
	return planNode{
		Kind:  planKindXHIFI,
		Coefs: hp[0],
		Hi:    bpL[0],
		Side:  bpH[0],
		Extra: lp[0],
		// first mixing stage: 1.2·gain·HP + gain·BP(d120)
		Mix: [2]int32{q315Round(1.2 * gain), q315Round(gain)},
		// second mixing stage: 1·(previous stage) + 1·LP(d240)
		MixB: [2]int32{q315Round(1.0), q315Round(1.0)},
		// the delay lengths of the two branches (samples): fs/400 and fs/200 (HiFi.cpp's PushZeros)
		Flags: int(math.Round(fs / 400.0)),
		Len:   int(math.Round(fs / 200.0)),
	}, nil
}
