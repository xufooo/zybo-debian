// SPDX-License-Identifier: GPL-2.0-only
// types.go -- the storage layer for "sound types" (headphones / small speakers / amplifier
// / custom)
//
// Why it does **not** reuse the "presets" machinery:
//
//   - A preset is a **genre** (rock / classical ...): apply it once and you are done, use it
//     and throw it away;
//   - A type is the **long-lived container** for "this is how I listen on this set of
//     equipment": switching a type = swapping the whole set, and changes have to be
//     remembered (across restarts).
//     The two differ in meaning and in lifetime, and their names should not take each other's
//     place either (a user calling a type "headphones" should not collide just because the
//     genre library has no "headphones"), so each gets its own directory.
//
// Storage: the three built-in ones are written in code (one code change takes effect at
// once, with no dependency on whether that file exists in the image);
//
//	user-defined ones are persisted to /var/lib/zybo-audio/types/*.json.
//	Writing is still "temp file + rename", so a power loss never leaves half a JSON.
//
// Why effects are **generic** (id + mode + params + bands) instead of hardcoded fields:
//
//	The effect catalogue lives on the UI side; adding new effects such as "compressor" or
//	"crossfeed" later only changes the UI, and the backend does not have to follow with
//	data-structure changes -- but **what reaches the hardware still goes through
//	chainToSlots' validation**, so nothing stored in a type file can bypass that gate.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	typeFormat    = "zybo-audio-type"
	typeFormatVer = 1
)

// userTypeDir is the directory for user-defined types (can be overridden by an environment
// variable, which helps testing).
var userTypeDir = envOr("ZYBO_TYPE_DIR", "/var/lib/zybo-audio/types")

// TypeEffect is one sound effect setting inside a type.
//
//	ID      : the id in the effect catalogue ("bass"/"vocal"/.../"limiter")
//	Enabled : whether this entry is on
//	Mode    : enum-style parameter (strength "light|mid|strong", limiter mode
//	          "feedback|truepeak")
//	Params  : numeric parameters (threshold, time constants ...)
//	Bands   : the **authoritative** per-section parameters (frequency / gain / Q). After a
//	          slider has been dragged in the UI they may differ from the values computed from
//	          the strength, so this is what gets stored rather than "recomputing it".
//	          Mode is only used to light up the strength button for the tier the user chose.
type TypeEffect struct {
	ID string `json:"id"`
	// Name / Kind / Desc are **the card's own identity**: cards created by "save as new
	// effect" in the panel (id shaped like u_xxx) and cards created by "import EQ" are not in
	// the UI's effect catalogue, so they can only state for themselves what they are called
	// and what kind they are (bands/cross/delay/dyn/limiter).
	// WARNING: these three fields were once missing: the panel sent them up but they were not
	// accepted here => they were lost on save => after a refresh the card had no name and was
	// not treated as an EQ card (its sections could not even enter the chain).
	Name    string             `json:"name,omitempty"`
	Kind    string             `json:"kind,omitempty"`
	Desc    string             `json:"desc,omitempty"`
	Enabled bool               `json:"enabled"`
	Mode    string             `json:"mode,omitempty"`
	Params  map[string]float64 `json:"params,omitempty"`
	Bands   []ChainItem        `json:"bands,omitempty"`
}

// SoundType is a whole set of sound settings.
type SoundType struct {
	Format  string       `json:"format"`
	Version int          `json:"version"`
	Name    string       `json:"name"`
	Note    string       `json:"note,omitempty"`
	Builtin bool         `json:"builtin,omitempty"`
	Effects []TypeEffect `json:"effects"`
	// File is the on-disk filename of a user type (empty for built-ins), used when deleting
	// or renaming.
	File string `json:"-"`
}

func validateType(t *SoundType) error {
	if t.Format != "" && t.Format != typeFormat {
		return fmt.Errorf("not this device's type format (format=%q)", t.Format)
	}
	if t.Version != 0 && t.Version != typeFormatVer {
		return fmt.Errorf("type version %d is not supported (this device supports %d)", t.Version, typeFormatVer)
	}
	t.Format = typeFormat
	t.Version = typeFormatVer
	if strings.TrimSpace(t.Name) == "" {
		return fmt.Errorf("type is missing name")
	}
	if t.Effects == nil {
		return fmt.Errorf("type is missing effects (the whole set of sound settings; pass [] explicitly to clear it)")
	}
	for i, e := range t.Effects {
		if strings.TrimSpace(e.ID) == "" {
			return fmt.Errorf("effect entry %d is missing id", i+1)
		}
		// per-section parameters must pass the same validation as the "chain": a type file
		// must not smuggle in something the hardware does not know either
		for j, b := range e.Bands {
			if err := chainItemCheck(b); err != nil {
				return fmt.Errorf("effect %q section %d: %w", e.ID, j+1, err)
			}
		}
	}
	return nil
}

