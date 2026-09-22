package main

// ir.go — impulse-response (IR) loading: WAV → resample to 48 kHz → truncate/window → Q3.15 coefficients
//
// Why it is needed (user goal: "match what JamesDSP does"): the tonal body of that JamesDSP chain is
// **convolution** (the reference setup runs thepbone-clear_bass-audio.irs + Butterworth.vdc).
// The engine-side convolution kernel is `fir_bank_long.v` (8192 taps/channel = two parallel cores, 16 DSP48s, 100 MHz; see
// the FIR slot design notes, fir-slot-design.md), and this file turns an on-disk IR into coefficients it can consume.
//
// Three things that must be gotten right:
//  1) **Resampling**: IRs are commonly 44.1 kHz (the reference is), while the engine is fixed at 48 kHz — using it directly is wrong
//     (the whole frequency response is pulled down by ~8.8%). A windowed sinc (Blackman, 32 taps) is used here, adding no dependency.
//  2) **Alignment**: a linear-phase IR peaks in the middle (that one peaks at sample 4095 = 92.9 ms), so
//     cutting the first N samples naively would **throw away half the energy**. The window is therefore peak-aligned: centred on the peak,
//     then moved to a causal position (leading part zeroed). The cost is a delay of N/2, which is stated in the docs and the UI hint.
//  3) **Fixed point**: the coefficients are 18-bit signed Q3.15 (±4.0). Anything out of range is scaled as a whole and **reported honestly**,
//     never silently clamped (clamping would clip the IR into something nonlinear).
//
// This file only does "bytes → coefficients + metadata" and never touches hardware; uploads use the dsp cidx ≥ 256 address space.

import (
	"encoding/binary"
	"fmt"
	"math"
)

const (
	irTaps      = 8192         // = the engine's FIR_TAPS (two parallel cores of 4096×2, see fir_bank_long.v)
	irCoefShift = 15           // Q3.15
	irCoefMax   = 3.9999694824 // (2^17-1)/2^15, the largest positive value Q3.15 can represent
)

// IRInfo is the metadata of one load (for the UI/logs; it takes no part in the arithmetic).
type IRInfo struct {
	Taps       int `json:"taps"`        // number of taps actually written (= irTaps)
	SourceRate int `json:"source_rate"` // sample rate of the source file
	Channels   int `json:"channels"`    // channel count of the source file
	// Stereo: **both channels are loaded separately** (true stereo, one coefficient set per side). JamesDSP runs 2-channel IRs
	// as 2×2 true-stereo convolution (the channels branch of convolver1D.c), and this implementation aligns with that.
	Stereo bool `json:"stereo"`
	// Downmixed: the source has >2 channels (e.g. a 4-channel HRTF set) and this implementation has no 2×4×2 matrix,
	// so it can only average to mono (**reported honestly**, without pretending to support it).
	Downmixed bool `json:"downmixed"`
	// ChannelDiff: the largest left/right coefficient difference after loading (1.0 = full scale). ≈0 means the IR is effectively mono.
	ChannelDiff float64 `json:"channel_diff"`
	PeakIndex   int     `json:"peak_index"` // position of the peak in the (resampled) original sequence
	Peak        float64 `json:"peak"`       // absolute value of the peak
	Gain        float64 `json:"gain"`       // scale factor applied so the peak fits into Q3.15 (1.0 = unscaled)
	ZeroPad     int     `json:"zero_pad"`   // leading zeros added to make it causal (= distance from the peak to the window start)
}

