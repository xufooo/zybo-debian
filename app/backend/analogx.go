// analogx.go -- A2: the three V4A `AnalogX` tiers (the "analog flavor" harmonic effect)
//
// Primary source (`AndroidAudioMods/ViPERFX_RE`, `effects/AnalogX.cpp` read line by line):
//
//	harmonic table (ANALOGX_HARMONICS, shared by all three tiers): {0.01, 0.02, 1e-4, 1e-3, 0, 0, 0, 0, 0, 0}
//	per-channel signal flow (`Process`):
//	    o = HP(240 Hz, Q0.717)(in)          <- only the high band enters the harmonic stage
//	    o = Harmonic(o)                      <- POLY stage (our opcode 6)
//	    o = LP(<tier frequency>, Q0.717)(in + o*gain)   <- **dry + wet x tier gain**, then low-passed
//	    o = Peak(633 Hz, Q6.28, +0.58 dB, param_7=true)(o * 0.8)
//
//	the three tiers (`Reset()`'s switch):
//	    model 0: gain **0.6**, LP **19650 Hz**
//	    model 1: gain **1.2**, LP **18233 Hz**
//	    model 2: gain **2.4**, LP **16307 Hz**
//	HP/Peak are the same for all three tiers (240/633).
//
// WARNING: **the Peak with `param_7 = true` uses a different alpha formula**
//	(MultiBiquad::RefreshFilter):
//	    `y = sinh((Q*ln2*w/2) / sinw) * sinw` (not the usual `sinw/(2Q)`),
//	    and **PEAK's gain uses `/40`**: `gain = 10^(gainAmp/40)`.
//	    Copying the wrong branch makes the 633 Hz peak a completely different shape
//	    (the "analog flavor" would sound wrong).
//
// WARNING: **one place is deliberately not copied**: while `freqRange < fs/4` (the first
//	    **0.25 seconds** after it is enabled) the source **zeroes the whole output**
//	    (`memset(samples, 0, ...)`) -- that is a "warm-up silence" which makes the user
//	    think there is no sound. This implementation does not do that muting (the engine
//	    has no "how long has the chain been running" counter either), and the difference
//	    is documented.
//
// Cost: **4 sections** (HP + POLY + LP + PEAK) + 1 MIX2 slot (dry/wet mixing; it does not
// use up a state section's useful purpose, but the engine's S_FIN writes state for every
// slot => planning accounts for 5 state slots, see the expansion in dsp_slot.go).

package main

import (
	"fmt"
	"math"
)

// analogxTiers holds the source's three tiers (the order is model 0/1/2).
var analogxTiers = [3]struct {
	Gain float64 // wet gain (in + o*gain)
	LPHz float64 // post-stage low-pass frequency
}{
	{0.6, 19650.0},
	{1.2, 18233.0},
	{2.4, 16307.0},
}

// ANALOGX_HARMONICS: shared by all three tiers (source: static const float[]).
var analogxHarmonics = [10]float64{0.01, 0.02, 0.0001, 0.001, 0, 0, 0, 0, 0, 0}

type analogxParams struct {
	Model int `json:"model"` // 0/1/2 (the source's processing model)
}

func analogxDefaultParams() analogxParams { return analogxParams{Model: 0} }

// analogxPeak is a port of `RefreshFilter(PEAK, 0.58, 633, fs, 6.28, param_7=true)`, and
// folds the **0.8** of the source's `ProcessSample(out * 0.8)` step into the numerator
// (linear => equivalent).
func analogxPeak(fs float64) [5]int32 {
	const (
		gainAmp  = 0.58
		freq     = 633.0
		q        = 6.28
		postGain = 0.8 // Peak(out*0.8): folded into b
	)
	// PEAK's gain is 10^(gainAmp/40) (not /20)
	gain := math.Pow(10, gainAmp/40.0)
	omega := 2 * math.Pi * freq / fs
	sinO, cosO := math.Sin(omega), math.Cos(omega)
	// the param_7 branch
	y := math.Sinh((q*math.Ln2*omega/2)/sinO) * sinO

	a0 := 1 + y/gain
	a2 := 1 - y/gain
	b0 := 1 + y*gain
	b1 := -2 * cosO
	b2 := 1 - y*gain
	// This implementation's convention (y = b0x + b1x1 + b2x2 - a1s*y1 - a2s*y2):
	//   denominator 1 + a1s z^-1 + a2s z^-2, where a1s = a1r/a0, a2s = a2r/a0 (a1r = -2cosw)
	return [5]int32{
		q315Round(b0 / a0 * postGain),
		q315Round(b1 / a0 * postGain),
		q315Round(b2 / a0 * postGain),
		q315Round(-2 * cosO / a0),
		q315Round(a2 / a0),
	}
}

// analogxNodes returns the **one** node for AnalogX (the compiler expands it into the five
// slots HP->POLY->MIX2->LP->PEAK).
func analogxNodes(p analogxParams, fs float64) ([]planNode, error) {
	if p.Model < 0 || p.Model >= len(analogxTiers) {
		return nil, fmt.Errorf("AnalogX has only the three tiers 0/1/2 (got %d)", p.Model)
	}
	tier := analogxTiers[p.Model]
	// the harmonic coefficients use the same expansion as the exciter (V4A's Harmonic
	// class is our POLY opcode)
	poly, err := harmonicQ315(exciterParams{Harmonics: analogxHarmonics, Mix: 1.0,
		HPFHz: 240.0, LPFHz: tier.LPHz})
	if err != nil {
		return nil, err
	}
	hp, err := multiBiquad("HP", 0, 240.0, 0.717, fs, false)
	if err != nil {
		return nil, err
	}
	lp, err := multiBiquad("LP", 0, tier.LPHz, 0.717, fs, false)
	if err != nil {
		return nil, err
	}
	return []planNode{{
		Kind:  planKindAnalogX,
		Coefs: hp,   // (1) pre-stage high-pass
		Hi:    lp,   // (4) post-stage low-pass
		Poly:  poly, // (2) POLY (12 words)
		// (3) dry/wet mix: out = in*1.0 + poly*gain (Q3.15; gain <= 2.4, well inside +/-4.0)
		Mix:  [2]int32{32767, q315Round(tier.Gain)},
		Side: analogxPeak(fs), // (5) 633 Hz peak (+0.58 dB, the param_7 branch)
	}}, nil
}

