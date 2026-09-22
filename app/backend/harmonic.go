// harmonic.go — harmonic exciter (the hardware basis of V4A's `SpectrumExtend` / `AnalogX` path)
//
// Primary sources: `AndroidAudioMods/ViPERFX_RE` @rewrite-continued
//   · `src/viper/utils/Harmonic.cpp`   — polynomial + leaky integration (this unit makes it the OP_POLY slot)
//   · `src/viper/effects/SpectrumExtend.cpp`, `AnalogX.cpp` — the two users of it
//
// Why this stage exists: `OP_POLY` is the **only nonlinear** opcode in the engine. BIQUAD/FIR/
// MIX2/DELAY/DYN are all linear (or slowly varying gain) operations and **cannot produce
// frequencies that are not already in the input**; yet V4A's "harmonic excitation" (VSE
// extending the highs, AnalogX tube warmth / even harmonics, ViPER Bass's harmonic mode) is
// all essentially "creating new frequencies". Without this stage those three could only be
// imitated with EQ, which is half-baked.
//
// Hardware-side algorithm (line by line against Harmonic.cpp):
//
//	p[n] = c0 + x·(c1 + x·(c2 + … + x·c10))        // Horner, order 10
//	y[n] = (p[n] + 0.999·y[n−1]) − p[n−1]          // leaky integration (also blocks DC)
//	the first |a|max·10000 samples output 0        // pop suppression (biggestCoeff)
//
// The software side **does exactly one thing**: expand "which harmonics I want and how large
// each one is" (V4A's user-facing convention) into the monomial coefficients c0..c10 and
// quantise them. The expansion copies `Harmonic::UpdateCoeffs`: what it really performs is a
// **Chebyshev basis → monomial basis** conversion (T_k(cosθ) = cos kθ), so
// p(cosθ) = Σ a_k·cos(kθ) — ask for the k-th harmonic a_k and that is exactly what comes out.
// `TestHarmonicCoeffsProduceRequestedHarmonics` pins this property down with a DFT.
//
// ⚠️ Fixed-point safety rests precisely on this Chebyshev structure: |T_k(x)| ≤ 1 (|x| ≤ 1) ⇒ |p| ≤ Σ|a_k| ≤ 1.
//    The normalisation (shrink everything when the sum of |a| exceeds 1) is `UpdateCoeffs`'s job;
//    the hardware does not saturate again. Note that "Σ|c_i| ≤ 1" must **not** be used to judge
//    safety: VSE's coefficient set has Σ|c| = 23.8, yet |p| ≤ 0.1 everywhere.

package main

import (
	"fmt"
	"math"
)

// exciterParams holds the user-facing parameters of the harmonic exciter.
type exciterParams struct {
	// Harmonics is the relative amplitude of the 1st..10th harmonics (V4A's `SetHarmonics` is exactly 10 numbers).
	Harmonics [10]float64 `json:"harmonics"`
	// Mix is "how much dry signal is added back" (V4A `SpectrumExtend`'s exciter gain).
	// **The complete convention** (2026-09-20 per-file audit + comparison): app-side UI 0.1…1.0
	// → `round(UI×5.6×100)` = 56…560 → `/100` inside the kernel ⇒ **0.56…5.6**; the UI **defaults
	// to 0.1** ⇒ 0.56 (this unit's default matches it).
	// ⚠️ **This unit cannot express the top of the range**: Mix lands on MIX2's Q3.15 coefficient,
	//    whose 18-bit signed limit = 131071/32768 = **3.9999**. V4A's 5.6 would be **silently
	//    saturated to 4.0** (2.9 dB short, with no hint in the UI) — so it is validated explicitly
	//    below and rejected. Rejecting is preferable to silent distortion. Covering the full range
	//    would require a hardware change (move the wet gain elsewhere or widen the coefficient field).
	Mix float64 `json:"mix"`
	// HPFHz / LPFHz are the band-pass around the harmonic generator (V4A: HPF 7600 Hz Q0.717,
	// LPF fs/2−2000 Q0.717). Harmonics are only created inside this band, so that low-frequency
	// intermodulation does not smear them.
	HPFHz float64 `json:"hpf_hz"`
	LPFHz float64 `json:"lpf_hz"`
}

