// conventions.go -- the **single source of truth** for parameter conventions (A6)
//
// Why this file exists: when V4A panel values (integers) reach the core, **the two quantity
// classes use different dB formulas**, and the source only implies this (it is scattered over the
// SetXxx methods of the various effects). We mixed them up early on, so "what exactly is panel 50
// in dB" had to be guessed. Here the two classes are made **explicitly separate**, and the parity
// data is pinned down.
//
// -- Primary sources (`AndroidAudioMods/ViPERFX_RE`; see the internal params-and-plumbing source audit) --
//
//	(1) **level class** ("this is a level/threshold/volume"): `dB = 20*log10(v/100)`
//	   · `0x10032 PARAM_HPFX_OUTPUT_VOLUME` (output gain)
//	   · `0x10034 PARAM_HPFX_LIMITER_THRESHOLD` (true-peak threshold)
//	   · `0x1001F PARAM_HPFX_AGC_VOLUME` (AGC linear ceiling)
//	   **Parity (against the `output_db` labels in arrays.xml)**: 100<->0, 90<->-0.92, 80<->-1.94,
//	   70<->-3.10, 50<->-6.02, 30<->-10.46, 10<->-20, 5<->-26.02, 1<->-40 -- all match.
//
//	(2) **gain class** ("this is a gain/multiplier"): `dB = 20*log10(1 + v/100)`
//	   · `0x10029 PARAM_HPFX_VIPERBASS_BASSGAIN` (bass gain, `/100` -> 0.5...6.0)
//	   · `0x1002C PARAM_HPFX_VIPERCLARITY_CLARITY` (clarity, `/100` -> 0...4.5)
//	   **Parity**: 50 -> 1.5 = **+3.52 dB** (the 8250 Hz shelf of Clarity OZONE is exactly this).
//
// ⚠️ Using the wrong class is not "slightly off": for the same panel value 50 the level class
//    gives -6.02 dB and the gain class gives +3.52 dB, a 9.5 dB difference -- audibly "why is this
//    effect so weak / so aggressive", and very hard to attribute.
//
// -- Two further conventions that **must pick a side explicitly** (not implemented in this file,
//    but the conclusion is recorded here) --
//   · V4A `DynamicBass`: `lowFreqX <= 120` takes the **simple branch** (it only adds one 55 Hz
//     lowpass back), and only `> 120` uses the full 6-tap pole filter; **the default preset
//     x1=100 lands exactly on the simple branch**.
//     This device's `dyn` is the DYN stage ("bass follows loudness") and is **not the same
//     algorithm** => the chain-order document already marks it "not equivalent".
//   · V4A `Cure`/crossfeed three tiers: Slight(0)/Moderate(1)/Extreme(2), default 0;
//     this device follows bs2b's `fcut/feed` convention (see crossfeed.go).

package main

import "math"

// gainClassDB is the **gain class** convention: panel value v (0...100+) -> `20*log10(1 + v/100)`.
// Used for "gain multipliers" such as the ViPERBass bass gain and the ViPERClarity clarity level.
func gainClassDB(v float64) float64 {
	return 20 * math.Log10(1+v/100.0)
}

// levelClassDB is the **level class** convention: panel value v (0...100) -> `20*log10(v/100)`.
// Used for "levels/thresholds" such as the output volume, the limiter threshold and the AGC ceiling.
// For v<=0 it returns -Inf (as the math says); callers should range-check first and must not feed -Inf into the chain.
func levelClassDB(v float64) float64 {
	if v <= 0 {
		return math.Inf(-1)
	}
	return 20 * math.Log10(v/100.0)
}

// gainClassLinear is the **linear** form of the gain class (`1 + v/100`) -- used when a filter coefficient has to be fed.
func gainClassLinear(v float64) float64 { return 1 + v/100.0 }

// levelClassLinear is the linear form of the level class (`v/100`) -- the linear value of a limiter threshold or output volume.
func levelClassLinear(v float64) float64 { return v / 100.0 }

