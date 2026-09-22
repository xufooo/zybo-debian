// SPDX-License-Identifier: GPL-2.0-only
// presets.go -- presets: format, built-in library, CRUD for user presets
//
// Design points:
//   · A preset **is not a black box**: it is a ChainItem chain (still adjustable) plus a name,
//     a note and the target device;
//   · The built-in library ships with the image (read-only) while user presets live in a writable
//     directory (one JSON file per preset);
//   · `requires` declares which effect types are used: **those the current engine does not
//     support must be greyed out in the UI**, instead of being force-fed wrong parameters (this is
//     exactly the 0.1 lesson: LSC/HSC were silently skipped and 10 bands became 6).
//   · Files are written as "temp file + rename", so a power cut never leaves half a JSON.

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
	presetFormat        = "zybo-audio-preset"
	presetLibraryFormat = "zybo-audio-preset-library"
	presetFormatVersion = 1
)

// Built-in library and user preset directories. Locations in the released image:
//
//	built-in library /usr/share/zybo-audio/presets/library.json (read-only)
//	user presets     /var/lib/zybo-audio/presets/*.json (writable)
//
// Both can be overridden with environment variables for development and testing.
var (
	presetLibraryPath = envOr("ZYBO_PRESET_LIB", "/usr/share/zybo-audio/presets/library.json")
	userPresetDir     = envOr("ZYBO_PRESET_DIR", "/var/lib/zybo-audio/presets")
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Preset is one audio effect preset. Stage only appears in the built-in library (P0/P2/P3); it
// tells the UI "this one cannot be selected yet, and which engine stage it needs".
type Preset struct {
	Format   string      `json:"format"`
	Version  int         `json:"version"`
	Name     string      `json:"name"`
	Author   string      `json:"author,omitempty"`
	Note     string      `json:"note,omitempty"`
	Target   string      `json:"target,omitempty"`
	Stage    string      `json:"stage,omitempty"`
	Requires []string    `json:"requires,omitempty"`
	Chain    []ChainItem `json:"chain"`
	PreampDB float64     `json:"preamp_db,omitempty"`
	VolumeDB float64     `json:"volume_db,omitempty"`
	// Builtin is only sent back to the UI (false for user presets); it is not written to the file.
	Builtin bool `json:"builtin,omitempty"`
	// File is the on-disk file name of a user preset (empty for the built-in library), used for delete/rename.
	File string `json:"-"`
}

type presetLibrary struct {
	Format  string   `json:"format"`
	Version int      `json:"version"`
	Note    string   `json:"note,omitempty"`
	Presets []Preset `json:"presets"`
}

// validatePreset checks the format, the version and the chain contents. Files imported by the user go through it too.
func validatePreset(p *Preset) error {
	if p.Format != "" && p.Format != presetFormat {
		return fmt.Errorf("not this device's preset format (format=%q)", p.Format)
	}
	if p.Version != 0 && p.Version != presetFormatVersion {
		return fmt.Errorf("preset version %d is not supported (this device supports %d)", p.Version, presetFormatVersion)
	}
	p.Format = presetFormat
	p.Version = presetFormatVersion
	if strings.TrimSpace(p.Name) == "" {
		return fmt.Errorf("preset is missing name")
	}
	for i, it := range p.Chain {
		nt := normalizeChainType(it.Type)
		if nt == "" {
			return fmt.Errorf("item %d is missing type", i+1)
		}
		if _, known := chainTypeToSlot[nt]; known {
			continue
		}
		if _, planned := chainPlannedType[nt]; planned {
			continue // planned types are allowed to exist (greyed out in the UI), but a typo is not
		}
		if _, fx := chainEffectType[nt]; fx {
			// crossfeed / surround in chain-item form: **implemented** (MIX2 / DELAY slots). When the
			// hardware lacks those slots, chainUnsupported() reports it truthfully and the UI greys them out
			// -- that is not "unknown type".
			continue
		}
		return fmt.Errorf("item %d has unknown type=%q", i+1, it.Type)
	}
	// If requires is given it must match the chain contents (so a preset author's mistake cannot grey out or light up the UI wrongly)
	if len(p.Requires) > 0 {
		declared := map[string]bool{}
		for _, r := range p.Requires {
			declared[normalizeChainType(r)] = true
		}
		for _, need := range chainRequires(p.Chain) {
			if !declared[need] {
				return fmt.Errorf("requires is missing %q, which the chain uses", need)
			}
		}
	}
	return nil
}

// presetUnsupported returns the list of reasons the current hardware cannot do it (empty = applies directly).
func presetUnsupported(p Preset) []string {
	bad := chainUnsupported(p.Chain)
	if len(bad) == 0 {
		return nil
	}
	out := make([]string, 0, len(bad))
	for t, why := range bad {
		out = append(out, t+": "+why)
	}
	sort.Strings(out)
	return out
}

// loadPresetLibrary reads the built-in library. A missing library is not an error (common in development); it returns an empty list.
func loadPresetLibrary() ([]Preset, error) {
	data, err := os.ReadFile(presetLibraryPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var lib presetLibrary
	if err := json.Unmarshal(data, &lib); err != nil {
		return nil, fmt.Errorf("failed to parse the built-in library (%s): %w", presetLibraryPath, err)
	}
	if lib.Format != "" && lib.Format != presetLibraryFormat {
		return nil, fmt.Errorf("built-in library format=%q is not %q", lib.Format, presetLibraryFormat)
	}
	if lib.Version != 0 && lib.Version != presetFormatVersion {
		return nil, fmt.Errorf("built-in library version %d is not supported", lib.Version)
	}
	out := make([]Preset, 0, len(lib.Presets))
	for i := range lib.Presets {
		p := lib.Presets[i]
		if err := validatePreset(&p); err != nil {
			return nil, fmt.Errorf("built-in library entry %d (%s): %w", i+1, p.Name, err)
		}
		p.Builtin = true
		out = append(out, p)
	}
	return out, nil
}

// presetFileName maps a preset name to a safe file name (non-ASCII names are allowed; path separators and control characters are not).
func presetFileName(name string) string {
	bad := func(r rune) bool {
		return r == '/' || r == '\\' || r == 0 || r < 0x20 || r == ':'
	}
	s := strings.Map(func(r rune) rune {
		if bad(r) {
			return '_'
		}
		return r
	}, strings.TrimSpace(name))
	s = strings.Trim(s, ". ")
	if s == "" {
		s = "preset"
	}
	return s + ".json"
}

// loadUserPresets reads the user preset directory (returns nothing when it does not exist).
func loadUserPresets() ([]Preset, error) {
	entries, err := os.ReadDir(userPresetDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]Preset, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(userPresetDir, e.Name()))
		if err != nil {
			continue // one unreadable file must not take down the whole list
		}
		var p Preset
		if err := json.Unmarshal(data, &p); err != nil {
			continue
		}
		if err := validatePreset(&p); err != nil {
			continue
		}
		p.File = e.Name()
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// saveUserPreset atomically writes one user preset (temp file first, then rename).
func saveUserPreset(p Preset) error {
	if err := validatePreset(&p); err != nil {
		return err
	}
	p.Builtin = false
	p.File = ""
	if err := os.MkdirAll(userPresetDir, 0o755); err != nil {
		return fmt.Errorf("failed to create the preset directory: %w", err)
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	final := filepath.Join(userPresetDir, presetFileName(p.Name))
	tmp, err := os.CreateTemp(userPresetDir, ".tmp-preset-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, final)
}

// deleteUserPreset deletes a user preset by name (it finds the real file name first instead of guessing).
func deleteUserPreset(name string) error {
	list, err := loadUserPresets()
	if err != nil {
		return err
	}
	for _, p := range list {
		if p.Name == name {
			return os.Remove(filepath.Join(userPresetDir, p.File))
		}
	}
	return fmt.Errorf("user preset %q not found", name)
}

// renameUserPreset renames a user preset: write the new name, then delete the old file.
//
// The order is deliberate (write the new one first, delete the old one second): the worst case
// after a power cut is "both names exist", whereas the reverse would be "both are gone" -- a user
// preset is something the user tuned himself, so a duplicate is preferable.
//
// Only **user presets** may be renamed: the built-in library ships with the image, so renaming one
// would be undone by the next upgrade. The user should "save a copy" instead (what the UI does),
// rather than silently editing something that will be overwritten.
func renameUserPreset(from, to string) (string, error) {
	from = strings.TrimSpace(from)
	to = strings.TrimSpace(to)
	if to == "" {
		return "", fmt.Errorf("the new name must not be empty")
	}
	if from == to {
		return to, nil // no change, not an error
	}
	list, err := loadUserPresets()
	if err != nil {
		return "", err
	}
	var found *Preset
	for i := range list {
		if list[i].Name == from {
			found = &list[i]
			break
		}
	}
	if found == nil {
		// Distinguish "no such preset" from "this one is built-in and cannot be renamed", otherwise the user thinks the feature is broken
		if lib, lerr := loadPresetLibrary(); lerr == nil {
			for _, p := range lib {
				if p.Name == from {
					return "", fmt.Errorf("%q is a built-in preset and cannot be renamed; use 'Save as my preset' to make a copy", from)
				}
			}
		}
		return "", fmt.Errorf("user preset %q not found", from)
	}
	// The new name must not collide with the built-in library (after a collision findPreset hits the user entry first, so the built-in one can never be selected again)
	if lib, lerr := loadPresetLibrary(); lerr == nil {
		for _, p := range lib {
			if p.Name == to {
				return "", fmt.Errorf("the new name %q collides with a built-in preset; please pick another", to)
			}
		}
	}
	// The new name must not collide with another user preset either (identical names overwrite each other: the on-disk file name is the name)
	for _, p := range list {
		if p.Name == to {
			return "", fmt.Errorf("a user preset named %q already exists", to)
		}
	}
	renamed := *found
	renamed.Name = to
	if err := saveUserPreset(renamed); err != nil {
		return "", err
	}
	if err := os.Remove(filepath.Join(userPresetDir, found.File)); err != nil {
		// The new file is already written; failing to delete the old one only leaves a duplicate and must not make the user think the rename failed
		log.Printf("preset: failed to delete the old file after rename (the new name is already in effect): %v", err)
	}
	return to, nil
}

// findPreset looks in the user presets first, then in the built-in library.
func findPreset(name string) (Preset, error) {
	if list, err := loadUserPresets(); err == nil {
		for _, p := range list {
			if p.Name == name {
				return p, nil
			}
		}
	}
	if list, err := loadPresetLibrary(); err == nil {
		for _, p := range list {
			if p.Name == name {
				return p, nil
			}
		}
	}
	return Preset{}, fmt.Errorf("preset %q not found", name)
}

// applyPreset applies a preset: the chain goes to the hardware; when the preset carries a preamp that one sets the headroom (otherwise it is automatic).
func applyPreset(p Preset) error {
	return applyChain(p.Chain, p.PreampDB, 0)
}
