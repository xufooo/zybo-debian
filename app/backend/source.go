// SPDX-License-Identifier: GPL-2.0-only
// source.go — source management
//
// Design: **source processes are managed by systemd**, and the backend only "translates" --
// turning the WebUI selection into `systemctl start/stop <unit>`. In the image shairport-sync.service / bluealsa-aplay.service
// are both enabled and work out of the box; the WebUI "stop" therefore really stops them.
//
// ⚠️ Why this was changed (three problems with the old implementation):
//   ① The backend spawned shairport-sync itself with exec.Command, while systemd had already started one at boot
//      ⇒ AirPlay port conflict, and the spawned one failed to bind and exited;
//   ② `-d` daemonized it, and after the parent exited the backend never reaped it ⇒ a <defunct> zombie was left in the process table;
//   ③ Clicking "stop" in the WebUI only killed the process it had spawned, while the systemd one kept playing ⇒ the button was fake.
//
// The one without a systemd unit (DLNA's gmediarender) is still spawned directly, and its child process is reaped on the exit signal.

package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
)

type sourceManager struct {
	mu     sync.Mutex
	active string // "airplay" / "dlna" / "bluetooth" / "idle"
	procs  map[string]*exec.Cmd
}

// source → systemd units (the order is the dependency order)
var sourceUnits = map[string][]string{
	"airplay":   {"shairport-sync.service"},
	"bluetooth": {"bluealsa.service", "bluealsa-aplay.service"},
}

// source → command run directly (not packaged as a systemd unit)
var sourceCommands = map[string]string{
	"dlna": "gmediarender",
}

func (sm *sourceManager) init() error {
	sm.procs = make(map[string]*exec.Cmd)
	sm.active = "idle"

	for name := range sourceUnits {
		if !sourceAvailable(name) {
			log.Printf("Source '%s': unavailable (unit or program missing)", name)
			continue
		}
		log.Printf("Source '%s': available", name)
	}
	for name, cmd := range sourceCommands {
		if _, err := exec.LookPath(cmd); err != nil {
			log.Printf("Source '%s': %s not in PATH, unavailable", name, cmd)
		} else {
			log.Printf("Source '%s': %s available", name, cmd)
		}
	}

	// At boot systemd may already have started a source, and the state must reflect that faithfully (otherwise the UI shows "idle")
	if unitActive("shairport-sync.service") {
		sm.active = "airplay"
		log.Printf("Detected AirPlay already running on the system (shairport-sync.service)")
	}
	return nil
}

// sourceAvailable reports whether a source is currently usable (so the UI can gray out its button).
func sourceAvailable(name string) bool {
	if units, ok := sourceUnits[name]; ok {
		for _, u := range units {
			if unitExists(u) {
				return true
			}
		}
		return false
	}
	if cmd, ok := sourceCommands[name]; ok {
		_, err := exec.LookPath(cmd)
		return err == nil
	}
	return false
}

// ── systemd helpers ───────────────────────────────────────────────────

func systemctl(args ...string) (string, error) {
	out, err := exec.Command("systemctl", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func unitExists(unit string) bool {
	out, err := systemctl("list-unit-files", unit, "--no-legend", "--no-pager")
	return err == nil && strings.TrimSpace(out) != ""
}

func unitActive(unit string) bool {
	out, err := systemctl("is-active", unit)
	return err == nil && strings.TrimSpace(out) == "active"
}

// ── start / stop ──────────────────────────────────────────────────────

func (sm *sourceManager) startAirPlay() error   { return sm.startUnitSource("airplay") }
func (sm *sourceManager) startBluetooth() error { return sm.startUnitSource("bluetooth") }

func (sm *sourceManager) startUnitSource(name string) error {
	units := sourceUnits[name]
	if len(units) == 0 {
		return fmt.Errorf("unknown source: %s", name)
	}
	if !sourceAvailable(name) {
		return fmt.Errorf("%s is not available on this machine (systemd unit missing)", name)
	}
	sm.mu.Lock()
	defer sm.mu.Unlock()

	// Stop the other sources first so that only one runs at a time
	sm.stopAllLocked()

	for _, u := range units {
		if out, err := systemctl("start", u); err != nil {
			return fmt.Errorf("systemctl start %s: %v (%s)", u, err, out)
		}
	}
	sm.active = name
	log.Printf("Source started: %s (%s)", name, strings.Join(units, ", "))
	return nil
}

func (sm *sourceManager) startDLNA() error {
	cmdName := sourceCommands["dlna"]
	if _, err := exec.LookPath(cmdName); err != nil {
		return fmt.Errorf("%s not in PATH (the DLNA renderer is not installed in this image)", cmdName)
	}
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.stopAllLocked()

	cmd := exec.Command(cmdName,
		"-f", "ZYBO Audio",
		"-p", "49494",
		"-u", "00000000-0000-0000-0000-000000000001",
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%s: %w", cmdName, err)
	}
	// A goroutine must reap it, otherwise a zombie is left after the process exits (the old implementation missed this step)
	go func() {
		if err := cmd.Wait(); err != nil {
			log.Printf("%s exited: %v", cmdName, err)
		}
	}()
	sm.procs["dlna"] = cmd
	sm.active = "dlna"
	log.Printf("Source started: dlna (%s, pid %d)", cmdName, cmd.Process.Pid)
	return nil
}

func (sm *sourceManager) stopAll() {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.stopAllLocked()
}

func (sm *sourceManager) stopAllLocked() {
	for name, units := range sourceUnits {
		if !sourceAvailable(name) {
			continue
		}
		for _, u := range units {
			if _, err := systemctl("stop", u); err != nil {
				log.Printf("systemctl stop %s: %v", u, err)
			}
		}
	}
	// The directly spawned one (DLNA) must be stopped too
	for name, cmd := range sm.procs {
		if cmd != nil && cmd.Process != nil {
			log.Printf("Stopping source: %s (pid %d)", name, cmd.Process.Pid)
			cmd.Process.Signal(os.Interrupt)
			cmd.Process.Kill()
		}
		delete(sm.procs, name)
	}
	sm.active = "idle"
}

// sourceStates provides each source's availability to /api/status (the UI uses it to gray out buttons).
func sourceStates() map[string]bool {
	return map[string]bool{
		"airplay":   sourceAvailable("airplay"),
		"dlna":      sourceAvailable("dlna"),
		"bluetooth": sourceAvailable("bluetooth"),
	}
}
