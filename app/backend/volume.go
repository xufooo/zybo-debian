// SPDX-License-Identifier: GPL-2.0-only
// volume.go — the one system volume knob: the codec's ALSA "Master" control
//
// The codec mixer is the only volume control in this product:
//   - the REST API / WebUI writes it (POST /api/volume)
//   - shairport-sync writes it for AirPlay (alsa.mixer_control_name = "Master"),
//     so the sender's volume slider moves the very same knob
//   - bluealsa-aplay writes it for Bluetooth (--volume=mixer)
//   - mpd never touches it (mixer_type "none")
//
// Every source therefore plays at unity gain into one shared control, and
// switching sources cannot change the loudness.
//
// The API must also read the control back: a phone (AirPlay) or a Bluetooth
// remote can move it at any time, and an in-memory copy goes stale across a
// reboot -- the WebUI showed 80% while the hardware actually sat at 70%.
//
// IMPORTANT: the control's *percentage* and its *dB* are not the same thing.
// Measured on this board (2026-09-15):
//
//	raw  0..47 : the driver reports -99999.99dB (no dB mapping) -> DEAD ZONE, silence
//	raw 48     : -74 dB
//	raw 102    : -20 dB      (what ALSA calls 80%)
//	raw 122    :   0 dB      (what ALSA calls 96%)
//	raw 127    :  +5 dB      (what ALSA calls 100%) -> digital gain, i.e. clipping
//
// So writing a raw percentage is wrong at both ends: the first 37% of the slider
// is silent and the last 4% adds up to +5 dB of gain ("stuck at a value, then it
// distorts when turned up"). This file therefore calibrates in dB:
//
//	0% -> mute (raw 0);  1..100% -> -60 dB .. 0 dB
//
// 100% is exactly 0 dB (unity gain, no clipping) and the whole slider is useful.
//
// Cost: one `amixer` run takes about 23 ms on this 650 MHz Cortex-A9, so the
// value is cached briefly.
//
// SECOND PITFALL: the mixer only has a 1 dB grid (raw steps by exactly 1 dB),
// while squeezing 60 dB into 100 positions means 1.667% per step. Several slider
// positions therefore cannot be represented, which is where "I set 50 and it
// reads back 52" came from (measured on hardware, 2026-09-15). The rules here:
//   - write: pctToDB, then ROUND to whole dB (Go's int() truncates toward zero,
//     so int(x+0.5) turns -29.5 into -29 -- that was the missing 1 dB), then raw
//   - read: derive pct from raw, and after writing the control seed the cache
//     with the value from that same grid (quantizePct), so what the UI shows and
//     what a later read returns can never disagree.

package main

import (
	"math"
	"os/exec"
	"regexp"
	"strconv"
	"sync"
	"time"
)

const (
	// mixerControl is the codec's hardware volume control (the only one we use).
	mixerControl = "Master"
	// volumeCacheTTL bounds how often the mixer is actually read.
	volumeCacheTTL = 2 * time.Second

	// volumeMinDB/volumeMaxDB: the dB span that slider 1%..100% covers.
	// 100% = 0 dB (unity gain); the codec goes on to +5 dB, which is pure gain.
	volumeMinDB = -60.0
	volumeMaxDB = 0.0

	// Measured: raw 48..127 is exactly -74dB..+5dB on this codec, i.e. 1 dB per
	// step, so raw = dB + 122 (0 dB == 122). Values are written as raw integers
	// rather than "<n>dB" because amixer uses GNU getopt and an argument like
	// "-12.0dB" is parsed as an option and rejected.
	mixerZeroDBRaw = 122
)

var (
	volumeMu     sync.Mutex
	volumePct    = -1
	volumeDB     float64
	volumeReadAt time.Time
)

// matches "[-20.00dB]" or "-99999.99dB"
var volumeDBRe = regexp.MustCompile(`\[(-?[0-9.]+)dB\]`)

// pctToDB converts the 0..100 slider value to a mixer dB value. 0 gets the
// minimum (setSystemVolume special-cases it to real mute).
func pctToDB(pct int) float64 {
	if pct <= 0 {
		return volumeMinDB
	}
	if pct >= 100 {
		return volumeMaxDB
	}
	return volumeMinDB + (volumeMaxDB-volumeMinDB)*float64(pct)/100.0
}

