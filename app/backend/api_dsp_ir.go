package main

// api_dsp_ir.go -- loading and downloading of the convolution IR (the "coefficient upload" half of the FIR slot)
//
// The end-to-end picture (the design rationale is in the FIR slot design notes, §5):
//   ① `POST /api/dsp/ir?name=xxx` with a WAV body -> `loadIR()` (WAV -> 48 kHz -> peak alignment -> Q3.15)
//   ② the coefficients are stored starting at `coefFIRBase` in the **engine coefficient address space** (channel 0 first, channel 1 after)
//   ③ the IR coefficient RAM has **no double bank** (4096×2 words is too expensive to duplicate), so changing the IR takes two COMMITs:
//      first download the layout with the FIR slot bypassed and write the coefficients, then download normally once to make the convolution stage active --
//      that way nothing half-new and half-old is audible while the IR is being changed.
//
// Why persist to disk: after a restart it must be read back automatically (the same pattern as type/state); for the filename the same
// "safe filename" logic as in presets.go is reused (strip path separators, leading dots, and so on).

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

var (
	irMu      sync.Mutex
	irBank    [2][]int32 // per-channel coefficients (length = firTaps)
	irMeta    *IRInfo    // metadata (shown to the UI/log)
	irName    string     // current IR name
	irDirty   bool       // whether there are changes not yet written to the hardware => the next download carries the coefficients
	irLoading bool       // loading in progress: the FIR slot is bypassed
)

// irDir is the directory where IRs are stored (it can be overridden by an environment variable, which helps testing).
var irDir = envOr("ZYBO_IR_DIR", "/var/lib/zybo-audio/irs")

// irNameSafe turns a user-supplied name into a safe filename (used by both IR and DDC; do not let ../ escape).
func irNameSafe(name string) string {
	n := strings.TrimSpace(name)
	n = strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ':' || r == 0 {
			return '_'
		}
		return r
	}, n)
	n = strings.TrimLeft(n, ".")
	if n == "" {
		n = "imported"
	}
	lower := strings.ToLower(n)
	// recognized extensions are kept as-is (IR: .irs/.wav, DDC: .vdc); anything else gets .irs appended
	if !strings.HasSuffix(lower, ".irs") && !strings.HasSuffix(lower, ".wav") &&
		!strings.HasSuffix(lower, ".vdc") {
		n += ".irs"
	}
	return n
}

// loadIRInto parses and remembers one IR: it fills irBank / irMeta / irName and marks it dirty.
// **It does not touch the hardware** -- the hardware write is done by the download that follows (dspDownloadSlotPlan), which keeps this part testable offline.
func loadIRInto(body []byte, name string) (IRInfo, error) {
	banks, info, err := loadIR(body)
	if err != nil {
		return IRInfo{}, err
	}
	// True stereo: each bank carries its own channel (the hardware supports coefFIRBase + ch*4096). For a mono IR,
	// loadIR has already filled the same data into both banks; more than 2 channels is reported faithfully as Downmixed=true.
	irMu.Lock()
	defer irMu.Unlock()
	for c := 0; c < 2; c++ {
		irBank[c] = append(irBank[c][:0], banks[c]...)
	}
	info.Taps = len(banks[0])
	irMeta = &info
	irName = irNameSafe(name)
	irDirty = true
	return info, nil
}

// irPlanCoefs returns the flat sequence "to be written into the FIR coefficient space by this download" (channel 0 then channel 1).
// It returns nil when nothing changed (the IR has 8192 words; copying them on every download would be wasteful).
func irPlanCoefs() []int32 {
	irMu.Lock()
	defer irMu.Unlock()
	if !irDirty || len(irBank[0]) == 0 {
		return nil
	}
	out := make([]int32, 0, 2*len(irBank[0]))
	out = append(out, irBank[0]...)
	if len(irBank[1]) > 0 {
		out = append(out, irBank[1]...)
	}
	return out
}

// irClearDirty is called after the coefficients have **been successfully written to the hardware**.
func irClearDirty() {
	irMu.Lock()
	irDirty = false
	irMu.Unlock()
}

// irView is the state returned to the UI/API.
func irView() map[string]any {
	irMu.Lock()
	defer irMu.Unlock()
	v := map[string]any{
		"available": firAvailable(),
		"name":      irName,
		"dirty":     irDirty,
		"max_taps":  firTaps,
		// whether this stage **is really on this chain** (= whether it will be computed).
		// Why report it separately: the chain of `GET /api/dsp/chain` is derived by chainFromSlots() from the EQ slots,
		// and it **structurally cannot express non-EQ stages such as FIR/DDC** -- looking only at that view would wrongly conclude "not connected".
		"in_chain": currentFIR != nil,
	}
	if irMeta != nil {
		v["info"] = *irMeta
		v["loaded_taps"] = len(irBank[0])
	}
	return v
}

