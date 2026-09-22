// SPDX-License-Identifier: GPL-2.0-only
// loudness.go -- loudness compensation (equal loudness / Fletcher-Munson): a software
// stage that **follows the volume**
//
// Why it is required: a static EQ + preamp cannot fix "it sounds weak at low volume".
// That is the equal-loudness problem -- the lower the volume, the less sensitive the ear
// is to low frequencies (and to high ones) (ISO 226 equal-loudness contours).
// Established implementations all do this: Audyssey Dynamic EQ, V4A's Equal Loudness,
// JamesDSP's Loudness. What they have in common: **the amount of compensation varies with
// volume**, the lower the volume the more is added.
//
// We have one convenience others do not: this board's volume (the codec's ALSA Master) is
// written by software, so "the current volume" is readable at any time => the amount of
// compensation is computed straight from it, **with no new hardware needed**.
//
// Design (following the shape of Audyssey Dynamic EQ, with our own coefficients):
//
//	compensation L(dB) = clamp( (REF - current volume dB) * k * strength, 0, MAX )
//
//	- no compensation while the current volume is above REF (listening loud does not lack
//	  bass anyway);
//	- curve shape: a broad low-frequency lift (three points at 60 / 150 / 400, imitating a
//	  low shelf) plus a little air (10k);
//	- the boost it produces still goes into the automatic Preamp's account (anti-clipping),
//	  so it is "relatively" fuller, not louder overall.
//
// WARNING: unlike an "overall gain" knob, what is boosted here is the **frequency response
// shape**, and it automatically expands and contracts with the volume;
//
//	it does not cancel the preamp out (that would clip), nor does it crush the dynamics the
//	way a limiter does.
package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
)

const (
	// loudnessRefDB is the **reference level** (0 dBFS = digital full scale).
	// It replicates Audyssey Dynamic EQ's model: it fixes the "cinema reference level" at
	// 0 dB and then applies a **Reference Level Offset** (0/5/10/15 dB, chosen by content:
	// movie 0 / classical 5 / jazz and TV 10 / pop and rock 15, see the Audyssey section of
	// Denon's official manual). The larger the offset = the louder that content was mixed,
	// the lower the point at which "turning down from reference starts adding compensation".
	loudnessRefDB = 0.0
	// loudnessPerDB: how many dB of low-frequency compensation per 1 dB the volume drops
	// (at 100% strength).
	loudnessPerDB = 0.35
	// loudnessMaxBoostDB: the most that is added at 60 Hz.
	loudnessMaxBoostDB = 10.0
	// loudnessTrebleFrac: the treble compensation as a fraction of the bass one (the
	// equal-loudness curve tilts up at both ends).
	loudnessTrebleFrac = 0.30
)

var (
	loudnessOn       = true
	loudnessStrength = 0.5
	// loudnessRefOffsetDB = Audyssey's Reference Level Offset: chosen by content.
	// Default 10 dB (jazz / general music; per the official recommendation pop and rock
	// 15, classical 5, movie 0).
	loudnessRefOffsetDB = 10.0
)

