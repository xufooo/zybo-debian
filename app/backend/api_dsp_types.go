// SPDX-License-Identifier: GPL-2.0-only
// api_dsp_types.go — the HTTP API for "effect types"
//
// Semantics (matching the UI):
//
//	A type = a container holding **its own set of effect cards** (different types can
//	have completely different cards: headphones may have 3, an amplifier may have only
//	the single "limiter protection" card).
//	Each card = { effect kind, on/off, strength tier, per-section values }.
//
// The backend only **stores and returns** these settings faithfully: it does not
// interpret "what effect this card is" (that is the UI's effect catalogue) — but
// **the path into the hardware is not here**: after the UI has turned cards into a
// chain it still goes through /api/dsp/chain, where chainToSlots checks whether it
// fits and whether the type is known. So nothing stuffed into a type file can bypass
// that validation.
package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
)

// handleDSPTypes lists / saves.
func handleDSPTypes(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		list, err := loadTypes()
		if err != nil {
			httpError(w, http.StatusInternalServerError, "failed to read types: "+err.Error())
			return
		}
		// builtin_names: the built-in type names are reported separately. The UI uses
		// them to decide whether a user type shadows a built-in one (and therefore
		// whether to offer a "restore factory contents"), so the UI **does not need to
		// hard-code the built-in names** (data the UI should not hold anyway).
		names := make([]string, 0, 3)
		for _, t := range builtinTypes() {
			names = append(names, t.Name)
		}
		writeJSON(w, map[string]any{"types": list, "builtin_names": names})
	case http.MethodPost:
		var body struct {
			Name    string       `json:"name"`
			Note    string       `json:"note"`
			Effects []TypeEffect `json:"effects"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
			httpError(w, http.StatusBadRequest, "request body is not valid JSON: "+err.Error())
			return
		}
		if strings.TrimSpace(body.Name) == "" {
			httpError(w, http.StatusBadRequest, "missing name")
			return
		}
		if body.Effects == nil {
			httpError(w, http.StatusBadRequest, "missing effects (the whole effect set; pass [] explicitly to clear it)")
			return
		}
		t := SoundType{Name: body.Name, Note: body.Note, Effects: body.Effects}
		if err := saveUserType(t); err != nil {
			httpError(w, http.StatusBadRequest, "save failed: "+err.Error())
			return
		}
		log.Printf("type: saved effect type %q (%d cards)", t.Name, len(t.Effects))
		writeJSON(w, map[string]any{"ok": true, "name": t.Name})
	default:
		httpError(w, http.StatusMethodNotAllowed, "only GET / POST supported")
	}
}

// handleDSPTypeDelete deletes a **user** type (built-in ones cannot be deleted).
func handleDSPTypeDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpError(w, http.StatusMethodNotAllowed, "only POST supported")
		return
	}
	var body struct{ Name string }
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		httpError(w, http.StatusBadRequest, "request body is not valid JSON: "+err.Error())
		return
	}
	if err := deleteUserType(body.Name); err != nil {
		status := http.StatusNotFound
		if strings.Contains(err.Error(), "built-in") {
			status = http.StatusConflict
		}
		httpError(w, status, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleDSPTypeRename renames a user type (built-in ones cannot be renamed; see types.go for why).
func handleDSPTypeRename(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpError(w, http.StatusMethodNotAllowed, "only POST supported")
		return
	}
	var body struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		httpError(w, http.StatusBadRequest, "request body is not valid JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(body.From) == "" {
		httpError(w, http.StatusBadRequest, "missing from (which type to rename)")
		return
	}
	to, err := renameUserType(body.From, body.To)
	if err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "built-in") {
			status = http.StatusNotFound
		}
		httpError(w, status, err.Error())
		return
	}
	log.Printf("type: renamed effect type %q → %q", body.From, to)
	writeJSON(w, map[string]any{"ok": true, "name": to})
}
