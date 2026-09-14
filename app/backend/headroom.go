// SPDX-License-Identifier: GPL-2.0-only
// headroom.go — automatic gain compensation (preamp) for the EQ
//
// Why this exists: as soon as one band boosts, the cascade exceeds 0 dBFS. Modern
// masters peak at 0 dBFS, so such material clips inside the saturating biquad
// stages -- audibly "harsh, flat, pumped".
//
// Measured 2026-09-14 (rock preset, real full-scale music, fixed-point Q3.15
// time-domain model):
//   no compensation       max response +3.00 dB   clipping onsets 10146
//   -3 dB                 max response +0.38 dB   clipping onsets  1101
//   -5 dB                 max response -1.93 dB   clipping onsets    85
//   -6 dB = -(3.0+3)      max response -3.00 dB   clipping onsets     0   <- chosen
//
// Rule: preamp = -(peak gain of the cascade + a 3 dB safety margin), then take
// min(user preamp). The 3 dB covers the fact that the *sample* peak exceeds the
// peak of |H(f)|: the stages' phase responses add up and overshoot by 1.5-3 dB,
// so compensating only the response peak is not enough (at -3 dB it still clipped).
//
// The hardware has no separate preamp stage (dspSetMasterVolume/dspSetPreamp are
// no-ops), so the gain is folded into the coefficients -- and it must be applied at
// the *input* of the chain (see dspFinalCoeffs): spreading it over all six bands
// lowers the overall gain too, but each individual stage keeps almost all of its
// own gain, so a full-scale input still clips the first stage.
//
// Do not push deeper: the numerator of the 60 Hz low shelf is a near-cancellation
// (its three coefficients sum to a single digit), so scaling it and rounding each
// coefficient swings the low-frequency response around -- measured, -7 dB was
// *worse* than -6 dB (clipping onsets went from 0 back to 167). Hence: one
// calculation, no iterative deepening.
package main

import (
	"math"
	"math/cmplx"
)

// preampSafetyMarginDB is the allowance on top of the response peak, covering
// sample-peak overshoot.
const preampSafetyMarginDB = 3.0

// cascadeMaxGainDB returns the largest magnitude (dB) of the cascaded response
// between 20 Hz and 20 kHz. The coefficients passed in already include the unity
// coefficients of inactive bands. They are the quantized Q3.15 values, so this is
// what the hardware actually does.
func cascadeMaxGainDB(coefs [maxHardwareBands][coefPerBand]int32) float64 {
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
		for b := 0; b < maxHardwareBands; b++ {
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

// effectivePreampDB returns the preamp to apply (dB, <= 0).
// maxGainDB is the peak gain of the cascade, userPreampDB what the user or preset asked for.
func effectivePreampDB(maxGainDB, userPreampDB float64) float64 {
	if userPreampDB > 0 {
		userPreampDB = 0
	}
	// Only an actual boost needs headroom; an all-cut or flat EQ must not be attenuated
	need := 0.0
	if maxGainDB > 0 {
		need = -(maxGainDB + preampSafetyMarginDB)
	}
	if userPreampDB < need {
		return userPreampDB
	}
	return need
}
