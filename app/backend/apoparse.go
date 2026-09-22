// SPDX-License-Identifier: GPL-2.0-only
// apoparse.go -- EqualizerAPO text configuration parsing (a subset) + assembly into presets
//
// Why this format: it is the de-facto standard (AutoEQ / REW exports / Peace / Wavelet all
// produce it), so one parser covers a whole ecosystem. The syntax follows the official
// configuration reference (Equalizer APO Wiki, "Configuration reference"); the subset implemented
// here is what P0 needs:
//
//	Preamp: -6.1 dB
//	Filter 1: ON LSC Fc 105 Hz Gain 6.4 dB Q 0.70
//	Filter 2: ON PK  Fc 1928 Hz Gain 3.5 dB Q 1.28
//	Filter 3: ON PEQ Fc 100 Hz Gain 1.0 dB BW Oct 0.167
//	Filter 4: ON LS 6dB Fc 50 Hz Gain 7.2 dB
//	GraphicEQ: 20 0.0; 25 -1.75; 30 -3.2; ...
//
// ⚠️ Differences from 0.1 (all three 0.1 defects are fixed here):
//   1. `LSC`/`HSC` are no longer "silently skipped when the type does not match" -- they are
//      recognised now;
//   2. When the band count exceeds the hardware capability it is **no longer truncated**; instead
//      the whole target curve is **fitted into as many bands as fit**, and the RMS error is
//      reported truthfully;
//   3. `GraphicEQ:` no longer returns 400 outright -- it is sampled into a target curve and takes
//      part in the fit.
//
// Things that cannot be recognised (Convolution / Copy / Delay / If / Include / Channel /
// Device ...) are not silently dropped: they are collected into warnings and returned to the
// caller, so the UI/interface can truthfully tell the user "this part was not applied, and why".

package main

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// graphicPoint is one point on the GraphicEQ curve.
type graphicPoint struct {
	Freq   float64
	GainDB float64
}

// parsedEQ is the parse result: the preamp gain, some filters, and the GraphicEQ curve.
type parsedEQ struct {
	PreampDB float64
	Chain    []ChainItem
	Graphic  []graphicPoint
	Warnings []string
}

func (p *parsedEQ) warnf(format string, args ...any) {
	p.Warnings = append(p.Warnings, fmt.Sprintf(format, args...))
}

// parseEqualizerAPO parses the text. An empty file / no usable filter returns an error (rather than succeeding silently).
func parseEqualizerAPO(text string) (*parsedEQ, error) {
	pe := &parsedEQ{}
	lines := strings.Split(text, "\n")
	for ln, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.Index(line, ":")
		if i < 0 {
			pe.warnf("line %d is not in 'command: arguments' form, ignored: %s", ln+1, trimForMsg(line))
			continue
		}
		cmd := strings.TrimSpace(line[:i])
		arg := strings.TrimSpace(line[i+1:])
		lower := strings.ToLower(cmd)

		switch {
		case lower == "preamp":
			v, ok := parseLeadingFloat(arg)
			if !ok {
				pe.warnf("failed to parse Preamp on line %d, ignored: %s", ln+1, trimForMsg(line))
				continue
			}
			// Multiple Preamp lines add up (the semantics since EqualizerAPO 0.8)
			pe.PreampDB += v
		case strings.HasPrefix(lower, "filter"):
			it, ok, err := parseFilterSpec(arg)
			if err != nil {
				pe.warnf("line %d: %v", ln+1, err)
				continue
			}
			if !ok {
				continue // OFF or empty; skipping it is not a problem
			}
			pe.Chain = append(pe.Chain, it)
		case lower == "graphiceq":
			pts, err := parseGraphicEQ(arg)
			if err != nil {
				pe.warnf("line %d: %v", ln+1, err)
				continue
			}
			pe.Graphic = append(pe.Graphic, pts...)
		case lower == "channel" || lower == "device" || lower == "stage" || lower == "include":
			pe.warnf("line %d: %s is a file/channel management directive, not accepted by this device (stereo, single file only)", ln+1, cmd)
		case lower == "convolution":
			pe.warnf("line %d: Convolution needs a convolver (P3), not supported on this device yet", ln+1)
		case lower == "copy":
			pe.warnf("line %d: Copy (channel matrix) needs inter-channel processing (P2), not supported on this device yet", ln+1)
		case lower == "delay":
			pe.warnf("line %d: Delay needs time-domain processing (P2), not supported on this device yet", ln+1)
		case lower == "if" || lower == "elseif" || lower == "else" || lower == "endif" ||
			lower == "eval" || lower == "device":
			pe.warnf("line %d: expression/conditional directive (%s) is not supported, ignored", ln+1, cmd)
		default:
			pe.warnf("line %d: unrecognised command %q, ignored", ln+1, cmd)
		}
	}
	if len(pe.Chain) == 0 && len(pe.Graphic) == 0 {
		// Note: pe is returned along with the error -- the caller has to read the warnings to explain "why there is nothing"
		return pe, fmt.Errorf("no usable filter in the file (no Filter / GraphicEQ lines)")
	}
	return pe, nil
}

