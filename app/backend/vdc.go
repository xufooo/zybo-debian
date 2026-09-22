package main

// vdc.go -- parsing and quantization of the ViPER DDC (`.vdc`)
//
// Why it is needed: the chain the author actually runs in the local JamesDSP is
// **convolver (clear_bass IR) + DDC (Butterworth.vdc) + master limiter**.
// The IR half is already wired up through the FIR slot + /api/dsp/ir; the DDC half is a **table of biquad coefficients**,
// and our engine is a biquad engine -- so **copying the coefficients directly** is more faithful than "fitting them again".
//
// Format (first-hand: the local ~/.config/jamesdsp/vdc/Butterworth.vdc):
//   `SR_48000:` followed by comma-separated floats, in groups of five = (b0,b1,b2,a1,a2);
//   the same file usually has both an SR_44100 and an SR_48000 line; pick by the engine's 48 kHz.
// Difference-equation convention (**confirmed by working back from the response shape; do not change it on a hunch**):
//   the a1/a2 in the file go with **y = b0·x + b1·x1 + b2·x2 + a1·y1 + a2·y2**,
//   i.e. the denominator of H(z) = 1 − a1·z⁻¹ − a2·z⁻² -- only then does it come out as a textbook Butterworth low-pass
//   (flat passband, −3 dB at 10.0 kHz); computing with the denominator 1 + a1 z⁻¹ + a2 z⁻² instead yields
//   a +2.9 dB low-frequency hump and an absurd −117 dB@10 kHz curve (caught once by a Go-side test case).
//   The BIQUAD in `dsp_engine.v` is **y = b0·x + b1·x1 + b2·x2 − c3·y1 − c4·y2**
//   (c3/c4 go through sub) => when loading into the engine, a1/a2 **must be negated** (done in vdcToQ315).
//
// This file only does "bytes -> coefficients + frequency response"; it does not touch the hardware.

import (
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
)

// vdcSection is one biquad section: b0,b1,b2,a1,a2 (floats, not quantized)
type vdcSection [5]float64

// vdcResponseDB computes the frequency response (dB) from cascaded biquads, using floating-point coefficients only -- for verification and reconciliation.
func vdcResponseDB(secs []vdcSection, f, fs float64) float64 {
	w := 2 * math.Pi * f / fs
	// z^-1 = e^{-jw}
	c1, s1 := math.Cos(w), -math.Sin(w)
	c2, s2 := math.Cos(2*w), -math.Sin(2*w)
	acc := 1.0
	for _, s := range secs {
		b0, b1, b2, a1, a2 := s[0], s[1], s[2], s[3], s[4]
		// numerator
		nr := b0 + b1*c1 + b2*c2
		ni := b1*s1 + b2*s2
		// denominator = 1 − a1 z^-1 − a2 z^-2 (see the file header: this is the .vdc convention)
		dr := 1 - a1*c1 - a2*c2
		di := -(a1*s1 + a2*s2)
		num := math.Hypot(nr, ni)
		den := math.Hypot(dr, di)
		if den < 1e-12 {
			den = 1e-12
		}
		acc *= num / den
	}
	return 20 * math.Log10(math.Max(acc, 1e-12))
}

