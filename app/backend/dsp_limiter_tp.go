// dsp_limiter_tp.go — backend support for the true-peak lookahead limiter (mode + log2-domain coefficient conversion)
//
// Why a mode of its own: the two limiter modes give `LIM_ATT/LIM_REL` **different meanings** (same register, different interpretation):
//
//	· the old limiter.v (feedback, linear domain): gain ← gain · k, k_att < 1 (compress), k_rel > 1 (recover)
//	· the true-peak limiter (first order in the log2 domain): g ← g + (tgt − g)·k, **k < 1** in both directions (approach the target)
//
// So switching modes forces the backend to switch formulas, otherwise "10 ms release" turns into a completely different time constant.
//
// ⚠️ **The sample-rate trap** (the main reason this file exists): the engine processes "one sample per ack",
//
//	and an ack is the **slot rate** (L/R at 48 kHz each ⇒ 96 kHz per sample), not 48 kHz.
//	The old formula used 48000 (dspSetLimiterTimes in dsp.go), so its "physical time constant"
//	is actually twice the nominal value — that is released 0.1 behavior and is **left alone**; the new mode uses the correct 96000.
package main

import (
	"math"
)

const (
	// limSampleRateHz: the limiter updates once per sample ⇒ frequency = slot rate = 2 × 48 kHz
	limSampleRateHz = 96000.0

	// The CTRL bit that selects the mode (matches DSP_CTRL_REG(18) in the I2S/AXI control RTL, axi_i2s_adi_v1_2.vhd)
	ctrlLimTP = 1 << 18

	// limMaxMsTP: the longest time constant Q1.15 can express.
	// 1 LSB of k = 2^-15, and τ = 1/(k·fs) ⇒ τ_max = 2^15/96000 s ≈ 341 ms.
	// ⚠️ A longer release time **cannot be written in this format** (it degrades to k=1, far slower than nominal) —
	//    so the conversion function clamps here, and the backend has to report the τ it actually achieves (limMsFromKTP),
	//    and must never claim a time constant it cannot deliver.
	limMaxMsTP = 341.0
)

// dspLimiterTP: whether the true-peak limiter is currently in use (false = the old feedback one)
// The true-peak lookahead mode is the default (see dspLimiterEnabled in dsp.go for the default and the reasoning).
var dspLimiterTP = true

// limKFromMsTP converts a time constant (ms) into a first-order log2-domain coefficient (Q1.15).
// First-order discretisation: k = 1 − exp(−1/(τ·fs)), τ in seconds, fs = 96 kHz.
// The result is always < qOne (otherwise it jumps instead of approaching), and at least 1 (otherwise it never moves).
func limKFromMsTP(ms float64) uint32 {
	if ms < 0.05 {
		ms = 0.05
	}
	if ms > limMaxMsTP {
		ms = limMaxMsTP // see limMaxMsTP: the Q1.15 representation limit
	}
	k := 1.0 - math.Exp(-1.0/(ms/1000.0*limSampleRateHz))
	q := uint32(math.Round(k * qOne))
	if q < 1 {
		q = 1
	}
	if q > qOne-1 { // must stay < 1.0
		q = qOne - 1
	}
	return q
}

// limMsFromKTP is the inverse of the above (for status display / self-check): τ = −1/(fs·ln(1−k))
func limMsFromKTP(q uint32) float64 {
	if q == 0 || q >= qOne {
		return 0
	}
	k := float64(q) / qOne
	return -1000.0 / (limSampleRateHz * math.Log(1.0-k))
}

// limiterModeName returns the current mode name (used by /api/status and capabilities)
func limiterModeName() string {
	if dspLimiterTP {
		return "truepeak"
	}
	return "feedback"
}

// dspSetLimiterTP switches the limiter mode (writes CTRL.bit18) and **re-downloads** the time constants
// — the same ms value yields different coefficients in the two modes, so a mode switch must recompute them.
func dspSetLimiterTP(tp bool) error {
	if !dspAvailable {
		return errDSPUnavailable
	}
	dspLimiterTP = tp
	if err := dspSetLimiterTimes(dspLimiterAttMs, dspLimiterRelMs); err != nil {
		return err
	}
	return dspWriteCtrl()
}
