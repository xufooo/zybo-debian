// dsp_design_test.go — offline verification that the RBJ designs + Q3.15 quantisation hold up at 48 kHz
//
// This layer of tests needs no board: it proves that the "WebUI slider/preset → 30 coefficients" path
//   (a) keeps every coefficient inside the 18-bit Q3.15 range (otherwise the hardware flips the sign),
//   (b) has stable second-order poles (Jury: |a2|<1 and |a1|<1+a2),
//   (c) matches the design target in actual magnitude response (a PK is exactly the gain at the centre frequency, a shelf is half of it).
//
// How to run: go test ./...

package main

import (
	"math"
	"testing"
)

func deq(v int32) float64 { return float64(v) / float64(qOne) }

// stable uses the Jury criterion to check that the poles of the second-order denominator 1 + a1·z⁻¹ + a2·z⁻² all lie inside the unit circle.
func stable(a1, a2 int32) bool {
	x, y := deq(a1), deq(a2)
	return math.Abs(y) < 1 && math.Abs(x) < 1+y
}

// magDB computes the magnitude response (in dB) at a given frequency.
func magDB(b0, b1, b2, a1, a2 int32, f float64) float64 {
	w := 2 * math.Pi * f / sampleRate
	c1, s1 := math.Cos(w), math.Sin(w)
	c2, s2 := math.Cos(2*w), math.Sin(2*w)
	numRe := deq(b0) + deq(b1)*c1 + deq(b2)*c2
	numIm := -(deq(b1)*s1 + deq(b2)*s2)
	denRe := 1 + deq(a1)*c1 + deq(a2)*c2
	denIm := -(deq(a1)*s1 + deq(a2)*s2)
	return 20 * math.Log10(math.Hypot(numRe, numIm)/math.Hypot(denRe, denIm))
}

func design(t *testing.T, sc slotConfig) (int32, int32, int32, int32, int32) {
	t.Helper()
	sc = sanitizeBand(sc)
	b0, b1, b2, a1, a2, err := designBiquad(sc)
	if err != nil {
		t.Fatalf("designBiquad(%+v) failed: %v", sc, err)
	}
	return b0, b1, b2, a1, a2
}

// Every preset and every active section must be designable and stable.
func TestPresetBandsStableAndInRange(t *testing.T) {
	for name, p := range presets {
		for i, sc := range p.Slots {
			if isBandOff(sc.Type) {
				continue
			}
			b0, b1, b2, a1, a2 := design(t, sc)
			if !stable(a1, a2) {
				t.Errorf("preset %s section %d %+v has unstable poles: a1=%d a2=%d", name, i+1, sc, a1, a2)
			}
			// A PK is exactly the gain at the centre frequency; a shelf is half of it (in dB) at the corner
			want := sc.GainDB
			if sc.Type == "LS" || sc.Type == "HS" {
				want = sc.GainDB / 2
			}
			if got := magDB(b0, b1, b2, a1, a2, sc.Freq); math.Abs(got-want) > 0.6 {
				t.Errorf("preset %s section %d %+v: response at %.0fHz is %.2f dB, expected %.2f dB",
					name, i+1, sc, sc.Freq, got, want)
			}
		}
	}
}

// The shelf direction must be right: a high shelf lifts/cuts the highs while leaving the lows alone; a low shelf is the mirror image.
// (rbjHighShelf once had an extra minus sign on a2, which reversed the whole high shelf; this test pins that down.)
func TestShelfDirection(t *testing.T) {
	for _, g := range []float64{6, -6} {
		b0, b1, b2, a1, a2 := design(t, slotConfig{Type: "HS", Freq: 1000, Q: 0.7, GainDB: g})
		if got := magDB(b0, b1, b2, a1, a2, 20000); math.Abs(got-g) > 1.0 {
			t.Errorf("high shelf %+.0fdB @1k: at 20kHz %.2f dB, expected ≈%.0f", g, got, g)
		}
		if got := magDB(b0, b1, b2, a1, a2, 50); math.Abs(got) > 1.0 {
			t.Errorf("high shelf %+.0fdB @1k: at 50Hz %.2f dB, expected ≈0", g, got)
		}

		b0, b1, b2, a1, a2 = design(t, slotConfig{Type: "LS", Freq: 1000, Q: 0.7, GainDB: g})
		if got := magDB(b0, b1, b2, a1, a2, 50); math.Abs(got-g) > 1.0 {
			t.Errorf("low shelf %+.0fdB @1k: at 50Hz %.2f dB, expected ≈%.0f", g, got, g)
		}
		if got := magDB(b0, b1, b2, a1, a2, 20000); math.Abs(got) > 1.0 {
			t.Errorf("low shelf %+.0fdB @1k: at 20kHz %.2f dB, expected ≈0", g, got)
		}
	}
}

// Coefficient readback decoding: the hardware hands back a 32-bit sign-extended value, so an 18-bit extension must not be applied again.
// (It once was, which made negative coefficients read back 2^18 smaller than they are.)
func TestCoeffReadbackDecode(t *testing.T) {
	cases := []struct {
		reg  uint32
		want int32
	}{
		{17906, 17906},        // +17906
		{0xFFFFA29D, -23907},  // 32-bit two's complement of -23907: 2^18 must never be subtracted again
		{1042, 1042},          // +1042
		{0xFFFBFFFF, -262145}, // out-of-range values must also be interpreted honestly
		{0x0001FFFF, 131071},  // Q3.15 positive full scale
		{0x00020000, 131072},  // just past the limit (should never occur by design; decoding itself does not clamp)
	}
	for _, c := range cases {
		if got := decodeCoefReadback(c.reg); got != c.want {
			t.Errorf("decodeCoefReadback(%#x) = %d, want %d", c.reg, got, c.want)
		}
	}
}

