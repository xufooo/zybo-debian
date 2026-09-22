// clarity.go — A4: V4A `ViPERClarity` (three tiers NATURAL / OZONE / XHIFI, all three implemented)
//
// XHIFI's branch structure is in xhifi.go (three parallel branches + two branch delays).
//
// First-hand sources (`AndroidAudioMods/ViPERFX_RE`, read line by line):
//   · `effects/ViPERClarity.cpp`
//       `Process()`: NATURAL ⇒ `noiseSharpening.Process()`; OZONE ⇒ two `HighShelf::Process` (one per channel);
//                     XHIFI ⇒ `hifi.Process()` (10 first-order sections + 110/220 sample delay; this unit does not have that much delay memory).
//       `Reset()`: `noiseSharpening.SetSamplingRate` + `SetClarityToFilter()`, and then it
//                  **sets the highShelf frequency to 8250 Hz** (the 12000 in the constructor is a
//                  **dead value**; Reset always overwrites it — copying only the constructor gets the
//                  frequency wrong).
//       `SetClarityToFilter()`: noiseSharpening's gain = `clarityGainPercent`;
//                  highShelf's gain = `clarityGainPercent + 1.0`; likewise for hifi.
//   · `ViPER.cpp` (parameter convention): `PARAM_FIDELITY_CLARITY_GAIN` ⇒ `SetClarity(val1 / 100.0)`.
//       ⇒ this unit's `Level` takes V4A's raw panel value 0..100 and uses `g = Level/100` internally.
//   · `utils/HighShelf.cpp` (the one OZONE uses):
//       `SetGain(g)` ⇒ `gain_dB = 20·log10(g)`;
//       `SetSamplingRate`:
//         x = 2πf/fs,  y = exp(gain_dB·ln10/40) = **√g**
//         z = √(2y)·sin x,  a = (y−1)·cos x,  b = (y+1) − a,  c = z + b
//         d = (y+1)·cos x,  e = (y+1) + a,    f = (y−1) − d
//         a0 = 1/c, a1 = 2f, a2 = b − z, b0 = (e+z)·y, b1 = −2y·((y−1)+d), b2 = (e−z)·y
//       `Process`: `y = (b0·x + b1·x₁ + b2·x₂ − a1·y₁ − a2·y₂) · a0`
//       ⚠️ that `· a0` is an **overall output gain** (not part of the coefficients) ⇒ when folding it
//          into this unit's "minus convention" biquad, **all 5 coefficients must be multiplied by
//          a0** (the feedback terms are inside the parentheses too; you cannot multiply only b).
//
//   · `utils/NoiseSharpening.cpp` (NATURAL):
//       pre-emphasis (per frame): `x_in = x + g·(x − x₁)`;
//       then a first-order low-pass: `IIR_1st::setLPF_BW(fs/2 − 1000)` (48k ⇒ 23000 Hz).
//       ⇒ both stages are LTI and can be **merged exactly into one biquad** (saving a section):
//            H₁(z) = (1+g) − g·z⁻¹                       (pre-emphasis, 2-tap FIR)
//            H₂(z) = b₀(1 + z⁻¹)/(1 − a₁·z⁻¹)            (first-order LPF, b₀ = t/(1+t))
//            H₁·H₂ = b₀·[(1+g) + 1·z⁻¹ − g·z⁻²] / (1 − a₁·z⁻¹)
//          ⇒ B0 = b₀(1+g), B1 = b₀, B2 = −b₀·g, A1 = −a₁ (this unit's minus convention), A2 = 0.
//
// ⚠️ **A self-contradiction in one source file, which must be spelled out** (this unit takes the
// branch that is "the class's design intent"):
//   `NoiseSharpening::Process` writes the state update as `prevSample = x_in·a1 + hist` — using the **input**;
//   whereas the generic implementation in the same header, `IIR_1st.h::do_filter`, is
//   `sample = prev + b0·sample; prev = sample·a1 + hist;` — where `sample` is then **already the output**.
//   The first two lines are word-for-word identical in both places and only the third swaps "output"
//   for "input" ⇒ judged a **typo made while renaming/transcribing** (updating the state from the input
//   degenerates the whole section into a two-point FIR, which contradicts the purpose of building a
//   low-pass with `setLPF_BW`). This unit implements the `do_filter` semantics (a real low-pass).
//   If the typo version is ever to be A/B-ed, only this one function's merge changes.
//
// Cost: NATURAL **1 section**, OZONE **1 section** (both a single BIQUAD, the same table run per channel).
// Index: handle `clr` (a runtime effect at the same level as tube / crossfeed).