// parseVDC parses .vdc text and picks the sample-rate line closest to wantRate.
func parseVDC(b []byte, wantRate int) (secs []vdcSection, rate int, err error) {
	lines := strings.Split(string(b), "\n")
	bestDiff := 1 << 30
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		if !strings.HasPrefix(ln, "SR_") {
			continue
		}
		colon := strings.Index(ln, ":")
		if colon < 0 {
			continue
		}
		r, perr := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(ln[:colon], "SR_")))
		if perr != nil || r <= 0 {
			continue
		}
		vals := []float64{}
		for _, tok := range strings.Split(ln[colon+1:], ",") {
			tok = strings.TrimSpace(tok)
			if tok == "" {
				continue
			}
			v, verr := strconv.ParseFloat(tok, 64)
			if verr != nil {
				return nil, 0, fmt.Errorf("the SR_%d line contains a number that cannot be parsed: %q", r, tok)
			}
			vals = append(vals, v)
		}
		if len(vals) < 5 {
			continue
		}
		// groups of five; if the last group is all zeros (some files use it as an end marker), drop it
		var got []vdcSection
		for i := 0; i+5 <= len(vals); i += 5 {
			var s vdcSection
			copy(s[:], vals[i:i+5])
			// The sentinel group must be skipped: files commonly end with an "end marker" such as `9.99e-13,0,0,0,0`.
			// ⚠️ The criterion must be "**the numerator is negligible**", not merely "all zeros" -- 1e-13 ≠ 0,
			//    so it would be taken as a section with b≈0 and the whole cascaded response would collapse to 0
			//    (symptoms: fixed-point evaluation stuck at −240 dB, and even the floating-point curve reduced to a fake flat line normalized at 1 kHz;
			//     it took two rounds of per-section printing to catch it).
			if math.Abs(s[0])+math.Abs(s[1])+math.Abs(s[2]) < 1e-9 {
				continue
			}
			got = append(got, s)
		}
		if len(got) == 0 {
			continue
		}
		d := r - wantRate
		if d < 0 {
			d = -d
		}
		if d < bestDiff {
			bestDiff, secs, rate = d, got, r
		}
	}
	if secs == nil {
		return nil, 0, fmt.Errorf("this file has no coefficient line starting with SR_<rate>: (not a ViPER DDC?)")
	}
	return secs, rate, nil
}

// vdcToQ315 quantizes the floating-point biquads into the engine's Q3.15 (18-bit signed).
// Returns (quantized sections, scale). A scale < 1 means everything was scaled down to avoid overflow; > 1 is not supported (no gain up).
func vdcToQ315(secs []vdcSection) ([][5]int32, float64, error) {
	const maxQ = 3.9999694824 // (2^17-1)/2^15
	lim := 0.0
	for _, s := range secs {
		for _, v := range s {
			if a := math.Abs(v); a > lim {
				lim = a
			}
		}
	}
	if lim == 0 {
		return nil, 0, fmt.Errorf("all coefficients are 0")
	}
	scale := 1.0
	if lim > maxQ {
		scale = maxQ / lim
	}
	out := make([][5]int32, 0, len(secs))
	for _, s := range secs {
		// b0,b1,b2 as-is; a1,a2 **negated** (.vdc uses "plus", the engine uses "minus")
		vals := [5]float64{s[0], s[1], s[2], -s[3], -s[4]}
		var q [5]int32
		for i, v := range vals {
			// Q3.15 = **18-bit** signed, sign-magnitude range ±131072 (i.e. ±4.0).
			// ⚠️ I once thought "1.0 cannot be represented and must be clamped to 32767" -- that was mistaking 18 bits for 17,
			//    which introduced a 1 LSB error for b0=1.0 and amplified it by 2.5 dB on a section whose poles sit close to the unit circle
			//    (caught by a test case). The clamp here uses the real boundary.
			qv := int32(math.Round(v * scale * 32768))
			if qv > 131071 {
				qv = 131071
			}
			if qv < -131072 {
				qv = -131072
			}
			q[i] = qv
		}
		out = append(out, q)
	}
	return out, scale, nil
}

// vdcQ315ResponseDB computes the frequency response from the **quantized engine coefficients**: the engine uses
// `y = b·x − c3·y1 − c4·y2` => denominator = 1 + c3 z⁻¹ + c4 z⁻² (c3/c4 are already the negated a's).
func vdcQ315ResponseDB(q [][5]int32, f, fs float64) float64 {
	w := 2 * math.Pi * f / fs
	c1, s1 := math.Cos(w), -math.Sin(w)
	c2, s2 := math.Cos(2*w), -math.Sin(2*w)
	acc := 1.0
	for _, s := range q {
		b0 := float64(s[0]) / 32768.0
		b1 := float64(s[1]) / 32768.0
		b2 := float64(s[2]) / 32768.0
		c3 := float64(s[3]) / 32768.0
		c4 := float64(s[4]) / 32768.0
		nr := b0 + b1*c1 + b2*c2
		ni := b1*s1 + b2*s2
		dr := 1 + c3*c1 + c4*c2
		di := c3*s1 + c4*s2
		num := math.Hypot(nr, ni)
		den := math.Hypot(dr, di)
		if den < 1e-12 {
			den = 1e-12
		}
		acc *= num / den
	}
	return 20 * math.Log10(math.Max(acc, 1e-12))
}

