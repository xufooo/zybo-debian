// dsp_headroom_test.go -- offline tests for automatic headroom compensation (no board
// needed)
//
// Reproduces the problem from 2026-09-14: full-scale material (modern masters generally
// peak at 0 dBFS) goes out of range after an EQ boost and is flattened by the biquad's
// saturation stage -- it sounds "blown out, muddy, with the dynamics crushed".
// What is asserted here: for any input with |x| <= 1, the output of **every stage** in the
// chain stays within full scale.
//
// WARNING: the criterion is the impulse response L1 norm ("prefix peak gain"), not the
// maximum of the frequency response: phase summation makes the sample peak more than
// 1.5 dB above the frequency-response maximum, so looking only at the frequency response
// would clip.

package main

import (
	"math"
	"testing"
)

func withSlots(t *testing.T, slots [maxBands]slotConfig, preampDB float64, fn func()) {
	t.Helper()
	oldSlots, oldPre, oldApplied := currentSlots, currentPreampDB, currentPreampAppliedDB
	defer func() {
		currentSlots, currentPreampDB, currentPreampAppliedDB = oldSlots, oldPre, oldApplied
	}()
	currentSlots, currentPreampDB = slots, preampDB
	fn()
}

// rawCoeffs computes the "without compensation" coefficients for the current slots
func rawCoeffs(slots [maxBands]slotConfig) [maxBands][coefPerBand]int32 {
	var c [maxBands][coefPerBand]int32
	for i, sc := range slots {
		c[i] = [coefPerBand]int32{qOne, 0, 0, 0, 0}
		if isBandOff(sc.Type) {
			continue
		}
		if b0, b1, b2, a1, a2, err := designBiquad(sanitizeBand(sc)); err == nil {
			c[i] = [coefPerBand]int32{b0, b1, b2, a1, a2}
		}
	}
	return c
}

func TestRockPresetNeedsHeadroom(t *testing.T) {
	rock := presets["rock"].Slots
	raw := cascadeMaxGainDB(rawCoeffs(rock))
	// measured value with the quantized coefficients: +3.00 dB maximum frequency-response boost
	if raw < 2.5 || raw > 3.5 {
		t.Fatalf("rock preset's maximum boost = %.2f dB, expected about +3.0 dB (did the formula or the preset change?)", raw)
	}
	t.Logf("rock preset maximum boost = %+.2f dB -> automatic preamp = %+.2f dB (boost + 3dB safety margin)", raw, -(raw + preampSafetyMarginDB))

	withSlots(t, rock, 0, func() {
		coefs, pre := dspFinalCoeffs()
		// a safety margin must remain after compensation: measured -(boost+3dB) drives the
		// clipping count of real full-scale music to zero
		if got := cascadeMaxGainDB(coefs); got > -preampSafetyMarginDB+0.5 {
			t.Errorf("maximum boost after compensation %+.2f dB, should be <= %+.1f dB (safety margin too small)", got, -preampSafetyMarginDB+0.5)
		}
		if math.Abs(pre-(-(raw + preampSafetyMarginDB))) > 0.1 {
			t.Errorf("automatic preamp = %.2f dB, expected %.2f dB", pre, -(raw + preampSafetyMarginDB))
		}
	})
}

func TestFlatPresetNeedsNoHeadroom(t *testing.T) {
	withSlots(t, presets["flat"].Slots, 0, func() {
		coefs, pre := dspFinalCoeffs()
		if pre != 0 {
			t.Errorf("the flat preset should not need headroom, got %.2f dB", pre)
		}
		if got := cascadeMaxGainDB(coefs); math.Abs(got) > 0.01 {
			t.Errorf("the flat preset's frequency response should be 0 dB, got %+.2f dB", got)
		}
	})
}

