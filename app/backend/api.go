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

// ── Structs (mapping to the JSON responses) ──────────────────────────

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
	// PreampDB is the headroom compensation **actually folded into the coefficients** (dB, ≤0): when the EQ boosts, this much is
	// automatically cut so the cascaded output never exceeds 0dBFS (see dsp_headroom.go). 0 = no compensation needed.
	PreampDB    float64      `json:"preamp_db"`
	BandsActive int          `json:"bands_active"`
	Limiter     bool         `json:"limiter"`
	LimiterMode string       `json:"limiter_mode"`
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
	// VolumeDB is the dB reading of Volume. The slider scale is calibrated in dB, which is not the same scale as
	// amixer's raw register percentage (60 dB over 100 steps = 1.667%/step, raw is 1 dB per step), so only the dB display adds up.
	VolumeDB float64      `json:"volume_db"`
	DSP      DSPStatus    `json:"dsp"`
	System   SystemStatus `json:"system"`
	// Sources reports whether each source is available (installed in this image or not); the UI uses it to grey out buttons
	Sources map[string]bool `json:"sources"`
}

// ── Global mutable state (simple, single-writer from API goroutine) ────

var (
	currentSource = "idle"
	currentTitle  = ""
	currentArtist = ""
	currentAlbum  = ""
	// Note: there used to be a currentPlaying in-memory flag here, now removed — it was both inaccurate and never
	// cleared, so "is it playing" comes from systemIsPlaying() in status.go reading the sound card state directly (see the note there).
	// Default volume 90% (−6 dB): the design brief of 2026-09-20 asked for "default 90%, leave some headroom".
	// Why it is worth keeping: the chain already has 18 dB of headroom covering the convolution's +11.5 dB, but that is there to **prevent clipping inside the chain**;
	// the extra 6 dB at the ALSA stage covers "material already at 0 dBFS full scale, after the limiter",
	// and it also steers clear of the ssm2602's nonlinear region near full scale. A value the user changed by hand is persisted
	// (see the Volume field in state.go) and is not overwritten by this default.
	currentVolume = 90
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

// handleDSPLimiter configures the peak limiter: {"enabled":true,"thr_db":-1.0,"att_ms":1,"rel_ms":80}
// All fields are optional; only the ones supplied are written.
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
		// "feedback" = the 0.1 feedback design (no lookahead); "truepeak" = the 0.2 lookahead version.
		// ⚠️ The two modes use **different formulas** for att_ms/rel_ms (multiplicative in the linear domain vs first order in the log2 domain),
		//    so the mode is switched first and the coefficients are then recomputed on the same machine (see dsp_limiter_tp.go).
		Mode *string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Validate the mode first: an invalid value gets a 400 outright and is never silently treated as the default
	if body.Mode != nil {
		switch *body.Mode {
		case "feedback", "truepeak":
		default:
			http.Error(w, "mode only supports feedback / truepeak", http.StatusBadRequest)
			return
		}
		if err := dspSetLimiterTP(*body.Mode == "truepeak"); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
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

	log.Printf("limiter: %s/%s thr=%.1f dBFS att=%.2f ms rel=%.0f ms",
		onOff(dspLimiterEnabled), limiterModeName(), dspLimiterThrDB, dspLimiterAttMs, dspLimiterRelMs)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(dspStatusSnapshot())
}

// handleDSPCheck reads back CTRL and all 30 coefficients and compares them against what the software expects (for the post-deploy self-check).
func handleDSPCheck(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	var ctrl uint32
	coeffs := []int32{}
	if dspAvailable {
		// Report every **readable** bit (the 0.2 bitstream has bit17 BANK_SEL / bit18 LIM_TP):
		// that way "did the limiter mode actually reach the hardware" is visible here instead of being guessed by ear.
		ctrl = regRead(regCtrl) & dspCtrlVerifyMask()
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

	// ALSA volume: the one knob for the whole system, **calibrated in dB** (0% = mute, 100% = 0 dB).
	// ⚠️ Do not write "<n>%": raw 0..47 on this control is a dead zone (the driver has no dB mapping) and raw 127 is
	// +5 dB of clipping gain, so writing a percentage gives "silent at the bottom, blasting at the top" (see volume.go)
	if err := setSystemVolume(body.Volume); err != nil {
		// A failed write must be reported (it once only logged while still returning 200, so external scripts thought it had worked).
		// No caching either: a value that was not written is not the current value.
		log.Printf("volume: %v", err)
		http.Error(w, "mixer write failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Note: historically this also called dspSetMasterVolume (the FPGA master volume). The new hardware has no such
	// register, that function is already a no-op (see dsp.go), and digital volume goes only through the ALSA Master above.
	currentVolume = body.Volume
	noteSystemVolume(body.Volume)
	// Loudness compensation follows the volume: when the volume changes, the compensation is re-merged for the new volume and downloaded.
	// (Audyssey Dynamic EQ / V4A Equal Loudness both behave this way.)
	if loudnessOn && dspAvailable {
		if err := reapplyCurrentChain(); err != nil {
			log.Printf("loudness compensation re-download failed: %v", err)
		}
	}
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

	// Stop every existing source, start the new one
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
		// Stop everything, start no new source
	default:
		http.Error(w, "unknown source: "+body.Source, http.StatusBadRequest)
		return
	}

	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	currentSource = body.Source
	clearMetadata() // switching source must clear the previous track's info (playing is decided by the sound card state, not set here)

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
	// Export only the **active sections**: exporting disabled ones (Type=off, Freq=0) would just emit a line
	// of fake "Fc 0 Hz" filters — that is noise, not information.
	for i := 0; i < bandLimit(); i++ {
		b := getBandConfig(i)
		if isBandOff(b.Type) {
			continue
		}
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
// ⚠️ Current state (measured 2026-09-15): **AirPlay yields no track information**, because shairport-sync's metadata
// is off by default (in the release config the whole `metadata = { ... }` block is commented out), and this file does not exist at all.
// The path below is only the historical "file-based" implementation, kept without pretending it works.
//
// Two things to watch when metadata is really wired up (do not trip over them):
//  1. shairport's pipe name defaults to /tmp/shairport-sync-metadata (not the one below), so
//     metadata = { enabled = "yes"; pipe_name = "..."; } has to be turned on explicitly;
//  2. that is a **FIFO**, not a regular file: os.ReadFile blocks in open until a writer appears, so once connected it would
//     stall the whole status push (once per second). It must become a resident goroutine reading the stream and parsing items
//     (<item><type>core</type><code>minm|asar|asal</code><data>…), rather than reading a file every time.
var metadataPath = "/tmp/shairport-metadata"

func collectMetadata() {
	switch currentSource {
	case "airplay":
		data, err := osReadFile(metadataPath)
		if err != nil {
			// The file is not there (which is the case in this image) ⇒ the previous track's info must be cleared,
			// otherwise the panel stays on the old track forever.
			clearMetadata()
			return
		}
		parseMetadata(string(data))
	case "dlna":
		// gmrender has no standard metadata, so leave it empty
		clearMetadata()
	}
}

func clearMetadata() {
	currentTitle, currentArtist, currentAlbum = "", "", ""
}

// parseMetadata parses "title=… / artist=… / album=…" text.
// Values accumulate in locals and are assigned as a whole: fields absent from the file are cleared instead of keeping the previous value.
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
