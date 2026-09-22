// SPDX-License-Identifier: GPL-2.0-only
// colorful.go -- planning and download for V4A `ColorfulMusic` (= DepthSurround + Stereo3DSurround)
//
// Why it needs a **joint-stereo frame pass** (rather than being squeezed into the per-channel chain like the other effects):
// `DepthSurround`'s `prev[0]/prev[1]` and its two delay lines are shared by the whole instance, and `prev[1]` feeds
// both channel outputs at once => L[n] and R[n] must be available **in the same tick**. Trying to piece it together on the engine's
// cross-channel bus ("the other channel one tick earlier") does not work: the phase difference of `diff=(l−r)/2` at 10 kHz is exactly 1 sample = 75°, so the side signal would come out clearly wrong.
// On the hardware side see the "joint-stereo frame pass" section in the file header of the engine RTL (dsp_engine.v),
// the reference model is the Python script colm_model.py, and the bit-exact joint-stereo testbench is tb_engine_js.v.
//
// First-hand evidence (all checked line by line in the upstream sources):
//
//	· `AndroidAudioMods/ViPERFX_RE` branch rewrite-continued:
//	    src/viper/utils/DepthSurround.cpp   -- joint recursion + ±gain + HP800
//	    src/viper/utils/Stereo3DSurround.cpp -- 2×2 matrix (outL=cL·(L+R)−cR·(R−L), outR=a+b)
//	    src/viper/utils/TimeConstDelay.cpp   -- true delays of fs×0.02 / fs×0.014 (48k => 960 / 672)
//	    src/viper/effects/ColorfulMusic.cpp  -- the order is DepthSurround and then Stereo3DSurround
//	· the official ViPER4Android client source (**first-hand parameter values**):
//	    ViPER4AndroidService.java:1763-1771 sends three integers into the core:
//	      PARAM_HPFX_COLM_WIDENING / _DEPTH (`colorfulmusic_coeffs`, values like "120;200")
//	      PARAM_HPFX_COLM_MIDIMAGE (`colorfulmusic_midimage`, default "150")
//	    value ranges in arrays.xml: widening **120..200** (9 steps), depth **200..800** (9 steps),
//	      midimage **100..200** (11 steps); defaults coeffs="120;200", midimage="150".
//	· the community ViPER4Android rewrite (viperfx-re): src/viper/ViPER.cpp:255-263 (how the core uses these three numbers):
//	      SetWidenValue((float) val1 / 100.0f);  SetMidImageValue((float) val1 / 100.0f);
//	      SetDepthValue(val1)                     <- depth is the strength directly (0..1000)
//	  => widen = widening/100 (default 1.2), midImage = midimage/100 (default 1.5),
//	    depth = strength (default 200).
//
// ⚠️ Effective only in the **headphone FX** preset: the official client sends these three parameters in the `headphonefx` namespace
//
//	(`viper4android.headphonefx.colorfulmusic.*`), and the SPEAKER preset was measured to have no effect
//	(the `viperfx_oracle` reference checkout's README and the Hindsight decision record). This device is a headphone output, so it is implemented per the headphone preset.
package main

import (
	"fmt"
	"math"
)

// colorfulParams holds ColorfulMusic's three parameters (**in core terms**, not panel terms):
//
//	Depth    = PARAM_HPFX_COLM_DEPTH → DepthSurround::SetDepthValue(short) (0..1000, 0 = off)
//	Widening = PARAM_HPFX_COLM_WIDENING → SetWidenValue(Widening/100), client range 120..200
//	MidImage = PARAM_HPFX_COLM_MIDIMAGE → SetMidImageValue(MidImage/100), client range 100..200
type colorfulParams struct {
	Depth    int     `json:"depth"`
	Widening float64 `json:"widening"`
	MidImage float64 `json:"mid_image"`
}