// dbToPct converts a mixer dB reading back to a slider percentage; the dead zone
// (no dB mapping) counts as 0%.
func dbToPct(db float64) int {
	if db <= volumeMinDB {
		return 0
	}
	if db >= volumeMaxDB {
		return 100
	}
	return int((db-volumeMinDB)/(volumeMaxDB-volumeMinDB)*100.0 + 0.5)
}

// mixerRawForPct returns the raw value to write for a slider position
// (0 means mute). The rounding must use math.Round: on negative numbers
// int(x+0.5) truncates toward zero and lands one dB high.
func mixerRawForPct(pct int) int {
	if pct <= 0 {
		return 0
	}
	raw := mixerZeroDBRaw + int(math.Round(pctToDB(pct)))
	if raw > 127 {
		raw = 127
	}
	if raw < 1 {
		raw = 1
	}
	return raw
}

// quantizePct maps a requested slider position onto the percentage the mixer
// grid can actually reach. On 1 dB hardware the two can differ by one step;
// using this keeps "what we write" and "what a read returns" the same value.
func quantizePct(pct int) int {
	return dbToPct(float64(mixerRawForPct(pct) - mixerZeroDBRaw))
}

// parseMixerDB pulls the dB value out of `amixer sget` output, e.g.
//
//	Front Left: Playback 102 [80%] [-20.00dB]
//
// It returns (0,false) when there is no dB field, and (volumeMinDB,true) for the
// dead zone (-99999.99dB), which is silence.
func parseMixerDB(out string) (float64, bool) {
	m := volumeDBRe.FindStringSubmatch(out)
	if m == nil {
		return 0, false
	}
	db, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, false
	}
	if db < volumeMinDB-5 { // dead zone: the driver reports -99999.99
		return volumeMinDB, true
	}
	return db, true
}

// readSystemVolume returns the live slider percentage (calibrated in dB), or
// fallback when the control cannot be read. Cached for volumeCacheTTL.
func readSystemVolume(fallback int) int {
	volumeMu.Lock()
	defer volumeMu.Unlock()
	if volumePct >= 0 && time.Since(volumeReadAt) < volumeCacheTTL {
		return volumePct
	}
	out, err := exec.Command("amixer", "-c", "0", "sget", mixerControl).Output()
	if err != nil {
		return fallback
	}
	db, ok := parseMixerDB(string(out))
	if !ok {
		return fallback
	}
	pct := dbToPct(db)
	volumePct, volumeDB, volumeReadAt = pct, db, time.Now()
	return pct
}

// systemVolumeDB returns the dB reading from the same cache readSystemVolume
// uses, so the UI can show the dB value and agree with `amixer`.
func systemVolumeDB(fallback int) float64 {
	readSystemVolume(fallback) // keeps the cache fresh (a hit does not run amixer)
	volumeMu.Lock()
	defer volumeMu.Unlock()
	return volumeDB
}

// setSystemVolume writes the mixer using the dB calibration; 0% is real mute.
func setSystemVolume(pct int) error {
	if pct <= 0 {
		return exec.Command("amixer", "-c", "0", "sset", mixerControl, "0").Run()
	}
	return exec.Command("amixer", "-c", "0", "sset", mixerControl,
		strconv.Itoa(mixerRawForPct(pct))).Run()
}

// noteSystemVolume seeds the cache after we wrote the control ourselves, so the
// UI reflects the change immediately instead of waiting for the next read. It
// seeds the value quantized onto the grid, so the UI does not show 50 and then
// jump to 52.
func noteSystemVolume(pct int) {
	volumeMu.Lock()
	if pct <= 0 {
		volumePct, volumeDB = 0, volumeMinDB
	} else {
		volumePct = quantizePct(pct)
		volumeDB = float64(mixerRawForPct(pct) - mixerZeroDBRaw)
	}
	volumeReadAt = time.Now()
	volumeMu.Unlock()
}
