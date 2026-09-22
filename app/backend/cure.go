// cure.go — V4A `Cure`'s trailing `PassFilter` stage (A5)
//
// Primary sources: the community ViPER4Android rewrite (AndroidAudioMods/ViPERFX_RE, rewrite-continued)
//
//	· `effects/Cure.cpp`: `Process()` = `crossfeed.ProcessFrames()` **then** `passFilter.ProcessFrames()`
//	  ⇒ the order is **crossfeed → PassFilter**, not the other way round.
//	· `utils/PassFilter.cpp`: `{IIR_NOrder_BW_LH(3), IIR_NOrder_BW_LH(3), IIR_NOrder_BW_LH(1), IIR_NOrder_BW_LH(1)}`;
//	  in `Reset()` `cutoff = (fs < 44100) ? fs − 100 : 18000`, `filters[0/1].setLPF(cutoff)`,
//	  `filters[2/3].setHPF(10 Hz)`; `ProcessFrames` per channel: `HPF(filters[2]) → LPF(filters[0])`.
//	· `utils/IIR_NOrder_BW_LH.cpp`: `setLPF/setHPF` apply **the same coefficient set to `order` first-order sections**
//	  ⇒ this is **not a standard N-th order Butterworth** but "N identical first-order sections in cascade". **Copying this detail is what is correct**.
//	· `utils/IIR_1st.cpp`'s `setHPF_BW/setLPF_BW` (PassFilter uses exactly the _BW variant):
//	      t = tan(π·f/fs)
//	      HPF: b0 = 1/(1+t),  b1 = −b0,  a1 = (1−t)/(1+t)
//	      LPF: b0 = t/(1+t),  b1 = b0,   a1 = (1−t)/(1+t)
//	· `utils/IIR_1st.h`'s `do_filter`: `y = prev + b0·x; prev = a1·y + b1·x`
//	  ⇒ H(z) = (b0 + b1·z⁻¹)/(1 − a1·z⁻¹) ⇒ under our engine's "minus" convention this is `a1_slot = −a1`.
//
// ⚠️ **Off by default**: JamesDSP's crossfeed is **pure bs2b** (`bs2b.c`) and has no such stage.
//
//	Our chain order follows JamesDSP, so by default bs2b is kept as is; for V4A's Cure ask for it explicitly with `pass_filter: true`.
//
// Cost: per channel HPF 1 section + LPF 3 sections = **4 BIQUAD sections** (the engine runs the same table per channel, so 4 sections and not 8).
package main

import "math"

// iir1LPF_BW / iir1HPF_BW port `IIR_1st::setLPF_BW` / `setHPF_BW` and return
// (b0, b1, a1_slot) in **our convention** (already negated, ready to feed a DF-I biquad).
func iir1LPF_BW(freq, fs float64) (float64, float64, float64) {
	t := math.Tan(math.Pi * freq / fs)
	b0 := t / (1 + t)
	a1 := (1 - t) / (1 + t)
	return b0, b0, -a1 // a1_slot = −a1 (see the do_filter derivation in the file header)
}

func iir1HPF_BW(freq, fs float64) (float64, float64, float64) {
	t := math.Tan(math.Pi * freq / fs)
	b0 := 1 / (1 + t)
	a1 := (1 - t) / (1 + t)
	return b0, -b0, -a1
}

// biquadFrom1st packs a first-order section into our 5-coefficient biquad (b2 = a2 = 0).
func biquadFrom1st(b0, b1, a1slot float64) [5]int32 {
	return [5]int32{
		q315Round(b0), q315Round(b1), 0,
		q315Round(a1slot), 0,
	}
}

// curePassFilterSections returns the 4 sections of V4A Cure's trailing PassFilter:
// **HPF(10 Hz, 1st order) → LPF(18000 Hz or fs−100, 3 identical first-order sections)**.
//
// The order matches `PassFilter::ProcessFrames` (HPF first, LPF second).
func curePassFilterSections(fs float64) [4][5]int32 {
	cutoff := 18000.0
	if fs < 44100 {
		cutoff = fs - 100
	}
	h0, h1, ha := iir1HPF_BW(10.0, fs)
	l0, l1, la := iir1LPF_BW(cutoff, fs)
	return [4][5]int32{
		biquadFrom1st(h0, h1, ha),
		biquadFrom1st(l0, l1, la),
		biquadFrom1st(l0, l1, la),
		biquadFrom1st(l0, l1, la),
	}
}
