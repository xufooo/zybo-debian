// SPDX-License-Identifier: GPL-2.0-only
// dyn.go -- parameter conversion and slot-table compilation for the `DYN` stage (dynamic bass / dynamic loudness)
//
// Reference: V4A `DynamicSystem` (bass-band X/Y side chain -> dynamic gain) + `PlaybackGain`
// (detector weight 2.2 kHz, Q 0.33; slow gain in the log domain). Parameter sources (first-hand evidence):
//
//	· side-chain band-pass 2.2 kHz / Q 0.33 -- V4A PlaybackGain.cpp: SetBandPassParameter(2200.0, fs, 0.33)
//	· parameter range: local JamesDSP configuration `bass_maxgain=5` (dynamic bass gain ceiling taken as 5~12 dB)
//
// The 5 words sent to the hardware (Q3.15): c0=gmax c1=gmin c2=ref c3=ks c4={rel[8:0],att[8:0]}
//
//	gmax  = 10^(gain_db/20)          maximum bass boost
//	gmin  = 10^(-cut_db/20)          maximum cut back (default 0 dB = 1.0, boost only)
//	ref   = 10^(ref_db/20)           reduction starts once the envelope exceeds it ("hold the loud part back")
//	ks    = slope (Q3.15): how far the gain is pulled down per unit the envelope exceeds ref
//	att/rel = first-order coefficient k = 1 - e^(-1/(τ·fs)), 9 bits each (k=1.0 -> 512)
package main

import (
	"fmt"
	"math"
)

const (
	// dynDetectFreq / dynDetectQ: detector band-pass (following V4A PlaybackGain's 2.2 kHz / Q 0.33)
	dynDetectFreq = 2200.0
	dynDetectQ    = 0.33
	// dynEnvShift: the envelope normalizes 24-bit full scale to Q3.15 (2^23 -> 2^15), matching the RTL's >>8
	dynEnvShift = 8
)