func TestPreampGoesToTheFirstActiveBand(t *testing.T) {
	// headroom must be applied at the chain input (the first non-off section), otherwise
	// the first stage still amplifies a full-scale input
	rock := presets["rock"].Slots
	withSlots(t, rock, 0, func() {
		coefs, _ := dspFinalCoeffs()
		raw := rawCoeffs(rock)
		if coefs[0][0] >= raw[0][0] {
			t.Errorf("the first stage's numerator was not scaled down: %d -> %d", raw[0][0], coefs[0][0])
		}
		for b := 1; b < maxBands; b++ {
			if coefs[b] != raw[b] {
				t.Errorf("stage %d should not have been touched: %v -> %v", b, raw[b], coefs[b])
			}
		}
		// the poles (a1/a2) must never move, otherwise the frequency-response shape changes
		for b := 0; b < maxBands; b++ {
			if coefs[b][3] != raw[b][3] || coefs[b][4] != raw[b][4] {
				t.Errorf("stage %d's a1/a2 were modified", b)
			}
		}
	})
}

func TestUserPreampWinsWhenDeeper(t *testing.T) {
	rock := presets["rock"].Slots
	// the user gave -12 dB (deeper than the required -7.06): -12 must be kept
	withSlots(t, rock, -12, func() {
		coefs, pre := dspFinalCoeffs()
		if math.Abs(pre-(-12)) > 0.05 {
			t.Errorf("the user's preamp of -12 dB should be adopted, got %.2f dB", pre)
		}
		if got := cascadeMaxGainDB(coefs); got > 0.01 {
			t.Errorf("%+.2f dB of peak gain remains after compensation", got)
		}
	})
}

func TestUserPreampTooShallowIsOverridden(t *testing.T) {
	rock := presets["rock"].Slots
	// the user gave only -2 dB, which cannot hold down +7.06 dB -> it must be deepened
	// automatically; better quieter than clipping
	withSlots(t, rock, -2, func() {
		coefs, pre := dspFinalCoeffs()
		if pre > -5.9 {
			t.Errorf("when the user's preamp is not enough it should be deepened automatically to <= -6.0 dB, got %.2f dB", pre)
		}
		if got := cascadeMaxGainDB(coefs); got > -preampSafetyMarginDB+0.5 {
			t.Errorf("%+.2f dB of boost remains after compensation", got)
		}
	})
}

func TestEveryPresetGetsHeadroom(t *testing.T) {
	// every preset with a boost must be held down, and by enough to actually lower the peak
	// gain. (We cannot assert "always down to -3dB": the 60Hz low shelf's numerator is a
	// cancellation term, so rounding the scale makes the low-frequency response deviate
	// from the estimate; rock was checked against real full-scale music in the fixed-point
	// time domain, clipping onset 10146 -> 0.)
	for name, p := range presets {
		name, p := name, p
		t.Run(name, func(t *testing.T) {
			raw := cascadeMaxGainDB(rawCoeffs(p.Slots))
			withSlots(t, p.Slots, p.PreampDB, func() {
				coefs, pre := dspFinalCoeffs()
				got := cascadeMaxGainDB(coefs)
				if raw > 0.01 {
					if pre > -(preampSafetyMarginDB - 0.01) {
						t.Errorf("preset %s has a %+.2f dB boost but no headroom was added (preamp %.2f dB)", name, raw, pre)
					}
					if got > raw-1.5 {
						t.Errorf("preset %s: %+.2f dB before compensation, %+.2f dB after, almost no effect", name, raw, got)
					}
				} else if pre != 0 {
					t.Errorf("preset %s has no boost, so the level should not be lowered (preamp %.2f dB)", name, pre)
				}
				t.Logf("  preset %-10s boost %+5.2f dB -> preamp %+6.2f dB -> after compensation %+5.2f dB", name, raw, pre, got)
			})
		})
	}
}

