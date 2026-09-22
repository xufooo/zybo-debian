// tube.go — tube warmth (V4A `TubeSimulator`, A3)
//
// Primary source: `AndroidAudioMods/ViPERFX_RE`, `src/viper/effects/TubeSimulator.cpp` (under 40 lines in total):
//
//	TubeProcess():
//	    for (uint32_t i = 0; i < size; i += 2) {
//	        acc[0] = (acc[0] + buffer[i * 2])     / 2.0;
//	        acc[1] = (acc[1] + buffer[i * 2 + 1]) / 2.0;
//	        buffer[i * 2]     = acc[0];
//	        buffer[i * 2 + 1] = acc[1];
//	    }
//
// ⇒ **a first-order moving average**: `y[n] = 0.5·x[n] + 0.5·y[n−1]`, one state per channel, **no nonlinearity**
//
//	(so the name "tube warmth / 6N1J" has nothing to do with the algorithm; it is just a first-order low-pass with strong low-frequency retention and a very low corner, fc ≈ fs/2π·…)
//
// ⚠️ **Deciding the index convention (an inference, but well grounded)**: `i += 2` together with `buffer[i * 2]` would process **every other frame**
//
//	(hitting only frames 0, 2, 4… and dropping 1, 3, 5…). In the same repository
//	`PassFilter::ProcessFrames` and `Crossfeed::ProcessFrames` both use the convention
//	"`size` = **number of frames**, the interleaved buffer indexed as `buffer[2x]`", and
//	under that convention `i += 2` has no interpretation that makes sense ⇒ judged a
//	**typo** and implemented as intended (**every frame is processed**).
//	This is recorded in the project's V4A source-audit notes under "not found / ambiguous".
//
// Local mapping: **one BIQUAD section** (b0=0.5, b1=0, b2=0, a1=−0.5, a2=0 — all powers of two, exact in Q3.15).
// Chain order: V4A runs `… → Cure → TubeSimulator → AnalogX → limiter`, so this unit sits **after** crossfeed/surround.
package main

// tubeParams has no parameters — V4A's TubeSimulator has just one switch (`enable`).
type tubeParams struct{}

var currentTube *tubeParams

// tubeAvailable: anything that can run BIQUAD can do this (no new hardware capability needed).
func tubeAvailable() bool {
	return dspEngineGen == 1 && dspReadEngineCaps().BiquadAvailable()
}

// setTube turns tube warmth on/off (nil = off).
func setTube(p *tubeParams) error {
	if p == nil {
		currentTube = nil
		markStateDirty()
		return nil
	}
	if !tubeAvailable() {
		return errDSPUnavailable
	}
	currentTube = p
	markStateDirty()
	return nil
}

// tubeView is what goes back to the UI (nil = off).
func tubeView() any {
	if currentTube == nil {
		return nil
	}
	return map[string]any{"on": true, "coef": [5]int32{tubeB0, 0, 0, tubeA1, 0}}
}

// y = 0.5x + 0.5y[n−1] ⇒ a1 = −0.5 under this unit's DF-I convention.
const (
	tubeB0 = 16384  // 0.5
	tubeA1 = -16384 // −0.5
)

// tubeNodes returns the tube-warmth slot nodes (one unit section).
func tubeNodes() []planNode {
	return []planNode{{
		Kind:  planKindBiquad,
		Coefs: [5]int32{tubeB0, 0, 0, tubeA1, 0},
	}}
}