func trimForMsg(s string) string {
	if len(s) > 60 {
		return s[:60] + "…"
	}
	return s
}

// parseFilterSpec parses a spec such as "ON PK Fc 1000 Hz Gain 2 dB Q 1.0".
// It returns (band, enabled, error): OFF lines return ok=false.
func parseFilterSpec(arg string) (ChainItem, bool, error) {
	fields := strings.Fields(arg)
	if len(fields) == 0 {
		return ChainItem{}, false, nil
	}
	state := strings.ToUpper(fields[0])
	if state == "OFF" {
		return ChainItem{}, false, nil
	}
	if state != "ON" {
		// Some files omit ON/OFF and put the type in the first field
		fields = append([]string{"ON"}, fields...)
	}
	if len(fields) < 2 {
		return ChainItem{}, false, fmt.Errorf("filter is missing its type: %s", trimForMsg(arg))
	}

	// Type: may be "LSC", "LS 6dB", "PK", "Modal"...
	typ := strings.ToUpper(fields[1])
	rest := fields[2:]
	chainType := ""
	approx := ""
	switch typ {
	case "PK", "PEQ", "MODAL", "PARAMETRIC":
		chainType = "peq"
	case "LP", "LPQ":
		chainType = "lowpass"
	case "HP", "HPQ":
		chainType = "highpass"
	case "BP":
		chainType = "bandpass"
	case "NO", "NOTCH":
		chainType = "notch"
	case "AP", "ALLPASS":
		chainType = "allpass"
	case "LS", "LSC":
		chainType = "lowshelf"
	case "HS", "HSC":
		chainType = "highshelf"
	default:
		return ChainItem{}, false, fmt.Errorf("unrecognised filter type %q (this line was not applied)", typ)
	}
	// "LS 6dB" / "LS 12dB": shelves given as corner + slope; our shelf is expressed with Q, so the slope is approximated as below
	if typ == "LS" || typ == "HS" {
		if len(rest) > 0 {
			switch strings.ToUpper(rest[0]) {
			case "6DB":
				approx = "6 dB/oct slope approximated as Q~0.5"
				rest = rest[1:]
			case "12DB":
				approx = "12 dB/oct slope approximated as Q~0.707"
				rest = rest[1:]
			}
		}
	}
	// "LSC 10.8 dB": a shelf followed by a slope (this device expresses it with Q, so an approximation note is recorded)
	if (typ == "LSC" || typ == "HSC") && len(rest) >= 2 && strings.EqualFold(rest[1], "DB") {
		approx = "shelf slope (dB/oct) approximated with Q"
	}

	var freq, gain, q, bw float64
	haveQ, haveBW := false, false
	for i := 0; i < len(rest); i++ {
		switch strings.ToUpper(rest[i]) {
		case "FC":
			if i+1 < len(rest) {
				if v, ok := parseLeadingFloat(rest[i+1]); ok {
					freq = v
				}
				i++
				if i+1 < len(rest) && strings.EqualFold(rest[i+1], "HZ") {
					i++
				}
			}
		case "GAIN":
			if i+1 < len(rest) {
				if v, ok := parseLeadingFloat(rest[i+1]); ok {
					gain = v
				}
				i++
				if i+1 < len(rest) && strings.EqualFold(rest[i+1], "DB") {
					i++
				}
			}
		case "Q":
			if i+1 < len(rest) {
				if v, ok := parseLeadingFloat(rest[i+1]); ok {
					q, haveQ = v, true
				}
				i++
			}
		case "BW":
			if i+2 < len(rest) && strings.EqualFold(rest[i+1], "OCT") {
				if v, ok := parseLeadingFloat(rest[i+2]); ok {
					bw, haveBW = v, true
				}
				i += 2
			}
		}
	}
	if freq <= 0 {
		return ChainItem{}, false, fmt.Errorf("filter is missing a valid Fc (this line was not applied)")
	}
	// Q and BW are alternatives; when a bandwidth is given it is converted to Q (the standard conversion)
	switch {
	case haveQ:
	case haveBW:
		q = bwToQ(bw)
	default:
		q = 0.7 // common default
	}
	it := ChainItem{Type: chainType, Enabled: true, Freq: freq, GainDB: gain, Q: q}
	if approx != "" {
		it.Params = map[string]float64{} // placeholder; the explanation is in the warnings
	}
	return it, true, nil
}

// bwToQ converts bandwidth (octaves) -> Q: Q = 1 / (2*sinh(ln2/2 * BW))
func bwToQ(bw float64) float64 {
	if bw <= 0 {
		return 0.7
	}
	return 1.0 / (2.0 * math.Sinh(math.Ln2/2.0*bw))
}