// A 0 dB PK must be **exactly** transparent (b1==a1, b2==a2, b0==1.0), so that sections which cannot be switched off stay transparent too.
func TestZeroGainPeakingIsExactUnity(t *testing.T) {
	for _, f := range []float64{60, 150, 400, 1000, 3000, 10000, 20000} {
		b0, b1, b2, a1, a2 := design(t, slotConfig{Type: "PK", Freq: f, Q: 0.7})
		if b0 != qOne || b1 != a1 || b2 != a2 {
			t.Errorf("%.0fHz 0dB PK is not exactly transparent: b0=%d b1=%d a1=%d b2=%d a2=%d",
				f, b0, b1, a1, b2, a2)
		}
		if m := magDB(b0, b1, b2, a1, a2, 1000); math.Abs(m) > 1e-9 {
			t.Errorf("%.0fHz 0dB PK response at 1kHz is %.6f dB, want 0", f, m)
		}
	}
}

// The unity coefficients (how a disabled section is implemented) must be bit-exactly transparent.
func TestUnityCoefficientsAreTransparent(t *testing.T) {
	if qOne != 32768 {
		t.Fatalf("Q3.15 1.0 should be 32768, got %d", qOne)
	}
	if m := magDB(qOne, 0, 0, 0, 0, 1000); math.Abs(m) > 1e-9 {
		t.Errorf("unity coefficients responded %.6f dB, want 0", m)
	}
}

// 60 Hz is the lower limit at Q3.15@48k: it must be stable, and anything lower must be clamped to 60 Hz.
func TestLowFrequencyFloor(t *testing.T) {
	low := sanitizeBand(slotConfig{Type: "PK", Freq: 20, Q: 0.7, GainDB: 12})
	if low.Freq != minBandFreq {
		t.Errorf("20Hz should be clamped to %.0fHz, got %.0fHz", minBandFreq, low.Freq)
	}

	// 60Hz ±12dB is the worst case: inside range + stable poles
	for _, g := range []float64{12, -12} {
		b0, b1, b2, a1, a2 := design(t, slotConfig{Type: "PK", Freq: 60, Q: 10, GainDB: g})
		if !stable(a1, a2) {
			t.Errorf("60Hz Q=10 %+.0fdB has unstable poles: a1=%d a2=%d", g, a1, a2)
		}
		for n, v := range map[string]int32{"b0": b0, "b1": b1, "b2": b2, "a1": a1, "a2": a2} {
			if v > coefMax || v < coefMin {
				t.Errorf("60Hz Q=10 %+.0fdB coefficient %s=%d is out of range", g, n, v)
			}
		}
	}
}

// The 6 slider frequencies of the old 0.1 panel must all be inside the safe range (kept in step with
// webui/index.html and presets in versions/0.1.0-beta). The 0.2 panel no longer has "a fixed 6 sliders" —
// each effect card carries its own frequencies (see EFFECTS in webui/index.html), so what is compared here is legacyBands.
func TestUIBandFrequencies(t *testing.T) {
	uiFreqs := []float64{60, 150, 400, 1000, 3000, 10000}
	if len(uiFreqs) != legacyBands {
		t.Fatalf("0.1 panel frequency count %d != the 0.1 hardware's fixed section count %d", len(uiFreqs), legacyBands)
	}
	for _, f := range uiFreqs {
		if f < minBandFreq || f > maxBandFreq {
			t.Errorf("WebUI frequency %.0fHz is outside the safe range [%.0f, %.0f]", f, minBandFreq, maxBandFreq)
		}
		for _, g := range []float64{12, -12} {
			_, _, _, a1, a2 := design(t, slotConfig{Type: "PK", Freq: f, Q: 0.7, GainDB: g})
			if !stable(a1, a2) {
				t.Errorf("%.0fHz %+.0fdB has unstable poles", f, g)
			}
		}
	}
}

// Coefficient layout: idx = band*5 + k, with off sections writing the unity coefficients and NR_BANDS being the last active section + 1.
func TestCoefficientLayoutAndBandCount(t *testing.T) {
	saved := currentSlots
	defer func() { currentSlots = saved }()

	for i := range currentSlots {
		currentSlots[i] = slotConfig{Type: "off"}
	}
	if n := dspActiveBands(); n != 0 {
		t.Errorf("with everything off NR_BANDS should be 0, got %d", n)
	}

	currentSlots[0] = slotConfig{Type: "PK", Freq: 1000, Q: 0.7, GainDB: 3}
	currentSlots[4] = slotConfig{Type: "PK", Freq: 3000, Q: 0.7, GainDB: -3}
	if n := dspActiveBands(); n != 5 {
		t.Errorf("with sections 0 and 4 active NR_BANDS should be 5, got %d", n)
	}

	exp := dspExpectedCoeffs()
	if len(exp) != coefTotal {
		t.Fatalf("coefficient count %d != %d", len(exp), coefTotal)
	}
	// bands 1..3 are off → the unity coefficients each
	for band := 1; band <= 3; band++ {
		if exp[band*coefPerBand] != qOne {
			t.Errorf("section %d (off) b0 should be %d, got %d", band, qOne, exp[band*coefPerBand])
		}
		for k := 1; k < coefPerBand; k++ {
			if exp[band*coefPerBand+k] != 0 {
				t.Errorf("section %d (off) coefficient %d should be 0", band, k)
			}
		}
	}
	// band 0 is a +3dB PK @1kHz → b0 != 1.0
	if exp[0] == qOne {
		t.Error("section 0's b0 at +3dB must not equal 1.0")
	}
}
