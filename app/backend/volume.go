// volume.go -- the **only** volume knob in the system: the codec's ALSA "Master" control
//
// This product has exactly one source of truth for volume, the codec's hardware mixer:
//   - the REST API / WebUI writes it (POST /api/volume)
//   - AirPlay's shairport-sync writes it (alsa.mixer_control_name = "Master"),
//     so the volume slider on the phone pushes **the same** knob
//   - Bluetooth's bluealsa-aplay writes it (--volume=mixer)
//   - mpd does not touch it at all (mixer_type "none")
//
// Every source enters this one control at unity gain, so **switching sources does not jump in volume**.
//
// Besides writing, the backend must also **read back**: the phone (AirPlay) or a Bluetooth remote can change it at any time, and a copy that lives
// only in memory goes stale on restart -- there was once a WebUI showing 80% while the hardware actually sat at 70%.
//
// ⚠️ Key point: this control's **percentage and dB are not the same thing** (measured 2026-09-15):
//
//	raw  0..47 : the driver reports -99999.99dB (no dB mapping) -> **dead zone, nearly silent**
//	raw 48     : -74 dB
//	raw 102    : -20 dB      (ALSA's 80%)
//	raw 122    :   0 dB      (ALSA's 96%)
//	raw 127    :  +5 dB      (ALSA's 100%) -> **digital clipping**
//
// So writing by "raw percentage" is wrong at both ends: the first 37% of the slider is all silence, and the last 4% adds +5 dB of gain to the codec
// (audibly, "turn it up and it blasts"). This was therefore changed to **calibration by dB**:
//
//	0% -> mute (raw 0); 1..100% -> -60dB ... 0dB
//
// 100% is exactly 0 dB (unity gain, no more clipping), and the whole slider is usable.
// The cost: one amixer call takes about 23ms on this 650MHz Cortex-A9, so reads are briefly cached.
//
// ⚠️ Second gotcha: **the mixer only has a 1 dB grid** (each raw step is exactly 1 dB), while 60 dB is squeezed into 100 steps, i.e.
// 1.667% per step. So several of the 10 positions on the slider do not land on the grid, which is where "set 50, read back 52" comes from
// (measured by the user on 2026-09-15: "webui shows 62? actually 80"). The rule here is:
//   - write: after pctToDB, **round** to an integer dB (Go's int() truncates toward zero for negative numbers, so adding 0.5 first
//     would cut -29.5 down to -29 -- that was exactly the cause of the 1 dB error), then write raw
//   - read: derive pct back from raw, and right after writing the control seed this **same grid value** into the cache
//     (quantizePct), so the value the UI shows and the value read back afterwards always agree and never jump on their own.

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
	// mixerControl: the codec's hardware volume control (we use only this one).
	mixerControl = "Master"
	// volumeCacheTTL: limits how often the mixer is really read.
	volumeCacheTTL = 2 * time.Second

	// volumeMinDB / volumeMaxDB: the dB range corresponding to slider 1%..100%.
	// 100% = 0 dB (unity gain); never go past it -- the codec has another +5 dB at the top, and that is clipping gain.
	volumeMinDB = -60.0
	volumeMaxDB = 0.0

	// Measured: this control's raw 48..127 is exactly -74dB..+5dB, **1 dB per step**.
	// So raw = dB + 122 (0dB -> 122). Writes use raw rather than "<n>dB":
	// amixer uses GNU getopt, so an argument like "-12.0dB" with a minus sign is taken as an option and errors out.
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

// pctToDB converts the 0..100 slider value into mixer dB. 0 is handled separately as mute (see setSystemVolume).
func pctToDB(pct int) float64 {
	if pct <= 0 {
		return volumeMinDB
	}
	if pct >= 100 {
		return volumeMaxDB
	}
	return volumeMinDB + (volumeMaxDB-volumeMinDB)*float64(pct)/100.0
}

// dbToPct converts the mixer's dB reading back into a slider percentage; no dB mapping (dead zone) counts as 0%.
func dbToPct(db float64) int {
	if db <= volumeMinDB {
		return 0
	}
	if db >= volumeMaxDB {
		return 100
	}
	return int((db-volumeMinDB)/(volumeMaxDB-volumeMinDB)*100.0 + 0.5)
}

// mixerRawForPct computes the raw value to write into the mixer for a slider value (0 means mute).
// That intermediate rounding must use math.Round: int(x+0.5) truncates toward zero for negative numbers.
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

// quantizePct folds the requested slider value onto the percentage the mixer grid **can actually land on**.
// On 1dB/step hardware the two can differ by at most ±1 step; this keeps "what is written" and "what is read back" the same value.
func quantizePct(pct int) int {
	return dbToPct(float64(mixerRawForPct(pct) - mixerZeroDBRaw))
}

// parseMixerDB extracts the dB from `amixer sget` output, for example
//
//	Front Left: Playback 102 [80%] [-20.00dB]
//
// When it cannot be obtained it returns (0,false). The dead zone (-99999.99dB) counts as mute and returns (volumeMinDB,true).
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

// readSystemVolume returns the mixer's live percentage (calibrated by dB); it falls back to fallback when it cannot be read.
// The result is cached for volumeCacheTTL.
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

// systemVolumeDB returns the dB from the same cache as readSystemVolume (for showing dB directly in the UI,
// so that it matches what amixer shows and there is no more argument over two scales, "62% or 80%").
func systemVolumeDB(fallback int) float64 {
	readSystemVolume(fallback) // keep the cache fresh (a cache hit does not actually call amixer)
	volumeMu.Lock()
	defer volumeMu.Unlock()
	return volumeDB
}

// setSystemVolume writes the mixer (calibrated by dB, landing on raw). 0% writes raw 0 directly (true mute).
func setSystemVolume(pct int) error {
	if pct <= 0 {
		return exec.Command("amixer", "-c", "0", "sset", mixerControl, "0").Run()
	}
	return exec.Command("amixer", "-c", "0", "sset", mixerControl,
		strconv.Itoa(mixerRawForPct(pct))).Run()
}

// noteSystemVolume seeds the cache after we write the control ourselves, so the UI reflects the change immediately
// instead of waiting for the next read-back. What is seeded is the value **folded onto the grid**, so the UI does not show 50 and then jump to 52.
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