// vdcSectionsToChain converts the quantized sections into the chain's coefficient form (the engine's applyChain takes [][5]int32).
// This is just a type-alias-style move; the function exists so that the "DDC -> chain" path has a name in the code.
func vdcSectionsToChain(q [][5]int32) [][5]int32 { return q }

// ── Small utility: extract a .vdc from a binary other than WAV (some distributions bundle the vdc inside) ──
// Not needed at present, so it is left undone.
var _ = binary.LittleEndian

// ── DDC loading and wiring ──────────────────────────────────────────────
//
// Why keep a separate copy instead of putting it into the user chain: the user chain holds "values the user set" (the 7-point grid path),
// while the DDC is **device correction** (an external file); the two have different origins. When wiring, the DDC sections go **before the convolution**
// (the reference implementation's effect order: EQ → crossfeed/widening → reverb/convolution → limiter),
// and the section count is still bounded by the 16-section limit (exceeding it is reported faithfully by the planner, not silently dropped).

var (
	ddcMu       sync.Mutex
	ddcSections [][5]int32
	// ddcNative: whether this DDC was "loaded as the sections carried by the .vdc itself" (true) or "Butterworth-fitted" (false).
	// It is written into the chain-item parameters so that after a restart the name alone tells which route was taken (otherwise 20 native sections would be re-fitted to 20 sections and the sound would silently change).
	ddcNative bool
	ddcName   string
	ddcRate   int
)

// setDDCFromVDC parses a .vdc and remembers it (returns the section count). It does not touch the hardware.
// setDDCFromVDC loads the sections **carried by the .vdc itself** as-is (no fitting).
//
// Why this is the "matching" approach: the reference implementation `vdc.c` **only reads the table, it does not design filters**
// (the (b0,b1,b2,a1,a2) of the `SR_44100`/`SR_48000` lines, with a1/a2 negated while parsing -- consistent with our
// `vdcToQ315` convention). Our whole DDC path used to go through `fitDDCFromVDC` (Butterworth fitting),
// so "the route was different"; this function has existed for a long time but **was never called** (dead code).
//
// ⚠️ Exceeding the section budget **must be an error**: silently dropping sections would quietly make the response shallower (the user would think "that is just how the file is").
func setDDCFromVDC(body []byte, name string) (int, error) {
	secs, rate, err := parseVDC(body, 48000)
	if err != nil {
		return 0, err
	}
	if len(secs) > maxSections {
		return 0, fmt.Errorf("this .vdc carries %d sections, over the engine limit of %d sections (NSEC) --"+
			"either fit with fewer sections via ?sections=N, or expand the engine", len(secs), maxSections)
	}
	q, _, err := vdcToQ315(secs)
	if err != nil {
		return 0, err
	}
	ddcMu.Lock()
	defer ddcMu.Unlock()
	ddcNative = true
	ddcSections = q
	// ⚠️ Do not strip the .vdc suffix before handing it to irNameSafe -- it would not recognize the extension and would append .irs
	//    (measured: "probe.vdc" became "probe.irs").
	nm := strings.TrimSpace(name)
	if nm == "" {
		nm = "ddc.vdc"
	}
	ddcName = irNameSafe(nm)
	ddcRate = rate
	return len(q), nil
}

// ddcClear turns the DDC off.
func ddcClear() {
	ddcMu.Lock()
	ddcSections, ddcName, ddcRate, ddcNative = nil, "", 0, false
	ddcMu.Unlock()
}

// ddcLoadedName is the name of the DDC currently in memory (empty string when none is loaded).
func ddcLoadedName() string {
	ddcMu.Lock()
	defer ddcMu.Unlock()
	return ddcName
}

