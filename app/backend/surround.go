package main

import (
	"fmt"
	"math"
)

// ============================================================================
// Spatial (Haas / V4A DiffSurround): **delay the whole right channel** by a few milliseconds
// ============================================================================
// Evidence (first-hand): `AndroidAudioMods/ViPERFX_RE` (branch rewrite-continued), file
// `src/viper/effects/DiffSurround.cpp`. That file is under 60 lines and the algorithm is simply:
//
//	// ctor: two WaveBuffer(1, 0x1000) (4096 samples per channel)
//	// Reset(): buffers[1].PushZeros(delayTime/1000 * samplingRate)  <- pre-fills the delay for the second channel only
//	// Process(): each channel enters and leaves the ring on its own (no mixing, no gain change)
//
// i.e. **DiffSurround = delay the right channel by delayTime milliseconds** (Haas effect), with no matrix/mixing/filtering.
// Mapping on this device: one `DELAY` slot + flags bit2 (`FL_DLY_R`, affecting the second channel only) --
// the engine runs the same slots for both channels along one chain, so this flag is what distinguishes them.
//
// ⚠️ Why not an M/S matrix: the "other channel" that `MIX2` reads is inherently one tick late (20.8 µs), and a wideband matrix would
// create a 37.5° phase difference at 5 kHz and pollute the side signal. See the effects reference replication notes §9.
// ⚠️ Parameter range: the decompiled repository only has `delayTime` (default 0.0); **the UI slider's upper bound is in the Java/settings layer**
// and was not obtained in this round => this device uses 0~20 ms, default 2.5 ms, marked **"self-calibrated"**
// (following the common Haas range: 1~20 ms widens, >30 ms is heard as an echo).
// ============================================================================

// surroundParams is one parameter in user terms: how many milliseconds to delay the right channel.
type surroundParams struct {
	DelayMs float64 `json:"delay_ms"`
}

// Value bounds for surround (milliseconds). The lower bound 0 = off; **the upper bound is not hard-coded** -- it is determined by the hardware ring length
// (hwDelayWords samples per channel); after the ring grew from 256 to 8192 on 2026-09-20 it is 170.7 ms.
// ⚠️ This used to hard-code `surroundMaxMs = 5.0` (the value from the 256-word era): the ring grew 32-fold while the UI limit stayed
//
//	5 ms, so the user simply could not set a long delay -- the same class of "stale hard-coded limit" as `FIR_BASE` and the DDC cap of 16 sections.
const surroundMinMs = 0.0

// surroundMaxMs is the maximum delay (in milliseconds) the current bitstream allows. It follows the ring length self-reported through CAP2.
func surroundMaxMs() float64 {
	return float64(hwDelayWords) / sampleRate * 1000.0
}

// surroundDefaultParams: default 2.5 ms -- **self-calibrated** (see the file header: the authoritative slider range was not obtained).
func surroundDefaultParams() surroundParams {
	return surroundParams{DelayMs: 2.5}
}

// surroundDelaySamples converts milliseconds into **that channel's** sample count (the RTL's ring-length convention).
// 0 ms = off (no delay slot); anything else is clamped to [1, maxDelayWords].
func surroundDelaySamples(p surroundParams) int {
	if p.DelayMs <= surroundMinMs {
		return 0
	}
	n := int(math.Round(p.DelayMs / 1000.0 * sampleRate))
	if n < 1 {
		n = 1
	}
	if n > hwDelayWords {
		n = hwDelayWords
	}
	return n
}

// ── Runtime state ─────────────────────────────────────────────────────

var currentSurround *surroundParams

// surroundAvailable: the spatial effect needs the `DELAY` slot (CAP1 bit4).
func surroundAvailable() bool {
	return dspEngineGen == 1 && dspReadEngineCaps().DelayAvailable()
}

// setSurround sets it (passing nil turns it off). When the hardware has no DELAY it errors out explicitly instead of dropping it silently.
func setSurround(p *surroundParams) error {
	if p == nil {
		currentSurround = nil
		markStateDirty()
		return nil
	}
	if !surroundAvailable() {
		return fmt.Errorf("this hardware has no DELAY slot (CAP1 bit4 is not set); the spatial effect cannot be configured")
	}
	q := clampSurroundMs(*p)
	currentSurround = &q
	markStateDirty()
	return nil
}

// clampSurroundMs clamps the delay to [surroundMinMs, surroundMaxMs()]. It is a pure function so that it can be tested:
// the upper bound follows the hardware ring length (see the file header); it was once hard-coded at 5.0 ms while the ring had already grown to 170.7 ms.
func clampSurroundMs(p surroundParams) surroundParams {
	if p.DelayMs < surroundMinMs {
		p.DelayMs = surroundMinMs
	}
	if p.DelayMs > surroundMaxMs() {
		p.DelayMs = surroundMaxMs()
	}
	return p
}

// surroundView is for the API/panel (nil = off).
func surroundView() any {
	if currentSurround == nil {
		return nil
	}
	p := *currentSurround
	return map[string]any{
		"delay_ms":  p.DelayMs,
		"samples":   surroundDelaySamples(p),
		"available": surroundAvailable(),
		"max_ms":    surroundMaxMs(),
		"note": "A V4A DiffSurround reproduction: delay the whole right channel by this many milliseconds (Haas), " +
			"which widens the sound and stops the vocals from being crammed in the center. Only 1~5 ms gives the \"widening\" effect (this device's ring limit is 5.3 ms); " +
			"beyond that it starts to sound like an echo. " +
			"The range is self-calibrated (the decompiled source only has delayTime; the UI slider's upper bound in the Java layer was not obtained)",
	}
}
