// dsp_headroom.go -- automatic headroom compensation (preamp) for the EQ
//
// Why this is needed: as soon as any band **boosts**, the cascade exceeds 0 dB. Modern masters
// usually peak at 0 dBFS, so such material necessarily goes over after a boost and gets
// flattened by the biquad saturation stage -- audibly "harsh, muddy, dynamics crushed".
//
// Measured 2026-09-14 (rock preset + real full-scale music, Q3.15 fixed-point time-domain model):
//
//	no compensation      max response +3.00 dB   clipping onset 10146
//	-3 dB                max response +0.38 dB   clipping onset  1101
//	-5 dB                max response -1.93 dB   clipping onset    85
//	-6 dB = -(3.0+3)     max response -3.00 dB   clipping onset     0   <- adopted
//
// Rule: preamp = -(max boost of the cascade response + 3 dB safety margin), then min(user preamp).
// Those 3 dB cover the part where **the sample peak exceeds the maximum of the frequency
// response**: phase summation across the filter stages makes the actual sample peak 1.5-3 dB
// higher than the peak of |H(f)|, so the response maximum alone cannot hold it back
// (measured: still clipping at -3 dB).
//
// The hardware has no separate preamp stage (dspSetMasterVolume/dspSetPreamp are both empty
// implementations), so this gain is **folded into the coefficients**, and it must be applied at
// the **input of the first stage** (see dspFinalCoeffs): spreading it over all 6 stages does
// lower the whole chain by g, but the first stage's own gain barely drops, so a full-scale input
// still blows it up.
//
// ⚠️ Do not push any deeper: the 60 Hz low-shelf numerator b0/b1/b2 are cancellation terms
// (their sum is only single digits), so rounding each of them by ±0.5 after scaling makes the
// low-frequency response jump wildly -- measured: -7 dB is actually worse than -6 dB (clipping
// onset goes back from 0 to 167). So there is no "iterative deepening" here; one exact
// calculation is enough.
package main

import (
	"math"
	"math/cmplx"
)

// preampSafetyMarginDB is the extra margin kept beyond the response maximum, covering sample-peak overshoot.
const preampSafetyMarginDB = 3.0

// preampDeadZoneDB is the threshold (dB) below which a boost counts as absent.
// Why it is unavoidable: the cascade maximum gain is obtained by sweeping the **quantised**
// coefficients, and Q3.15 rounding makes even a **purely attenuating** chain come out with a
// "+0.04 dB maximum boost" -- so it gets 3 dB of compensation for nothing.
// Measured on the board: a chain holding only a single 600 Hz lowpass came back as
// `preamp_db = -3.05` (see the listening-verification section of the internal true-peak limiter
// design note).
// What the user hears is "I only wanted to tame the highs and the whole thing came out quieter".
const preampDeadZoneDB = 0.25

// cascadeMaxGainDB returns the maximum magnitude (dB) of the cascade response from 20 Hz to
// 20 kHz. The input already contains unit coefficients for off bands. Computing with the
// **quantised** Q3.15 coefficients gives the hardware's real response.
func cascadeMaxGainDB(coefs [maxBands][coefPerBand]int32) float64 {
	const (
		fLo = 20.0
		fHi = 20000.0
		n   = 400
	)
	maxG := 0.0
	first := true
	for i := 0; i < n; i++ {
		f := fLo * math.Pow(fHi/fLo, float64(i)/float64(n-1))
		w := 2 * math.Pi * f / sampleRate
		z1 := complex(math.Cos(w), -math.Sin(w))
		z2 := z1 * z1
		h := complex(1, 0)
		for b := 0; b < maxBands; b++ {
			c := coefs[b]
			num := complex(float64(c[0]), 0) + complex(float64(c[1]), 0)*z1 + complex(float64(c[2]), 0)*z2
			den := complex(float64(qOne), 0) + complex(float64(c[3]), 0)*z1 + complex(float64(c[4]), 0)*z2
			if den == complex(0, 0) {
				continue
			}
			h *= num / den
		}
		db := 20 * math.Log10(cmplx.Abs(h))
		if first || db > maxG {
			maxG = db
			first = false
		}
	}
	return maxG
}

// effectivePreampDB returns the preamp actually to be applied (dB, <= 0).
// maxGainDB = maximum cascade boost; userPreampDB = the preamp requested by the user/preset.
func effectivePreampDB(maxGainDB, userPreampDB float64) float64 {
	if userPreampDB > 0 {
		userPreampDB = 0
	}
	// Headroom is only needed when a boost **really** exists; an all-cut/bypass chain (including the +0.04 dB quantisation-noise level) must not be attenuated
	need := 0.0
	if maxGainDB > preampDeadZoneDB {
		need = -(maxGainDB + preampSafetyMarginDB)
	}
	if userPreampDB < need {
		return userPreampDB
	}
	return need
}