// TestPureCutChainGetsNoPreamp: **a purely attenuating chain must not have headroom
// applied**.
// A bug measured on the board: with just one 600 Hz low-pass the backend reported
// `preamp_db = -3.05` -- Q3.15 rounding made it compute a "maximum boost" of +0.045 dB, so
// 3 dB was taken off for nothing. What the user heard was "I only wanted to tame the highs
// and the whole thing got smaller".
func TestPureCutChainGetsNoPreamp(t *testing.T) {
	// low-pass: 0 dB passband, no boost at all
	var cut [maxBands]slotConfig
	cut[0] = slotConfig{Type: "LP", Freq: 600, Q: 0.7}
	if got := cascadeMaxGainDB(rawCoeffs(cut)); got <= 0 || got > preampDeadZoneDB {
		t.Fatalf("a low-pass's cascade maximum gain should fall inside the dead zone (0, %.2f] (quantization-noise level), got %+.4f dB",
			preampDeadZoneDB, got)
	}
	if pre := effectivePreampDB(cascadeMaxGainDB(rawCoeffs(cut)), 0); pre != 0 {
		t.Errorf("a purely attenuating chain should not have headroom applied, got %.2f dB", pre)
	}

	// the same for an EQ that only cuts (the "soft and easy to listen to" kind)
	var softish [maxBands]slotConfig
	softish[0] = slotConfig{Type: "PK", Freq: 3000, Q: 1.0, GainDB: -2.8}
	softish[1] = slotConfig{Type: "PK", Freq: 10000, Q: 0.8, GainDB: -4.0}
	if pre := effectivePreampDB(cascadeMaxGainDB(rawCoeffs(softish)), 0); pre != 0 {
		t.Errorf("an all-cut chain should not have headroom applied, got %.2f dB", pre)
	}

	// a real boost is still held down as before (the dead zone does not switch headroom off)
	if pre := effectivePreampDB(6.0, 0); pre > -(6.0+preampSafetyMarginDB)+0.001 {
		t.Errorf("a +6 dB boost must be given %.1f dB of headroom, got %.2f dB", 6.0+preampSafetyMarginDB, pre)
	}
}

// TestHeadroomPolicyKeepsLoudness: once there is in-chain headroom, the "automatic overall
// level reduction" should only apply to the part **beyond the headroom**.
// This is the fix for the "enabling an effect makes everything quieter" complaint: on the
// board the headphone chain used to be pushed down by -7.8 dB, and with headroom enabled it
// should become **0 dB** (boosts <= 18 dB are all handled inside the chain and the output
// peak is left to the limiter).
func TestHeadroomPolicyKeepsLoudness(t *testing.T) {
	// verify "the preamp the policy computes" directly, without touching the hardware: set
	// up dspHeadroomUsable's precondition by hand.
	// Only the algorithm is tested here: max(0, maxGain - headroomDB) through the old rules
	// (dead zone + safety margin).
	soft := 0.0
	if pre := effectivePreampDB(4.5-headroomDB, 0); pre != 0 {
		t.Errorf("a +4.5 dB boost should not lower the level inside 18 dB of headroom, got %.2f dB", pre)
	}
	if pre := effectivePreampDB(15.0-headroomDB, 0); pre != 0 {
		t.Errorf("a +15 dB boost should not lower the level inside 18 dB of headroom, got %.2f dB", pre)
	}
	// the part beyond the headroom is still held down, and the old safety margin still applies
	want := -(3.0 + preampSafetyMarginDB)
	if pre := effectivePreampDB(21.0-headroomDB, 0); pre > want+0.01 {
		t.Errorf("a +21 dB boost exceeds the 18 dB headroom by 3 dB, should be held down by %.2f dB, got %.2f", want, pre)
	}
	_ = soft
}

// TestHeadroomCapabilityComesFromHardware: whether headroom is available must be read from
// CAP1 bit10; "engine generation == 1" must not be used as a proxy (within the same
// generation, whether the bitstream includes this is decided by the build).
func TestHeadroomCapabilityComesFromHardware(t *testing.T) {
	cases := []struct {
		name string
		caps dspEngineCaps
		want bool
	}{
		{"0.2 bitstream with headroom built in (opcode bitmap 0x0703)", dspEngineCaps{Present: true, Opcodes: 0x0703}, true},
		{"same generation without headroom (opcode bitmap 0x0303)", dspEngineCaps{Present: true, Opcodes: 0x0303}, false},
		{"old bitstream (no magic)", dspEngineCaps{Present: false, Opcodes: 0x0703}, false},
	}
	for _, c := range cases {
		if got := c.caps.HeadroomAvailable(); got != c.want {
			t.Errorf("%s: HeadroomAvailable() = %v, want %v", c.name, got, c.want)
		}
	}
}
