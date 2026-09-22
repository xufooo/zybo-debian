// SPDX-License-Identifier: GPL-2.0-only
// response.go -- frequency response evaluation (for fitting and verification)
//
// Key point: **evaluate with the Q3.15-quantized coefficients**, so what comes out is the
// hardware's real response rather than the ideal formula's response.
// The cascade adds up in the dB domain (stages multiply => dB add), so every stage can be
// computed on its own and then summed; while fitting, changing one candidate parameter
// only requires recomputing that one stage -- on a 650 MHz board that is an
// order-of-magnitude difference.

package main

import (
	"math"
	"math/cmplx"
)

// logGrid generates logarithmically spaced frequency points (Hz).
func logGrid(fLo, fHi float64, n int) []float64 {
	if n < 2 {
		n = 2
	}
	out := make([]float64, n)
	for i := 0; i < n; i++ {
		out[i] = fLo * math.Pow(fHi/fLo, float64(i)/float64(n-1))
	}
	return out
}

// bandResponseDB returns the magnitude response (dB) of one biquad stage at the given
// frequency points.
// With a fixed set of frequency points, the per-stage results can be summed directly in
// the dB domain to get the whole-chain response.
func bandResponseDB(sc slotConfig, freqs []float64) []float64 {
	out := make([]float64, len(freqs))
	if isBandOff(sc.Type) {
		return out // all 0 dB
	}
	s := sanitizeBand(sc)
	b0, b1, b2, a1, a2, err := designBiquad(s)
	if err != nil {
		return out
	}
	// Q3.15 fixed point -> float, same as the hardware
	const scale = float64(qOne)
	cb0, cb1, cb2 := float64(b0)/scale, float64(b1)/scale, float64(b2)/scale
	ca1, ca2 := float64(a1)/scale, float64(a2)/scale
	for i, f := range freqs {
		w := 2 * math.Pi * f / sampleRate
		z1 := complex(math.Cos(w), -math.Sin(w))
		z2 := z1 * z1
		num := complex(cb0, 0) + complex(cb1, 0)*z1 + complex(cb2, 0)*z2
		den := complex(1, 0) + complex(ca1, 0)*z1 + complex(ca2, 0)*z2
		if den == complex(0, 0) {
			continue
		}
		out[i] = 20 * math.Log10(cmplx.Abs(num/den))
	}
	return out
}

// evalChainDB returns the response (dB) of the whole chain at the given frequency points:
// the per-stage dB values added together.
func evalChainDB(chain []ChainItem, freqs []float64) []float64 {
	total := make([]float64, len(freqs))
	for _, it := range chain {
		if !it.Enabled {
			continue
		}
		st, ok := chainSlotType(it.Type)
		if !ok {
			continue // stages that cannot reach the hardware take no part (the caller tells the user)
		}
		r := bandResponseDB(slotConfig{Type: st, Freq: it.Freq, Q: it.Q, GainDB: it.GainDB}, freqs)
		for i := range total {
			total[i] += r[i]
		}
	}
	return total
}

// rmsDB is the root-mean-square error between two curves (dB).
func rmsDB(a, b []float64) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return math.Inf(1)
	}
	sum := 0.0
	for i := range a {
		d := a[i] - b[i]
		sum += d * d
	}
	return math.Sqrt(sum / float64(len(a)))
}