// ddcNodes returns "the DDC sections to wire into the chain" (one biquad node per section). Returns nil when there is none.
func ddcNodes() []planNode {
	ddcMu.Lock()
	defer ddcMu.Unlock()
	if len(ddcSections) == 0 {
		return nil
	}
	out := make([]planNode, 0, len(ddcSections))
	for _, s := range ddcSections {
		out = append(out, planNode{Kind: planKindBiquad, Coefs: s})
	}
	return out
}

// ddcView is the state returned to the UI/API.
func ddcView() map[string]any {
	ddcMu.Lock()
	defer ddcMu.Unlock()
	v := map[string]any{
		"on":       len(ddcSections) > 0,
		"name":     ddcName,
		"rate":     ddcRate,
		"sections": len(ddcSections),
		// native = whether this DDC was "loaded as the sections carried by the .vdc itself" (matching the reference implementation),
		// or Butterworth-fitted (the fallback used when saving sections). **It must be visible through the API** --
		// the two can have the same section count but a different shape, and `sections` alone cannot tell them apart.
		"native": ddcNative,
	}
	// Measured response (relative to 1 kHz), so that "what shape this DDC actually has" is directly readable from the API --
	// this is the only on-board source of evidence for A1 (loading the 20 sections as-is).
	if len(ddcSections) > 0 {
		ref := vdcQ315ResponseDB(ddcSections, 1000, 48000)
		resp := map[string]float64{}
		for _, f := range []float64{100, 1000, 4000, 8000, 10000, 12000, 16000} {
			resp[fmt.Sprintf("%.0f", f)] = math.Round((vdcQ315ResponseDB(ddcSections, f, 48000)-ref)*10) / 10
		}
		v["resp_db"] = resp
	}
	return v
}

// ── Fit by response: fold the DDC (19~20 sections) into N second-order Butterworth low-pass sections ──────────
//
// Why fitting is necessary: the owner's Butterworth.vdc has **19 sections**, while the engine's section budget is only
// **16 sections** (and those must be shared with the user EQ) => copying the section table **does not fit** (the planner reports this faithfully).
// Reproducing its **response** (the −3 dB point, the roll-off steepness) is what can actually be shipped, with the deviation reported faithfully.
//
// Coefficient convention: the a1/a2 given by the RBJ formulas **are themselves** the engine BIQUAD's c3/c4
// (the engine computes `− c3·y1 − c4·y2`, and the RBJ difference equation is also `− a1·y1 − a2·y2`) =>
// **do not negate them again** (only the `.vdc` path needs negation, see vdcToQ315).

// butterworthLP designs an N-section cascaded second-order Butterworth low-pass (RBJ Audio EQ Cookbook).
func butterworthLP(n int, fc, fs float64) []vdcSection {
	out := make([]vdcSection, 0, n)
	w0 := 2 * math.Pi * fc / fs
	cs, sn := math.Cos(w0), math.Sin(w0)
	// ⚠️ The section order must be **Q from low to high** (k from large to small). This is not an aesthetic preference, it is fixed-point overflow:
	//    Q = 1/(2·sin((2k+1)π/(4n))), and the k=0 section has the largest Q (its poles sit closest to the unit circle).
	//    With the high-Q section first, the signal is amplified ~Q times before entering the later sections, so the internal cascade state can reach
	//    dozens of times the input, while the engine's state registers are only **24 bits** (RTL: SAMPLE_W=24);
	//    the whole cascade then clips and distorts. Saturation counts measured in fixed-point simulation (0.85 FS multitone, 24-bit state):
	//      high Q first: n=4 => 4200, n=8 => 34191, n=16 => **103600** (still 111353 at −6 dBFS)
	//      low Q first: n=4/8/16 => **all 0**
	//    The symptom on the board is "very severe noise" (reported by the author on 2026-09-18, gone after bypass).
	//    Section order does not affect the transfer function (the product commutes), so this change does not change the response, only the internal dynamic range.
	for k := n - 1; k >= 0; k-- {
		q := 1.0 / (2.0 * math.Sin((2*float64(k)+1)*math.Pi/(4*float64(n))))
		alpha := sn / (2 * q)
		b1 := 1 - cs
		b0 := b1 / 2
		b2 := b0
		a0 := 1 + alpha
		a1 := -2 * cs
		a2 := 1 - alpha
		out = append(out, vdcSection{b0 / a0, b1 / a0, b2 / a0, a1 / a0, a2 / a0})
	}
	return out
}