// ============================================================================
// A6 item 3: V4A `DynamicBass` -- **done (simple branch)**; the full branch is not implemented.
// ⚠️ Corrected 2026-09-22: the blocker is **slots** (~11-12 slots; the engine has 24 and the baseline chain already uses 22), **not hardware**.
// ============================================================================
//
// Status: **decided and implemented**. The body is in dynbass.go (params/coefs/nodes), the slot
// expansion is the planKindDynBass branch in dsp_slot.go, and the API field is `dynamic_bass`
// (see api_dsp_chain.go). The original "branch choice" analysis is kept here as first-hand evidence.
//
//	(a) Differences between the two branches (source-confirmed)
//
//	`the community ViPER4Android rewrite (viperfx-re): src/viper/utils/DynamicBass.cpp:21-28` -- **simple branch** (`lowFreqX <= 120`):
//	    `avg = lowPass.ProcessSample(L + R); L += avg; R += avg`
//	    only **one 55 Hz low-pass** (`lowPass.SetLowPassParameter(55, fs, qPeak/666+0.5)`,
//	    see `:17` / `:47` / `:59`) acting on L+R, then added back to both channels.
//	    => no filter bank, no side gain; structurally it is just "add one mono low-frequency path". **Implemented.**
//	`the community ViPER4Android rewrite (viperfx-re): src/viper/utils/DynamicBass.cpp:29-41` -- **full branch** (`lowFreqX > 120`):
//	    `filterX` (x1/x2 band-pass) -> `filterY` (y1/y2 band-pass) -> weighted sum of 6 intermediate values
//	    `L' = x2 + y3 + sideGainX·y2 + sideGainY·y1 + x3` (the right channel has the same form)
//	    => two 6-tap pole-filter strings (`PolesFilter`) × 2 channels + two side gains s1/s2.
//	    **Not implemented** (setDynamicBass rejects it with an error). ⚠️ Corrected (2026-09-22): it **needs no new hardware** --
//	    `PolesFilter` (`utils/PolesFilter.cpp:29-47`) is two **4-stage first-order low-passes** per channel + a 3-sample input delay,
//	    all three outputs are same-channel quantities with **no cross-channel terms**; two identical first-order sections in cascade = one second-order section,
//	    which can be built from the existing OP_BIQUAD/OP_DELAY/OP_MIX2 (~11-12 slots). The real limit is **slots**
//	    (24 engine slots, 22 already used by the baseline chain) => it does not fit with the current chain; deferred to 0.3 (effect combinations + UI).
//	    9 of the 10 official device presets have x1 > 120 (`arrays.xml:525-534`); only
//	    "Common Earphone(v2)" (100;5600;40;80;50;50) takes the simple branch.
//
//	Where `lowFreqX` comes from: the client sends the 6 numbers of `coeffs` down verbatim
//	(the community ViPER4Android 4.x app: com/vipercn/viper4android_v2/service/ViPER4AndroidService.java:1825-1834
//	 `dsParameter[0]` -> `PARAM_HPFX_DYNSYS_XCOEFFS`, core side `ViPER.cpp:322-337`).
//
//	(b) This device's DYN stage is **not** V4A DynamicBass (source-confirmed; the conclusion is unchanged)
//
//	`dynParams` in `dyn.go` + `planKindDyn` in `dsp_slot.go` expand into the **DYN stage**:
//	side-chain band-pass (2.2 kHz/Q0.33) detection -> envelope -> slow gain (`OP_DYN` in `dsp_engine.v:217-226`).
//	It handles "low frequencies tracking **loudness**" (≈ V4A's PlaybackGain), whereas V4A's `DynamicBass`
//	shapes low frequencies by **headphone-model preset** (plus a Q that follows the bass preset) -- the two are **not the same algorithm**.
//	So this is **adding a stage**, not "changing a branch".
//
//	(c) Why only the simple branch is possible, and how far this device goes (measured on the TB, not inferred)
//
//	The simple branch needs `LP55(L + R)`, i.e. every channel must obtain the **other channel**. In the frozen bitstream the only
//	cross-channel path is bus 22 + the `xf_lo` register pair; its timing was pinned down by **pass-by-pass measurement on the new TB**
//	(the engine cross-phase testbench `tb_engine_xphase.v`, already in the run_sim.sh list):
//
//	    the **R pass** of frame n reads L[n] from the **same frame**   -- difference of **0** samples
//	    the **L pass** of frame n reads R[n−1] from the **previous frame** -- difference of **1** sample
//
//	⚠️ Correction: this used to say "the other channel value read from the cross-channel bus is the **previous tick**" (citing dsp_engine.v:230).
//	   Measurement shows that only holds for the **L pass**; because L runs first and R second, `xf_lo[0]` is already
//	   written by S_DONE of the L pass in this frame, so the R pass reads L from the **same frame**. This asymmetry is the
//	   entire source of error when this device implements DynamicBass.
//
//	=> This device implements "one 55 Hz low-pass per channel, side signal = this channel + the other channel (L pass 1 sample late)".
//	   The input sequence of the R-channel filter equals V4A's `L[n]+R[n]` sample by sample => **its output is exactly V4A's
//	   `avg[n]` (bit for bit)**; the L-channel one is fed `L[n]+R[n−1]`, where only the R component is 1 sample late
//	   (at 55 Hz: 360°·55/48000 = 0.4125°).
//
//	   Two filters (rather than "one shared avg") is forced by the hardware; both reasons are pinned down in
//	   dynbass_test.go:
//	     - BIQUAD state is partitioned by channel (`st_r = sidx + stb`, L uses 0..47 / R uses 48..95,
//	       `dsp_engine.v:1165`/`:1264`); a 16-bit address cannot produce the same index (`stb = -NSEC`
//	       does wrap back to the same number, but the read-side `st_r >= NCH*NSEC` guard treats it as "zero state");
//	     - "compute on one path, reuse on the other" would have to leave the intermediate value on the bus **across passes**, but a L-only slot's
//	       bypass action on the other pass is `bus[out] <= bus[in]` (`dsp_engine.v:1217`), which necessarily clobbers it.
//
//	(d) Cost and trade-offs (measured; see dynbass_test.go / TestDynamicBassPlanShape)
//
//	4 slots / 3 sections / 15 coefficients (the send slot n=0 takes the bypass branch and occupies no section). Baseline chain 22 slots => 26 slots >
//	NSLOT=24 => **the planner rejects it explicitly** ("not enough slots...") instead of silently dropping slots. Wanting both stages at once requires freeing slots first.
//	Frequency-response deviation (Q3.15 quantization vs V4A floating point): 20 Hz −1.06 dB, 55 Hz −0.02 dB, 200 Hz +0.11 dB;
//	the main cause is that the denominator `1+a1+a2` has only 1.686 LSB in Q3.15 (2.000 after quantization), an inherent limit of the frozen hardware.
// ============================================================================