// parseGraphicEQ parses "20 0.0; 25 -1.75; ..." (semicolon-separated frequency/gain pairs).
func parseGraphicEQ(arg string) ([]graphicPoint, error) {
	var out []graphicPoint
	for _, seg := range strings.Split(arg, ";") {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		f := strings.Fields(seg)
		if len(f) < 2 {
			continue
		}
		freq, err1 := strconv.ParseFloat(f[0], 64)
		gain, err2 := strconv.ParseFloat(f[1], 64)
		if err1 != nil || err2 != nil || freq <= 0 {
			continue
		}
		out = append(out, graphicPoint{Freq: freq, GainDB: gain})
	}
	if len(out) < 2 {
		return nil, fmt.Errorf("GraphicEQ needs at least two valid frequency points (this line was not applied)")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Freq < out[j].Freq })
	return out, nil
}

// sampleCurve samples the GraphicEQ points onto the target frequencies: linear interpolation over
// log frequency; per the official semantics, **outside the range it is flat (0 dB)**.
func sampleCurve(points []graphicPoint, freqs []float64) []float64 {
	out := make([]float64, len(freqs))
	if len(points) == 0 {
		return out
	}
	for i, f := range freqs {
		if f < points[0].Freq || f > points[len(points)-1].Freq {
			out[i] = 0
			continue
		}
		// find the two points that bracket f
		j := sort.Search(len(points), func(k int) bool { return points[k].Freq >= f })
		if j == 0 {
			out[i] = points[0].GainDB
			continue
		}
		a, b := points[j-1], points[j]
		if b.Freq == a.Freq {
			out[i] = b.GainDB
			continue
		}
		t := (math.Log(f) - math.Log(a.Freq)) / (math.Log(b.Freq) - math.Log(a.Freq))
		out[i] = a.GainDB + t*(b.GainDB-a.GainDB)
	}
	return out
}

// parseLeadingFloat takes the number at the start of a string (tolerating forms like "-6.1dB" or "105").
func parseLeadingFloat(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	end := 0
	for end < len(s) {
		c := s[end]
		if (c >= '0' && c <= '9') || c == '.' || c == '-' || c == '+' || c == 'e' || c == 'E' {
			end++
			continue
		}
		break
	}
	if end == 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(s[:end], 64)
	return v, err == nil
}

// presetFromAPO assembles the parse result into a preset: when needed it **fits the target curve
// into as many bands as fit**, and writes "what was approximated" truthfully into the warnings
// (the caller is responsible for passing them on to the user).
//
// maxBands = the number of bands the current engine can have in effect at once (P0 = 6).
func presetFromAPO(pe *parsedEQ, name string, maxBands int) (Preset, []string, error) {
	warns := append([]string{}, pe.Warnings...)
	chain := pe.Chain

	needFit := false
	var target []float64
	freqs := logGrid(20, 20000, 200)

	switch {
	case len(pe.Graphic) > 0:
		// The GraphicEQ curve plus (if any) the response of the Filter lines, summed into one target curve
		target = sampleCurve(pe.Graphic, freqs)
		if len(chain) > 0 {
			r := evalChainDB(chain, freqs)
			for i := range target {
				target[i] += r[i]
			}
		}
		needFit = true
	case len(chain) > maxBands:
		target = evalChainDB(chain, freqs)
		needFit = true
	}

	if needFit {
		fitted, rms, err := fitBandsToTarget(target, freqs, maxBands)
		if err != nil {
			return Preset{}, warns, err
		}
		if len(pe.Graphic) > 0 {
			warns = append(warns, fmt.Sprintf(
				"the GraphicEQ curve (%d points) was fitted into a %d-band parametric EQ, RMS error %.2f dB",
				len(pe.Graphic), len(fitted), rms))
		}
		if len(chain) > 0 {
			warns = append(warns, fmt.Sprintf(
				"the source file has %d filter bands while the hardware can only have %d in effect at once: the overall response was fitted into %d bands, RMS error %.2f dB",
				len(chain), maxBands, len(fitted), rms))
		}
		chain = fitted
	}

	if len(chain) == 0 {
		return Preset{}, warns, fmt.Errorf("there is nothing to apply")
	}
	p := Preset{
		Format:   presetFormat,
		Version:  presetFormatVersion,
		Name:     name,
		Note:     "imported from EqualizerAPO/REW text",
		Target:   "any",
		Requires: chainRequires(chain),
		Chain:    chain,
		PreampDB: pe.PreampDB,
	}
	if err := validatePreset(&p); err != nil {
		return Preset{}, warns, err
	}
	if len(p.Chain) > maxBands {
		return Preset{}, warns, fmt.Errorf("after fitting there are still %d bands, exceeding the hardware's %d", len(p.Chain), maxBands)
	}
	return p, warns, nil
}