// maxMixQ315 is the largest linear gain MIX2's wet coefficient (Q3.15, 18-bit signed) can express:
// 131071 / 32768 = 3.99997…. This is a **hardware** limit, not a taste we chose.
const maxMixQ315 = 131071.0 / 32768.0

// exciterWetScale returns "the factor by which the monomial coefficients are scaled as a whole" (1.0 = no scaling).
//
// When scaling is needed: Q3.15's 18-bit signed range only reaches ±4.0, while VSE's coefficient
// set goes up to 10.24 ⇒ everything must be scaled down, and that factor must then be **folded
// back into the wet gain** (`Mix × scale`). This is both a range question and what decides how
// large Mix can be: after folding, Mix itself can go far beyond 4.0 (VSE's top tier of 5.6 does),
// because what lands in MIX2 is `Mix×scale`.
func exciterWetScale(p exciterParams) float64 {
	c := harmonicMonomialCoeffs(p.Harmonics)
	if mx := polyMaxCoef(c); mx > 3.9 {
		return 3.9 / mx
	}
	return 1.0
}

// validateExciterMix checks the **wet coefficient actually downloaded to MIX2** (`Mix × scale`) against the Q3.15 range.
//
// ⚠️ What is measured here is the product, not Mix itself (corrected 2026-09-21): the old version
// compared Mix directly against 4.0, so
//
//	V4A VSE's **top 3 tiers** (UI 0.8/0.9/1.0 ⇒ 4.48/5.04/5.6) were rejected; in reality the
//	polynomial has to be scaled by 10.24/3.9 before it fits Q3.15, and what lands in MIX2 is
//	1.71/1.92/2.13 — **it fits**. The criterion is "would the downloaded coefficient be silently
//	saturated by q315Round", so the product is what must be examined.
func validateExciterMix(p exciterParams) error {
	if p.Mix < 0 {
		return fmt.Errorf("the harmonic exciter's mix cannot be negative (got %.3f)", p.Mix)
	}
	wet := p.Mix / exciterWetScale(p)
	limit := maxMixQ315 * maxMixQ315 // one gain stage (in_a=in_b=wet bus) can multiply the range by another 4.0
	if wet > limit {
		return fmt.Errorf("the harmonic exciter's equivalent wet gain mix/scale=%.3f exceeds this unit's limit %.2f: "+
			"the wet signal lands on MIX2's Q3.15 coefficient (18-bit signed ⇒ one coefficient tops out at "+
			"131071/32768≈4.0, and one gain stage multiplies that by 4.0 ⇒ %.2f in total); beyond that it is silently saturated (no hint in the UI)",
			wet, limit, limit)
	}
	return nil
}

// exciterMuteSamples returns V4A's "pop suppression" length: |a|max × 10000 (Harmonic.cpp's
// `biggestCoeff`). The hardware outputs 0 for that many samples. The upper bound is 65535 (the
// coefficient word is only 18-bit signed, but reaching 10000 already means the extreme case of
// |a|=1; it is clamped once more here so that hand-edited parameters cannot go out of range).
func exciterMuteSamples(p exciterParams) int {
	big := 0.0
	for _, a := range p.Harmonics {
		if v := math.Abs(a); v > big {
			big = v
		}
	}
	n := int(big * 10000.0)
	if n < 0 {
		n = 0
	}
	if n > 65535 {
		n = 65535
	}
	return n
}

// exciterDefaultParams is V4A `SpectrumExtend`'s factory shape (the numbers in the source):
// odd harmonics only, amplitude 0.02; exciter gain 0.56.
func exciterDefaultParams() exciterParams {
	var p exciterParams
	for i := 0; i < 10; i += 2 { // the 1st/3rd/5th/7th/9th
		p.Harmonics[i] = 0.02
	}
	p.Mix = 0.56
	p.HPFHz = 7600
	p.LPFHz = float64(sampleRate)/2 - 2000
	return p
}