// dynamicBassSimpleBranchMaxX1 is the branch criterion of `DynamicBass::FilterSamples`:
// `lowFreqX <= 120` takes the simple branch (`DynamicBass.cpp:21`).
const dynamicBassSimpleBranchMaxX1 = 120.0

// dynamicBassDefaultPresetX1 is the x1 of V4A's **default device preset**.
// Source: `headset_preferences_l2.xml:274` (`android:defaultValue="100;5600;40;80;50;50"`)
// and the Java fallback in `ViPER4AndroidService.java:1826-1827` (also starting with 100).
const dynamicBassDefaultPresetX1 = 100.0

// dynamicBassBranchOf reports which branch a given x1 takes. Pure function, used for API echo and as a criterion.
//
// Return values: "simple" (`DynamicBass.cpp:21-28`) / "full" (`:29-41`).
func dynamicBassBranchOf(x1 float64) string {
	if x1 <= dynamicBassSimpleBranchMaxX1 {
		return "simple"
	}
	return "full"
}

// dynamicBassBranchNote is the one-line conclusion returned to the API/panel (including the "this is not the local DYN" reminder).
//
// ⚠️ Updated 2026-09-22: this item is **decided and implemented** (simple branch). `implemented` changed from false to true and
// `needs_owner_decision` from true to false. It still hangs off the `dyn_bass` view (rather than only living in
// the new effect's view) because "local DYN ≠ V4A DynamicBass" is exactly what it puts in front of the panel.
func dynamicBassBranchNote() map[string]any {
	return map[string]any{
		"implemented":              currentDynamicBass != nil,
		"branch_of_default_preset": dynamicBassBranchOf(dynamicBassDefaultPresetX1),
		"default_preset_x1":        dynamicBassDefaultPresetX1,
		"simple_branch_max_x1":     dynamicBassSimpleBranchMaxX1,
		"source": "the community ViPER4Android rewrite (viperfx-re): src/viper/utils/DynamicBass.cpp:21-28 (simple branch)/:29-41 (full branch); " +
			"default preset x1=100, the community ViPER4Android 4.x app: res/xml/headset_preferences_l2.xml:274",
		"decision": "**simple branch** implemented (x1 ≤ 120; the official default preset 100;5600;40;80;50;50 is this one); " +
			"see dynbass.go for the implementation, API field `dynamic_bass`; " +
			"the full branch (x1 > 120) is **not implemented and explicitly rejected**: " + dynamicBassFullBranchBlocker(),
		"needs_owner_decision": false,
		"why": "local DYN is \"side-chain detection + level-dependent slow gain\" (≈ V4A PlaybackGain), " +
			"which is not the same algorithm as V4A DynamicBass => this is **adding a stage**, not changing a branch; " +
			"the simple branch's \"same-tick L+R\" can only be done on this device as \"R pass same frame, L pass 1 sample late\" " +
			"(measured on the TB, see the engine cross-phase testbench tb_engine_xphase.v)",
		"side_channel_lag_samples": dynamicBassSideLagSmp,
		"api_field":                "dynamic_bass",
	}
}