// The value ranges from the client's arrays.xml (values with margin below the **upper bound** of the self-calibrated range are accepted too, see validate).
const (
	colorfulWidenMin = 0.0 // the core allows 0 (= no widening, pure matrix)
	colorfulWidenMax = 4.0 // widening ≤ 400: beyond this, y=1/(w+2) in ConfigureVariables is already very small,
	//                       and the matrix coefficients must fit Q3.15 (see colorfulMatrix)
	colorfulMidMin   = 0.0
	colorfulMidMax   = 4.0
	colorfulDepthMax = 1000
	// colorfulDepthMin: 0 = off (the reference implementation uses `enabled = strength != 0`), 1 = shallowest
	colorfulDepthMin = 1
)

// colorfulDefaultParams holds the **official client defaults**: coeffs "120;200" (widening 120 / depth 200)
// and midimage "150" => core values widen=1.2, midImage=1.5, depth=200.
func colorfulDefaultParams() colorfulParams {
	return colorfulParams{Depth: 200, Widening: 1.2, MidImage: 1.5}
}

// ── Runtime state ─────────────────────────────────────────────────────

var currentColorful *colorfulParams

// colorfulAvailable: ColorfulMusic's **hardware prerequisite** is the joint-stereo frame pass (CAP1 bit12).
// ⚠️ The "engine generation" is not consulted: whether this is built into a given bitstream generation is a build decision (omitting the logic is a legitimate simplification),
// and using the version number as a proxy would diverge from the facts.
func colorfulAvailable() bool {
	return dspEngineGen == 1 && dspReadEngineCaps().JointStereoAvailable()
}

// ── Coefficients ──────────────────────────────────────────────────────

// colorfulDepthGain returns DepthSurround's two gains (floats, before Q3.15):
//
//	gain = min(1, 10^(((strength/1000)·10 − 15)/20))     (RefreshStrength)
//	prev[0] uses **+gain**, prev[1] uses **±gain** (negative when strength ≥ 500; the two lines in Process)
//
// => two **independent** coefficients: g and g1 = ±g. Merging them into one would make the whole "negative branch" wrong (this bit the RTL side,
// and criteria ①② of tb_engine_js caught it by comparing the two branches separately).
func colorfulDepthGain(depth int) (float64, float64) {
	if depth <= 0 {
		return 0, 0
	}
	g := math.Pow(10, ((float64(depth)/1000.0)*10.0-15.0)/20.0)
	if g > 1.0 {
		g = 1.0
	}
	if depth >= 500 {
		return g, -g
	}
	return g, g
}

// colorfulHP800 is the high-pass inside DepthSurround (Biquad::SetHighPassParameter(800, fs, −11 dB, Q0.72));
// it returns (b0,b1,b2,a1,a2) in **V4A terms** (a1/a2 already negated -- the hardware's S_JDST adds all five products).
func colorfulHP800(fs float64) (b0, b1, b2, a1, a2 float64) {
	const freq, dbGain, q = 800.0, -11.0, 0.72
	omega := 2.0 * math.Pi * freq / fs
	sinOmega, cosOmega := math.Sin(omega), math.Cos(omega)
	A := math.Pow(10.0, dbGain/40.0)
	sqrtA := math.Sqrt(A)
	z := sinOmega / 2.0 * math.Sqrt((1.0/A+A)*(1.0/q-1.0)+2.0)
	a0 := (A + 1.0) - (A-1.0)*cosOmega + (sqrtA*2.0)*z
	ra1 := ((A - 1.0) - (A+1.0)*cosOmega) * 2.0
	ra2 := (A + 1.0) - (A-1.0)*cosOmega - (sqrtA*2.0)*z
	rb0 := ((A + 1.0) + (A-1.0)*cosOmega + (sqrtA*2.0)*z) * A * omega
	rb1 := A * -2.0 * ((A - 1.0) + (A+1.0)*cosOmega) * omega
	rb2 := ((A + 1.0) + (A-1.0)*cosOmega - (sqrtA*2.0)*z) * A * omega
	// V4A's SetCoeffs: b*/a0, and a* negated before dividing by a0
	return rb0 / a0, rb1 / a0, rb2 / a0, -(ra1 / a0), -(ra2 / a0)
}