// ============================================================================
// A6 item 2: the registry of **every** "panel value -> dB" conversion point (2026-09-22)
// ============================================================================
//
// Why it exists: the two formulas themselves (gainClassDB / levelClassDB) have a single source,
// but "which one a given effect should use" is **scattered** -- each effect does its own thing
// and anyone can silently use the wrong class (9.5 dB off). This table registers every conversion
// point together with its **criterion and conclusion**; a test (`a6_parity_test.go`) checks line
// by line that "the registered number" equals "the number the implementation computes", so a
// missed registration is reported by that test.
//
// Table columns:
//
//	Effect  -- short name of the effect/parameter (also the key on the implementation side)
//	Panel   -- the panel value used for the parity check
//	Class   -- level / gain / linearByConstruction / notApplicable
//	WantDB  -- the dB this panel value **actually** lands on (level/gain via the formula; linear via the in-core multiplier)
//	Wet     -- linearByConstruction only: the in-core linear multiplier (WantDB = 20log10(Wet))
//	Caller  -- where the backend implements it (`file:symbol`); "not implemented" means this device has no such stage
//	Source  -- primary source (`file:line`)
//	Verdict -- the call: change / do not change, and why
type dbClass string

const (
	// dbClassLevel: the panel value is a level/threshold => dB = 20*log10(v/100)
	dbClassLevel dbClass = "level"
	// dbClassGain: the panel value is "the percentage of a gain" => dB = 20*log10(1 + v/100)
	dbClassGain dbClass = "gain"
	// dbClassLinear: the panel value **is itself a linear multiplier** (multiplied directly in the
	// core) => neither formula applies; its dB is merely the **conversion result** of
	// 20*log10(multiplier), not a "panel value -> dB" formula.
	dbClassLinear dbClass = "linearByConstruction"
	// dbClassNA: the panel value is not a gain/level at all (e.g. it is just a tier index) => not applicable.
	dbClassNA dbClass = "notApplicable"
)

type panelDBConvention struct {
	Effect  string
	Param   string
	Panel   float64
	Class   dbClass
	WantDB  float64
	Wet     float64 // used by dbClassLinear only
	Caller  string
	Source  string
	Verdict string
}