// ── A port of Harmonic::UpdateCoeffs ────────────────────────────────────────
//
// The input is the 10 harmonic amplitudes a1..a10 (**not** monomial coefficients) and the output
// is 11 monomial coefficients c0..c10. It corresponds to the C++ line by line (keeping the
// meaning of its two temporary array names) and only swaps float for float64.
//
// A hint for reading this code: the inner `c[i-j] = c[i-j-1]*2 − tmp` is the Chebyshev recursion
// T_n = 2x·T_{n−1} − T_{n−2} being expanded into monomials "in place"; the outer `u2` array
// holds the previous round's old values (saved as it rolls). Do not try to "simplify" it — its
// correctness is guaranteed by the DFT test, and rewriting it very easily gets an index wrong.
func harmonicMonomialCoeffs(amps [10]float64) [11]float64 {
	var u1 [11]float64 // u1[1..10] = the normalised harmonic amplitudes
	absSum := 0.0
	for _, a := range amps {
		absSum += math.Abs(a)
	}
	scale := 1.0
	if absSum > 1.0 {
		scale = 1.0 / absSum
	}
	for i := 0; i < 10; i++ {
		u1[i+1] = amps[i] * scale
	}

	var c [11]float64
	var u2 [11]float64
	c[10] = u1[10]
	for i := 2; i < 11; i++ {
		for j := 0; j < i; j++ {
			tmp := u2[i-j]
			u2[i-j] = c[i-j]
			c[i-j] = c[i-j-1]*2.0 - tmp
		}
		tmp := u1[10-i+1] - u2[0]
		u2[0] = c[0]
		c[0] = tmp
	}
	for i := 1; i < 11; i++ {
		c[10-i+1] = c[10-i] - u2[10-i+1]
	}
	c[0] = u1[0]/2.0 - u2[0]
	return c
}

// harmonicQ315 folds the harmonic amplitudes into the 12 words the hardware wants: c0..c10 (Q3.15) + the mute length.
//
// Quantisation uses "round + saturate", the same convention as the project's other coefficients
// (rbj*/vdc); the error is always ≤ half an LSB (3.05e-5), around −90 dB, inaudible.
func harmonicQ315(p exciterParams) ([12]int32, error) {
	if len(p.Harmonics) != 10 {
		return [12]int32{}, fmt.Errorf("there must be exactly 10 harmonic amplitudes (got %d)", len(p.Harmonics))
	}
	for i, a := range p.Harmonics {
		if math.IsNaN(a) || math.IsInf(a, 0) {
			return [12]int32{}, fmt.Errorf("harmonic %d is not a finite number", i+1)
		}
		if math.Abs(a) > 1.0 {
			return [12]int32{}, fmt.Errorf("harmonic %d amplitude %.4f exceeds 1.0 (decide how much you want before normalising)", i+1, a)
		}
	}
	c := harmonicMonomialCoeffs(p.Harmonics)
	var out [12]int32
	for i, v := range c {
		out[i] = q315Round(v)
	}
	out[11] = int32(exciterMuteSamples(p))
	return out, nil
}

// q315Round rounds a real number to Q3.15 and saturates it into the range an 18-bit signed value can express.
// Note the range: Q3.15's 18-bit signed value only reaches ±4.0 (±131071/32768).
// VSE's monomial coefficients go up to 10.24 — **they do not fit**! So this is not "clamp it and
// be done": the whole thing must be scaled down, otherwise the waveform is flattened (which is the real distortion).
//
// Why scaling everything down is allowed: in Harmonic.cpp's Process(), scaling the coefficients is
// equivalent to scaling the output, and the output is immediately followed by the `×exciter` mix;
// so the software can fold "the scale factor" into Mix (see `polyScale` in exciterNodes). Miss
// this and the VSE tier turns straight into hard clipping.
func q315Round(v float64) int32 {
	const half = 0.5 / 32768.0
	q := math.Round((v + math.Copysign(half, v)) * 32768.0)
	if q > 131071 {
		q = 131071
	}
	if q < -131072 {
		q = -131072
	}
	return int32(q)
}

// polyMaxCoef returns the largest absolute value among the polynomial coefficients (used to decide whether to scale everything).
func polyMaxCoef(c [11]float64) float64 {
	m := 0.0
	for _, v := range c {
		if a := math.Abs(v); a > m {
			m = a
		}
	}
	return m
}

// ── The harmonic exciter on the chain ────────────────────────────────────────────────────