// colorfulMatrix holds the 2×2 matrix coefficients of Stereo3DSurround (ConfigureVariables):
//
//	tmp = widen+1; x = tmp+1; y = (x < 2) ? 0.5 : 1/x
//	cL = midImage·y; cR = tmp·y
//	Process: a = cL·(L+R), b = cR·(R−L), outL = a−b, outR = a+b
//	expanding => outL = (cL+cR)·L + (cL−cR)·R, outR = (cL−cR)·L + (cL+cR)·R
//	so the hardware only needs two words: ca = cL+cR, cb = cL−cR.
func colorfulMatrix(widen, midImage float64) (float64, float64) {
	tmp := widen + 1.0
	x := tmp + 1.0
	y := 0.5
	if x >= 2.0 {
		y = 1.0 / x
	}
	cL := midImage * y
	cR := tmp * y
	return cL + cR, cL - cR
}

// colorfulJointCoefs folds the parameters into the two coefficient groups the hardware needs (Q3.15):
//
//	JDST 9 words (**the order is tied to the RTL's fetch phases**, see the S_JDST phase table in dsp_engine.v):
//	  c0 = g (gain of prev0, always positive)
//	  c1 = g1 (gain of prev1 = ±g)
//	  c2 = D0 length (fs×0.02), c3 = D1 length (fs×0.014)
//	  c4..c8 = HP800's b0,b1,b2,a1,a2 (V4A terms)
//	J3DS 2 words: c0 = ca = cL+cR, c1 = cb = cL−cR
func colorfulJointCoefs(p colorfulParams, fs float64) ([9]int32, [2]int32, error) {
	var jsd [9]int32
	var js3 [2]int32
	q := func(name string, v float64) (int32, error) {
		n := floatToQ315(v)
		if n > coefMax || n < coefMin {
			return 0, fmt.Errorf("ColorfulMusic %s = %.6f is out of Q3.15 range (%d)", name, v, n)
		}
		return n, nil
	}
	g, g1 := colorfulDepthGain(p.Depth)
	b0, b1, b2, a1, a2 := colorfulHP800(fs)
	ca, cb := colorfulMatrix(p.Widening, p.MidImage)
	l0 := int(fs * 0.02)  // TimeConstDelay[0]: SetParameters(fs, 0.02)
	l1 := int(fs * 0.014) // TimeConstDelay[1]: SetParameters(fs, 0.014)
	if l0 < 1 || l1 < 1 {
		return jsd, js3, fmt.Errorf("at sample rate %.0f the two delay lengths come out as %d/%d (<1)", fs, l0, l1)
	}
	var err error
	for _, v := range []struct {
		name string
		val  float64
		dst  *int32
	}{
		{"g", g, &jsd[0]}, {"g1", g1, &jsd[1]},
		{"HP800 b0", b0, &jsd[4]}, {"HP800 b1", b1, &jsd[5]}, {"HP800 b2", b2, &jsd[6]},
		{"HP800 a1", a1, &jsd[7]}, {"HP800 a2", a2, &jsd[8]},
		{"3D matrix ca", ca, &js3[0]}, {"3D matrix cb", cb, &js3[1]},
	} {
		if *v.dst, err = q(v.name, v.val); err != nil {
			return jsd, js3, err
		}
	}
	jsd[2], jsd[3] = int32(l0), int32(l1)
	// The delay lengths must also fit the coefficient domain (18-bit signed) and not exceed the ring depth -- the ring depth is self-reported through CAP2, so only a hard lower bound is enforced here.
	if l0 > coefMax || l1 > coefMax {
		return jsd, js3, fmt.Errorf("delay lengths %d/%d do not fit the 18-bit coefficients", l0, l1)
	}
	return jsd, js3, nil
}

// colorfulJointDelayWords is the **in-ring offset budget** the joint section's two delay lines need:
// both lines use the same slot number but live in the left/right physical rings respectively, with the same offset => the longer one decides.
func colorfulJointDelayWords(fs float64) int {
	l0, l1 := int(fs*0.02), int(fs*0.014)
	if l0 < l1 {
		l0 = l1
	}
	if l0 < 1 {
		l0 = 1
	}
	return l0
}

// ── Nodes and validation ──────────────────────────────────────────────

