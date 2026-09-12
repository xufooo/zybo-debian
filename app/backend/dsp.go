// SPDX-License-Identifier: GPL-2.0-only
// dsp.go — FPGA DSP control (S5: dual-mono 6-band biquad cascade + peak limiter)
//
// The DSP logic is implemented inside the Digilent axi_i2s_adi IP (dsp_insert.v / biquad_filter.v /
// limiter.v); its registers live in the IP's own AXI space and are accessed through /dev/mem.
//
// Register map (word offsets, base address 0x43C00000):
//   word 12 (0x30) CTRL : bit0    = full bypass (1 = DSP out of the chain, reset default 1)
//                       bit[3:1] = NR_BANDS (number of active bands 0..6)
//                       bit4    = limiter bypass
//   word 13 (0x34) CIDX : coefficient index 0..29
//   word 14 (0x38) CDAT : write → coef[CIDX] = low 18 bits, index auto-increments next cycle (wraps to 0)
//                        read → coef[CIDX] (18-bit sign extension)
//   word 15 (0x3C) THR  : limiter threshold   Q1.15 (relative to full scale, reset 29491 = 0.9 FS)
//   word 16 (0x40) ATT  : limiter attack coefficient Q1.15 (< 1.0, reset 26214 = 0.8)
//   word 17 (0x44) REL  : limiter release coefficient Q1.15 (> 1.0, reset 32784 = 1.0005)
//   words 18..31        : reserved (FIR taps)
//
// Coefficient index: idx = band*5 + k, k = 0..4 → b0, b1, b2, a1, a2
//           Q3.15 (SHIFT=15, 18 bit), raw RBJ values, a1 is **not pre-negated**:
//           y[n] = b0·x[n] + b1·x[n-1] + b2·x[n-2] - a1·y[n-1] - a2·y[n-2]
//
// Sample rate: **48 kHz**. From S5 on the left and right channels each have their own
//          filter set (dual mono), and the filters run at the 48 kHz frame rate (not the 96 kHz interleaved slot rate), so the coefficients must be designed for 48 kHz.
//
// The Q3.15 range is [-4, 4): for low bands a1 → -2, and insufficient precision flips the sign
// and self-oscillates; measured at 48 kHz there is positive pole margin at ≥60 Hz and self-oscillation below, so the frequency is clamped to a 60 Hz lower bound here.
//
// Note: the two software mirror variables dspEnabled / dspBypass are declared in api.go; they are only read and written here.

package main

import (
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"strings"
	"syscall"
	"unsafe"
)

const (
	dspBaseAddr = 0x43C00000
	dspMapSize  = 0x10000

	regCtrl   = 0x30 // word 12
	regCidx   = 0x34 // word 13
	regCdat   = 0x38 // word 14
	regLimThr = 0x3C // word 15
	regLimAtt = 0x40 // word 16
	regLimRel = 0x44 // word 17

	maxHardwareBands = 6 // = DSP_NB in axi_i2s_adi_v1_2.vhd
	maxSlots         = maxHardwareBands
	coefPerBand      = 5
	coefTotal        = maxHardwareBands * coefPerBand // 30

	sampleRate = 48000.0 // dual mono: the filters run at the frame rate
	coefShift  = 15      // Q3.15
	qOne       = 1 << coefShift
	coefMax    = (1 << 17) - 1 // +131071 (18-bit signed)
	coefMin    = -(1 << 17)    // -131072
	coefMask   = 0x3FFFF       // writing CDAT takes only the low 18 bits
	ctrlMask   = 0x1F          // CTRL readback mask

	// Frequency/Q/gain clamp ranges (Q3.15 stability + perceptual sanity)
	minBandFreq = 60.0
	maxBandFreq = 20000.0
	minBandQ    = 0.1
	maxBandQ    = 10.0
	maxBandGain = 24.0

	// Limiter reset defaults (matching the VHDL). The ms values are derived back from the Q
	// values so dspInit can restore the hardware defaults 26214 / 32784 bit-exactly.
	limDefaultThrQ = 29491 // 0.9 FS
	limDefaultAttQ = 26214 // 0.8
	limDefaultRelQ = 32784 // 1.0005
)

