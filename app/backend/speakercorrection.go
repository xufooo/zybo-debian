// speakercorrection.go — A4 second half: V4A `SpeakerCorrection`
//
// Primary sources (read line by line):
//   · `effects/SpeakerCorrection.cpp`:
//       `Reset()`:
//         lowPass  = `SetLowPassParameter(13500.0, fs, 1.0)`
//         highPass = `RefreshFilter(HIGH_PASS, 0.0, 80.0, fs, 1.0, false)`
//         bandPass = `SetBandPassParameter(420.0, fs, 3.88)`
//       `Process()` (per channel):
//         `z = HP(LP(x))`  → `z /= 2` → `y = z + BP(z)`
//       ⚠️ BP is fed the **already halved** z: `y = z/2 + BP(z/2)`. Since BP is linear,
//          `BP(z/2) = BP(z)/2` ⇒ `y = 0.5·(z + BP(z))`.
//   · `utils/MultiBiquad.cpp::RefreshFilter` (the `param_7 = false` branch):
//         `y_alpha = sin(ω)/(2Q)`, LP/HP/BP are all textbook RBJ;
//         what is stored is `this.a1 = −(a1/a0)`, `this.a2 = −(a2/a0)`, `b*/a0`,
//         while `ProcessSample` is `y = b0x + b1x₁ + b2x₂ + a1y₁ + a2y₂`
//         ⇒ **character-for-character identical** to our engine's "minus convention" (`− a1·y₁`), so no further negation is needed.
//
// ── Why only **3 sections**, and why no MIX2 slot is needed ───────────
//   V4A's structure adds two branches (z and BP(z)); copying that literally would need a MIX2 slot plus
//   extra bus routing. But `0.5·(1 + H_BP)` is still a rational function with **the same second-order denominator**:
//
//       1 + H_BP = 1 + N(z)/D(z) = (D(z) + N(z)) / D(z)
//
//   so the addition can be folded into the **numerator** of the third section (`B' = 0.5·(B + D)`, denominator unchanged),
//   and the whole chain becomes **LP → HP → (1+BP)/2**, three ordinary biquads in series. Mathematically **exactly equivalent**
//   (both are LTI; the only difference is fixed-point rounding), saving one slot and one bus mix.
//   The equivalence is pinned by `TestSpeakerCorrectionMergeMatchesDirectForm`, which compares sample by sample against the direct form.
//
// Cost: **3 BIQUAD sections** (the same table runs per channel).
// Index: handle `spk` (a runtime effect on the same level as tube / clarity).

package main

import "math"

// bq holds biquad coefficients under our convention (float, so the algebra happens before rounding):
//
//	H(z) = (b0 + b1·z⁻¹ + b2·z⁻²) / (1 + a1·z⁻¹ + a2·z⁻²)
//
// Note the denominator carries `+a1`: the engine computes `y = b0x + b1x₁ + b2x₂ − a1·y₁ − a2·y₂`.
type bq struct{ b0, b1, b2, a1, a2 float64 }

func (c bq) i32() [5]int32 {
	return [5]int32{q315Round(c.b0), q315Round(c.b1), q315Round(c.b2),
		q315Round(c.a1), q315Round(c.a2)}
}

// rbj ports `MultiBiquad::RefreshFilter` (param_7 = false):
// `alpha = sin(ω)/(2Q)`, for the LP/HP/BP types (SpeakerCorrection uses only these three).
// kind: `"LP"` / `"HP"` / `"BP"`.
func rbj(kind string, freq, q, fs float64) bq {
	omega := 2 * math.Pi * freq / fs
	sinO, cosO := math.Sin(omega), math.Cos(omega)
	alpha := sinO / (2 * q) // MultiBiquad: y = sinOmega / (qFactor + qFactor)

	a0 := 1 + alpha
	a1 := -2 * cosO
	a2 := 1 - alpha
	var b0, b1, b2 float64
	switch kind {
	case "LP":
		b0 = (1 - cosO) / 2
		b1 = 1 - cosO
		b2 = b0
	case "HP":
		b0 = (1 + cosO) / 2
		b1 = -(1 + cosO)
		b2 = b0
	case "BP": // the constant 0 dB peak-gain variant
		b0 = alpha
		b1 = 0
		b2 = -alpha
	}
	return bq{b0 / a0, b1 / a0, b2 / a0, a1 / a0, a2 / a0}
}

// speakerCorrectionBiquads returns the 3 sections of V4A SpeakerCorrection (float).
// The order matches the data flow of `Process()`: LP(13500) → HP(80) → (1 + BP(420))/2.
func speakerCorrectionBiquads(fs float64) [3]bq {
	lp := rbj("LP", 13500.0, 1.0, fs)
	hp := rbj("HP", 80.0, 1.0, fs)
	bp := rbj("BP", 420.0, 3.88, fs)
	// Third section = 0.5·(1 + BP): denominator unchanged, numerator = 0.5·(BP numerator + denominator)
	//   D's coefficients are (1, a1, a2) (our convention), B's are (b0, b1, b2)
	sum := bq{
		b0: 0.5 * (bp.b0 + 1),
		b1: 0.5 * (bp.b1 + bp.a1),
		b2: 0.5 * (bp.b2 + bp.a2),
		a1: bp.a1,
		a2: bp.a2,
	}
	return [3]bq{lp, hp, sum}
}

func speakerCorrectionNodes(fs float64) []planNode {
	bqs := speakerCorrectionBiquads(fs)
	out := make([]planNode, 0, len(bqs))
	for _, c := range bqs {
		out = append(out, planNode{Kind: planKindBiquad, Coefs: c.i32()})
	}
	return out
}

// ── Runtime effect state (same level as currentTube / currentClarity) ────

// speakerCorrectionParams is just an on/off switch: this V4A stage has no adjustable parameters (all three frequencies/Q are hardcoded).
type speakerCorrectionParams struct{}

var currentSpeakerCorrection *speakerCorrectionParams

func speakerCorrectionAvailable() bool {
	return dspEngineGen == 1 && dspReadEngineCaps().BiquadAvailable()
}

func setSpeakerCorrection(p *speakerCorrectionParams) error {
	if p == nil {
		currentSpeakerCorrection = nil
		markStateDirty()
		return nil
	}
	if !speakerCorrectionAvailable() {
		return errDSPUnavailable
	}
	currentSpeakerCorrection = &speakerCorrectionParams{}
	markStateDirty()
	return nil
}

func speakerCorrectionView() map[string]any {
	if currentSpeakerCorrection == nil {
		return nil
	}
	return map[string]any{
		"on":       true,
		"sections": 3,
		// The parameters are hardcoded in V4A; echoing them saves digging through the source when debugging
		"lowpass_hz":  13500.0,
		"lowpass_q":   1.0,
		"highpass_hz": 80.0,
		"highpass_q":  1.0,
		"bandpass_hz": 420.0,
		"bandpass_q":  3.88,
		"note":        "V4A SpeakerCorrection: z = HP80(LP13500(x)), y = 0.5·(z + BP420(z))",
	}
}
