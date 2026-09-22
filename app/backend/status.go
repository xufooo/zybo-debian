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

// ── "is it playing?": the only honest signal is the sound card itself ────────────────
//
// This used to be an in-memory flag: it was set true when a source was selected,
// and true again when a title showed up in the metadata, and it **never went back to
// false**; a backend restart reset it to false. Consequences (the author
// measured this on 2026-09-15): while AirPlay really was playing, the panel showed
// "no source connected", or showed "airplay connected" while nothing was playing at
// all — the first reported "not playing" as "no source connected", the second
// reported "source selected" as "device connected".
//
// Every source (AirPlay / Bluetooth / local MPD) ends up writing the same sound card,
// so the card's own PCM status is the most direct reading: `state: RUNNING` means it
// really is producing sound (the file reads "closed" while the device is not open).
// It is one /proc read: microseconds, no side effects (far cheaper than amixer's 23 ms).
//
// It is a variable so it can be tested offline: the test points it at a fixture file and does not depend on a real board.
var pcmStatusPath = "/proc/asound/card0/pcm0p/sub0/status"

func systemIsPlaying() bool {
	out, err := ioutilReadFile(pcmStatusPath)
	if err != nil {
		return false // no such file (no sound card / wrong path) means nothing is playing
	}
	return strings.Contains(string(out), "state: RUNNING")
}

// ── system status ─────────────────────────────────────────────────────

func getSystemStatus() SystemStatus {
	return SystemStatus{
		CPUPercent: getCPUPercent(),
		MemUsedMB:  getMemUsedMB(),
		MemTotalMB: 512,
		UptimeSec:  int64(time.Since(startTime).Seconds()),
	}
}

func getCPUPercent() int {
	// read CPU usage from /proc/stat
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
			// no complex analysis: just look at MemAvailable
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

// buildStatus is the **only** place where status is assembled: both HTTP /api/status
// and the WebSocket push go through it.
//
// Why (2026-09-15): the two paths each built their own APIStatus, so when volume_db was
// added only the HTTP copy was updated; the WebSocket then pushed a volume_db=0 payload
// every second and overwrote the UI — right just after a page refresh, wrong a moment
// later. Status fields may only be assembled in this one function.
func buildStatus() APIStatus {
	return APIStatus{
		Version: appVersion,
		Source:  currentSource,
		Title:   currentTitle,
		Artist:  currentArtist,
		Album:   currentAlbum,
		// read the sound card's real state, not an in-memory flag (see the note on systemIsPlaying)
		Playing: systemIsPlaying(),
		// read back the mixer: WebUI / REST writes it, and Bluetooth's bluealsa-aplay pushes it
		// (AirPlay's ignore_volume_control=yes was changed to leave it alone), so the UI must follow it (see volume.go)
		Volume:   readSystemVolume(currentVolume),
		VolumeDB: systemVolumeDB(currentVolume),
		DSP:      dspStatusSnapshot(),
		System:   getSystemStatus(),
		Sources:  sourceStates(),
	}
}

// ── status push ───────────────────────────────────────────────────────

func statusPusher(hub *wsHub, sm *sourceManager) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	var lastChecksum string

	for range ticker.C {
		// collect metadata
		collectMetadata()

		status := buildStatus()

		// push only when the state changed (simple checksum)
		jsonBytes, _ := json.Marshal(status)
		checksum := string(jsonBytes)
		if checksum != lastChecksum {
			hub.broadcast(jsonBytes)
			lastChecksum = checksum
		}
	}
}

// ── file reading (Go 1.16+ uses os.ReadFile, no longer io/ioutil) ────────────

var ioutilReadFile = os.ReadFile // Go 1.17+
var osReadFile = os.ReadFile
