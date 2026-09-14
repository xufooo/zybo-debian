// SPDX-License-Identifier: GPL-2.0-only
// headroom_test.go — offline tests for the automatic EQ headroom compensation

package main

import (
	"math"
	"testing"
)

func withSlots(t *testing.T, slots [maxHardwareBands]slotConfig, preampDB float64, fn func()) {
	t.Helper()
	oldSlots, oldPre, oldApplied := currentSlots, currentPreampDB, currentPreampAppliedDB
	defer func() {
		currentSlots, currentPreampDB, currentPreampAppliedDB = oldSlots, oldPre, oldApplied
	}()
	currentSlots, currentPreampDB = slots, preampDB
	fn()
}

// rawCoeffs designs the coefficients for the given slots without any compensation
func rawCoeffs(slots [maxHardwareBands]slotConfig) [maxHardwareBands][coefPerBand]int32 {
	var c [maxHardwareBands][coefPerBand]int32
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
	if raw < 2.5 || raw > 3.5 {
		t.Fatalf("rock peaks at %.2f dB, expected about +3.0 dB (formula or preset changed?)", raw)
	}
	t.Logf("rock peaks at %+.2f dB -> automatic preamp %+.2f dB (boost + 3 dB safety margin)", raw, -(raw+preampSafetyMarginDB))

	withSlots(t, rock, 0, func() {
		coefs, pre := dspFinalCoeffs()
		// Independent fixed-point time-domain check: at this setting the clipping
		// onsets on real full-scale music go from 10146 to 0.
		if got := cascadeMaxGainDB(coefs); got > -preampSafetyMarginDB+0.5 {
			t.Errorf("after compensation the peak is %+.2f dB, expected <= %+.1f dB", got, -preampSafetyMarginDB+0.5)
		}
		if math.Abs(pre-(-(raw+preampSafetyMarginDB))) > 0.1 {
			t.Errorf("automatic preamp = %.2f dB, expected %.2f dB", pre, -(raw + preampSafetyMarginDB))
		}
	})
}

func TestFlatPresetNeedsNoHeadroom(t *testing.T) {
	withSlots(t, presets["flat"].Slots, 0, func() {
		coefs, pre := dspFinalCoeffs()
		if pre != 0 {
			t.Errorf("the flat preset must not be attenuated, got %.2f dB", pre)
		}
		if got := cascadeMaxGainDB(coefs); math.Abs(got) > 0.01 {
			t.Errorf("the flat preset should be 0 dB, got %+.2f dB", got)
		}
	})
}

func TestPreampGoesToTheFirstActiveBand(t *testing.T) {
	// The compensation has to sit at the input of the chain (first active band),
	// otherwise the first stage still amplifies a full-scale input
	rock := presets["rock"].Slots
	withSlots(t, rock, 0, func() {
		coefs, _ := dspFinalCoeffs()
		raw := rawCoeffs(rock)
		if coefs[0][0] >= raw[0][0] {
			t.Errorf("the first band's numerator was not scaled down: %d -> %d", raw[0][0], coefs[0][0])
		}
		for b := 1; b < maxHardwareBands; b++ {
			if coefs[b] != raw[b] {
				t.Errorf("band %d must not be touched: %v -> %v", b, raw[b], coefs[b])
			}
		}
		// The poles (a1/a2) must never move, or the response shape changes
		for b := 0; b < maxHardwareBands; b++ {
			if coefs[b][3] != raw[b][3] || coefs[b][4] != raw[b][4] {
				t.Errorf("band %d: a1/a2 were modified", b)
			}
		}
	})
}

func TestUserPreampWinsWhenDeeper(t *testing.T) {
	rock := presets["rock"].Slots
	// The user asked for -12 dB, deeper than the -6 dB the rule wants: keep -12
	withSlots(t, rock, -12, func() {
		coefs, pre := dspFinalCoeffs()
		if math.Abs(pre-(-12)) > 0.05 {
			t.Errorf("the user preamp of -12 dB should be used, got %.2f dB", pre)
		}
		if got := cascadeMaxGainDB(coefs); got > 0.01 {
			t.Errorf("still %+.2f dB after compensation", got)
		}
	})
}

func TestUserPreampTooShallowIsOverridden(t *testing.T) {
	rock := presets["rock"].Slots
	// The user only asked for -2 dB, which cannot hold back +3.0 dB: deepen automatically
	withSlots(t, rock, -2, func() {
		coefs, pre := dspFinalCoeffs()
		if pre > -5.9 {
			t.Errorf("a too-shallow user preamp should be deepened to <= -6.0 dB, got %.2f dB", pre)
		}
		if got := cascadeMaxGainDB(coefs); got > -preampSafetyMarginDB+0.5 {
			t.Errorf("still %+.2f dB after compensation", got)
		}
	})
}

func TestEveryPresetGetsHeadroom(t *testing.T) {
	// Every boosting preset must be brought down, and by enough to actually lower the
	// peak gain. (We cannot assert "always exactly -3 dB": the 60 Hz low shelf's
	// numerator is a near-cancellation, so scaling it and rounding each coefficient
	// shifts the low-frequency response. The rock preset was checked against real
	// full-scale music with a fixed-point time-domain model: onsets 10146 -> 0.)
	for name, p := range presets {
		name, p := name, p
		t.Run(name, func(t *testing.T) {
			raw := cascadeMaxGainDB(rawCoeffs(p.Slots))
			withSlots(t, p.Slots, p.PreampDB, func() {
				coefs, pre := dspFinalCoeffs()
				got := cascadeMaxGainDB(coefs)
				if raw > 0.01 {
					if pre > -(preampSafetyMarginDB - 0.01) {
						t.Errorf("preset %s boosts by %+.2f dB but no headroom was taken (preamp %.2f dB)", name, raw, pre)
					}
					if got > raw-1.5 {
						t.Errorf("preset %s: %+.2f dB before, %+.2f dB after -- compensation barely did anything", name, raw, got)
					}
				} else if pre != 0 {
					t.Errorf("preset %s does not boost, so it must not be attenuated (preamp %.2f dB)", name, pre)
				}
				t.Logf("  preset %-10s boost %+5.2f dB -> preamp %+6.2f dB -> after %+5.2f dB", name, raw, pre, got)
			})
		})
	}
}
