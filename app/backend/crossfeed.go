package main

import (
	"fmt"
	"math"
)

// ============================================================================
// crossfeed (headphone sound field): a bs2b replication
// ============================================================================
// Source (first-hand): bs2b 3.1.0 `src/bs2b.c`, `init()` + `cross_feed_d()`;
// parameters taken from the reference local JamesDSP configuration
// `crossfeed_bs2b_fcut=700` / `crossfeed_bs2b_feed=60` (= bs2b's Light tier).
// The full derivation and the local mapping are in the internal effects
// reference replication notes, §8.
//
// **Key fact**: bs2b's crossfeed has **no delay line**. Its "delay" is only an equivalent
// phase delay reported outwards (`bs2b_level_delay(fcut)`). The body is just two first-order
// filters per channel plus one cross addition:
//
//	lo[n] = a0_lo·x[n] + b1_lo·lo[n−1]
//	hi[n] = a0_hi·x[n] + a1_hi·x[n−1] + b1_hi·hi[n−1]
//	out_L = gain·(hi_L + lo_R) ;  out_R = gain·(hi_R + lo_L)
//
// Local mapping (all existing opcodes; no new numerical algorithm was added for it):
//   · lo section = one BIQUAD section degenerated to first order: {b0:a0_lo, b1:0, b2:0, a1:−b1_lo, a2:0}
//   · hi section = one BIQUAD section:                      {b0:a0_hi, b1:a1_hi, b2:0, a1:−b1_hi, a2:0}
//   · cross      = one MIX2 slot: out = c0·in_a(this channel's hi) + c1·in_b(the other channel's lo register)
//
// The engine's BIQUAD convention is `y = b0·x + b1·x1 + b2·x2 − a1·y1 − a2·y2` (see the
// `m_sub_n` branch of dsp_engine.v and the backend's rbj* family), so to obtain
// `+b1_lo·lo[n−1]` you must write a1 = −b1_lo.
// ============================================================================

// crossfeedParams holds the two user-facing parameters (matching bs2b's level encoding).
type crossfeedParams struct {
	FcutHz float64 `json:"fcut_hz"` // low-pass cutoff frequency of the crossed-over signal (bs2b: BS2B_MINFCUT..MAXFCUT)
	Feed   float64 `json:"feed"`    // cross amount in units of 0.1 dB (bs2b: 60 = 6.0 dB; smaller means more cross)
	// PassFilter: whether to add the tail stage of V4A `Cure` (per channel HPF 10 Hz → LPF 18 kHz ×3 identical first-order sections).
	// **Off by default** = pure bs2b (that is what JamesDSP's crossfeed does, and this unit's chain order follows JamesDSP).
	// Only when true is it the complete form of V4A `Cure` (see cure.go).
	PassFilter bool `json:"pass_filter,omitempty"`
}

// crossfeedTiers is V4A `Cure`'s **three tiers**. Purpose: the UI picks a tier by name
// instead of making the user type fcut/feed.
//
//	tier 0 Slight   : 650 Hz / 95
//	tier 1 Moderate : 700 Hz / 60   ← **this tier is the local default**
//	tier 2 Extreme  : 700 Hz / 45
//
// First-hand sources (the tier values themselves, confirmed in ① the source):
//
//	the community ViPER4Android rewrite (viperfx-re): src/viper/ViPER.cpp:370-401
//	    the switch on `case PARAM_CURE_CROSS_FEED_STRENGTH:` —
//	    `val1 == 0 ⇒ {cutoff=650, feedback=95}` (:372-380)
//	    `val1 == 1 ⇒ {cutoff=700, feedback=60}` (:381-389)
//	    `val1 == 2 ⇒ {cutoff=700, feedback=45}` (:390-398)
//	    each branch still carries the original binary's constant comments `0x5f028a / 0x3c02bc / 0x2d02bc`.
//	Tier names (official client panel, one-to-one with the table above):
//	    the decompiled ViPER4Android 4.x client, res/values/arrays.xml:256-265
//	        cure_crossfeed = Slight/Moderate/Extreme, cure_crossfeed_values = 0/1/2
//	    the decompiled ViPER4Android 4.x client, res/values-zh-rCN/arrays.xml:98-102
//	        cure_crossfeed = the same three tier names, localised to Chinese
//	Plus an independent cross-check from a second source unrelated to V4A (the JamesDSP side):
//	    JamesDSP for Linux, resources/assets/default.conf:16-17
//	        `crossfeed_bs2b_fcut=700` / `crossfeed_bs2b_feed=60` ← exactly the Moderate tier above
//	    JamesDSP for Linux, src/MainWindow.cpp:975-982
//	        BS2B weak ⇒ `setValueA(700)` / `setValueA(60)`; BS2B strong ⇒ `(650)` / `(95)`
//	    JamesDSP for Linux, src/audio/base/DspHost.cpp:474-475: the `fcut | (feed<<16)` packing
//	        is **bit-for-bit identical** to V4A's `0x3c02bc` (= 60<<16 | 700) ⇒ the same bs2b convention.
//
// ⚠️ Convention difference (must be stated): **V4A's own Cure default tier is 0 (Slight, 650/95)** —
//
//	the decompiled ViPER4Android 4.x client, res/xml/headset_preferences_l2.xml:374
//	    `android:defaultValue="0"` (paired with `cure_crossfeed_values`'s 0/1/2)
//	the decompiled ViPER4Android 4.x client, src/com/vipercn/viper4android_v2/service/ViPER4AndroidService.java:1874-1875
//	    `preferences.getString("viper4android.headphonefx.cure.crossfeed", "0")`
//	whereas **this unit's default is tier 1 Moderate (700/60)** — this is **intentional**: the
//	local chain order and defaults follow the reference installed JamesDSP configuration
//	(those two lines of `default.conf`), and A6's convention is also "default = Moderate".
//	For V4A's default, just pick tier 0 (`/api/dsp/chain {"crossfeed":{"fcut_hz":650,"feed":95}}`).
var crossfeedTiers = []struct {
	Name   string  `json:"name"`
	Cn     string  `json:"cn"`
	FcutHz float64 `json:"fcut_hz"`
	Feed   float64 `json:"feed"`
}{
	{"Slight", "Slight", 650, 95},
	{"Moderate", "Moderate", 700, 60},
	{"Extreme", "Extreme", 700, 45},
}