// loudnessAmount returns the dB to add at 60 Hz (0 means none).
func loudnessAmount() float64 {
	if !loudnessOn {
		return 0
	}
	db := systemVolumeDB(volumePct)
	need := (loudnessRefDB - loudnessRefOffsetDB) - db
	if need <= 0 {
		return 0
	}
	v := need * loudnessPerDB * loudnessStrength
	if v > loudnessMaxBoostDB {
		v = loudnessMaxBoostDB
	}
	if v < 0 {
		v = 0
	}
	return v
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

// loudnessCurve is the current compensation curve (a low-shelf shape plus a little air).
// It returns empty when L is too small.
func loudnessCurve() []ChainItem {
	l := loudnessAmount()
	if l < 0.25 {
		return nil
	}
	return []ChainItem{
		{Type: "peq", Enabled: true, Freq: 60, GainDB: round1(l), Q: 0.7},
		{Type: "peq", Enabled: true, Freq: 150, GainDB: round1(l * 0.75), Q: 0.8},
		{Type: "peq", Enabled: true, Freq: 400, GainDB: round1(l * 0.30), Q: 0.8},
		{Type: "peq", Enabled: true, Freq: 10000, GainDB: round1(l * loudnessTrebleFrac), Q: 0.8},
	}
}

// withLoudness merges the compensation into the chain: **added at the same frequency
// point** (otherwise it would add an extra section, and the user's own adjustment and the
// automatic compensation would fight over two filters).
func withLoudness(chain []ChainItem) []ChainItem {
	curve := loudnessCurve()
	if len(curve) == 0 {
		return chain
	}
	out := make([]ChainItem, 0, len(chain)+len(curve))
	out = append(out, chain...)
	for _, c := range curve {
		merged := false
		for i := range out {
			if math.Abs(out[i].Freq-c.Freq) < 1 {
				out[i].GainDB = round1(out[i].GainDB + c.GainDB)
				merged = true
				break
			}
		}
		if !merged {
			out = append(out, c)
		}
	}
	return out
}

// ── HTTP ─────────────────────────────────────────────────────────────

// handleDSPLoudness: the on/off switch and the strength (the panel's "overall" section).
func handleDSPLoudness(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, loudnessState())
	case http.MethodPost:
		var body struct {
			Enabled  *bool    `json:"enabled"`
			Strength *float64 `json:"strength"`
			// OffsetDB = the Audyssey-style reference level offset (chosen by content:
			// movie 0 / classical 5 / jazz and TV 10 / pop and rock 15). It declares "how
			// loud this content was mixed", not a strength.
			OffsetDB *float64 `json:"offset_db"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
			httpError(w, http.StatusBadRequest, "request body is not valid JSON: "+err.Error())
			return
		}
		if body.Enabled != nil {
			loudnessOn = *body.Enabled
		}
		if body.OffsetDB != nil {
			v := *body.OffsetDB
			if v < 0 {
				v = 0
			}
			if v > 15 {
				v = 15
			}
			loudnessRefOffsetDB = v
		}
		if body.Strength != nil {
			s := *body.Strength
			if s < 0 {
				s = 0
			}
			if s > 1 {
				s = 1
			}
			loudnessStrength = s
		}
		// recompute the whole chain immediately with the new settings
		if err := reapplyCurrentChain(); err != nil {
			httpError(w, http.StatusInternalServerError, "re-apply failed: "+err.Error())
			return
		}
		writeJSON(w, loudnessState())
	default:
		httpError(w, http.StatusMethodNotAllowed, "only GET / POST is supported")
	}
}

func loudnessState() map[string]any {
	return map[string]any{
		"enabled":    loudnessOn,
		"strength":   loudnessStrength,
		"amount_db":  round1(loudnessAmount()), // how much is currently added at 60 Hz
		"volume_db":  round1(systemVolumeDB(volumePct)),
		"ref_db":     loudnessRefDB - loudnessRefOffsetDB,
		"ref_offset": loudnessRefOffsetDB,
		"max_boost":  loudnessMaxBoostDB,
		"per_db":     loudnessPerDB,
		"curve":      loudnessCurve(),
	}
}

// reapplyCurrentChain pushes the current slots out again (called whenever the volume or
// the loudness settings change).
// It goes through the same dspWriteAllBands path, so the preamp automatically accounts for
// the compensation's boost.
func reapplyCurrentChain() error {
	if !dspAvailable {
		return errDSPUnavailable
	}
	if err := compileCurrentChain(); err != nil {
		return err
	}
	return dspWriteAllBands()
}

// compileCurrentChain recompiles the slot table from the "current user chain + the
// **current** equal-loudness state" (memory only, the hardware is untouched).
//
// WARNING: why this step is required: `currentSlots` is the compilation result *after*
// `applyChain` merged the equal loudness in, and equal loudness varies with the volume and
// with the on/off switch. Previously `reapplyCurrentChain()` only called
// `dspWriteAllBands()`, and that function recomputed the coefficients from the **old**
// `currentSlots` every time => turning equal loudness off, or changing the volume, left
// the hardware unchanged.
// Measured (2026-09-18): `POST /api/dsp/loudness {"enabled":false}` returned
// `enabled:false / amount:0`, but the engine still reported `sections = 8`
// (= 4 compensation sections + 4 DDC sections) -- the compensation stayed on the chain.
func compileCurrentChain() error {
	slots, _, err := chainToSlots(withLoudness(currentUserChain))
	if err != nil {
		return fmt.Errorf("failed to recompile the chain: %w", err)
	}
	currentSlots = slots
	return nil
}