// parseWAV parses WAV: PCM 16/24/32-bit and IEEE float32, mono or multichannel.
// For multichannel input it takes the **average across channels**: headphone-correction IRs are usually identical left/right (the reference measures 1e-4 max),
// and averaging is steadier than picking a channel at random; true-stereo IRs (left ≠ right) are unsupported in this version and are stated as such.
// parseWAVTracks parses WAV: PCM 16/24/32-bit and IEEE float32, returning **per-channel tracks** (no averaging).
//
// Why per channel: a true-stereo IR (left ≠ right) must be handled separately — resampling the interleaved
// two-channel data as one mono track stirs the two channels together (losing the stereo image and matching neither channel).
func parseWAVTracks(b []byte) (tracks [][]float64, rate int, err error) {
	if len(b) < 44 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return nil, 0, fmt.Errorf("not a WAV (missing RIFF/WAVE)")
	}
	var (
		fmtTag, bits, ch int
		rateV            int
		data             []byte
	)
	for p := 12; p+8 <= len(b); {
		id := string(b[p : p+4])
		sz := int(binary.LittleEndian.Uint32(b[p+4 : p+8]))
		body := p + 8
		if sz < 0 || body+sz > len(b) {
			sz = len(b) - body // some files have inaccurate length fields, so truncate to the real size
		}
		switch id {
		case "fmt ":
			if sz < 16 {
				return nil, 0, fmt.Errorf("fmt chunk too short")
			}
			fmtTag = int(binary.LittleEndian.Uint16(b[body : body+2]))
			ch = int(binary.LittleEndian.Uint16(b[body+2 : body+4]))
			rateV = int(binary.LittleEndian.Uint32(b[body+4 : body+8]))
			bits = int(binary.LittleEndian.Uint16(b[body+14 : body+16]))
		case "data":
			data = b[body : body+sz]
		}
		p = body + sz
		if sz%2 == 1 {
			p++ // chunks are aligned to an even byte boundary
		}
	}
	if ch <= 0 || rateV <= 0 {
		return nil, 0, fmt.Errorf("WAV is missing the fmt chunk")
	}
	if data == nil {
		return nil, 0, fmt.Errorf("WAV is missing the data chunk")
	}
	bytesPer := bits / 8
	if bytesPer == 0 {
		return nil, 0, fmt.Errorf("unsupported bit depth %d", bits)
	}
	sample := func(off int) (float64, error) {
		switch {
		case fmtTag == 3 && bits == 32: // IEEE float
			return float64(math.Float32frombits(binary.LittleEndian.Uint32(data[off : off+4]))), nil
		case bits == 16:
			return float64(int16(binary.LittleEndian.Uint16(data[off:off+2]))) / 32768.0, nil
		case bits == 24:
			v := int32(data[off]) | int32(data[off+1])<<8 | int32(data[off+2])<<16
			if v&0x800000 != 0 {
				v |= ^0xFFFFFF // sign extension
			}
			return float64(v) / 8388608.0, nil
		case bits == 32:
			return float64(int32(binary.LittleEndian.Uint32(data[off:off+4]))) / 2147483648.0, nil
		}
		return 0, fmt.Errorf("unsupported bit depth %d / format %d", bits, fmtTag)
	}
	n := len(data) / (bytesPer * ch)
	tracks = make([][]float64, ch)
	for c := range tracks {
		tracks[c] = make([]float64, n)
	}
	for i := 0; i < n; i++ {
		for c := 0; c < ch; c++ {
			v, err := sample((i*ch + c) * bytesPer)
			if err != nil {
				return nil, 0, err
			}
			tracks[c][i] = v
		}
	}
	return tracks, rateV, nil
}

// parseWAV is a wrapper kept for old callers: it returns a mono track with the channels **averaged** plus the channel count.
// Used only where stereo does not matter (test cases, old paths); IR loading goes through parseWAVTracks.
func parseWAV(b []byte) (samples []float64, rate, channels int, err error) {
	tracks, rateV, err := parseWAVTracks(b)
	if err != nil {
		return nil, 0, 0, err
	}
	out := make([]float64, len(tracks[0]))
	for i := range out {
		sum := 0.0
		for _, t := range tracks {
			sum += t[i]
		}
		out[i] = sum / float64(len(tracks))
	}
	return out, rateV, len(tracks), nil
}

// resampleSinc resamples x from rate `from` to rate `to` (windowed sinc, 32-tap Blackman).
// IRs are only a few thousand points long, so plain O(n·taps) is enough; FFT is avoided to add no dependency and to keep point-by-point review easy.
func resampleSinc(x []float64, from, to int) []float64 {
	if from == to || len(x) == 0 {
		return append([]float64(nil), x...)
	}
	const half = 16 // taps per side
	nOut := int(float64(len(x)) * float64(to) / float64(from))
	out := make([]float64, nOut)
	ratio := float64(from) / float64(to)
	for i := 0; i < nOut; i++ {
		pos := float64(i) * ratio
		i0 := int(math.Floor(pos))
		var acc, wsum float64
		for k := i0 - half + 1; k <= i0+half; k++ {
			if k < 0 || k >= len(x) {
				continue
			}
			t := pos - float64(k)
			if math.Abs(t) >= float64(half) {
				continue
			}
			// sinc × Blackman
			s := 1.0
			if math.Abs(t) > 1e-12 {
				s = math.Sin(math.Pi*t) / (math.Pi * t)
			}
			w := 0.42 + 0.5*math.Cos(math.Pi*t/float64(half)) +
				0.08*math.Cos(2*math.Pi*t/float64(half))
			acc += x[k] * s * w
			wsum += s * w
		}
		if math.Abs(wsum) > 1e-9 {
			out[i] = acc / wsum // normalise, avoiding passband ripple
		}
	}
	return out
}