// saveIRFile writes the original WAV to disk (read back after a restart). A write failure is only reported; it does not block loading.
func saveIRFile(name string, body []byte) error {
	if err := os.MkdirAll(irDir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(irDir, irNameSafe(name))
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// irUnload removes the convolution stage from the chain and unloads the in-memory IR.
// Why it is needed: `applyChainEffects` leaves things **untouched** when "the chain has no such item" (the panel's PUT never carries FIR/DDC items,
// and "clear it when absent" would make the user lose the convolution every time they click a card). The cost: a loaded convolution **could not be turned off** --
// the item was deleted from the chain data while `currentFIR` stayed, so the slot count always said "convolution present". Turning it off therefore has to be explicit (the same as ?off=1 for DDC).
func irUnload() {
	irMu.Lock()
	irBank = [2][]int32{}
	irMeta = nil
	irName = ""
	irDirty = false
	irMu.Unlock()
	currentFIR = nil
	dropChainItem("convolver")
}

// applyChainForIR downloads twice with the current chain: first bypassing the FIR to write the coefficients, then making the convolution stage active.
func applyChainForIR() error {
	irMu.Lock()
	irLoading = true
	irMu.Unlock()
	if err := applyChain(currentUserChain, currentPreampDB, 0); err != nil {
		irMu.Lock()
		irLoading = false
		irMu.Unlock()
		return fmt.Errorf("loading the IR (bypass phase) failed: %w", err)
	}
	irMu.Lock()
	irLoading = false
	irMu.Unlock()
	if err := applyChain(currentUserChain, currentPreampDB, 0); err != nil {
		return fmt.Errorf("loading the IR (active phase) failed: %w", err)
	}
	return nil
}

// handleDSPIR -- GET reports the state; POST loads an IR (the request body is a WAV).
func handleDSPIR(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, irView())
	case http.MethodPost:
		// Explicit off switch: if loading means using it, it must be possible not to use it (empty body + ?off=1, the same convention as /api/dsp/ddc)
		if r.URL.Query().Get("off") != "" {
			irUnload()
			if err := applyChain(currentUserChain, currentPreampDB, 0); err != nil {
				httpError(w, http.StatusInternalServerError, "re-download after removing the convolution failed: "+err.Error())
				return
			}
			writeJSON(w, map[string]any{"ok": true, "on": false})
			return
		}
		raw, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		if err != nil {
			httpError(w, http.StatusBadRequest, "reading the request body failed: "+err.Error())
			return
		}
		if len(raw) == 0 {
			httpError(w, http.StatusBadRequest, "the request body is empty (the POST body should be a WAV file)")
			return
		}
		if !firAvailable() {
			httpError(w, http.StatusConflict,
				"this bitstream has no FIR slot (CAP1 bit5): the convolution needs the 0.2 bitstream")
			return
		}
		name := r.URL.Query().Get("name")
		info, err := loadIRInto(raw, name)
		if err != nil {
			httpError(w, http.StatusBadRequest, "this IR cannot be read: "+err.Error())
			return
		}
		warn := ""
		if err := saveIRFile(name, raw); err != nil {
			warn = "the IR could not be written to disk (it will have to be loaded again after a restart): " + err.Error()
		}
		// Load = use it: add a convolution item to the chain if there is none (otherwise every API says ok but the sound does not change)
		ensureChainItem("convolver", irNameSafe(name))
		if err := applyChainForIR(); err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		resp := map[string]any{"ok": true, "name": irNameSafe(name), "info": info}
		if warn != "" {
			resp["warning"] = warn
		}
		writeJSON(w, resp)
	default:
		httpError(w, http.StatusMethodNotAllowed, "only GET / POST are supported")
	}
}

// ── Read back by name (the chain survives a restart, so the coefficients have to come back on their own) ─────
//
// Why this is mandatory: the chain (including the `convolver` item and the IR name) is stored in state.json,
// while **the coefficients live only in memory**. If the chain is restored at boot without reading them back, the FIR runs with 0 coefficients --
// audible silence, with every API still reporting normal (the hardest kind to diagnose). So anything named in the chain is read back by name when it is loaded.

// irLoadedName is the name of the IR currently in memory (empty string when none is loaded).
func irLoadedName() string {
	irMu.Lock()
	defer irMu.Unlock()
	return irName
}

