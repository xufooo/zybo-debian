// SPDX-License-Identifier: GPL-2.0-only
// api.go — REST API handlers

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
)

// ── Structs (mirroring the JSON responses) ───────────────────────────

type BandStatus struct {
	Type   string  `json:"type"`
	Freq   float64 `json:"freq"`
	GainDB float64 `json:"gain_db"`
	Q      float64 `json:"q"`
}

type DSPStatus struct {
	Available bool   `json:"available"`
	Enabled   bool   `json:"enabled"`
	Bypass    bool   `json:"bypass"`
	Preset    string `json:"preset"`
	// PreampDB is the headroom compensation actually folded into the coefficients
	// (dB, <=0): when the EQ boosts, this much is taken off so the cascade stays
	// within 0 dBFS (see headroom.go). 0 means no compensation was needed.
	PreampDB    float64      `json:"preamp_db"`
	BandsActive int          `json:"bands_active"`
	Limiter     bool         `json:"limiter"`
	LimThrDB    float64      `json:"lim_thr_db"`
	LimAttMs    float64      `json:"lim_att_ms"`
	LimRelMs    float64      `json:"lim_rel_ms"`
	Bands       []BandStatus `json:"bands"`
}

type SystemStatus struct {
	CPUPercent int   `json:"cpu_percent"`
	MemUsedMB  int   `json:"mem_used_mb"`
	MemTotalMB int   `json:"mem_total_mb"`
	UptimeSec  int64 `json:"uptime_sec"`
}

type APIStatus struct {
	Version string `json:"version"`
	Source  string `json:"source"`
	Title   string `json:"title"`
	Artist  string `json:"artist"`
	Album   string `json:"album"`
	Playing bool   `json:"playing"`
	Volume  int    `json:"volume"`
	// VolumeDB is the dB reading of Volume. The slider scale is calibrated in dB
	// and is NOT the percentage amixer reports for the raw register (60 dB across
	// 100 positions = 1.667%/step, while raw steps by 1 dB), so the UI has to show
	// the dB value to agree with amixer.
	VolumeDB float64      `json:"volume_db"`
	DSP      DSPStatus    `json:"dsp"`
	System   SystemStatus `json:"system"`
	// Sources reports whether each source is available (installed in this image); the UI greys out buttons based on it
	Sources map[string]bool `json:"sources"`
}

// ── Global mutable state (simple, single-writer from API goroutine) ────

var (
	currentSource = "idle"
	currentTitle  = ""
	currentArtist = ""
	currentAlbum  = ""
	// There used to be a currentPlaying flag here. It was removed: it was never
	// accurate and never went back to false. "Is it playing" now comes from the
	// sound card itself -- see systemIsPlaying() in status.go.
	currentVolume = 80
	currentPreset = "flat"
	dspEnabled    = true
	dspBypass     = false
)

// ── Handlers ──────────────────────────────────────────────────────────

func handleStatus(w http.ResponseWriter, r *http.Request) {
	collectMetadata()

	status := buildStatus()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
}

func handleDSPEnable(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var body struct{ Enable bool }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := dspSetEnable(body.Enable); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	dspEnabled = body.Enable
	w.WriteHeader(http.StatusOK)
}

func handleDSPBypass(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var body struct{ Bypass bool }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := dspSetBypass(body.Bypass); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	dspBypass = body.Bypass
	w.WriteHeader(http.StatusOK)
}

