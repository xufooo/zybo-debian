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
// Cost: one `amixer` run takes about 23 ms on this 650 MHz Cortex-A9, so the
// value is cached briefly instead of being read on every 1 Hz status push.

package main

import (
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
)

var (
	volumeMu     sync.Mutex
	volumePct    = -1
	volumeReadAt time.Time
)

var volumePctRe = regexp.MustCompile(`\[(\d+)%\]`)

// parseMixerPercent pulls the percentage out of `amixer sget` output, e.g.
//
//	Front Left: Playback 102 [80%] [-20.00dB]
//
// and returns -1 when no percentage is present.
func parseMixerPercent(out string) int {
	m := volumePctRe.FindStringSubmatch(out)
	if m == nil {
		return -1
	}
	pct, err := strconv.Atoi(m[1])
	if err != nil || pct < 0 || pct > 100 {
		return -1
	}
	return pct
}

// readSystemVolume returns the live mixer percentage, or fallback when the
// control cannot be read. The result is cached for volumeCacheTTL.
func readSystemVolume(fallback int) int {
	volumeMu.Lock()
	defer volumeMu.Unlock()
	if volumePct >= 0 && time.Since(volumeReadAt) < volumeCacheTTL {
		return volumePct
	}
	out, err := exec.Command("amixer", "sget", mixerControl).Output()
	if err != nil {
		return fallback
	}
	pct := parseMixerPercent(string(out))
	if pct < 0 {
		return fallback
	}
	volumePct, volumeReadAt = pct, time.Now()
	return pct
}

// noteSystemVolume seeds the cache after we wrote the control ourselves, so the
// UI reflects the change immediately instead of waiting for the next read.
func noteSystemVolume(pct int) {
	volumeMu.Lock()
	volumePct, volumeReadAt = pct, time.Now()
	volumeMu.Unlock()
}