var (
	limDefaultThrDB = 20 * math.Log10(float64(limDefaultThrQ)/qOne)
	limDefaultAttMs = -1000.0 / (sampleRate * math.Log(float64(limDefaultAttQ)/qOne))
	limDefaultRelMs = 1000.0 / (sampleRate * math.Log(float64(limDefaultRelQ)/qOne))
)

var (
	dspMem       []byte
	dspFile      *os.File
	dspAvailable bool

	// Software-side limiter state (kept in sync with the registers, shown by /api/status)
	dspLimiterEnabled = true
	dspLimiterThrDB   = limDefaultThrDB
	dspLimiterAttMs   = limDefaultAttMs
	dspLimiterRelMs   = limDefaultRelMs

	preampWarned bool
)

var errDSPUnavailable = errors.New("DSP unavailable (/dev/mem not mapped or IP not present)")

// ── Presets (6 bands, frequencies map one-to-one to the WebUI sliders) ─────────
//
// Frequency layout: 60 / 150 / 400 / 1k / 3k / 10k Hz —— all ≥ minBandFreq.
// PreampDB is kept in the struct for import/export, but the new hardware has no separate
// preamp gain stage: use the volume (ALSA Master) for static gain, the limiter catches dynamic overload, so all values here are 0.

type slotConfig struct {
	Type   string // "PK","LS","HS","LP","HP","NO","BP","AP","off"
	Freq   float64
	Q      float64
	GainDB float64
}

type eqPreset struct {
	Name     string
	PreampDB float64
	Slots    [maxHardwareBands]slotConfig
}

var presets = map[string]eqPreset{
	"flat": {"Flat", 0, [maxHardwareBands]slotConfig{
		{"off", 0, 0, 0}, {"off", 0, 0, 0}, {"off", 0, 0, 0},
		{"off", 0, 0, 0}, {"off", 0, 0, 0}, {"off", 0, 0, 0},
	}},
	"rock": {"Rock", 0, [maxHardwareBands]slotConfig{
		{"LS", 60, 0.7, 4}, {"PK", 150, 0.7, 2}, {"PK", 400, 0.7, 1},
		{"PK", 1000, 0.7, -2}, {"PK", 3000, 0.7, 2}, {"HS", 10000, 0.7, 3},
	}},
	"jazz": {"Jazz", 0, [maxHardwareBands]slotConfig{
		{"LS", 60, 0.7, 3}, {"PK", 150, 0.7, 2}, {"PK", 400, 0.5, 1},
		{"PK", 1000, 0.5, 1.5}, {"PK", 3000, 0.7, 1}, {"HS", 10000, 0.7, 2},
	}},
	"classical": {"Classical", 0, [maxHardwareBands]slotConfig{
		{"LS", 60, 0.5, 2}, {"PK", 150, 0.5, 1}, {"PK", 400, 0.5, -1},
		{"PK", 1000, 0.7, 1}, {"PK", 3000, 0.7, 1}, {"HS", 10000, 0.7, 2},
	}},
	"vocal": {"Vocal", 0, [maxHardwareBands]slotConfig{
		{"PK", 60, 0.7, -3}, {"PK", 150, 0.7, -1}, {"PK", 400, 0.7, 2},
		{"PK", 1000, 1.0, 4}, {"PK", 3000, 0.7, 2}, {"PK", 10000, 0.7, -1},
	}},
	"bass": {"Bass Boost", 0, [maxHardwareBands]slotConfig{
		{"LS", 60, 0.5, 6}, {"PK", 150, 0.5, 4}, {"PK", 400, 0.5, 2},
		{"PK", 1000, 0.7, 1}, {"PK", 3000, 0.7, 0}, {"HS", 10000, 0.7, 1},
	}},
}

var (
	currentSlots    [maxHardwareBands]slotConfig
	currentPreampDB float64
)

// ── mmap / register access ───────────────────────────────────────────

