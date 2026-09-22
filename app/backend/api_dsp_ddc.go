package main

// api_dsp_ddc.go -- loading interface for device correction (ViPER DDC / .vdc)
//
// Isomorphic to /api/dsp/ir: POST carries the file body (here **text**), GET reports the state.
// An empty body = turn DDC off (kept distinct from "forgot to send the field": clearing requires
// an explicit empty body + ?off=1).

import (
	"io"
	"net/http"
	"strconv"
)

func handleDSPDDC(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, ddcView())
	case http.MethodPost:
		if r.URL.Query().Get("off") != "" {
			ddcClear()
			dropChainItem("ddc")
			if err := applyChain(currentUserChain, currentPreampDB, 0); err != nil {
				httpError(w, http.StatusInternalServerError, "re-apply after turning DDC off failed: "+err.Error())
				return
			}
			writeJSON(w, map[string]any{"ok": true, "on": false})
			return
		}
		raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			httpError(w, http.StatusBadRequest, "reading request body failed: "+err.Error())
			return
		}
		if len(raw) == 0 {
			httpError(w, http.StatusBadRequest, "empty request body (the POST body should be .vdc text; use ?off=1 to turn it off)")
			return
		}
		// -- Two paths: **by default load the file's own sections as-is** (native); only `?sections=N` goes through fitting --
		//
		// Why the default changed (2026-09-20, corrected after a per-file audit plus numerical checks):
		//   the reference implementation `vdc.c` **only reads the table and designs no filters** (`a1 = -val`
		//   is loaded, `w1 = x1 - a1*v1 - a2*v2`), and the net form is `1 - a_file*z^-1 - a_file*z^-2` --
		//   **exactly the same storage** as our `vdcToQ315`.
		//   Computed under this convention the file's own sections have an **absolute gain of about 0 dB**
		//   (Butterworth.vdc at 1 kHz = -0.13 dB), so they can be cascaded directly and **no normalisation
		//   is needed**.
		//   ⚠️ Correcting that year's conclusion: the code once said "each section has a DC gain of 2.8-5x,
		//      the cascade is +240 dB, b is only the shape and cannot be used directly"
		//      -- that was a **misreading with the denominator sign flipped**, and it is exactly what made
		//      the whole DDC path switch to Butterworth fitting.
		//   How far off it was: Butterworth.vdc with its 20 sections as-is => 12 kHz **-87.4 dB** (the
		//   file's real steepness); fitted into 16 sections => -73.6 dB. **Loading as-is is the right
		//   reference.**
		native := r.URL.Query().Get("sections") == "" // passing sections means "I want fitting"
		name := r.URL.Query().Get("name")
		var n int
		var fc, dev float64
		var err2 error
		if native {
			n, err2 = setDDCFromVDC(raw, name)
		} else {
			nsec := 4
			if x, e := strconv.Atoi(r.URL.Query().Get("sections")); e == nil && x >= 1 {
				if x > hwMaxSections {
					x = hwMaxSections
				}
				nsec = x
			}
			n, fc, dev, err2 = fitDDCFromVDC(raw, name, nsec)
		}
		if err2 != nil {
			httpError(w, http.StatusBadRequest, "cannot read this DDC: "+err2.Error())
			return
		}
		_ = dev
		// Persist the raw .vdc: if the chain still references this name after a restart, applyChain reads
		// it back by name (without that read-back the DDC would silently disappear while the chain still
		// looks correct).
		warn := ""
		if err := saveDDCFile(r.URL.Query().Get("name"), raw); err != nil {
			warn = "DDC was not persisted (it must be loaded again after a restart): " + err.Error()
		}
		// Same as IR: loading = using it; if the chain has no DDC item, add one (with the name, so it reads back after a restart)
		ensureChainItem("ddc", ddcLoadedName())
		// Also record the section count in the chain item: otherwise read-back by name on restart/re-apply falls back to the default 4 sections
		setChainItemParam("ddc", "sections", float64(n))
		natFlag := 0.0
		if native {
			natFlag = 1
		}
		setChainItemParam("ddc", "native", natFlag)
		ddcSnap := captureChainState() // ⚠️ Same as above: the snapshot must also cover the state the handler changed itself
		if err := applyChain(currentUserChain, currentPreampDB, 0); err != nil {
			ddcSnap.restore()
			ddcClear()
			httpError(w, http.StatusInternalServerError, "re-apply after loading DDC failed: "+err.Error())
			return
		}
		if warn != "" {
			writeJSON(w, map[string]any{"ok": true, "warning": warn, "view": ddcView()})
			return
		}
		v := ddcView()
		v["ok"] = true
		v["loaded"] = n
		v["fit_fc_hz"] = fc
		v["fit_max_dev_db"] = dev
		writeJSON(w, v)
	default:
		httpError(w, http.StatusMethodNotAllowed, "only GET / POST are supported")
	}
}
