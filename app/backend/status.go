// SPDX-License-Identifier: GPL-2.0-only
// status.go — system status collection + periodic push

package main

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"time"
)

var startTime = time.Now()

// ── "Is it playing?" -- the sound card is the only honest signal ───────
//
// This used to be an in-memory flag: set when a source was selected, set again
// when metadata appeared with a title, and NEVER cleared -- while a backend
// restart reset it to false. The panel therefore said "No source connected"
// while music was playing, or "airplay connected" while nothing was playing
// (reported on hardware, 2026-09-15).
//
// Every source (AirPlay / Bluetooth / local MPD) ends up writing the same sound
// card, so the card's PCM state is the honest answer: `state: RUNNING` means
// audio is actually flowing (the driver writes "closed" when the device is not
// open). One /proc file read, microseconds, no side effects.
//
// It is a variable so tests can point it at a fixture file instead of a board.
var pcmStatusPath = "/proc/asound/card0/pcm0p/sub0/status"

func systemIsPlaying() bool {
	out, err := ioutilReadFile(pcmStatusPath)
	if err != nil {
		return false // no such file (no card / wrong path) means not playing
	}
	return strings.Contains(string(out), "state: RUNNING")
}

// ── System status ─────────────────────────────────────────────────────

func getSystemStatus() SystemStatus {
	return SystemStatus{
		CPUPercent: getCPUPercent(),
		MemUsedMB:  getMemUsedMB(),
		MemTotalMB: 512,
		UptimeSec:  int64(time.Since(startTime).Seconds()),
	}
}

func getCPUPercent() int {
	// Read CPU usage from /proc/stat
	data, err := ioutilReadFile("/proc/stat")
	if err != nil {
		return 0
	}
	line := strings.Split(string(data), "\n")[0]
	fields := strings.Fields(line)
	if len(fields) < 5 {
		return 0
	}
	// cpu  user nice system idle iowait ...
	var user, nice, system, idle uint64
	user, _ = strconv.ParseUint(fields[1], 10, 64)
	nice, _ = strconv.ParseUint(fields[2], 10, 64)
	system, _ = strconv.ParseUint(fields[3], 10, 64)
	idle, _ = strconv.ParseUint(fields[4], 10, 64)

	total := user + nice + system + idle
	if total == 0 {
		return 0
	}
	used := total - idle
	return int(used * 100 / total)
}

func getMemUsedMB() int {
	data, err := ioutilReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			// No complex analysis here, just look at MemAvailable
		}
		if strings.HasPrefix(line, "MemAvailable:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				kb, _ := strconv.ParseInt(fields[1], 10, 64)
				return 512 - int(kb/1024) // total - available = used
			}
		}
	}
	return 80 // fallback
}

// ── Status push ───────────────────────────────────────────────────────

// buildStatus is the ONE place that assembles APIStatus: both the HTTP
// /api/status handler and the WebSocket pusher call it.
//
// Why it exists (2026-09-15): the two used to build their own APIStatus, and
// adding volume_db only to the HTTP copy left the WebSocket pushing volume_db=0
// once a second, which overwrote the UI value -- correct on page load, wrong a
// second later. Status fields may only be assembled in this function.
func buildStatus() APIStatus {
	return APIStatus{
		Version: appVersion,
		Source:  currentSource,
		Title:   currentTitle,
		Artist:  currentArtist,
		Album:   currentAlbum,
		// Ask the sound card, not an in-memory flag (see systemIsPlaying)
		Playing: systemIsPlaying(),
		// Read the mixer back: the WebUI/REST writes it and bluealsa-aplay
		// (Bluetooth) moves it (AirPlay no longer touches it since
		// ignore_volume_control=yes), so the UI must follow it.
		Volume:   readSystemVolume(currentVolume),
		VolumeDB: systemVolumeDB(currentVolume),
		DSP:      dspStatusSnapshot(),
		System:   getSystemStatus(),
		Sources:  sourceStates(),
	}
}

func statusPusher(hub *wsHub, sm *sourceManager) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	var lastChecksum string

	for range ticker.C {
		// Collect metadata
		collectMetadata()

		status := buildStatus()

		// Push only when the state changes (simple checksum)
		jsonBytes, _ := json.Marshal(status)
		checksum := string(jsonBytes)
		if checksum != lastChecksum {
			hub.broadcast(jsonBytes)
			lastChecksum = checksum
		}
	}
}

// ── File reading (Go 1.16+ uses os.ReadFile, no more io/ioutil) ───────

var ioutilReadFile = os.ReadFile // Go 1.17+
var osReadFile = os.ReadFile
