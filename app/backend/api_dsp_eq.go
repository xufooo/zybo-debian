package main

// POST /api/dsp/eq/parse — parse and fit only; **nothing is stored and nothing is downloaded to hardware**.
//
// Why a separate endpoint: the panel's "import EQ" wants "turn an external curve into **one card of the current type**",
// not "quietly add an entry to the preset library + replace the whole chain". The old /api/dsp/presets/import did both —
// once the panel dropped the genre preset list, that side effect became an invisible hand (audio changed after the import,
// yet the card cannot be found in the UI), so the two jobs were split: **parsing belongs to this endpoint, which card it becomes belongs to the panel**.
//
// Input: request body = REW / EqualizerAPO / AutoEQ text, or a single preset JSON object from this device.
// Output: {ok, name, bands:[ChainItem…], count, warnings:[…]}
//
// Note: this handler **deliberately does not call** importAndApply / saveUserPreset — the side effects of that path
// are exactly why this endpoint exists, and the test (api_dsp_eq_test.go) checks that "not a single file was added on disk".

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

func handleDSPEQParse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		httpError(w, http.StatusBadRequest, "failed to read request body: "+err.Error())
		return
	}
	text := strings.TrimSpace(string(raw))
	if text == "" {
		httpError(w, http.StatusBadRequest, "empty request body")
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		name = "Imported EQ"
	}

	// 1) preset JSON from this device (single object): hand back its own chain unchanged
	if strings.HasPrefix(text, "{") {
		var single Preset
		if err := json.Unmarshal(raw, &single); err != nil || strings.TrimSpace(single.Name) == "" {
			httpError(w, http.StatusBadRequest, "this does not look like a preset JSON from this device (missing name)")
			return
		}
		if err := validatePreset(&single); err != nil {
			httpError(w, http.StatusBadRequest, "invalid preset: "+err.Error())
			return
		}
		warns := []string{}
		if why := presetUnsupported(single); len(why) > 0 {
			warns = append(warns, "this preset uses effects this device does not have yet: "+strings.Join(why, "; "))
		}
		writeJSON(w, map[string]any{
			"ok": true, "name": single.Name, "bands": single.Chain,
			"count": len(single.Chain), "warnings": warns,
		})
		return
	}

	// 2) REW / EqualizerAPO / AutoEQ text: parse + fit within this device's band limit
	pe, perr := parseEqualizerAPO(text)
	if perr != nil && pe == nil {
		httpError(w, http.StatusBadRequest, perr.Error())
		return
	}
	p, warns, err := presetFromAPO(pe, name, bandLimit())
	if err != nil {
		httpError(w, http.StatusBadRequest, "parse failed: "+err.Error())
		return
	}
	if perr != nil {
		warns = append(warns, perr.Error())
	}
	if warns == nil {
		warns = []string{}
	}
	writeJSON(w, map[string]any{
		"ok": true, "name": p.Name, "bands": p.Chain,
		"count": len(p.Chain), "warnings": warns,
	})
}