// loadIRNamed reads an IR back from irDir by name (a missing one is an error, so the caller can report it faithfully).
func loadIRNamed(name string) error {
	b, err := os.ReadFile(filepath.Join(irDir, irNameSafe(name)))
	if err != nil {
		return fmt.Errorf("IR %q not found (load it first with POST /api/dsp/ir): %w", name, err)
	}
	_, err = loadIRInto(b, name)
	return err
}

// ddcDir is the directory where DDCs (.vdc) are stored.
var ddcDir = envOr("ZYBO_DDC_DIR", "/var/lib/zybo-audio/ddc")

// saveDDCFile writes the original .vdc to disk (read back after a restart).
func saveDDCFile(name string, body []byte) error {
	if err := os.MkdirAll(ddcDir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(ddcDir, irNameSafe(name))
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// loadDDCNamed reads a .vdc back by name.
//
// native=true: **load the sections carried by the file as-is** (the same route as the reference implementation `vdc.c`, which also only reads the table and does no design).
// native=false: fit `sections` sections with Butterworth (used when saving budget).
func loadDDCNamed(name string, sections int, native bool) error {
	b, err := os.ReadFile(filepath.Join(ddcDir, irNameSafe(name)))
	if err != nil {
		return fmt.Errorf("DDC %q not found (load it first with POST /api/dsp/ddc): %w", name, err)
	}
	if native {
		_, err = setDDCFromVDC(b, name)
		return err
	}
	_, _, _, err = fitDDCFromVDC(b, name, sections)
	return err
}

// ── Loading means wiring: loading an IR / DDC automatically adds an item to the current chain ─────────────
//
// Why this is mandatory: `applyChainEffects` sets `currentFIR` only when **a convolver item appears in the chain**,
// and the FIR nodes/slots are generated from it by `buildChainNodes`. So "POST an IR only"
// would result in the API reporting ok and the coefficient set being loaded, but **the chain having no convolution stage at all** -- no audible change,
// with every API showing normal (painful to diagnose). The same applies to loading a DDC.
//
// The semantics are: **loading = using it**. After loading, an item is added automatically (an existing item of the same name just has its name updated), then the chain is downloaded again.

func ensureChainItem(typ, name string) {
	// first check whether the chain already has this kind of item (compared by the kind normalized through chainEffectType)
	want := typ
	for i := range currentUserChain {
		if k, ok := chainEffectType[normalizeChainType(currentUserChain[i].Type)]; ok {
			if k == want || (want == "convolver" && k == "fir") {
				currentUserChain[i].Enabled = true
				currentUserChain[i].Name = name
				return
			}
		}
	}
	currentUserChain = append(currentUserChain, ChainItem{
		Type: typ, Enabled: true, Name: name,
	})
}

// setChainItemParam records a parameter on the chain item "of this stage" (it is persisted with state.json).
//
// Why it is needed: the DDC is **fitted with N sections**, and when it is read back by name after a restart, not knowing how many sections were chosen
// would fall back to the default 4 sections -- silently throwing away the steepness just obtained. Measured (2026-09-18):
// after `POST /api/dsp/ddc?sections=16`, the by-name read-back inside `applyChain` re-fitted it to 4 sections
// (the reported fit deviation was the value for 16 sections while `sections` said 4; only the contradiction between the two numbers exposed it).
func setChainItemParam(typ, key string, val float64) {
	for i := range currentUserChain {
		if k, ok := chainEffectType[normalizeChainType(currentUserChain[i].Type)]; ok {
			if k == typ || (typ == "convolver" && k == "fir") {
				if currentUserChain[i].Params == nil {
					currentUserChain[i].Params = map[string]float64{}
				}
				currentUserChain[i].Params[key] = val
				return
			}
		}
	}
	currentUserChain = append(currentUserChain, ChainItem{
		Type: typ, Enabled: true, Params: map[string]float64{key: val},
	})
}

// chainItemParam reads the parameter recorded on the chain item (returns def when missing).
func chainItemParam(typ, key string, def float64) float64 {
	for i := range currentUserChain {
		if k, ok := chainEffectType[normalizeChainType(currentUserChain[i].Type)]; ok {
			if k == typ || (typ == "convolver" && k == "fir") {
				if v, ok := currentUserChain[i].Params[key]; ok {
					return v
				}
			}
		}
	}
	return def
}

func dropChainItem(typ string) {
	out := currentUserChain[:0]
	for _, it := range currentUserChain {
		if k, ok := chainEffectType[normalizeChainType(it.Type)]; ok &&
			(k == typ || (typ == "convolver" && k == "fir")) {
			continue
		}
		out = append(out, it)
	}
	currentUserChain = out
}