// builtinTypes are the three types present from the factory.
//
// The contents are not guesswork: these three match the most common compensation directions
// for "three kinds of listening environment" --
//
//   - Headphones: in-ear / over-ear extend well in the low end, but at low volume the bass is
//     perceived weakly => a light bass lift + a little taming of treble harshness;
//   - Small speakers: the physical size means the bass is lacking and the upper mids are
//     bright => a moderate bass lift + a touch more vocal;
//   - Amplifier / large speakers: reproduction capability is already sufficient, **no
//     coloration**, only limiter protection against clipping.
//
// The strength tiers are based on 4 / 6 / 8 dB (matching STRENGTH_DB in the UI file
// webui/index.html); what bands stores are the actual values already computed from the
// strength -- Mode only makes the UI light up that tier, and bands are what count.
//
// Why the basis moved from 2/4/6 to 4/6/8: a 2 dB tilt sits right at the audibility
// threshold, and the automatic headroom then turns a "boost" into "everything gets smaller"
// (the bass band's absolute level barely moves while the upper mids drop -2.4 dB) => the user
// turns a card on and cannot hear it (measured on the board: toggling "super bass" on the
// headphones changed 60Hz by only 0.05 dB). So the three types' defaults moved up one tier
// as well, while keeping each type's Mode and the bands values it stores consistent.
func builtinTypes() []SoundType {
	return []SoundType{
		{
			Format: typeFormat, Version: typeFormatVer, Name: "Headphones", Builtin: true,
			Note: "In-ear / over-ear: light bass lift, a little taming of treble harshness",
			Effects: []TypeEffect{
				// bass: lifting 60/150/400 together = one broad low shelf (lifting only 60 is
				// almost inaudible; V4A "ViPER Bass" / JamesDSP "Dynamic Bass" both use a broad
				// 6~12 dB tilt)
				{ID: "bass", Enabled: true, Mode: "light", Bands: []ChainItem{
					{Type: "peq", Enabled: true, Freq: 60, GainDB: 5, Q: 0.7},
					{Type: "peq", Enabled: true, Freq: 150, GainDB: 4.3, Q: 0.8},
					{Type: "peq", Enabled: true, Freq: 400, GainDB: 2, Q: 0.8},
				}},
				{ID: "soft", Enabled: true, Mode: "light", Bands: []ChainItem{
					{Type: "peq", Enabled: true, Freq: 3000, GainDB: -2, Q: 1.0},
					{Type: "peq", Enabled: true, Freq: 6000, GainDB: -2.5, Q: 1.0},
					{Type: "peq", Enabled: true, Freq: 10000, GainDB: -3.5, Q: 0.8},
				}},
				// crossfeed (headphone soundstage): **off by default**, but the card is laid
				// out so the user can just switch it on. The parameters use bs2b's Light tier
				// (700 Hz / 6.0 dB), the same values as this device's JamesDSP configuration.
				{ID: "spatial", Enabled: false, Params: map[string]float64{
					"fcut_hz": 700, "feed": 60,
				}},
				// dynamic bass (DYN stage): **off by default**, but the card is laid out so the
				// user can just switch it on. Like the other cards it is stored in this type
				// (it used to live in the whole-device state, half in the type and half in the
				// device).
				{ID: "dyn", Enabled: false, Params: map[string]float64{"gain_db": 6}},
				{ID: "limiter", Enabled: true, Mode: "truepeak", Params: map[string]float64{
					"thr_db": 0, "att_ms": 0.09, "rel_ms": 60,
				}},
			},
		},
		{
			Format: typeFormat, Version: typeFormatVer, Name: "Small speakers", Builtin: true,
			Note: "Desktop / Bluetooth speakers: add bass, lift the vocal out of the background",
			Effects: []TypeEffect{
				{ID: "bass", Enabled: true, Mode: "mid", Bands: []ChainItem{
					{Type: "peq", Enabled: true, Freq: 60, GainDB: 8, Q: 0.7},
					{Type: "peq", Enabled: true, Freq: 150, GainDB: 6.8, Q: 0.8},
					{Type: "peq", Enabled: true, Freq: 400, GainDB: 3.2, Q: 0.8},
				}},
				{ID: "vocal", Enabled: true, Mode: "light", Bands: []ChainItem{
					{Type: "peq", Enabled: true, Freq: 400, GainDB: -2, Q: 0.8},
					{Type: "peq", Enabled: true, Freq: 1000, GainDB: 3.6, Q: 1.0},
					{Type: "peq", Enabled: true, Freq: 3000, GainDB: 4, Q: 1.0},
					{Type: "peq", Enabled: true, Freq: 6000, GainDB: 2, Q: 1.0},
				}},
				// dynamic bass (DYN stage): **off by default**, but the card is laid out so the
				// user can just switch it on. Like the other cards it is stored in this type
				// (it used to live in the whole-device state, half in the type and half in the
				// device).
				{ID: "dyn", Enabled: false, Params: map[string]float64{"gain_db": 6}},
				{ID: "limiter", Enabled: true, Mode: "truepeak", Params: map[string]float64{
					"thr_db": 0, "att_ms": 0.09, "rel_ms": 60,
				}},
			},
		},
		{
			Format: typeFormat, Version: typeFormatVer, Name: "Amplifier", Builtin: true,
			Note: "Large speakers / amplifier: no coloration, limiter protection only",
			Effects: []TypeEffect{
				{ID: "bass", Enabled: false, Mode: "mid"},
				{ID: "vocal", Enabled: false, Mode: "mid"},
				{ID: "vocalhd", Enabled: false, Mode: "mid"},
				{ID: "soft", Enabled: false, Mode: "mid"},
				{ID: "night", Enabled: false, Mode: "mid"},
				// dynamic bass (DYN stage): **off by default**, but the card is laid out so the
				// user can just switch it on. Like the other cards it is stored in this type
				// (it used to live in the whole-device state, half in the type and half in the
				// device).
				{ID: "dyn", Enabled: false, Params: map[string]float64{"gain_db": 6}},
				{ID: "limiter", Enabled: true, Mode: "truepeak", Params: map[string]float64{
					"thr_db": 0, "att_ms": 0.09, "rel_ms": 60,
				}},
			},
		},
	}
}