// dynParams holds the tunable parameters on the `dynamic_bass` chain node (all dB / ms).
type dynParams struct {
	GainDB float64 `json:"gain_db"` // maximum bass boost (0..18)
	CutDB  float64 `json:"cut_db"`  // maximum cut back (0..12, default 0 = boost only)
	RefDB  float64 `json:"ref_db"`  // envelope reference (-40..0 dBFS); reduction starts above it
	KS     float64 `json:"ks"`      // slope (0.25..2.0): how many dB the gain is pulled down per dB above ref
	AttMs  float64 `json:"att_ms"`  // attack (0.5..50 ms)
	RelMs  float64 `json:"rel_ms"`  // release (20..1000 ms)
}

func dynDefaultParams() dynParams {
	// Defaults chosen to be "audible but not overdone": +6 dB boost, no cut below 25 dB, gentle reduction once the envelope is exceeded.
	// (local JamesDSP bass_maxgain=5; V4A's dynamic bass presets publish no values either => marked here as "self-calibrated".)
	return dynParams{GainDB: 6, CutDB: 0, RefDB: -25, KS: 0.75, AttMs: 5, RelMs: 200}
}

func dbToQ315(db float64) int32 {
	return int32(math.Round(math.Pow(10, db/20) * qOne))
}

// shiftFromMs converts a time constant into the RTL's **shift encoding**: τ = 2^s / fs (s takes 5 bits).
//
// ⚠️ Why not a "first-order coefficient k": in fixed point k = 1−e^(−1/(τ·fs)) can only go down to 1/512,
// which corresponds to a longest τ of only ~10 ms, while dynamic bass needs 100 ms~1 s (found by unit test).
// The shift encoding s=0..31 covers τ ≈ 0.02 ms ~ 0.7 s, and the RTL's envelope update becomes a shift instead of a multiply.
func shiftFromMs(ms float64) int32 {
	if ms <= 0 {
		return 0
	}
	tau := ms / 1000.0
	s := math.Log2(tau * sampleRate)
	v := int32(math.Round(s))
	if v < 0 {
		v = 0
	}
	if v > 31 {
		v = 31
	}
	return v
}

// dynParamsToCoefs converts the parameters into the RTL's 5 coefficients (Q3.15).
func dynParamsToCoefs(p dynParams) ([5]int32, error) {
	var c [5]int32
	if p.GainDB < 0 {
		p.GainDB = 0
	}
	// ⚠️ The ceiling is **+12 dB**, not a guess: gmax is sent as an 18-bit Q3.15 coefficient,
	// 2.0 (i.e. +6 dB)=65536 is fine, but +12 dB ≈ 130000 is already close to coefMax=131071,
	// and anything larger no longer fits the coefficient format (this is what unit test TestDynParamsToCoefs ran into).
	if p.GainDB > 12 {
		p.GainDB = 12
	}
	if p.CutDB < 0 {
		p.CutDB = 0
	}
	if p.CutDB > 12 {
		p.CutDB = 12
	}
	if p.RefDB > 0 {
		p.RefDB = 0
	}
	if p.RefDB < -60 {
		p.RefDB = -60
	}
	gmax := dbToQ315(p.GainDB)
	gmin := dbToQ315(-p.CutDB)
	ref := dbToQ315(p.RefDB)
	// ks: in the RTL, gain = gmax − ((env−ref)·ks)>>15 (both env/ref are Q3.15 normalized).
	// every 6 dB the env is above ref (×2) pulls down the Q3.15 amount that ks represents => ks is taken directly in Q3.15.
	ks := int32(math.Round(p.KS * qOne))
	for _, v := range []struct {
		n string
		v int32
	}{{"gmax", gmax}, {"gmin", gmin}, {"ref", ref}, {"ks", ks}} {
		if v.v > coefMax || v.v < coefMin {
			return c, fmt.Errorf("DYN parameter %s=%d is out of Q3.15 range", v.n, v.v)
		}
	}
	att := shiftFromMs(p.AttMs)
	rel := shiftFromMs(p.RelMs)
	c[0], c[1], c[2], c[3] = gmax, gmin, ref, ks
	c[4] = (rel << 5) | att // {rel_s[4:0], att_s[4:0]}
	return c, nil
}