package main

import (
	"fmt"
	"math"
)

// clarity's three tiers: one-to-one with the enum values of V4A `ViPERClarity::ClarityMode` (0/1/2).
const (
	clarityModeNatural = 0 // noise sharpening (pre-emphasis + first-order low-pass)
	clarityModeOzone   = 1 // 8250 Hz high shelf
	clarityModeXHIFI   = 2 // three parallel branches + two branch delays (see xhifi.go)
)

type clarityParams struct {
	Mode  int     `json:"mode"`  // clarityModeNatural / Ozone / XHIFI
	Level float64 `json:"level"` // V4A's raw panel value 0..100 (internally g = Level/100)
}

func clarityDefaultParams() clarityParams { return clarityParams{Mode: clarityModeOzone, Level: 50} }

// for logs / the API; also accepts the common spellings.
func clarityModeFromString(s string) (int, bool) {
	switch normalizeChainType(s) {
	case "natural", "noisesharpening", "noise_sharpening":
		return clarityModeNatural, true
	case "ozone":
		return clarityModeOzone, true
	case "xhifi", "x-hifi", "hi-fi", "hifi":
		return clarityModeXHIFI, true
	}
	return 0, false
}

func clarityModeName(m int) string {
	switch m {
	case clarityModeNatural:
		return "natural"
	case clarityModeOzone:
		return "ozone"
	case clarityModeXHIFI:
		return "xhifi"
	}
	return fmt.Sprintf("unknown(%d)", m)
}

// highShelfOzone is a port of `HighShelf::SetSamplingRate`, returning this unit's 5 coefficients.
// freq/gainLinear are supplied by the caller under V4A's convention (freq = 8250 Hz, gainLinear = g + 1).
func highShelfOzone(freq, fs, gainLinear float64) [5]int32 {
	// `HighShelf.cpp:17-19` is exactly `gain_dB = 20·log10(gain)` — this **copies that source line**.
	//
	// It is identically equal to the **gain class** convention of conventions.go (not an "approximation"):
	//   gain = clarityGainPercent + 1 = Level/100 + 1   (ViPERClarity.cpp:65)
	//   ⇒ 20·log10(gain) = 20·log10(1 + Level/100) = gainClassDB(Level)
	//
	// ⚠️ Early on this was written round-about as `gainClassDB((gainLinear-1)*100)`: first restore
	//    gainLinear to the panel value, then apply the gain class formula. Mathematically equivalent,
	//    but it is an extra round trip and one more place that can be got wrong (if the meaning of
	//    gainLinear ever changes, that restore expression silently goes wrong). The criterion is in
	//    a6_parity_test.go's `TestClarityOzoneUsesSourceFormula`.
	gainDB := 20 * math.Log10(gainLinear)
	x := 2 * math.Pi * freq / fs
	sinX, cosX := math.Sin(x), math.Cos(x)
	y := math.Exp(gainDB * math.Ln10 / 40.0) // = √gainLinear
	z := math.Sqrt(2*y) * sinX
	a := (y - 1) * cosX
	b := (y + 1) - a
	c := z + b
	d := (y + 1) * cosX
	e := (y + 1) + a
	f := (y - 1) - d

	a0 := 1 / c
	// raw coefficients (inside the parentheses), then multiplied as a whole by a0 to fold into this unit's convention
	b0 := (e + z) * y * a0
	b1 := -y * 2 * ((y - 1) + d) * a0
	b2 := (e - z) * y * a0
	a1 := 2 * f * a0
	a2 := (b - z) * a0
	return [5]int32{q315Round(b0), q315Round(b1), q315Round(b2), q315Round(a1), q315Round(a2)}
}