// loadUserTypes reads the user type directory (returns empty when it does not exist).
func loadUserTypes() ([]SoundType, error) {
	entries, err := os.ReadDir(userTypeDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []SoundType
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(userTypeDir, e.Name()))
		if err != nil {
			log.Printf("failed to read type %s, skipping: %v", e.Name(), err)
			continue
		}
		var t SoundType
		if err := json.Unmarshal(data, &t); err != nil {
			log.Printf("type %s is not valid JSON, skipping: %v", e.Name(), err)
			continue
		}
		if err := validateType(&t); err != nil {
			log.Printf("type %s failed validation, skipping: %v", e.Name(), err)
			continue
		}
		t.Builtin = false
		t.File = e.Name()
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// loadTypes returns built-in + user types (user wins on a name collision: a user-modified
// "Headphones" should shadow the built-in one).
func loadTypes() ([]SoundType, error) {
	user, err := loadUserTypes()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []SoundType
	for _, t := range user {
		out = append(out, t)
		seen[t.Name] = true
	}
	for _, t := range builtinTypes() {
		if !seen[t.Name] {
			out = append(out, t)
		}
	}
	return out, nil
}

// saveUserType atomically writes one user type (write a temp file first, then rename).
func saveUserType(t SoundType) error {
	if err := validateType(&t); err != nil {
		return err
	}
	t.Builtin = false
	t.File = ""
	if err := os.MkdirAll(userTypeDir, 0o755); err != nil {
		return fmt.Errorf("failed to create the type directory: %w", err)
	}
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	final := filepath.Join(userTypeDir, presetFileName(t.Name))
	tmp, err := os.CreateTemp(userTypeDir, ".tmp-type-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, final)
}

// deleteUserType deletes a user type by name.
// A built-in type cannot be deleted (it is not on disk) -- the "delete" the UI offers only
// appears for user types.
func deleteUserType(name string) error {
	list, err := loadUserTypes()
	if err != nil {
		return err
	}
	for _, t := range list {
		if t.Name == name {
			return os.Remove(filepath.Join(userTypeDir, t.File))
		}
	}
	if isBuiltinType(name) {
		return fmt.Errorf("%q is a built-in type and cannot be deleted", name)
	}
	return fmt.Errorf("user type %q not found", name)
}

func isBuiltinType(name string) bool {
	for _, t := range builtinTypes() {
		if t.Name == name {
			return true
		}
	}
	return false
}

// renameUserType renames a user type: write the new name first, then delete the old file
// (the worst case on a power loss is "both exist").
func renameUserType(from, to string) (string, error) {
	from = strings.TrimSpace(from)
	to = strings.TrimSpace(to)
	if to == "" {
		return "", fmt.Errorf("the new name cannot be empty")
	}
	if from == to {
		return to, nil
	}
	if isBuiltinType(to) {
		return "", fmt.Errorf("the new name %q collides with a built-in type; please pick another", to)
	}
	list, err := loadUserTypes()
	if err != nil {
		return "", err
	}
	var found *SoundType
	for i := range list {
		if list[i].Name == from {
			found = &list[i]
			break
		}
	}
	if found == nil {
		if isBuiltinType(from) {
			return "", fmt.Errorf("%q is a built-in type and cannot be renamed; use \"Save as\" to make a copy", from)
		}
		return "", fmt.Errorf("user type %q not found", from)
	}
	for _, t := range list {
		if t.Name == to {
			return "", fmt.Errorf("a type named %q already exists", to)
		}
	}
	renamed := *found
	renamed.Name = to
	if err := saveUserType(renamed); err != nil {
		return "", err
	}
	if err := os.Remove(filepath.Join(userTypeDir, found.File)); err != nil {
		log.Printf("failed to delete the old file after the type rename (the new name is already in effect): %v", err)
	}
	return to, nil
}

// findType finds a type by name (user types first).
func findType(name string) (SoundType, error) {
	list, err := loadTypes()
	if err != nil {
		return SoundType{}, err
	}
	for _, t := range list {
		if t.Name == name {
			return t, nil
		}
	}
	return SoundType{}, fmt.Errorf("type %q not found", name)
}