func handleDSPEQ(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	// URL: /api/dsp/eq/2  → band index from path
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/dsp/eq/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, "missing band index", http.StatusBadRequest)
		return
	}
	band, err := strconv.Atoi(parts[0])
	if err != nil || band < 0 || band > 5 {
		http.Error(w, "invalid slot index (0-5)", http.StatusBadRequest)
		return
	}

	var body struct {
		GainDB float64 `json:"gain_db"`
		Freq   float64 `json:"freq"`
		Q      float64 `json:"q"`
		Type   string  `json:"type"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if body.Freq <= 0 {
		body.Freq = 1000
	}
	if body.Q <= 0 {
		body.Q = 0.7
	}
	if body.Type == "" {
		body.Type = "PK"
	}

	sc := slotConfig{Type: body.Type, Freq: body.Freq, Q: body.Q, GainDB: body.GainDB}
	if err := dspSetSlot(band, sc); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("EQ band %d: gain=%+.1f dB, freq=%.0f Hz, Q=%.1f",
		band, body.GainDB, body.Freq, body.Q)
	w.WriteHeader(http.StatusOK)
}

// handleDSPLimiter sets the peak limiter: {"enabled":true,"thr_db":-1.0,"att_ms":1,"rel_ms":80}
// All fields are optional; only the ones that were sent are written.
func handleDSPLimiter(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Enabled *bool    `json:"enabled"`
		ThrDB   *float64 `json:"thr_db"`
		AttMs   *float64 `json:"att_ms"`
		RelMs   *float64 `json:"rel_ms"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	enabled := dspLimiterEnabled
	thrDB := dspLimiterThrDB
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	if body.ThrDB != nil {
		thrDB = *body.ThrDB
	}
	if err := dspSetLimiter(enabled, thrDB); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	attMs, relMs := dspLimiterAttMs, dspLimiterRelMs
	if body.AttMs != nil {
		attMs = *body.AttMs
	}
	if body.RelMs != nil {
		relMs = *body.RelMs
	}
	if err := dspSetLimiterTimes(attMs, relMs); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	log.Printf("limiter: %s thr=%.1f dBFS att=%.2f ms rel=%.0f ms",
		onOff(dspLimiterEnabled), dspLimiterThrDB, dspLimiterAttMs, dspLimiterRelMs)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(dspStatusSnapshot())
}

// handleDSPCheck reads back CTRL and all 30 coefficients and compares them against the software's expectation (post-deployment self-check).
func handleDSPCheck(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	var ctrl uint32
	coeffs := []int32{}
	if dspAvailable {
		ctrl = regRead(regCtrl) & ctrlMask
		coeffs = dspDumpCoeffs()
	}
	resp := map[string]interface{}{
		"available":    dspAvailable,
		"ctrl":         ctrl,
		"bands":        dspActiveBands(),
		"coefficients": coeffs,
	}
	if err := dspSelfCheck(); err != nil {
		resp["ok"] = false
		resp["error"] = err.Error()
		w.WriteHeader(http.StatusInternalServerError)
	} else {
		resp["ok"] = true
	}
	json.NewEncoder(w).Encode(resp)
}

func handleDSPPreset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var body struct{ Name string }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := dspApplyPreset(body.Name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	currentPreset = body.Name
	log.Printf("DSP preset: %s", body.Name)
	w.WriteHeader(http.StatusOK)
}

func handleVolume(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var body struct{ Volume int }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if body.Volume < 0 {
		body.Volume = 0
	}
	if body.Volume > 100 {
		body.Volume = 100
	}

	// ALSA volume: the single system knob, calibrated in dB (0% = mute,
	// 100% = 0 dB). Do NOT write "<n>%": on this codec raw 0..47 has no dB
	// mapping (silence) and raw 127 is +5 dB of gain, so a raw percentage is
	// silent at the bottom and clips at the top (see volume.go).
	if err := setSystemVolume(body.Volume); err != nil {
		// A failed write must be reported: this used to log and still answer 200,
		// so external scripts believed the volume had been set. Do not seed the
		// cache either -- a value that was never written is not the current value.
		log.Printf("volume: %v", err)
		http.Error(w, "mixer write failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	currentVolume = body.Volume
	noteSystemVolume(body.Volume)
	w.WriteHeader(http.StatusOK)
}

func handleSourceSelect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var body struct{ Source string }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Stop all existing sources, then start the new one
	srcMgr.stopAll()

	var err error
	switch body.Source {
	case "airplay":
		err = srcMgr.startAirPlay()
	case "dlna":
		err = srcMgr.startDLNA()
	case "bluetooth":
		err = srcMgr.startBluetooth()
	case "idle":
		// Stop everything, do not start a new source
	default:
		http.Error(w, "unknown source: "+body.Source, http.StatusBadRequest)
		return
	}

	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	currentSource = body.Source
	clearMetadata() // switching sources must drop the previous track's info

	log.Printf("Source switched: %s", body.Source)
	w.WriteHeader(http.StatusOK)
}

func handleReboot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	go func() { exec.Command("reboot").Run() }()
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"msg":"rebooting"}`))
}

func handleShutdown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	go func() { exec.Command("poweroff").Run() }()
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"msg":"shutting down"}`))
}