// irToCoefs turns an IR into the Q3.15 coefficients fir_bank wants (length = taps).
// Peak alignment → windowed truncation → shift to causal (zero-pad the front) → scale → quantise.
// irQuantiseWindow quantises one track into Q3.15 coefficients for a given window start/gain.
// For true stereo the two channels **must share** start and gain: different starts would put a time offset between the sides,
// and different gains would put a relative level difference between them.
func irQuantiseWindow(x []float64, start, taps int, gain float64) []int32 {
	coefs := make([]int32, taps)
	for i := 0; i < taps; i++ {
		idx := start + i
		if idx < 0 || idx >= len(x) {
			continue
		}
		q := int32(math.Round(x[idx] * gain * (1 << irCoefShift)))
		if q > (1<<17)-1 {
			q = (1 << 17) - 1
		}
		if q < -(1 << 17) {
			q = -(1 << 17)
		}
		coefs[i] = q
	}
	return coefs
}

// irToCoefs is the mono path (old signature kept; test cases and mono IRs still use it):
// find the peak itself → take a peak-aligned window → quantise.
func irToCoefs(x []float64, taps int) (coefs []int32, info IRInfo, err error) {
	if len(x) == 0 {
		return nil, IRInfo{}, fmt.Errorf("IR is empty")
	}
	pk, pv := 0, 0.0
	for i, v := range x {
		if a := math.Abs(v); a > pv {
			pv, pk = a, i
		}
	}
	if pv == 0 {
		return nil, IRInfo{}, fmt.Errorf("IR is all zeros")
	}
	start := pk - taps/2
	gain := 1.0
	if pv > irCoefMax {
		gain = irCoefMax / pv
	}
	coefs = irQuantiseWindow(x, start, taps, gain)
	info = IRInfo{
		Taps: taps, Channels: 1, PeakIndex: pk, Peak: pv, Gain: gain,
		ZeroPad: func() int {
			if start < 0 {
				return -start
			}
			return 0
		}(),
	}
	return coefs, info, nil
}

// loadIR in one step: bytes → 48 kHz → irTaps (8192) × (1 or 2 channels of coefficients) + metadata.
//
// **True stereo (2 channels)**: the two channels are resampled and turned into coefficients separately, sharing one window start and gain.
// The hardware supports one coefficient set per channel anyway (`coefFIRBase + ch*4096`), and JamesDSP also treats a 2-channel IR as
// 2×2 true stereo (see the effects reference replication notes, §13).
// The original code averaged the channels into one and filled both banks with the same set — lossless for the clear_bass the author
// uses now (max left/right difference 6e-5), but for matis-FDS / topjor-srs in his IR library (max difference ≈1.03)
// it throws the stereo information away entirely.
//
// **>2 channels** (a 4-channel HRTF set): this implementation has no 2×4×2 matrix, so it can only average to mono,
// set Downmixed=true and **report it honestly**, without pretending to support it.
func loadIR(b []byte) ([2][]int32, IRInfo, error) {
	tracks, rate, err := parseWAVTracks(b)
	if err != nil {
		return [2][]int32{}, IRInfo{}, err
	}
	rs := make([][]float64, len(tracks))
	for i, t := range tracks {
		rs[i] = resampleSinc(t, rate, 48000)
	}
	var ch2 [2][]float64
	info := IRInfo{SourceRate: rate, Channels: len(rs), Taps: irTaps}
	switch len(rs) {
	case 1:
		ch2[0], ch2[1] = rs[0], rs[0]
	case 2:
		ch2[0], ch2[1] = rs[0], rs[1]
		info.Stereo = true
	default:
		n := len(rs[0])
		mono := make([]float64, n)
		for i := 0; i < n; i++ {
			sum := 0.0
			for _, t := range rs {
				sum += t[i]
			}
			mono[i] = sum / float64(len(rs))
		}
		ch2[0], ch2[1] = mono, mono
		info.Downmixed = true
	}
	pk, pv := 0, 0.0
	for _, t := range ch2 {
		for i, v := range t {
			if a := math.Abs(v); a > pv {
				pv, pk = a, i
			}
		}
	}
	if pv == 0 {
		return [2][]int32{}, IRInfo{}, fmt.Errorf("IR is all zeros")
	}
	start := pk - irTaps/2
	gain := 1.0
	if pv > irCoefMax {
		gain = irCoefMax / pv
	}
	var out [2][]int32
	for c := 0; c < 2; c++ {
		out[c] = irQuantiseWindow(ch2[c], start, irTaps, gain)
	}
	info.PeakIndex, info.Peak, info.Gain = pk, pv, gain
	if start < 0 {
		info.ZeroPad = -start
	}
	var maxd int32
	for i := range out[0] {
		d := out[0][i] - out[1][i]
		if d < 0 {
			d = -d
		}
		if d > maxd {
			maxd = d
		}
	}
	info.ChannelDiff = float64(maxd) / float64(int32(1)<<irCoefShift)
	return out, info, nil
}