// panelDBConventions is the **conclusion table** for A6 item 2 (8 rows, covering every place that
// appears in the backend).
//
// The conclusion in one sentence: **every real "panel value -> dB" conversion in the backend goes
// through those two formulas** (`clarity.go`'s OZONE uses the gain class; the level-class V4A
// parameters are not implemented on this device, because our interface takes dB/percentages
// directly and ALSA volume follows the codec's own dB grid). **No deviation was found.**
// Three further places look like "panel value -> dB" but are actually **in-core linear
// multipliers** (VSE exciter / ViPERBass wet signal / Clarity NATURAL pre-emphasis), plus one
// **tier index** (AnalogX) -- they do not belong to those two formulas, and each has its own
// verdict and reason for not changing.
var panelDBConventions = []panelDBConvention{
	{
		Effect: "output_volume", Param: "0x10032 PARAM_HPFX_OUTPUT_VOLUME (panel key viper4android.headphonefx.outvol)",
		Panel: 50, Class: dbClassLevel, WantDB: levelClassDB(50), // = -6.02 dB
		Caller: "not implemented (this device's volume follows the ALSA codec's own dB grid, see pctToDB in volume.go)",
		Source: "the community ViPER4Android rewrite (viperfx-re): src/viper/ViPER.cpp:414-416 (frameScale = val1/100); " +
			"panel entries and defaults in the original ViPER4Android app (viper4android_fx): res/xml/headset_preferences_l2.xml:403-410; " +
			"the shipping line ViPER4AndroidService.java:1905-1906; the dB labels are the output_db array in arrays.xml",
		Verdict: "do not change. V4A's panel integers 0...100 do not exist on this device (this device's volume is its " +
			"own 1...100 slider plus the codec's 1 dB grid, calibrated against measured dB in volume.go). It is " +
			"registered here so that the convention is documented, not in order to switch to V4A's slider",
	},
	{
		Effect: "limiter_threshold", Param: "0x10034 PARAM_HPFX_LIMITER_THRESHOLD (panel key viper4android.headphonefx.limiter)",
		Panel: 50, Class: dbClassLevel, WantDB: levelClassDB(50), // = -6.02 dB
		Caller: "not implemented (this device's /api/dsp/limiter takes dB directly: dspLimiterThrDB, see dsp.go)",
		Source: "panel entries = the output_db array in arrays.xml (the labels are dB already), default 100, " +
			"the original ViPER4Android app (viper4android_fx): res/xml/headset_preferences_l2.xml:421-428; " +
			"the shipping line ViPER4AndroidService.java:1909-1910",
		Verdict: "do not change. This device's interface is in dB to begin with (-30...0), with no percentage step; " +
			"if the panel ever exposes V4A-style 0...100 steps it must use levelClassDB (the output_db labels " +
			"100<->0 / 90<->-0.92 / 50<->-6.02 have been checked point by point)",
	},
	{
		Effect: "viperbass_natural", Param: "0x10029 PARAM_HPFX_VIPERBASS_BASSGAIN（vbass_boost 50…600）",
		Panel: 50, Class: dbClassGain, WantDB: gainClassDB(50), // = +3.52 dB
		Caller: "viperbass.go:196 / :266 (wet := p.Gain/100, one per branch)",
		Source: "the community ViPER4Android rewrite (viperfx-re): src/viper/ViPER.cpp:351 (SetBassFactor(val1/100)); " +
			"src/viper/effects/ViPERBass.cpp:42-43 (out = x + LP(x)*bassFactor)",
		Verdict: "do not change (**already gain class**). The signal flow is `x + (g/100)*LP(x)`, a mono low " +
			"band => 1 + g/100 = 1.5 => +3.52 dB, bit-for-bit identical to gainClassDB(50). " +
			"The implementation writes the **wet coefficient** g/100 (not 1+g/100), which is correct: writing " +
			"gainClassLinear(50) would multiply the dry signal as well (3.5 dB extra)",
	},
	{
		Effect: "clarity_ozone", Param: "0x1002C PARAM_HPFX_VIPERCLARITY_CLARITY (panel 0...100)",
		Panel: 50, Class: dbClassGain, WantDB: gainClassDB(50), // = +3.52 dB
		Caller: "clarity.go:96 (gainClassDB inside highShelfOzone)",
		Source: "the community ViPER4Android rewrite (viperfx-re): src/viper/effects/ViPERClarity.cpp:63-67 " +
			"（SetGain(clarityGainPercent + 1.0)）; " +
			"src/viper/utils/HighShelf.cpp:17-19 (gain_dB = 20*log10(gain)); " +
			"units in src/viper/ViPER.cpp:362-364 (SetClarity(val1/100))",
		Verdict: "do not change (**already gain class**, bit-for-bit identical to the source). The source " +
			"convention is 20*log10(g+1) with g = Level/100, which is exactly gainClassDB(Level). " +
			"The implementation took a detour through `gainClassDB((gainLinear-1)*100)` (turning gainLinear " +
			"back into a panel value before applying the formula); it is mathematically equivalent but hard " +
			"to read, so it now writes the source's own line 20*log10(gainLinear) directly, pinned by a test",
	},
	{
		Effect: "clarity_natural", Param: "NoiseSharpening pre-emphasis coefficient (the same 0x1002C panel value)",
		Panel: 50, Class: dbClassLinear, WantDB: 20 * math.Log10(0.5), Wet: 0.5, // = -6.02 dB (linear 0.5)
		Caller: "clarity.go:120-127 (g = Level/100 in noiseSharpeningBiquad)",
		Source: "the community ViPER4Android rewrite (viperfx-re): src/viper/effects/ViPERClarity.cpp:64 " +
			"(SetGain(clarityGainPercent)); src/viper/utils/NoiseSharpening.cpp:45-47 (this->gain = gain, linear); " +
			":18-19（x + g·(x − x₁)）",
		Verdict: "do not change. Here g **is the linear coefficient** (NoiseSharpening::SetGain applies no dB " +
			"transform at all), so panel value 50 => g=0.5 => a maximum pre-emphasis boost of 20log10(1+2g)=+6.02 dB " +
			"(at z=-1). Neither dB formula applies here -- forcing the gain class would give +3.52 dB (**2.5 dB off**)",
	},
	{
		Effect: "vse_exciter", Param: "0x1000E PARAM_SPECTRUM_EXTENSION_BARK_RECONSTRUCT (panel gear 0.1...1.0)",
		Panel: 0.1, Class: dbClassLinear, WantDB: 20 * math.Log10(0.56), Wet: 0.56, // = -5.03 dB (linear 0.56)
		Caller: "vse.go:95-103 (vseExciterParams => Mix = reconstruct/100); harmonic.go:118 (factory 0.56)",
		Source: "the original ViPER4Android app (viper4android_fx, android_4.x-5.x): src/com/vipercn/viper4android_v2/service/" +
			"ViPER4AndroidService.java:1708-1710（nSEValue = round(UI×5.6×100)）; " +
			"the community ViPER4Android rewrite (viperfx-re): src/viper/ViPER.cpp:242-245 (SetExciter(val1/100)); " +
			"src/viper/effects/SpectrumExtend.cpp:33,38 (tmp * this->exciter)",
		Verdict: "do not change. In the core it is a **linear multiplication**: panel 0.1 => integer 56 => " +
			"0.56 in the core (a -5.03 dB wet signal). Neither dB formula applies (the gain class would give " +
			"+3.52 dB, 8.5 dB off). ⚠️ The top panel gear 1.0 => 5.6, which exceeds this device's MIX2 Q3.15 " +
			"coefficient limit of 4.0, so this device folds it into range by scaling the whole polynomial plus " +
			"one gain stage (harmonic.go:56-89) and **reports an explicit error** (validateExciterMix), never truncating",
	},
	{
		Effect: "analogx_model", Param: "0x10031 PARAM_ANALOGX_MODE (tier index 0/1/2)",
		Panel: 0, Class: dbClassNA, WantDB: 20 * math.Log10(0.6), Wet: 0.6, // 0.6 is a constant hard-selected per tier in the core
		Caller: "analogx.go:38-45（analogxTiers）",
		Source: "the community ViPER4Android rewrite (viperfx-re): src/viper/ViPER.cpp:410-412 " +
			"(SetProcessingModel(val1), **index only**); src/viper/effects/AnalogX.cpp:62-93 (gain 0.6/1.2/2.4 per tier)",
		Verdict: "do not change. This panel item **sends only a tier index**; no percentage passes through it; " +
			"gain 0.6/1.2/2.4 are constants hard-selected per tier in the core => neither dB formula applies. " +
			"(The Wet column just carries model 0's 0.6 as a reference value for WantDB)",
	},
	{
		Effect: "agc_playback_gain", Param: "0x1001F PARAM_HPFX_AGC_VOLUME (AGC linear ceiling)",
		Panel: 50, Class: dbClassLevel, WantDB: levelClassDB(50), // = -6.02 dB
		Caller: "not implemented (upstream V4A's write-back loop body in `PlaybackGain::Process` is empty)",
		Source: "the community ViPER4Android rewrite (viperfx-re): " +
			"src/viper/utils/../effects/PlaybackGain.cpp:74-77 (`for (...) { }` empty body); units in this file's head, :12",
		Verdict: "not done (settled). Upstream is an empty shell => there is no algorithm to copy; it is registered " +
			"here only to pin down that it belongs to the level class, so that it is not re-discussed later",
	},
}