// ── EQ Config Import / Export ────────────────────────────────────────

func handleEQImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	// Accept multipart upload or raw body
	contentType := r.Header.Get("Content-Type")
	var reader io.Reader

	if strings.HasPrefix(contentType, "multipart/form-data") {
		file, _, err := r.FormFile("config")
		if err != nil {
			http.Error(w, "missing 'config' file in upload", http.StatusBadRequest)
			return
		}
		defer file.Close()
		reader = file
	} else {
		reader = r.Body
	}

	cfg, err := ParseEQConfig(reader)
	if err != nil {
		http.Error(w, fmt.Sprintf("parse error: %v", err), http.StatusBadRequest)
		return
	}

	if err := cfg.ApplyToDSP(); err != nil {
		http.Error(w, fmt.Sprintf("DSP apply error: %v", err), http.StatusInternalServerError)
		return
	}

	currentPreset = cfg.Name
	log.Printf("EQ config imported: %s (%d bands)", cfg.Name, len(cfg.Bands))

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":    "ok",
		"preset":    cfg.Name,
		"bands":     len(cfg.Bands),
		"preamp_db": cfg.Preamp,
	})
}

func handleEQExport(w http.ResponseWriter, r *http.Request) {
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "rew"
	}

	// Build config from current DSP state
	cfg := &EQPresetConfig{
		Name:   currentPreset,
		Preamp: 0,
	}
	for i := 0; i < maxHardwareBands; i++ {
		b := getBandConfig(i)
		cfg.Bands = append(cfg.Bands, EQBand{
			Type:   FilterPeaking, // default — bands are peaking by design
			Freq:   b.Freq,
			GainDB: b.GainDB,
			Q:      b.Q,
		})
	}

	switch format {
	case "rew":
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(cfg.ExportREW()))
	case "json":
		w.Header().Set("Content-Type", "application/json")
		data, _ := cfg.ExportJSON()
		w.Write([]byte(data))
	default:
		http.Error(w, "unknown format: "+format, http.StatusBadRequest)
	}
}

// ── Metadata ────────────────────────────────────────────────────────
//
// CURRENT STATE (measured on hardware, 2026-09-15): AirPlay shows no track info,
// because shairport-sync metadata is disabled in the shipped configuration (the
// whole `metadata = { ... }` block is commented out) and this file never exists.
// The path below is the older file-based implementation; it is kept honest
// rather than pretending to work.
//
// Two traps for whoever wires this up:
//  1. shairport's pipe name defaults to /tmp/shairport-sync-metadata (not the
//     path below) and requires metadata = { enabled = "yes"; pipe_name = "..."; }
//  2. that pipe is a FIFO, not a regular file: os.ReadFile blocks on open until
//     a writer appears, which would stall the once-a-second status pusher.
//     Read it from a long-lived goroutine instead, parsing <item> blocks
//     (<type>core</type>, <code>minm|asar|asal</code>, <data>...</data>).
var metadataPath = "/tmp/shairport-metadata"

func collectMetadata() {
	switch currentSource {
	case "airplay":
		data, err := osReadFile(metadataPath)
		if err != nil {
			// No file (the case in this image): the previous track's info has to
			// be cleared, otherwise the panel stays stuck on the old title.
			clearMetadata()
			return
		}
		parseMetadata(string(data))
	case "dlna":
		// gmrender has no standard metadata
		clearMetadata()
	}
}

func clearMetadata() {
	currentTitle, currentArtist, currentAlbum = "", "", ""
}

// parseMetadata parses "title=... / artist=... / album=..." text.
// It accumulates into locals and assigns at the end, so fields missing from the
// file are cleared rather than left over from the previous track.
func parseMetadata(raw string) {
	title, artist, album := "", "", ""
	for _, line := range strings.Split(raw, "\n") {
		if strings.HasPrefix(line, "title=") {
			title = strings.TrimPrefix(line, "title=")
		}
		if strings.HasPrefix(line, "artist=") {
			artist = strings.TrimPrefix(line, "artist=")
		}
		if strings.HasPrefix(line, "album=") {
			album = strings.TrimPrefix(line, "album=")
		}
	}
	currentTitle, currentArtist, currentAlbum = title, artist, album
}