// dynSideChainCoefs returns the 5 coefficients of the detector band-pass (Q3.15, using the existing RBJ band-pass design).
// The RTL side chain normalizes with >>8, so designing for 24-bit full scale here is fine.
func dynSideChainCoefs() [5]int32 {
	b0, b1, b2, a1, a2 := rbjBandPass(dynDetectFreq, dynDetectQ, sampleRate)
	return [5]int32{b0, b1, b2, a1, a2}
}

// ── Dynamic bass on the chain (DYN stage) ─────────────────────────────

var currentDynBass *dynParams

// dynBassAvailable reports whether this hardware can currently accept DYN (CAP1 bit11 self-report + 0.2 bitstream).
func dynBassAvailable() bool {
	return dspEngineGen == 1 && dspReadEngineCaps().DynAvailable()
}

// setDynBass sets (or clears, when passed nil) the dynamic bass parameters. When the hardware does not support it, it **errors out explicitly** rather than dropping the request silently.
func setDynBass(p *dynParams) error {
	if p == nil {
		currentDynBass = nil
		markStateDirty()
		return nil
	}
	if !dynBassAvailable() {
		return fmt.Errorf("this hardware has no DYN stage (CAP1 bit11 is not set); dynamic bass cannot be configured")
	}
	if _, err := dynParamsToCoefs(*p); err != nil {
		return err
	}
	q := *p
	currentDynBass = &q
	markStateDirty()
	return nil
}

// buildChainNodes compiles "biquad sections + optional DYN stage" into a slot-table node sequence.
//
// The order follows the reference implementation (chain order of the plan document §149): **dynamic bass comes before the user EQ** --
// first raise/hold back the low end by signal level, then let the following EQ shape the tone of the whole chain.
func buildChainNodes(sections [][5]int32, dyn *dynParams, cross *crossfeedParams,
	surround *surroundParams) ([]planNode, error) {
	return buildChainNodesWithBass(sections, dyn, cross, surround, currentViPERBass)
}

