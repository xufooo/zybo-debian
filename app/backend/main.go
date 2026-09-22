// main.go -- ZYBO Go backend service entry point
//
// Features:
//   - HTTP REST API (status query / DSP control / source switching)
//   - WebSocket push (track title / EQ / volume / system status)
//   - Audio source process management (shairport-sync / gmrender / bluealsa)
//   - FPGA DSP control (/dev/mem mmap)
//
// Cross-compile:
//   CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -ldflags="-s -w" -o zybo-audio-web .
//   -> binary ~6MB, runtime memory ~8-12MB

package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
)

var (
	srcMgr *sourceManager
)

// appVersion is returned to the frontend by /api/status and printed in the startup banner.
//
// This is the **first beta**: both hardware and software have only been verified on the
// development board, with no production testing, so the version stays at 0.x
// (1.0.0 waits until there is a formal acceptance).
const appVersion = "0.2.0"

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Printf("ZYBO Audio Web v%s starting...", appVersion)

	// ── Open the FPGA DSP control interface ─────────────────────
	if err := dspOpen(); err != nil {
		log.Printf("WARNING: FPGA DSP not available: %v", err)
		log.Printf("  → EQ/DSP control disabled; audio pass-through only")
	} else {
		log.Printf("FPGA DSP registers mapped at 0x43C00000")
		defer dspClose()
		// Init: enable DSP, default flat preset
		dspInit()
		// Then bring back **the settings the previous run was using** (chain / dynamic
		// bass / loudness / limiter / headroom, see state.go).
		// Done after dspInit: init puts the hardware into a known state first, and the
		// restore only overrides on top of it.
		restoreRuntimeState()
	}

	// ── Init the source manager ─────────────────────────────────
	srcMgr = &sourceManager{}
	if err := srcMgr.init(); err != nil {
		log.Printf("Source manager init: %v", err)
	}
	// At boot systemd may already be running some source (shairport-sync is enabled in
	// the image), so the detection result must be synced to the external state; otherwise
	// the WebUI shows "idle" while audio is actually playing.
	currentSource = srcMgr.active

	// ── Start the WebSocket hub ─────────────────────────────────
	hub := newWSHub()
	go hub.run()

	// ── Register HTTP routes ────────────────────────────────────
	mux := http.NewServeMux()

	// API
	mux.HandleFunc("/api/status", handleStatus)
	mux.HandleFunc("/api/dsp/enable", handleDSPEnable)
	mux.HandleFunc("/api/dsp/bypass", handleDSPBypass)
	mux.HandleFunc("/api/dsp/eq/", handleDSPEQ)
	mux.HandleFunc("/api/dsp/preset", handleDSPPreset)
	mux.HandleFunc("/api/dsp/limiter", handleDSPLimiter)
	mux.HandleFunc("/api/dsp/loudness", handleDSPLoudness)
	mux.HandleFunc("/api/dsp/check", handleDSPCheck)
	mux.HandleFunc("/api/dsp/import", handleEQImport)
	// 0.2: capability self-description / effect chain / preset system
	mux.HandleFunc("/api/dsp/capabilities", handleDSPCapabilities)
	mux.HandleFunc("/api/dsp/chain", handleDSPChain)
	mux.HandleFunc("/api/dsp/presets", handleDSPPresets)
	mux.HandleFunc("/api/dsp/presets/apply", handleDSPPresetApply)
	mux.HandleFunc("/api/dsp/presets/delete", handleDSPPresetDelete)
	mux.HandleFunc("/api/dsp/presets/rename", handleDSPPresetRename)
	// Sound "types": a container for a whole set of sound settings (headphones / small
	// speakers / amplifier + user-defined), kept apart from "genre presets": a preset is
	// applied once and done, a type is the long-lived set whose changes must be remembered.
	mux.HandleFunc("/api/dsp/types", handleDSPTypes)
	mux.HandleFunc("/api/dsp/types/delete", handleDSPTypeDelete)
	mux.HandleFunc("/api/dsp/types/rename", handleDSPTypeRename)
	mux.HandleFunc("/api/dsp/presets/export", handleDSPPresetExport)
	mux.HandleFunc("/api/dsp/presets/import", handleDSPPresetImport)
	mux.HandleFunc("/api/dsp/eq/parse", handleDSPEQParse)
	mux.HandleFunc("/api/dsp/ir", handleDSPIR)
	mux.HandleFunc("/api/dsp/ddc", handleDSPDDC)
	mux.HandleFunc("/api/dsp/export", handleEQExport)
	mux.HandleFunc("/api/volume", handleVolume)
	mux.HandleFunc("/api/source/select", handleSourceSelect)
	mux.HandleFunc("/api/reboot", handleReboot)
	mux.HandleFunc("/api/shutdown", handleShutdown)

	// WebSocket
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		serveWS(hub, w, r)
	})

	// Frontend static files
	mux.Handle("/", http.FileServer(http.Dir("/var/www/zybo-audio")))

	// ── Start the push goroutine (pushes status to every WS client once a second) ──
	go statusPusher(hub, srcMgr)

	// ── HTTP service ────────────────────────────────────────────
	port := ":8080"
	if p := os.Getenv("PORT"); p != "" {
		port = ":" + p
	}

	server := &http.Server{
		Addr:    port,
		Handler: mux,
	}

	// Graceful shutdown
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		log.Println("Shutting down...")
		// WARNING: do **not** call srcMgr.stopAll() here: the sources (shairport-sync etc.)
		// are independent systemd services and the backend is only their controller. Its
		// exiting or restarting on its own must not kill the music that is playing,
		// otherwise `systemctl restart zybo-audio-web` would interrupt listening.
		// (The "stop" button goes through /api/source/select{idle}; only that path calls
		// stopAll.)
		server.Close()
	}()

	fmt.Printf(`
╔══════════════════════════════════════════╗
║   ZYBO Audio Web v%-8s           ║
║   Listening on %-22s ║
║   WebUI: http://zybo-audio.local%-5s ║
╚══════════════════════════════════════════╝
`, appVersion, port, port)

	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatal(err)
	}
	log.Println("Server stopped")
}