// sectionsToQ315Engine quantizes coefficients that are "already in the engine convention" directly (no negation).
func sectionsToQ315Engine(secs []vdcSection) [][5]int32 {
	const maxQ = 3.9999694824
	lim := 0.0
	for _, s := range secs {
		for _, v := range s {
			if a := math.Abs(v); a > lim {
				lim = a
			}
		}
	}
	scale := 1.0
	if lim > maxQ {
		scale = maxQ / lim
	}
	out := make([][5]int32, 0, len(secs))
	for _, s := range secs {
		var q [5]int32
		for i, v := range s {
			qv := int32(math.Round(v * scale * 32768))
			if qv > 131071 {
				qv = 131071
			}
			if qv < -131072 {
				qv = -131072
			}
			q[i] = qv
		}
		out = append(out, q)
	}
	return out
}

// vdcMinus3dB finds the −3 dB point (relative to 1 kHz), used to measure the key parameters of the original file.
func vdcMinus3dB(secs []vdcSection, fs float64) float64 {
	ref := vdcResponseDB(secs, 1000, fs)
	lo, hi := 1000.0, fs/2*0.999
	for i := 0; i < 60; i++ {
		mid := math.Sqrt(lo * hi)
		if vdcResponseDB(secs, mid, fs)-ref > -3.0 {
			lo = mid
		} else {
			hi = mid
		}
	}
	return math.Sqrt(lo * hi)
}

// fitDDCFromVDC is the main "load DDC" path: parse the original file -> measure the −3 dB point -> fit to n sections.
// Returns: (fitted sections, −3 dB point in Hz, worst deviation before/after fitting in dB (100 Hz~12 kHz))
func fitDDCFromVDC(body []byte, name string, n int) (int, float64, float64, error) {
	src, _, err := parseVDC(body, 48000)
	if err != nil {
		return 0, 0, 0, err
	}
	if n < 1 {
		n = 4
	}
	fc := vdcMinus3dB(src, 48000)
	fit := butterworthLP(n, fc, 48000)
	q := sectionsToQ315Engine(fit)
	ddcMu.Lock()
	ddcSections = q
	nm := strings.TrimSpace(name)
	if nm == "" {
		nm = "ddc.vdc"
	}
	ddcName = irNameSafe(nm)
	ddcRate = 48000
	ddcMu.Unlock()

	// Deviation: **compare shapes**, not absolute gain.
	// ⚠️ **Correction (2026-09-20, per-file audit + numerical discrimination)**: this used to say "the 19 sections of that .vdc
	//    cannot be cascaded directly -- each section has a DC gain of 2.8~5x, the cascade reaches +240 dB, and b is only a shape so it cannot be loaded into the filter directly".
	//    **That was a misjudgement caused by getting the denominator sign backwards**: with the correct convention (the reference implementation `vdc.c` loads `a1=-val` and
	//    processes `w1 = x1 - a1*v1 - a2*v2`, net form `1 - a_file·z⁻¹ - a_file·z⁻²`,
	//    exactly matching our `vdcToQ315`), the **absolute gain of the sections carried by the file is ≈ 0 dB**
	//    (Butterworth.vdc: 1 kHz = −0.13 dB, peak −0.1 dB); **they can be cascaded directly, with no normalization needed**.
	//    => so "load as-is" (`setDDCFromVDC`) is the matching route; fitting is only the fallback when saving sections.
	//    The shape comparison is still meaningful (it is how fit quality is judged), so the block below is kept.
	srcRef := vdcResponseDB(src, 1000, 48000)
	fitRef := vdcQ315ResponseDB(q, 1000, 48000)
	worst := 0.0
	for _, f := range []float64{100, 200, 500, 1000, 2000, 4000, 6000, 8000, 10000, 12000} {
		d := math.Abs((vdcResponseDB(src, f, 48000) - srcRef) - (vdcQ315ResponseDB(q, f, 48000) - fitRef))
		if d > worst {
			worst = d
		}
	}
	return len(q), fc, worst, nil
}