// buildChainNodesWithBass is the same as buildChainNodes, only with ViPERBass passed in explicitly (convenient for unit tests).
func buildChainNodesWithBass(sections [][5]int32, dyn *dynParams, cross *crossfeedParams,
	surround *surroundParams, bass *viperBassParams) ([]planNode, error) {
	nodes := make([]planNode, 0, len(sections)+5)
	// Harmonic excitation comes first (following V4A's effect order: SpectrumExtend → IIRFilter(EQ)):
	// create the high-frequency harmonics first, then let the following EQ / device correction / convolution shape them.
	// It works above 7.6 kHz while dynamic bass only handles the low-frequency envelope, so the two bands do not overlap => order does not affect the result.
	if currentExciter != nil {
		ex, err := exciterNodes(*currentExciter)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, ex...)
	}
	// Chain order follows the reference implementation (plan document §149): dynamic bass is **before** the user EQ, crossfeed is **after** it.
	// Rationale: DYN must decide the low-frequency boost from "how loud the whole thing is", so it belongs before tone shaping;
	// crossfeed is spatial processing, so the two channels should be mixed only after the tone is settled.
	if dyn != nil {
		c, err := dynParamsToCoefs(*dyn)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, planNode{Kind: planKindDyn, Coefs: c, Side: dynSideChainCoefs()})
	}
	// V4A `DynamicBass` (the bass half of `DynamicSystem`) -- **simple branch**, see dynbass.go.
	// Chain-order evidence (source-confirmed): the processing order in the community ViPER4Android rewrite (viperfx-re), src/viper/ViPER.cpp:153-156, is
	//   `… → playbackGain(AGC) → fetCompressor → dynamicSystem(DynamicBass) → viperBass → clarity → cure → analogX`
	// This device's DYN stage is the playbackGain path (side-chain detection + slow gain), so DynamicBass sits directly
	// after it; this device's EQ/convolution/DDC come after both (an existing chain-order choice, rationale in the paragraph below).
	if currentDynamicBass != nil {
		dbn, err := dynamicBassNodes(*currentDynamicBass)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, dbn...)
	}
	for _, s := range sections {
		nodes = append(nodes, planNode{Kind: planKindBiquad, Coefs: s})
	}
	// ── Chain order: follows JamesDSP's **authoritative implementation** (checked line by line 2026-09-20) ──
	// The actual order in `jdspController.c:JamesDSPProcess` is:
	//   VacuumTube → Compressor → BassBoost → MultimodalEQ → ArbEq
	//   → **[locked] convolver → DDC → liveprog [unlocked]** → Crossfeed → StereoEnhancement
	//   → Reverb → postGain → limiter (the limiter code is inlined at the end of this function)
	// This device's counterparts: exciter (≈V4A's VSE; JamesDSP has no such stage, V4A puts it first) → dyn (≈BassBoost/dbb)
	// → EQ (≈mEQ/ArbEq) → **convolution → DDC** → crossfeed → spatial (≈StereoEnh) → (limiter at the engine's end).
	//
	// ⚠️ **Correction**: this used to say "DDC comes before the convolution", and the comment claimed that was "the reference implementation's effect order" --
	//    which does not match the JamesDSP source (there the convolution comes first). Why the difference is inaudible: this entire stretch is **LTI**
	//    (biquad cascade / convolution / delay / linear cross-mix), and LTI∘LTI commutes => mathematically equivalent;
	//    the only differences are **fixed-point rounding** and whether intermediate levels hit saturation. But the point of matching a reference is structural
	//    agreement: otherwise inserting a non-linear stage in between later (e.g. moving the exciter after DDC) would really diverge.
	// Convolution: a purely per-channel stage, it does not touch the inter-channel bus.
	if currentFIR != nil {
		nodes = append(nodes, planNode{Kind: planKindFIR, Blocks: firParamsBlocks(currentFIR)})
	}

	// DDC (device correction, from an external .vdc): **after the convolution** (per the authoritative chain order above).
	// It is an ordinary biquad section and counts against the section limit as usual -- exceeding it is reported faithfully by the planner.
	nodes = append(nodes, ddcNodes()...)

	// ViPERBass (the NATURAL_BASS preset of V4A `ViPERBass`): sits directly **before** Clarity.
	//
	// Evidence (the order in which the client itself commits parameters = V4A's effect chain order):
	// `ViPER4AndroidService.java:1845` is the first thing in the "Fidelity Control" block (setting ViPERBass),
	// and only at `:1860` does it set ViPERClarity => bass is **before** Clarity and the two are adjacent.
	// This device's remaining stages (dyn/EQ/convolution/DDC) all come before it, matching the relative position in V4A.
	// Structure: scaling + low-pass + MIX2 = 2 slots, 3 sections (see viperbass.go).
	if bass != nil {
		bn, err := viperBassNodes(*bass)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, bn...)
	}

	// ViPERClarity (second half of A4): V4A's chain order is `… → dynamic bass(10) → bass(11) → Clarity(12) → Cure(13) → tube(14)`,
	// i.e. **before crossfeed**. This device puts dyn/EQ/convolution/DDC before it, matching the relative position in V4A.
	// 1 section (NATURAL's "pre-emphasis × first-order low-pass" is already merged exactly into one section; OZONE is a 8250 Hz high shelf).
	if currentClarity != nil {
		cn, err := clarityNodes(*currentClarity, sampleRate)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, cn...)
	}

	// Inter-channel processing comes **after** DDC/convolution (JamesDSP: … → DDC → liveprog → crossfeed → StereoEnh → Reverb).
	// Between the two, crossfeed still comes first and the spatial stage second: crossfeed first "pulls the two ends together",
	// then the spatial stage opens the time difference back up -- the other way round, crossfeed would flatten part of the Haas delay again.
	if cross != nil {
		lo, hi, mix, err := crossfeedCoefs(*cross)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, planNode{Kind: planKindCross, Coefs: lo, Hi: hi, Mix: mix})
		// V4A `Cure` adds one `PassFilter` stage beyond bs2b (**after** crossfeed):
		// per channel HPF(10 Hz, 1st order) → LPF(18 kHz, **3 identical first-order sections**).
		// Off by default (plain bs2b = JamesDSP's form); see cure.go and A5.
		if cross.PassFilter {
			for _, c := range curePassFilterSections(sampleRate) {
				nodes = append(nodes, planNode{Kind: planKindBiquad, Coefs: c})
			}
		}
	}
	if surround != nil {
		if n := surroundDelaySamples(*surround); n > 0 {
			nodes = append(nodes, planNode{
				Kind: planKindDelay, Len: n, Flags: flDlyRight,
			})
		}
	}
	// ColorfulMusic (V4A `ColorfulMusic` = DepthSurround + Stereo3DSurround): a **spatial** effect,
	// placed after crossfeed/spatial and before SpeakerCorrection (the V4A client sends it in the
	// "Field Surround" block of headphonefx, next to DiffSurround).
	// ⚠️ It is the chain's **only** joint-stereo stage (needs L and R in the same tick); its hardware prerequisite is CAP1 bit12;
	//    without that bit, colorfulNodes is never reached (setColorfulMusic already rejects it explicitly),
	//    but the planner still checks the capability bit once more here: better to report an error than to emit a table that cannot run correctly.
	if currentColorful != nil {
		if !colorfulAvailable() {
			return nil, fmt.Errorf("ColorfulMusic is on the chain, but this bitstream has no joint-stereo frame pass" +
				" (CAP1 bit12) -- not configuring it (silently dropping it would make the user think it is on)")
		}
		cn, err := colorfulNodes(*currentColorful)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, cn...)
	}
	// SpeakerCorrection (second half of A4): V4A's chain order is `… reverb(6) → SpeakerCorrection(7) → … tube(14)`,
	// i.e. **after reverb/spatial and before the tube stage**. This device's counterpart position is exactly "after the spatial stage, before the tube stage".
	// 3 sections: LP(13500,Q1) → HP(80,Q1) → 0.5·(1 + BP(420,Q3.88)) (the addition is already folded into the third section's numerator).
	if currentSpeakerCorrection != nil {
		nodes = append(nodes, speakerCorrectionNodes(sampleRate)...)
	}
	// Tube flavor (V4A `TubeSimulator`): V4A's order is `… → Cure → TubeSimulator → AnalogX`,
	// so it goes **after** inter-channel processing and before the limiter. One first-order section, zero hardware cost.
	if currentTube != nil {
		nodes = append(nodes, tubeNodes()...)
	}
	// AnalogX (first half of A2): in V4A's chain order it is the **last effect stage** (`… tube(14) → AnalogX(15) → limiter`),
	// so it must come **after** the tube stage. Four sections + one MIX2 slot (HP240 → POLY → MIX2(dry + wet×preset gain) → LP → Peak633).
	if currentAnalogX != nil {
		an, err := analogxNodes(*currentAnalogX, sampleRate)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, an...)
	}
	// ── Frame budget check (computed from the **hardware-reported** cost, see chainFrameCost in dsp_slot.go) ──
	// Why block at compile time: exceeding the budget shows up as **dropped samples** (heard as a "pop", STATUS[31:24] non-zero),
	// and the user only perceives "the occasional click", which is very hard to attribute. Better to report an error clearly here and say which item to turn off.
	if err := checkFrameBudget(nodes); err != nil {
		return nil, err
	}
	return nodes, nil
}