// panelDBLog10Files lists the backend source files that are **allowed** to contain `math.Log10`
// (the guard for A6 item 2).
//
// Why per file rather than per line: line numbers drift, whereas "why does this file need log10"
// is a stable question. When adding a file that computes dB -- first work out whether it is
// "panel value -> dB" (then it belongs in panelDBConventions) or some other quantity
// (response/headroom/IR gain), then add it here with a stated reason.
var panelDBLog10Files = map[string]string{
	"conventions.go":  "the definitions of the two formulas themselves (gainClassDB / levelClassDB)",
	"clarity.go":      "OZONE's gain-class conversion (registered as clarity_ozone) + NATURAL's **displayed** upper bound 20log10(1+2g)",
	"crossfeed.go":    "the 20log10(Ghi) in bs2b's Fchi formula -- not a panel-value conversion but bs2b's own definition",
	"dsp.go":          "the default limiter threshold (back-computed from a Q1.15 constant) and preamp (back-computed from a linear gain) -- both are **internal** quantities",
	"dsp_headroom.go": "the magnitude of the complex response (headroom computation), not a panel value",
	"dsp_slot.go":     "the IR gain report (Q3.15 -> dB), not a panel value",
	"response.go":     "the response curve (complex magnitude -> dB), not a panel value",
	"vdc.go":          "the DDC response (complex magnitude -> dB), not a panel value",
}