// colorfulNodes builds ColorfulMusic's slot nodes (one node = two joint slots, see buildSlotPlanNodes).
func colorfulNodes(p colorfulParams) ([]planNode, error) {
	jsd, js3, err := colorfulJointCoefs(p, sampleRate)
	if err != nil {
		return nil, err
	}
	return []planNode{{Kind: planKindColorfulMusic, Joint: jsd, Mix3D: js3}}, nil
}

// setColorfulMusic sets the parameters (passing nil turns it off). **When the hardware has no joint-stereo frame pass it errors out explicitly**,
// never degrading silently to "only the 3D matrix" or "doing nothing" (which would make the user think it is on when it is not).
func setColorfulMusic(p *colorfulParams) error {
	if p == nil {
		currentColorful = nil
		markStateDirty()
		return nil
	}
	if !colorfulAvailable() {
		return fmt.Errorf("this hardware has no joint-stereo frame pass (CAP1 bit12 is not set): " +
			"ColorfulMusic's DepthSurround is a joint-stereo processor (it needs L and R in the same tick), " +
			"which a per-channel engine cannot do, so it cannot be configured")
	}
	q, err := colorfulValidate(*p)
	if err != nil {
		return err
	}
	currentColorful = &q
	markStateDirty()
	return nil
}

// colorfulValidate validates and clamps the parameters. **Clamping follows the client's value ranges**; going out of range is not an error (a panel slider can
// land outside them), but Depth=0 counts as off (handled by the caller).
func colorfulValidate(p colorfulParams) (colorfulParams, error) {
	if p.Depth < 0 {
		p.Depth = 0
	}
	if p.Depth > colorfulDepthMax {
		return p, fmt.Errorf("ColorfulMusic depth=%d is outside the core range 0..%d"+
			" (the official client panel is 200..800)", p.Depth, colorfulDepthMax)
	}
	if p.Widening < colorfulWidenMin {
		p.Widening = colorfulWidenMin
	}
	if p.Widening > colorfulWidenMax {
		return p, fmt.Errorf("ColorfulMusic widening=%.2f is outside the accepted range %.1f..%.1f"+
			" (the official client panel is 1.2..2.0)", p.Widening, colorfulWidenMin, colorfulWidenMax)
	}
	if p.MidImage < colorfulMidMin {
		p.MidImage = colorfulMidMin
	}
	if p.MidImage > colorfulMidMax {
		return p, fmt.Errorf("ColorfulMusic mid_image=%.2f is outside the accepted range %.1f..%.1f"+
			" (the official client panel is 1.0..2.0)", p.MidImage, colorfulMidMin, colorfulMidMax)
	}
	if p.Depth != 0 && p.Depth < colorfulDepthMin {
		p.Depth = colorfulDepthMin
	}
	return p, nil
}

// colorfulView is for the API/panel (nil = off).
func colorfulView() any {
	if currentColorful == nil {
		return nil
	}
	p := *currentColorful
	g, g1 := colorfulDepthGain(p.Depth)
	ca, cb := colorfulMatrix(p.Widening, p.MidImage)
	l0 := int(sampleRate * 0.02)
	l1 := int(sampleRate * 0.014)
	return map[string]any{
		"depth":     p.Depth,
		"widening":  p.Widening,
		"mid_image": p.MidImage,
		"available": colorfulAvailable(),
		// the derived core values (for panel display and self-check reconciliation)
		"gain": g, "gain_prev1": g1,
		"matrix_a": ca, "matrix_b": cb,
		"delay0_samples": l0, "delay1_samples": l1,
		// the original client values (×100) -- the panel sends exactly these three integers
		"client_widening": int(math.Round(p.Widening * 100)),
		"client_midimage": int(math.Round(p.MidImage * 100)),
		"note": "V4A ColorfulMusic = DepthSurround (joint stereo: shared prev0/prev1 + " +
			"two 960/672-sample delays + HP800) → Stereo3DSurround (2×2 matrix). " +
			"Parameter convention: the client sends widening/depth (coeffs " +
			"\"120;200\") and midimage (\"150\"), and the core uses them divided by 100. Effective only in the headphone FX preset.",
	}
}