func dspOpen() error {
	var err error
	dspFile, err = os.OpenFile("/dev/mem", os.O_RDWR|os.O_SYNC, 0)
	if err != nil {
		return fmt.Errorf("open /dev/mem: %w", err)
	}
	dspMem, err = syscall.Mmap(int(dspFile.Fd()), int64(dspBaseAddr), dspMapSize,
		syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		dspFile.Close()
		return fmt.Errorf("mmap 0x%x: %w", dspBaseAddr, err)
	}
	dspAvailable = true
	return nil
}

func dspClose() {
	if dspMem != nil {
		syscall.Munmap(dspMem)
	}
	if dspFile != nil {
		dspFile.Close()
	}
}

func regWrite(off uint32, val uint32) {
	if !dspAvailable {
		return
	}
	*(*uint32)(unsafe.Pointer(&dspMem[off])) = val
}

func regRead(off uint32) uint32 {
	if !dspAvailable {
		return 0
	}
	return *(*uint32)(unsafe.Pointer(&dspMem[off]))
}

// dspInit brings the hardware to a known initial state: write the limiter defaults → bypass released, 0 active bands.
func dspInit() {
	if err := dspSetLimiter(dspLimiterEnabled, dspLimiterThrDB); err != nil {
		log.Printf("DSP limiter init failed: %v", err)
	}
	if err := dspSetLimiterTimes(dspLimiterAttMs, dspLimiterRelMs); err != nil {
		log.Printf("DSP limiter time constant init failed: %v", err)
	}
	if err := dspApplyPreset("flat"); err != nil {
		log.Printf("DSP initial preset failed: %v", err)
	}
	dspEnabled = true
	dspBypass = false
	if err := dspWriteCtrl(); err != nil {
		log.Printf("DSP CTRL init failed: %v", err)
	}
	log.Printf("DSP ready: 6-band dual mono @%.0fHz, limiter %s @%.1fdBFS",
		sampleRate, onOff(dspLimiterEnabled), dspLimiterThrDB)
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// ── CTRL ─────────────────────────────────────────────────────────────

// dspActiveBands returns the number of bands that must be handed to the hardware.
// The band to coefficient slot mapping is fixed (band*5), so "disabling a middle band" uses unity
// coefficients (exact passthrough); NR_BANDS takes the band after the last active one to avoid needless work.
func dspActiveBands() int {
	n := 0
	for i := 0; i < maxHardwareBands; i++ {
		if !isBandOff(currentSlots[i].Type) {
			n = i + 1
		}
	}
	return n
}

func dspWriteCtrl() error {
	if !dspAvailable {
		return errDSPUnavailable
	}
	val := uint32(0)
	// bit0 = 1 means the DSP is out of the chain: explicit bypass, or the master switch turned off
	if dspBypass || !dspEnabled {
		val |= 1
	}
	val |= uint32(dspActiveBands()&0x7) << 1
	if !dspLimiterEnabled {
		val |= 1 << 4
	}
	regWrite(regCtrl, val)
	if back := regRead(regCtrl) & ctrlMask; back != val&ctrlMask {
		return fmt.Errorf("CTRL readback mismatch: wrote %#x read %#x", val&ctrlMask, back)
	}
	return nil
}

// ── Global switches ─────────────────────────────────────────────────

func dspSetEnable(en bool) error {
	dspEnabled = en
	return dspWriteCtrl()
}

func dspSetBypass(bp bool) error {
	dspBypass = bp
	return dspWriteCtrl()
}

// dspSetMasterVolume has no corresponding register in the new hardware: digital volume goes
// through ALSA Master (see api.go handleVolume); the FPGA no longer applies another gain stage.
func dspSetMasterVolume(vol float64) error {
	return nil
}

// dspSetPreamp likewise has no separate gain stage. Imported REW/AutoEQ presets often carry
// a Preamp value: only one log line is emitted here — volume handles static gain, the limiter catches overloads.
func dspSetPreamp(gain float64) error {
	if gain > 0.999 && gain < 1.001 {
		return nil
	}
	if !preampWarned {
		preampWarned = true
		log.Printf("preamp %.2f not applied: the new hardware has no separate preamp stage (volume handles static gain, the limiter catches overloads)", gain)
	}
	return nil
}

// ── Limiter ─────────────────────────────────────────────────────────

// dspSetLimiter sets the limiter switch and threshold (dBFS, e.g. -1.0).
func dspSetLimiter(enabled bool, thrDB float64) error {
	if !dspAvailable {
		return errDSPUnavailable
	}
	if thrDB > 0 {
		thrDB = 0
	}
	if thrDB < -60 {
		thrDB = -60
	}
	thrQ := uint32(math.Round(math.Pow(10, thrDB/20) * qOne))
	if thrQ < 1 {
		thrQ = 1
	}
	if thrQ > qOne { // 1.0 FS = never triggers
		thrQ = qOne
	}
	regWrite(regLimThr, thrQ)
	back := regRead(regLimThr) & coefMask
	if back != thrQ {
		return fmt.Errorf("limiter THR readback mismatch: wrote %d read %d", thrQ, back)
	}
	dspLimiterEnabled = enabled
	dspLimiterThrDB = 20 * math.Log10(float64(thrQ)/qOne)
	return dspWriteCtrl()
}

// dspSetLimiterTimes sets the coefficients from attack/release times (ms).
// First-order smoothing: gain ← gain · k, once per frame (48 kHz)
//
//	k_att = exp(-1/(t_att·fs)) < 1, k_rel = exp(+1/(t_rel·fs)) > 1
func dspSetLimiterTimes(attMs, relMs float64) error {
	if !dspAvailable {
		return errDSPUnavailable
	}
	if attMs < 0.05 {
		attMs = 0.05
	}
	if attMs > 50 {
		attMs = 50
	}
	if relMs < 1 {
		relMs = 1
	}
	if relMs > 2000 {
		relMs = 2000
	}
	kAtt := math.Exp(-1.0 / (attMs / 1000.0 * sampleRate))
	kRel := math.Exp(1.0 / (relMs / 1000.0 * sampleRate))
	attQ := uint32(math.Round(kAtt * qOne))
	relQ := uint32(math.Round(kRel * qOne))
	if attQ > qOne-1 { // must be < 1.0, otherwise it cannot attenuate
		attQ = qOne - 1
	}
	if attQ < 1 {
		attQ = 1
	}
	if relQ < qOne+1 { // must be > 1.0, otherwise it cannot recover
		relQ = qOne + 1
	}
	regWrite(regLimAtt, attQ)
	regWrite(regLimRel, relQ)
	if back := regRead(regLimAtt) & coefMask; back != attQ {
		return fmt.Errorf("limiter ATT readback mismatch: wrote %d read %d", attQ, back)
	}
	if back := regRead(regLimRel) & coefMask; back != relQ {
		return fmt.Errorf("limiter REL readback mismatch: wrote %d read %d", relQ, back)
	}
	dspLimiterAttMs = attMs
	dspLimiterRelMs = relMs
	return nil
}

// ── Coefficient writes ─────────────────────────────────────────────

func dspWriteBand(band int, b0, b1, b2, a1, a2 int32) {
	regWrite(regCidx, uint32(band*coefPerBand))
	for _, v := range [coefPerBand]int32{b0, b1, b2, a1, a2} {
		regWrite(regCdat, uint32(v)&coefMask)
	}
	regRead(regCtrl) // read a register on the same slave to make sure the posted writes have landed
}

// decodeCoefReadback restores a coefficient from a CDAT readback value.
//
// The hardware in dsp_insert.v already **sign-extends the 18-bit coefficient to 32 bits**:
//
//	cdat_rd = {{(32-COEF_W){coef_sel[COEF_W-1]}}, coef_sel}
//
// so it can simply be interpreted as int32 here; do **not** sign-extend it by 18 bits again,
// otherwise negative coefficients come back 2^18 too small (-23907 would read as -286051).
func decodeCoefReadback(v uint32) int32 {
	return int32(v)
}

func dspReadCoeff(idx int) int32 {
	if idx < 0 || idx >= coefTotal {
		return 0
	}
	regWrite(regCidx, uint32(idx))
	return decodeCoefReadback(regRead(regCdat))
}

// dspDumpCoeffs reads back all 30 coefficients (for self-check/debugging).
func dspDumpCoeffs() []int32 {
	out := make([]int32, coefTotal)
	for i := range out {
		out[i] = dspReadCoeff(i)
	}
	return out
}

// ── Per-band configuration ───────────────────────────────────────────────

func isBandOff(t string) bool {
	return t == "" || strings.EqualFold(t, "off") || strings.EqualFold(t, "none")
}

func sanitizeBand(sc slotConfig) slotConfig {
	if sc.Type == "" {
		sc.Type = "PK"
	}
	sc.Type = strings.ToUpper(sc.Type)
	if sc.Freq < minBandFreq {
		sc.Freq = minBandFreq
	}
	if sc.Freq > maxBandFreq {
		sc.Freq = maxBandFreq
	}
	if sc.Freq > sampleRate*0.45 { // Nyquist margin
		sc.Freq = sampleRate * 0.45
	}
	if sc.Q < minBandQ {
		sc.Q = minBandQ
	}
	if sc.Q > maxBandQ {
		sc.Q = maxBandQ
	}
	if sc.GainDB > maxBandGain {
		sc.GainDB = maxBandGain
	}
	if sc.GainDB < -maxBandGain {
		sc.GainDB = -maxBandGain
	}
	return sc
}

func checkCoef(name string, v int32) (int32, error) {
	if v > coefMax || v < coefMin {
		return 0, fmt.Errorf("coefficient %s=%d out of the Q3.15 range [%d,%d] (frequency/gain too extreme)",
			name, v, coefMin, coefMax)
	}
	return v, nil
}

// designBiquad designs one band following the RBJ cookbook and returns Q3.15 integer coefficients.
func designBiquad(sc slotConfig) (int32, int32, int32, int32, int32, error) {
	var b0, b1, b2, a1, a2 int32
	switch sc.Type {
	case "PK":
		b0, b1, b2, a1, a2 = rbjPeakingEQ(sc.Freq, sc.Q, sc.GainDB, sampleRate)
	case "LS":
		b0, b1, b2, a1, a2 = rbjLowShelf(sc.Freq, sc.Q, sc.GainDB, sampleRate)
	case "HS":
		b0, b1, b2, a1, a2 = rbjHighShelf(sc.Freq, sc.Q, sc.GainDB, sampleRate)
	case "LP", "LPQ":
		b0, b1, b2, a1, a2 = rbjLowPass(sc.Freq, sc.Q, sampleRate)
	case "HP", "HPQ":
		b0, b1, b2, a1, a2 = rbjHighPass(sc.Freq, sc.Q, sampleRate)
	case "NO":
		b0, b1, b2, a1, a2 = rbjNotch(sc.Freq, sc.Q, sampleRate)
	case "BP":
		b0, b1, b2, a1, a2 = rbjBandPass(sc.Freq, sc.Q, sampleRate)
	case "AP":
		b0, b1, b2, a1, a2 = rbjAllPass(sc.Freq, sc.Q, sampleRate)
	default:
		return 0, 0, 0, 0, 0, fmt.Errorf("unknown filter type: %s", sc.Type)
	}

	for _, c := range []struct {
		name string
		v    int32
	}{{"b0", b0}, {"b1", b1}, {"b2", b2}, {"a1", a1}, {"a2", a2}} {
		if _, err := checkCoef(c.name, c.v); err != nil {
			return 0, 0, 0, 0, 0, err
		}
	}
	return b0, b1, b2, a1, a2, nil
}

// dspSetSlot configures one band. band is 0..5, matching the WebUI slider order.
func dspSetSlot(band int, sc slotConfig) error {
	if !dspAvailable {
		return errDSPUnavailable
	}
	if band < 0 || band >= maxHardwareBands {
		return fmt.Errorf("band %d out of range (0..%d)", band, maxHardwareBands-1)
	}

	if isBandOff(sc.Type) {
		// unity coefficients = exact passthrough (y = 32768·x >> 15 = x); the band mapping is unchanged
		dspWriteBand(band, qOne, 0, 0, 0, 0)
		currentSlots[band] = slotConfig{Type: "off"}
		return dspWriteCtrl()
	}

	sc = sanitizeBand(sc)
	b0, b1, b2, a1, a2, err := designBiquad(sc)
	if err != nil {
		return fmt.Errorf("band %d: %w", band, err)
	}
	dspWriteBand(band, b0, b1, b2, a1, a2)
	currentSlots[band] = sc
	return dspWriteCtrl()
}

// getBandConfig returns the current configuration of one band (for export/status display).
func getBandConfig(band int) slotConfig {
	if band < 0 || band >= maxHardwareBands {
		return slotConfig{Type: "off"}
	}
	return currentSlots[band]
}

func dspApplyPreset(name string) error {
	p, ok := presets[name]
	if !ok {
		return fmt.Errorf("unknown preset: %s", name)
	}
	if !dspAvailable {
		return errDSPUnavailable
	}
	for i, sc := range p.Slots {
		if err := dspSetSlot(i, sc); err != nil {
			return fmt.Errorf("preset %s band %d: %w", name, i+1, err)
		}
	}
	if p.PreampDB != 0 {
		dspSetPreamp(math.Pow(10.0, p.PreampDB/20.0))
	}
	currentPreampDB = p.PreampDB
	return nil
}

// ── Self-check ──────────────────────────────────────────────────────

// dspExpectedCoeffs computes the expected value of all 30 coefficients from the current software state, for readback verification.
func dspExpectedCoeffs() []int32 {
	out := make([]int32, coefTotal)
	for band := 0; band < maxHardwareBands; band++ {
		sc := currentSlots[band]
		vals := [coefPerBand]int32{qOne, 0, 0, 0, 0}
		if !isBandOff(sc.Type) {
			if b0, b1, b2, a1, a2, err := designBiquad(sanitizeBand(sc)); err == nil {
				vals = [coefPerBand]int32{b0, b1, b2, a1, a2}
			}
		}
		copy(out[band*coefPerBand:], vals[:])
	}
	return out
}

// dspSelfCheck reads back CTRL + all coefficients and compares them against the software expectation.
func dspSelfCheck() error {
	if !dspAvailable {
		return errDSPUnavailable
	}
	want := dspExpectedCoeffs()
	got := dspDumpCoeffs()
	for i := range want {
		if want[i] != got[i] {
			return fmt.Errorf("coefficient %d (band %d, #%d) mismatch: expected %d read back %d",
				i, i/coefPerBand, i%coefPerBand, want[i], got[i])
		}
	}
	ctrl := regRead(regCtrl) & ctrlMask
	if nb := int((ctrl >> 1) & 0x7); nb != dspActiveBands() {
		return fmt.Errorf("NR_BANDS mismatch: expected %d read back %d", dspActiveBands(), nb)
	}
	return nil
}

// ── Q3.15 conversion ────────────────────────────────────────────────

func floatToQ315(val float64) int32 {
	return int32(math.Round(val * float64(qOne)))
}

// ── Status snapshot ─────────────────────────────────────────────────

// dspStatusSnapshot assembles the DSP status (used by /api/status, WebSocket pushes and the self-check).
func dspStatusSnapshot() DSPStatus {
	st := DSPStatus{
		Available:   dspAvailable,
		Enabled:     dspEnabled,
		Bypass:      dspBypass,
		Preset:      currentPreset,
		BandsActive: dspActiveBands(),
		Limiter:     dspLimiterEnabled,
		LimThrDB:    dspLimiterThrDB,
		LimAttMs:    dspLimiterAttMs,
		LimRelMs:    dspLimiterRelMs,
		Bands:       make([]BandStatus, maxHardwareBands),
	}
	for i := 0; i < maxHardwareBands; i++ {
		sc := currentSlots[i]
		st.Bands[i] = BandStatus{Type: sc.Type, Freq: sc.Freq, GainDB: sc.GainDB, Q: sc.Q}
	}
	return st
}