// noiseSharpeningBiquad merges NATURAL's two stages (pre-emphasis FIR × first-order low-pass) into one biquad.
// The merge is **exact** (both are LTI), not an approximation — see the derivation at the top of the file.
func noiseSharpeningBiquad(fs, g float64) [5]int32 {
	l0, _, la := iir1LPF_BW(fs/2.0-1000.0, fs) // first-order LPF: b0 = b1 = t/(1+t), a1_slot = −a1
	// H₁·H₂ = b₀·[(1+g) + 1·z⁻¹ − g·z⁻²] / (1 − a₁·z⁻¹)
	b0 := l0 * (1 + g)
	b1 := l0
	b2 := -l0 * g
	return [5]int32{q315Round(b0), q315Round(b1), q315Round(b2), q315Round(la), 0}
}

// clarityNodes returns the slot nodes for the top half of A4 (always 1 section).
func clarityNodes(p clarityParams, fs float64) ([]planNode, error) {
	g := p.Level / 100.0
	if g < 0 {
		g = 0
	}
	switch p.Mode {
	case clarityModeNatural:
		return []planNode{{Kind: planKindBiquad, Coefs: noiseSharpeningBiquad(fs, g)}}, nil
	case clarityModeOzone:
		// V4A: highShelf's gain = clarityGainPercent + 1.0, frequency 8250 Hz (see Reset())
		return []planNode{{Kind: planKindBiquad, Coefs: highShelfOzone(8250.0, fs, g+1.0)}}, nil
	case clarityModeXHIFI:
		// V4A HiFi: three parallel branches + two branch delays (fs/400, fs/200) ⇒
		// it needs a bitstream that can hold two delay slots (CAP3[23:16] ≥ 2), otherwise the compiler rejects it.
		n, err := xhifiNode(p.Level, fs)
		if err != nil {
			return nil, err
		}
		return []planNode{n}, nil
	}
	return nil, fmt.Errorf("unknown Clarity tier %d", p.Mode)
}

// ── runtime effect state (same level as currentTube / currentCrossfeed) ──────────────

var currentClarity *clarityParams

func clarityAvailable() bool {
	return dspEngineGen == 1 && dspReadEngineCaps().BiquadAvailable()
}

func setClarity(p *clarityParams) error {
	if p == nil {
		currentClarity = nil
		markStateDirty()
		return nil
	}
	if p.Level < 0 {
		p.Level = 0
	}
	// V4A panel range 0..100 (ViPER.cpp: SetClarity(val1/100)). Above that it is rejected honestly, not silently clamped.
	if p.Level > 100 {
		return fmt.Errorf("Clarity range is 0..100 (V4A's raw panel value), got %g", p.Level)
	}
	if p.Mode != clarityModeNatural && p.Mode != clarityModeOzone && p.Mode != clarityModeXHIFI {
		return fmt.Errorf("unknown Clarity tier %d", p.Mode)
	}
	cur := *p
	currentClarity = &cur
	markStateDirty()
	return nil
}

func clarityView() map[string]any {
	if currentClarity == nil {
		return nil
	}
	return map[string]any{
		"mode":     clarityModeName(currentClarity.Mode),
		"mode_id":  currentClarity.Mode,
		"level":    currentClarity.Level,
		"gain_db":  clarityGainDB(*currentClarity),
		"sections": 1,
		"note":     "V4A ViPERClarity; all three tiers implemented (natural / ozone / xhifi)",
	}
}

// clarityGainDB is the conventional value of "how many dB this tier lifts above 8250 Hz", for display/troubleshooting only:
// OZONE is 20·log10(g+1); NATURAL is not a shelf, so this gives pre-emphasis's maximum 20·log10(1+2g) (when z=Nyquist).
func clarityGainDB(p clarityParams) float64 {
	g := p.Level / 100.0
	if p.Mode == clarityModeOzone {
		return 20 * math.Log10(g+1)
	}
	return 20 * math.Log10(1+2*g)
}