// crossfeedTierIndexModerate is the index of the local default tier inside crossfeedTiers.
//
// It is a separate constant so that "default = Moderate" lives in exactly one place in the
// code (both crossfeedDefaultParams and the parity test reference it) instead of being
// written twice as 700/60 or twice as 1.
const crossfeedTierIndexModerate = 1

// crossfeedTierOf reverse-looks-up which tier a given (fcut, feed) hits (returns −1 if none).
func crossfeedTierOf(p crossfeedParams) int {
	for i, t := range crossfeedTiers {
		if t.FcutHz == p.FcutHz && t.Feed == p.Feed {
			return i
		}
	}
	return -1
}

// crossfeedDefaultParams uses **tier 1 Moderate** (fcut=700 Hz, feed=60): these two are
// **first-hand numbers** (JamesDSP for Linux, resources/assets/default.conf:16-17:
// `crossfeed_bs2b_fcut=700` / `crossfeed_bs2b_feed=60`), and they are bit-for-bit identical
// to V4A's Moderate tier (the community ViPER4Android rewrite, src/viper/ViPER.cpp:381-389).
//
// Why not V4A's own default tier 0 (Slight, 650/95): see the "convention difference" note
// above crossfeedTiers — the local default follows the reference installed JamesDSP
// configuration, and A6's convention is also "default = Moderate".
func crossfeedDefaultParams() crossfeedParams {
	t := crossfeedTiers[crossfeedTierIndexModerate]
	return crossfeedParams{FcutHz: t.FcutHz, Feed: t.Feed}
}

// bs2b's legal ranges (bs2b.h): values below/above are clamped back to the default tier by the library itself.
const (
	bs2bMinFcut = 300.0
	bs2bMaxFcut = 2000.0
	bs2bMinFeed = 10.0
	bs2bMaxFeed = 150.0
)

// crossfeedCoefs turns (fcut, feed) into three things: the lo section, the hi section, and MIX2's two weights.
//
// It follows bs2b's init() line by line, converting only the results into the engine's Q3.15 fixed point.
func crossfeedCoefs(p crossfeedParams) (lo [5]int32, hi [5]int32, mix [2]int32, err error) {
	fcut := p.FcutHz
	if fcut < bs2bMinFcut {
		fcut = bs2bMinFcut
	}
	if fcut > bs2bMaxFcut {
		fcut = bs2bMaxFcut
	}
	feed := p.Feed
	if feed < bs2bMinFeed {
		feed = bs2bMinFeed
	}
	if feed > bs2bMaxFeed {
		feed = bs2bMaxFeed
	}

	level := feed / 10.0

	GBlo := level*-5.0/6.0 - 3.0 // gain of the low-pass branch that is crossed over (dB)
	GBhi := level/6.0 - 3.0      // gain of this channel's high-pass branch (dB)

	Glo := math.Pow(10, GBlo/20.0)
	Ghi := 1.0 - math.Pow(10, GBhi/20.0)

	// the high-pass branch's cutoff is derived from "the two branches must meet at low frequencies" (the same line in bs2b's source)
	Fchi := fcut * math.Pow(2.0, (GBlo-20.0*math.Log10(Ghi))/12.0)

	xLo := math.Exp(-2.0 * math.Pi * fcut / sampleRate)
	a0Lo := Glo * (1.0 - xLo)

	xHi := math.Exp(-2.0 * math.Pi * Fchi / sampleRate)
	a0Hi := 1.0 - Ghi*(1.0-xHi)
	a1Hi := -xHi

	gain := 1.0 / (1.0 - Ghi + Glo) // mono content must still come out at 1.0 through it (bs2b's normalisation)

	// lo[n] = a0_lo·x + b1_lo·lo[n−1]  ⇒  a1(engine) = −b1_lo
	lo = [5]int32{floatToQ315(a0Lo), 0, 0, floatToQ315(-xLo), 0}
	// hi[n] = a0_hi·x + a1_hi·x[n−1] + b1_hi·hi[n−1]  ⇒  b1 = a1_hi, a1(engine) = −b1_hi
	hi = [5]int32{floatToQ315(a0Hi), floatToQ315(a1Hi), 0, floatToQ315(-xHi), 0}
	mix = [2]int32{floatToQ315(gain), floatToQ315(gain)}

	for _, c := range []struct {
		name string
		v    int32
	}{
		{"lo.b0", lo[0]}, {"lo.a1", lo[3]},
		{"hi.b0", hi[0]}, {"hi.b1", hi[1]}, {"hi.a1", hi[3]},
		{"mix", mix[0]},
	} {
		if _, err := checkCoef(c.name, c.v); err != nil {
			return lo, hi, mix, err
		}
	}
	return lo, hi, mix, nil
}