// checkFrameBudget computes the in-frame cycles from the node list and, when over budget, returns an **actionable** error (what to turn off).
func checkFrameBudget(nodes []planNode) error {
	sections, slots := 0, 0
	// slotExtra: when a node occupies **more than one slot** in the slot table, add the extra ones
	// (checkFrameBudget approximates "1 slot per node"; DynamicBass is 1 node, 4 slots).
	slotExtra := 0
	hasFIR, hasPOLY, hasSFIR, hasJoint := false, false, false, false
	for _, n := range nodes {
		switch n.Kind {
		case planKindBiquad:
			sections++
		case planKindCross:
			sections += 2 // lo + hi sections (MIX2 occupies no state section)
		case planKindDyn:
			sections += 2
		case planKindExciter:
			sections += 3 // HPF + POLY + LPF
			if n.MixB[0] != 0 {
				sections++ // the extra gain stage used when the wet weight exceeds the MIX2 range
			}
			hasPOLY = true
		case planKindViPERBass:
			sections += 3 // scaling + low-pass + MIX2
		case planKindViPERBassPBP:
			// The section count is accounted the same way as NATURAL (scaling + low-pass + MIX2 = 3), but it adds **one more
			// small FIR** (dry path) => the frame budget must add sfirCycles (19 cycles/channel, see dsp_slot.go).
			sections += 3
			hasSFIR = true
		case planKindColorfulMusic:
			// Joint section: the section count records only the **2 state words it permanently occupies** (the last two words of the left-channel section),
			// and the cycles add colorfulJointCycles (= 40; JDST 19+7 and J3DS 7+7 + slot scheduling are already counted separately).
			sections += jointStateReserve
			hasJoint = true
		case planKindFIR:
			hasFIR = true
		case planKindDynBass:
			sections += dynamicBassSections
			slotExtra += dynamicBassSlots - 1 // this node occupies 4 slots in the slot table
		}
		slots++ // node count ≈ slot count (close enough)
	}
	slots += slotExtra
	// ⚠️ The joint section actually takes **two slots**, whereas slots here counts nodes (1).
	//    The 2 cycles/slot difference of slot scheduling already includes descriptor fetch (7 cycles/slot) in colorfulJointCycles,
	//    so it is not added again here -- better to undercount by 2 cycles than to double count; colorfulJointCycles is itself a **measured** value.
	cost := chainFrameCost(sections, hasFIR, hasPOLY, hasSFIR, hasJoint, slots)
	if cost <= frameBudgetCycles {
		return nil
	}
	return fmt.Errorf("this chain does not fit the in-frame budget: %d sections%s%s ≈ %d cycles > %d cycles per channel (%s)"+
		" -- exceeding it **drops samples** (heard as occasional clicks). Please turn off a section/disable the convolution, or split the effects",
		sections, map[bool]string{true: " + convolution", false: ""}[hasFIR],
		map[bool]string{true: " + small FIR", false: ""}[hasSFIR],
		cost, frameBudgetCycles, fmt.Sprintf("%d cycles per section", costPerSectionRuntime()))
}

// dynBassView is the current dynamic bass state returned to the UI/API (nil means off).
func dynBassView() any {
	if currentDynBass == nil {
		return nil
	}
	p := *currentDynBass
	return map[string]any{
		"gain_db": p.GainDB, "cut_db": p.CutDB, "ref_db": p.RefDB,
		"ks": p.KS, "att_ms": p.AttMs, "rel_ms": p.RelMs,
		"detect_hz": dynDetectFreq, "detect_q": dynDetectQ,
		"available": dynBassAvailable(),
		// A6 item 3: expose "local DYN ≠ V4A DynamicBass" and the conclusions on V4A's two branches via the API,
		// so the panel (0.3) can label them faithfully without having to re-derive the source itself.
		"v4a_dynamic_bass": dynamicBassBranchNote(),
	}
}