// multiBiquad is a general port of `MultiBiquad::RefreshFilter` (LP/HP/BP/PEAK, both alpha
// branches).
// kind: `"LP"`/`"HP"`/`"BP"`/`"PEAK"`; with param7=true the sinh branch is used (AnalogX's
// Peak uses it).
func multiBiquad(kind string, gainAmp, freq, q, fs float64, param7 bool) ([5]int32, error) {
	omega := 2 * math.Pi * freq / fs
	sinO, cosO := math.Sin(omega), math.Cos(omega)

	// gain convention: PEAK / shelves use /40, everything else uses /20
	gain := math.Pow(10, gainAmp/20.0)
	if kind == "PEAK" || kind == "LOW_SHELF" || kind == "HIGH_SHELF" {
		gain = math.Pow(10, gainAmp/40.0)
	}
	var y float64
	switch {
	case kind == "LOW_SHELF" || kind == "HIGH_SHELF":
		y = sinO / 2 * math.Sqrt((1/gain+gain)*(1/q-1)+2)
	case param7:
		y = math.Sinh((q*math.Ln2*omega/2)/sinO) * sinO
	default:
		y = sinO / (q + q)
	}

	var a0, a1, a2, b0, b1, b2 float64
	switch kind {
	case "LP":
		a0, a1, a2 = 1+y, -2*cosO, 1-y
		b0, b1, b2 = (1-cosO)/2, 1-cosO, (1-cosO)/2
	case "HP":
		a0, a1, a2 = 1+y, -2*cosO, 1-y
		b0, b1, b2 = (1+cosO)/2, -(1 + cosO), (1+cosO)/2
	case "BP":
		a0, a1, a2 = 1+y, -2*cosO, 1-y
		b0, b1, b2 = y, 0, -y
	case "PEAK":
		a0, a1, a2 = 1+y/gain, -2*cosO, 1-y/gain
		b0, b1, b2 = 1+y*gain, -2*cosO, 1-y*gain
	default:
		return [5]int32{}, fmt.Errorf("multiBiquad: unknown type %q", kind)
	}
	// this implementation's "minus convention": a1s = a1r/a0, a2s = a2r/a0 (a1r = -2cosw,
	// a2r as above)
	return [5]int32{
		q315Round(b0 / a0), q315Round(b1 / a0), q315Round(b2 / a0),
		q315Round(a1 / a0), q315Round(a2 / a0),
	}, nil
}

// ── Runtime effect state ──────────────────────────────────────────────

var currentAnalogX *analogxParams

func analogxAvailable() bool {
	return dspEngineGen == 1 && dspReadEngineCaps().PolyAvailable()
}

func setAnalogX(p *analogxParams) error {
	if p == nil {
		currentAnalogX = nil
		markStateDirty()
		return nil
	}
	if !analogxAvailable() {
		return fmt.Errorf("AnalogX needs the POLY slot (CAP1 bit6 is not set); this hardware cannot do it")
	}
	if p.Model < 0 || p.Model >= len(analogxTiers) {
		return fmt.Errorf("AnalogX has only the three tiers 0/1/2 (got %d)", p.Model)
	}
	q := *p
	currentAnalogX = &q
	markStateDirty()
	return nil
}

func analogxView() map[string]any {
	if currentAnalogX == nil {
		return nil
	}
	t := analogxTiers[currentAnalogX.Model]
	return map[string]any{
		"on":         true,
		"model":      currentAnalogX.Model,
		"gain":       t.Gain,
		"lowpass_hz": t.LPHz,
		"sections":   4,
		"note":       "V4A AnalogX: HP240 -> POLY -> LP(in + poly*gain) -> Peak633(+0.58dB)",
	}
}

// analogxTiersView is the **three-tier table** handed back to the UI/API (including the
// English and Chinese tier names), derived from analogxTiers.
//
// Why it exists: `caps["analogx"]["tiers"]` used to **copy those three value sets again**
// (0.6/19650 and so on), so "the numbers the API reports" and "the numbers the
// implementation uses" became two places -- change one and forget the other and the two
// silently diverge. Now analogxTiers is the single source of truth, and the English and
// Chinese names match the ViPER4Android app's arrays.xml word for word (see below).
func analogxTiersView() []map[string]any {
	out := make([]map[string]any, 0, len(analogxTiers))
	for i, t := range analogxTiers {
		out = append(out, map[string]any{
			"model":      i,
			"gain":       t.Gain,
			"lowpass_hz": t.LPHz,
			// where the tier names come from: the ViPER4Android app's English
			// arrays.xml:266-270 and Chinese values-zh-rCN/arrays.xml:103-107
			"name_en": fxAnalogXPresetIDSuffix[i],
			"name_cn": fxAnalogXCn[i],
			"preset":  "analogx-" + fxAnalogXPresetIDSuffix[i], // the matching ID among the four A2' presets
		})
	}
	return out
}