// crossfeedEffectiveFcut returns the frequency that will actually be used (after clamping), for the UI to display the "effective value".
func crossfeedEffectiveFcut(p crossfeedParams) float64 {
	f := p.FcutHz
	if f < bs2bMinFcut {
		f = bs2bMinFcut
	}
	if f > bs2bMaxFcut {
		f = bs2bMaxFcut
	}
	return f
}

// ── runtime state ─────────────────────────────────────────────────────

// currentCrossfeed non-nil = crossfeed is on (same convention as currentDynBass).
var currentCrossfeed *crossfeedParams

// crossfeedAvailable: does this bitstream have MIX2 (crossfeed's hardware prerequisite, CAP1 bit3)?
func crossfeedAvailable() bool {
	return dspEngineGen == 1 && dspReadEngineCaps().Mix2Available()
}

// setCrossfeed sets it (nil = off). When the hardware has no MIX2 it reports an explicit error instead of silently dropping it.
func setCrossfeed(p *crossfeedParams) error {
	if p == nil {
		currentCrossfeed = nil
		markStateDirty()
		return nil
	}
	if !crossfeedAvailable() {
		return fmt.Errorf("this hardware has no MIX2 slot (CAP1 bit3 not set); crossfeed cannot be downloaded")
	}
	// if the coefficients cannot be computed (out of range), do not record this setting —
	// otherwise the UI would show it as on while nothing was actually downloaded
	if _, _, _, err := crossfeedCoefs(*p); err != nil {
		return err
	}
	q := *p
	currentCrossfeed = &q
	markStateDirty()
	return nil
}

// crossfeedView is for the API/panel (nil = off).
func crossfeedView() any {
	if currentCrossfeed == nil {
		return nil
	}
	p := *currentCrossfeed
	return map[string]any{
		"fcut_hz": p.FcutHz, "feed": p.Feed,
		"tier":              crossfeedTierOf(p),
		"effective_fcut_hz": crossfeedEffectiveFcut(p),
		"available":         crossfeedAvailable(),
		"note":              "bs2b replication: the lower fcut, the less low frequency is crossed over; the larger feed, the lighter the cross (units of 0.1 dB). Mono content passes through it without a level change",
	}
}

// crossfeedTiersView is the **three-tier table** returned to the UI/API (the tier names plus which tier is the local default).
//
// Why it lives in the API: the first A6 item to land was "tier names matching the values one-to-one
// + default tier = Moderate". The panel (0.3) builds its dropdown from this and does not have to
// copy numbers such as 650/95 itself (a second copy means changing one place and forgetting the
// other — this repository has been bitten by that repeatedly).
func crossfeedTiersView() map[string]any {
	out := make([]map[string]any, 0, len(crossfeedTiers))
	for i, t := range crossfeedTiers {
		out = append(out, map[string]any{
			"tier":    i,
			"name_en": t.Name,
			"name_cn": t.Cn,
			"fcut_hz": t.FcutHz,
			"feed":    t.Feed,
			"default": i == crossfeedTierIndexModerate, // local default = Moderate
		})
	}
	return map[string]any{
		"tiers":        out,
		"default_tier": crossfeedTierIndexModerate,
		"v4a_default":  0, // ⚠️ V4A's own Cure default is tier 0 Slight (see the note on crossfeedTiers)
		"source":       "the community ViPER4Android rewrite (viperfx-re): src/viper/ViPER.cpp:370-401 (the three tier values)",
		"source_names": "the decompiled ViPER4Android 4.x client: res/values/arrays.xml:256-265 + res/values-zh-rCN/arrays.xml:98-102",
		"crosscheck":   "JamesDSP for Linux: resources/assets/default.conf:16-17 (700/60 cross-checked bit for bit)",
		"default_why":  "the local default follows the reference installed JamesDSP configuration (700/60 = V4A Moderate), not V4A's tier 0",
	}
}