var currentExciter *exciterParams

// exciterAvailable: does this hardware have the POLY opcode (self-reported by CAP1 bit6, independent of the engine generation)?
func exciterAvailable() bool {
	return dspEngineGen == 1 && dspReadEngineCaps().PolyAvailable()
}

// setExciter sets (or clears, by passing nil) the harmonic exciter. When the hardware does not support it this **reports an explicit error** instead of silently dropping it.
func setExciter(p *exciterParams) error {
	if p == nil {
		currentExciter = nil
		markStateDirty()
		return nil
	}
	if !exciterAvailable() {
		return fmt.Errorf("this hardware has no POLY stage (CAP1 bit6 not set); the harmonic exciter cannot be downloaded")
	}
	q := *p
	if _, err := harmonicQ315(q); err != nil {
		return err
	}
	if err := validateExciterMix(q); err != nil {
		return err
	}
	currentExciter = &q
	markStateDirty()
	return nil
}

// exciterView is what goes back to the UI: the current parameters (nil = off).
func exciterView() any {
	if currentExciter == nil {
		return nil
	}
	return currentExciter
}

// exciterNodes compiles the harmonic exciter into 4 slots (following the shape of V4A `SpectrumExtend`):
//
//	HPF(7600, Q0.717) → POLY(harmonic generator) → LPF(fs/2−2000, Q0.717) → MIX2(dry + wet)
//
// Only **one** POLY slot is used: it is time-division multiplexed per channel and each channel has
// its own state (the state sections are allocated by the compiler), so L/R each create their own harmonics without bleeding into each other.
func exciterNodes(p exciterParams) ([]planNode, error) {
	coefs, err := harmonicQ315(p)
	if err != nil {
		return nil, err
	}
	// Range check on the monomial coefficients: Q3.15's 18 bits only reach ±4.0. Beyond that the
	// coefficients are **scaled down as a whole by s** (VSE's factory shape has max|c| = 10.24 ⇒
	// s = 3.9/10.24 = 0.3809), and the wet gain must be **multiplied by 1/s**: the hardware
	// computes `wet weight × poly(c·s·x)`, so only by scaling the weight up by 1/s does the product
	// return to `Mix × poly(c·x)`.
	// ⚠️ This used to be written `mix = p.Mix * scale` (multiplying by s in the same direction) ⇒ the
	//    wet signal was really **s²** times, i.e. VSE was **16.8 dB weaker** than the official core
	//    (caught on 2026-09-21 by measuring against the oracle: at UI 0.1 the 3rd harmonic was
	//    −97 dBFS while the core gives −78 dBFS). After the fix the two agree to within 0.05 dB.
	c := harmonicMonomialCoeffs(p.Harmonics)
	scale := exciterWetScale(p)
	if scale != 1.0 {
		for i := 0; i < 11; i++ {
			coefs[i] = q315Round(c[i] * scale)
		}
	}
	mix := p.Mix / scale
	// The wet weight itself only reaches ±4.0: beyond that one more **gain stage** is added (MIX2 with
	// in_a=in_b=wet bus, writing the output back to the same bus — the engine "latches a slot's inputs
	// before writing the bus", so amplifying in place is safe). VSE's top tier needs
	// 5.6/0.3809 = 14.7, and one ×4.0 gain stage is enough (limit 16.0).
	var gainStage int32
	wet := mix
	if wet > maxMixQ315 {
		gainStage = q315Round(wet / maxMixQ315)
		wet = maxMixQ315
	}
	h0, h1, h2, h3, h4 := rbjHighPass(p.HPFHz, 0.717, sampleRate)
	hpf := [5]int32{h0, h1, h2, h3, h4}
	l0, l1, l2, l3, l4 := rbjLowPass(p.LPFHz, 0.717, sampleRate)
	lpf := [5]int32{l0, l1, l2, l3, l4}
	return []planNode{
		{Kind: planKindExciter, Coefs: hpf, Hi: lpf, Poly: coefs,
			Mix: [2]int32{32767, q315Round(wet)},
			// MixB[0] is reused as "the gain stage's multiplier" (0 = not needed). exciter did not use MixB before.
			MixB: [2]int32{gainStage, 0}},
	}, nil
}
