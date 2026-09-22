// SPDX-License-Identifier: GPL-2.0-only
// fit.go -- fit a target curve into N peaking bands
//
// Why this is needed: AutoEQ / EqualizerAPO files routinely carry a 10-band parametric EQ or
// a 127-point GraphicEQ, while the 0.1 hardware has only 6 bands. **Truncating to the first 6
// bands is wrong** (that is what 0.1 did: the 2k-16k correction was silently dropped and the
// user believed the correction had taken effect). Here the whole target curve is instead fitted
// into as many bands as fit.
//
// Algorithm: pick log-spaced center frequencies -> coordinate descent (each band picks the
//       gain x Q candidate that minimises the total error) -> iterate a few passes. The
//       evaluation uses the Q3.15-quantised response, matching the hardware.
// Report: returns the fitted bands and the **RMS error** (dB), so the caller can truthfully
// tell the user "how closely this curve was approximated".

package main

import "math"

// fitCenterCandidates returns maxBands log-spaced center frequencies (Hz).
// The upper bound is min(16 kHz, 0.45*fs); the lower bound is the hardware's minBandFreq (60 Hz, the Q3.15 stability limit).
func fitCenters(maxBands int) []float64 {
	hi := 16000.0
	if cap := sampleRate * 0.45; hi > cap {
		hi = cap
	}
	if hi < minBandFreq*2 {
		hi = minBandFreq * 2
	}
	g := logGrid(minBandFreq, hi, maxBands)
	for i := range g {
		g[i] = math.Round(g[i]*10) / 10 // round to 0.1 Hz for readability and reuse
	}
	return g
}

// fitQSet is the set of Q values available per band. Too large is meaninglessly narrow at low frequencies; too small bleeds into the neighbouring bands.
var fitQSet = []float64{0.5, 0.7, 1.0, 1.4}

// fitBandsToTarget fits the target curve (dB, over freqs) into <= maxBands peaking bands.
// It returns the bands, the RMS error (dB) and whether it succeeded.
func fitBandsToTarget(target, freqs []float64, maxBands int) ([]ChainItem, float64, error) {
	if len(freqs) == 0 || len(freqs) != len(target) {
		return nil, 0, errFitBadInput
	}
	if maxBands < 1 {
		maxBands = 1
	}
	centers := fitCenters(maxBands)

	// Each band's response is computed once (all gains 0); during fitting only the band that changed is recomputed
	chain := make([]ChainItem, maxBands)
	band := make([][]float64, maxBands)
	for i, fc := range centers {
		chain[i] = ChainItem{Type: "peq", Enabled: true, Freq: fc, GainDB: 0, Q: 0.7}
		band[i] = bandResponseDB(slotConfig{Type: "PK", Freq: fc, Q: 0.7, GainDB: 0}, freqs)
	}
	sum := make([]float64, len(freqs))
	addBand := func(dst, src []float64, sign float64) {
		for i := range dst {
			dst[i] += sign * src[i]
		}
	}
	for i := range band {
		addBand(sum, band[i], 1)
	}

	// Coordinate descent: each pass walks the bands and takes the (gain, Q) candidate with the smallest SSE
	const (
		gainLo, gainHi, gainStep = -12.0, 12.0, 0.5
		passes                   = 4
	)
	for pass := 0; pass < passes; pass++ {
		moved := false
		for i := range centers {
			best := chain[i]
			bestSSE := sse(sum, target)
			changed := false
			for _, q := range fitQSet {
				for g := gainLo; g <= gainHi+1e-9; g += gainStep {
					cand := ChainItem{Type: "peq", Enabled: true, Freq: centers[i], GainDB: math.Round(g*10) / 10, Q: q}
					r := bandResponseDB(slotConfig{Type: "PK", Freq: cand.Freq, Q: cand.Q, GainDB: cand.GainDB}, freqs)
					// Trial: swap out the old band and add the new one
					addBand(sum, band[i], -1)
					addBand(sum, r, 1)
					e := sse(sum, target)
					addBand(sum, r, -1)
					addBand(sum, band[i], 1)
					if e < bestSSE-1e-9 {
						bestSSE, best, changed = e, cand, true
					}
				}
			}
			if changed {
				r := bandResponseDB(slotConfig{Type: "PK", Freq: best.Freq, Q: best.Q, GainDB: best.GainDB}, freqs)
				addBand(sum, band[i], -1)
				addBand(sum, r, 1)
				band[i] = r
				chain[i] = best
				moved = true
			}
		}
		if !moved {
			break
		}
	}
	return chain, rmsDB(sum, target), nil
}

func sse(got, want []float64) float64 {
	s := 0.0
	for i := range got {
		d := got[i] - want[i]
		s += d * d
	}
	return s
}

// fitError is the error for invalid fit input (avoids pulling in the errors package for a single string).
var errFitBadInput = errFitInput{}

type errFitInput struct{}

func (errFitInput) Error() string {
	return "invalid fit input (target curve and frequency points differ in length)"
}
