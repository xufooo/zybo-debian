// SPDX-License-Identifier: GPL-2.0-only
// main.go — ZYBO Go backend service entry point
//
// Features:
//   - HTTP REST API (status query / DSP control / source switching)
//   - WebSocket real-time push (track title / EQ / volume / system status)
//   - Source process management (shairport-sync / gmrender / bluealsa)
//   - FPGA DSP control (/dev/mem mmap)
//
// Cross-compilation:
//   CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -ldflags="-s -w" -o zybo-audio-web .
//   → binary ~6MB, runtime memory ~8-12MB

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

// appVersion is returned to the frontend via /api/status and is also printed in the startup banner.
//
// This is the **first beta**: hardware and software have only been validated on the development board, with no production testing,
// so the version stays at 0.x (1.0.0 is reserved until after formal acceptance).
const appVersion = "0.1.0-beta"

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Printf("ZYBO Audio Web v%s starting…", appVersion)

	// ── Open the FPGA DSP control interface ─────────────────────
	if err := dspOpen(); err != nil {
		log.Printf("WARNING: FPGA DSP not available: %v", err)
		log.Printf("  → EQ/DSP control disabled; audio pass-through only")
	} else {
		log.Printf("FPGA DSP registers mapped at 0x43C00000")
		defer dspClose()
		// Init: enable DSP, flat preset by default
		dspInit()
	}

	// ── Initialize the source manager ───────────────────────────
	srcMgr = &sourceManager{}
	if err := srcMgr.init(); err != nil {
		log.Printf("Source manager init: %v", err)
	}
	// At boot systemd may already be running a source (shairport-sync is enabled in the image), so the
	// detection result must be reflected in the exposed state; otherwise the WebUI shows "idle" while audio is playing.
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
	mux.HandleFunc("/api/dsp/check", handleDSPCheck)
	mux.HandleFunc("/api/dsp/import", handleEQImport)
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

	// ── Start the push goroutine (push status to all WS clients every second) ──
	go statusPusher(hub, srcMgr)

	// ── HTTP server ─────────────────────────────────────────────
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
		// ⚠️ Do **not** call srcMgr.stopAll() here: the sources (shairport-sync etc.) are independent
		// systemd services, and the backend is only a controller. Its own exit/restart must not cut off
		// the music that is playing, otherwise `systemctl restart zybo-audio-web` would interrupt listening.
		// (The "stop" button goes through /api/source/select{idle}; only that path calls stopAll.)
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
