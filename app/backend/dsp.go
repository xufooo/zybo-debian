// dsp.go — FPGA DSP control (S5: dual-mono 6-section biquad cascade + peak limiter)
//
// The DSP logic lives inside the Digilent axi_i2s_adi IP (dsp_insert.v / biquad_filter.v /
// limiter.v); its registers sit in the IP's own AXI space and are accessed through /dev/mem.
//
// Register map (word offsets, base address 0x43C00000):
//   word 12 (0x30) CTRL : bit0    = global bypass (1 = DSP not in the chain; reset default 1)
//                      bit[3:1] = NR_BANDS (number of active sections 0..6)
//                      bit4    = limiter bypass
//   word 13 (0x34) CIDX : coefficient index 0..29
//   word 14 (0x38) CDAT : write → coef[CIDX] = low 18 bits, index auto-increments next cycle (wraps to 0)
//                       read  → coef[CIDX] (18-bit sign-extended)
//   word 15 (0x3C) THR  : limiter threshold   Q1.15 (relative to full scale, reset 29491 = 0.9 FS)
//   word 16 (0x40) ATT  : limiter attack coefficient Q1.15 (< 1.0, reset 26214 = 0.8)
//   word 17 (0x44) REL  : limiter release coefficient Q1.15 (> 1.0, reset 32784 = 1.0005)
//   words 18..31        : reserved (FIR taps)
//
// Coefficient index: idx = band*5 + k, k = 0..4 → b0, b1, b2, a1, a2
//           Q3.15 (SHIFT=15, 18 bit), using RBJ's raw values, a1 is **not pre-negated**:
//           y[n] = b0·x[n] + b1·x[n-1] + b2·x[n-2] - a1·y[n-1] - a2·y[n-2]
//
// Sample rate: **48 kHz**. From S5 on, left and right each have their own filter bank (dual
//          mono); the filters run at the frame rate of 48 kHz (not the 96 kHz interleaved slot
//          rate), so the coefficients must be designed for 48 kHz.
//
// Q3.15's range is [-4, 4): a low-band a1 → -2, and insufficient precision makes it flip sign
// and self-oscillate. Measured, at 48 kHz there is positive pole margin at ≥60 Hz and
// self-oscillation below that, so the frequency is clamped to a 60 Hz lower bound here.
//
// Note: the two software mirror variables dspEnabled / dspBypass are declared in api.go; this file only reads and writes them.

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

	// legacyBands is the **0.1 bitstream** path: the hardware is a fixed 6-section biquad cascade
	// whose coefficient window is only 30 words (= DSP_NB × 5). It is **no longer the chain's
	// capacity** — it only says "how wide the old hardware window is": the coefficient readback
	// range, the old path's whole-window write and the export are all sized by it.
	legacyBands = 6 // = DSP_NB in axi_i2s_adi_v1_2.vhd
	// maxBands is the **compile-time dimension** of the slot array / coefficient table: the total
	// number of sections the 0.2 engine can hold (= dsp_slot.go's maxSections = NSEC = 48;
	// 2026-09-20 widened NSEC from 24 to 36, 2026-09-21 to 48, otherwise the real chain
	// "DDC 18 sections + convolution + XHIFI + ViPERBass + VSE" would not fit).
	// Note: it follows NSEC **only**; the slot count (NSLOT=24) does not take part in this dimension.
	// Compile-time arrays are all sized by it, so that "runtime capacity changed but the array did
	// not follow" cannot happen.
	maxBands    = maxSections
	coefPerBand = 5
	// coefTotal is the old bitstream's coefficient window width (30 words).
	// The 0.2 window varies with the section count; use coefWindow() to get it.
	coefTotal = legacyBands * coefPerBand // 30

	sampleRate = 48000.0 // dual mono: the filters run at the frame rate
	coefShift  = 15      // Q3.15
	qOne       = 1 << coefShift
	coefMax    = (1 << 17) - 1 // +131071 (18-bit signed)
	coefMin    = -(1 << 17)    // -131072
	coefMask   = 0x3FFFF       // writing CDAT takes only the low 18 bits
	ctrlMask   = 0x1F          // CTRL bits present in both 0.1 and 0.2 (bypass / section count / limiter bypass)

	// frequency / Q / gain clamping ranges (Q3.15 stability + reasonable sound)
	minBandFreq = 60.0
	maxBandFreq = 20000.0
	minBandQ    = 0.1
	maxBandQ    = 10.0
	maxBandGain = 24.0

	// ctrlHeadroom: CTRL.bit19 = in-chain headroom switch (0.2 bitstream; capability in CAP1 bit10).
	ctrlHeadroom = 1 << 19
	// preampFreeBoostDB: with in-chain headroom on, the dB that is "allowed not to be
	// compensated" — i.e. what is left for the limiter to shave off peaks.
	// Why not 0: compensating everything (preamp following the boost) turns every effect into
	// "quieter overall", and what the user hears is "turning the effect on = the sound goes
	// dull" (measured on the board: toggling super bass at 60 Hz changes it by only 0.05 dB);
	// compensating nothing at all keeps the limiter **pressing all the time** (dynamics crushed,
	// and the effect still inaudible).
	// 3 dB is the compromise that measured acceptable: the tilt is largely preserved and the
	// limiter only works on peaks.
	preampFreeBoostDB = 3.0
	// headroomDB corresponds to RTL's HEADROOM=3 bits: each step is 6 dB, 18 dB down in total
	// inside the chain. The software knows only this one number: with headroom on, both the
	// pre-compression policy and the limiter threshold are computed from it.
	headroomDB = 18.0

	// limiter reset defaults (matching the VHDL). The ms values are derived back from the Q
	// values so that dspInit can restore the hardware defaults 26214 / 32784 bit for bit.
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

	// software-side limiter state (kept consistent with the registers, used for /api/status display)
	// ⚠️ Off by default (since 2026-09-15). Running a bit-exact fixed-point model of limiter.v on
	//    real 0 dBFS masters, it rewrites the waveform about 1770 times per second (discontinuities
	//    with a median of -40 dBFS and peaks of -14 dBFS) — audible glitches on full-scale material.
	//    It was originally there to leave headroom for EQ boost, but the automatic preamp already
	//    guarantees a cascade peak of ≤ -3 dBFS, so on full-scale material it is only harmful. It
	//    remains available when switched on in the UI.
	// ⚠️ On by default (changed 2026-09-17). It used to default to off because the old feedback
	// form produced thousands of waveform discontinuities per second on full-scale material
	// (see §18 of the internal true-peak limiter design notes).
	// But 0.2 defaults to the **true-peak lookahead mode** (dspLimiterTP = true), which removes
	// that problem; and every type in the panel already carries limiter=on anyway, so the board's
	// listening state has always been on — defaulting to off would only leave an unprotected
	// window "after a restart, before the user touches the panel".
	dspLimiterEnabled = true
	// The threshold defaults to **0 dBFS** (= JamesDSP's master_limthreshold=0): it only acts
	// when the peak really hits full scale, and is bit-transparent the rest of the time — so
	// "a chain with boost" is not turned down in advance.
	dspLimiterThrDB = 0.0
	dspLimiterAttMs = limDefaultAttMs
	dspLimiterRelMs = limDefaultRelMs
)

var errDSPUnavailable = errors.New("DSP unavailable (/dev/mem not mapped or the IP is absent)")

// ── Presets (6 sections, frequencies one-to-one with the WebUI sliders) ──────────────
//
// Frequency layout: 60 / 150 / 400 / 1k / 3k / 10k Hz — all ≥ minBandFreq.
// PreampDB is the preset's own preamp (common in REW/AutoEQ exports). The hardware has no
// separate preamp gain stage, so dspFinalCoeffs folds it into the coefficients and it takes
// part in the automatic headroom computation (see dsp_headroom.go).

type slotConfig struct {
	Type   string // "PK","LS","HS","LP","HP","NO","BP","AP","off"
	Freq   float64
	Q      float64
	GainDB float64
}

type eqPreset struct {
	Name     string
	PreampDB float64
	Slots    [maxBands]slotConfig
}

var presets = map[string]eqPreset{
	"flat": {"Flat", 0, [maxBands]slotConfig{
		{"off", 0, 0, 0}, {"off", 0, 0, 0}, {"off", 0, 0, 0},
		{"off", 0, 0, 0}, {"off", 0, 0, 0}, {"off", 0, 0, 0},
	}},
	"rock": {"Rock", 0, [maxBands]slotConfig{
		{"LS", 60, 0.7, 4}, {"PK", 150, 0.7, 2}, {"PK", 400, 0.7, 1},
		{"PK", 1000, 0.7, -2}, {"PK", 3000, 0.7, 2}, {"HS", 10000, 0.7, 3},
	}},
	"jazz": {"Jazz", 0, [maxBands]slotConfig{
		{"LS", 60, 0.7, 3}, {"PK", 150, 0.7, 2}, {"PK", 400, 0.5, 1},
		{"PK", 1000, 0.5, 1.5}, {"PK", 3000, 0.7, 1}, {"HS", 10000, 0.7, 2},
	}},
	"classical": {"Classical", 0, [maxBands]slotConfig{
		{"LS", 60, 0.5, 2}, {"PK", 150, 0.5, 1}, {"PK", 400, 0.5, -1},
		{"PK", 1000, 0.7, 1}, {"PK", 3000, 0.7, 1}, {"HS", 10000, 0.7, 2},
	}},
	"vocal": {"Vocal", 0, [maxBands]slotConfig{
		{"PK", 60, 0.7, -3}, {"PK", 150, 0.7, -1}, {"PK", 400, 0.7, 2},
		{"PK", 1000, 1.0, 4}, {"PK", 3000, 0.7, 2}, {"PK", 10000, 0.7, -1},
	}},
	"bass": {"Bass Boost", 0, [maxBands]slotConfig{
		{"LS", 60, 0.5, 6}, {"PK", 150, 0.5, 4}, {"PK", 400, 0.5, 2},
		{"PK", 1000, 0.7, 1}, {"PK", 3000, 0.7, 0}, {"HS", 10000, 0.7, 1},
	}},
}

var (
	// dspHeadroomOn: whether in-chain headroom was used when the coefficients were last computed
	// (it decides CTRL.bit19 and the limiter threshold offset). It is derived from the policy in
	// dspFinalCoeffs, and writing CTRL / writing the threshold must reach the same conclusion.
	dspHeadroomOn bool
	// chainGainDB is the overall gain the **user** pulls in the "overall" column (0..+12 dB):
	// it can win back the volume the automatic headroom took away — at the cost of peaks being
	// shaved by the limiter (or clipped inside the chain). This is the "output gain" that
	// V4A/JamesDSP/Poweramp all have: loudness versus distortion is left to the ear.
	chainGainDB float64
	// currentUserChain is the **user's own** chain (without loudness compensation): whenever the
	// volume changes, the compensation must be merged again and re-downloaded at the new volume,
	// so it has to be kept (currentSlots is the merged result and cannot be reversed).
	currentUserChain []ChainItem
	currentSlots     [maxBands]slotConfig
	currentPreampDB  float64 // the preamp requested by the user/preset (dB)
	// currentPreampAppliedDB is the part **actually folded into the coefficients** (dB, ≤0):
	// the result after automatic headroom compensation. The status API reports it so that
	// "how much was taken off" can be checked.
	currentPreampAppliedDB float64
)

// ── Capacity: how many sections this generation of bitstream can actually take ─────────
//
// Why this function exists instead of writing constants everywhere: **the same chain has
// different capacity on the two bitstream generations**. 0.1's hardware is "a 6-section
// cascade hard-wired into the RTL", while 0.2 is a slot-table engine (the section count is
// decided by the coefficient/state RAM, up to maxSections). The UI, the import fitting and
// the chain's capacity checks must all ask **the same source**, otherwise you get "the UI
// lets you build 16 sections, the backend 400" or "it can hold 16 sections but only lets you
// use 6".
func bandLimit() int {
	if dspEngineGen == 1 {
		// maxBands is the compile-time array dimension; hwMaxSections is the section count the
		// **bitstream reports itself**. Take the smaller: what the array cannot hold must not be
		// promised, and even less what the hardware cannot hold (protection for a new backend
		// paired with an old bitstream).
		if hwMaxSections < maxBands {
			return hwMaxSections
		}
		return maxBands
	}
	return legacyBands
}

// dspHeadroomUsable: can this hardware use "in-chain headroom" right now?
// All three conditions are required: ① a 0.2 bitstream; ② this version of the bitstream
// **really compiled it in** (CAP1 bit10, regardless of generation); ③ the limiter is on
// (headroom trades "no level reduction" for "output peaks are caught by the limiter", and
// without that catcher it must not be done).
func dspHeadroomUsable() bool {
	if dspEngineGen != 1 || !dspLimiterEnabled {
		return false
	}
	return dspReadEngineCaps().HeadroomAvailable()
}

// coefWindow is the number of words the **coefficient window** can address under the current
// bitstream (the upper bound of band*5+k): 0.1 is fixed at 30 words; 0.2 is the whole bank
// (which of them were written is reported by coefWritten()).
func coefWindow() int {
	return bandLimit() * coefPerBand
}

// coefWritten is the number of coefficients **really written by the current configuration** — readback reconciliation uses only this.
//
// Why coefWindow() cannot be used for reconciliation: the two paths write different ranges.
//   - the 0.1 old path: rewrites the whole window every time (legacyBands*5 = 30 words, writing
//     unit coefficients for disabled sections);
//   - the 0.2 slot table: only the active sections are downloaded (band 0..active-1); the slots
//     at the tail of the chain were **never written** and still hold the previous configuration's
//     leftovers — reconciling against them only produces false failures.
//     Measured on the board: a 7-section chain downloads successfully (STATUS decodes 7 sections
//     / 3 slots, 0 dropped samples), yet `/api/dsp/check` reports ok=false; the difference is
//     exactly those never-written words at the tail of the chain.
//
// The engine also only reads the coefficients the slot table points at (coef_base + k*5, all
// inside the written range), so leftover data takes no part in the computation.
// lastPlanCoefs is **the string of values most recently really written into the coefficient
// RAM** (in ascending coefficient-address order).
//
// ⚠️ The self-check (`/api/dsp/check`) must reconcile against it, not against "recompute the EQ
// sections": with dynamic bass (DYN) on, slot 0 of the coefficient RAM is the **side-chain
// band-pass** (2.2 kHz / Q 0.33, see dyn.go's dynSideChainCoefs), not band 0's EQ — comparing
// against the EQ is bound to fail falsely.
// Measured on the board (2026-09-17, DYN on + 5-section EQ): "expected 32849, read back 3253",
// while the engine side was in fact fine (overrun 0, chain in effect).
var lastPlanCoefs []int32

func coefWritten() int {
	if dspEngineGen == 1 {
		if len(lastPlanCoefs) == 0 {
			return dspActiveBands() * coefPerBand // not downloaded yet: fall back to the EQ section count
		}
		return len(lastPlanCoefs)
	}
	return legacyBands * coefPerBand
}

// ── mmap / register access ─────────────────────────────────────────────────

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

// dspInit brings the hardware to a definite initial state: write the limiter defaults → bypass released, 0 sections active.
func dspInit() {
	// first ask the hardware "which engine generation are you" (0.2 has CAP0 magic, 0.1 does not) —
	// which path the coefficient download takes afterwards all depends on that conclusion
	dspProbeEngine()
	// ⚠️ must follow the probe immediately and precede **any** coefficient / section-count download:
	//    it decides the four runtime capacities hwMaxSections/hwCoefWords/hwCoefFIRBase/hwDelayWords
	//    (tightened when the bitstream is smaller than the backend constant).
	dspAdoptHardwareCaps()
	if err := dspSetLimiter(dspLimiterEnabled, dspLimiterThrDB); err != nil {
		log.Printf("DSP limiter init failed: %v", err)
	}
	if err := dspSetLimiterTimes(dspLimiterAttMs, dspLimiterRelMs); err != nil {
		log.Printf("DSP limiter time-constant init failed: %v", err)
	}
	if err := dspApplyPreset("flat"); err != nil {
		log.Printf("DSP initial preset failed: %v", err)
	}
	dspEnabled = true
	dspBypass = false
	if err := dspWriteCtrl(); err != nil {
		log.Printf("DSP CTRL init failed: %v", err)
	}
	log.Printf("DSP ready: up to %d sections dual mono @%.0fHz, limiter %s (%s) @%.1fdBFS",
		bandLimit(), sampleRate, onOff(dspLimiterEnabled), limiterModeName(), dspLimiterThrDB)
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// ── CTRL ─────────────────────────────────────────────────────────────

// dspActiveBands returns the number of sections that are **really active** (0..maxBands).
// The section number to coefficient slot mapping is fixed (band*5), so "turning off a section
// in the middle" is done with unit coefficients (exact pass-through), and the section count is
// taken as the section after the last active one, avoiding pointless computation.
func dspActiveBands() int {
	n := 0
	for i := 0; i < maxBands; i++ {
		if !isBandOff(currentSlots[i].Type) {
			n = i + 1
		}
	}
	return n
}

// dspCtrlBandsField is the number written into CTRL[3:1] (the old NR_BANDS).
// That field has **only 3 bits** (at most 7) and cannot hold 0.2's 16 sections; and 0.2's
// active section count is not in CTRL at all — it is in the slot table header (see dsp_slot.go's
// slotHdrAddr). So this only clamps, so that "what is written" and "what is compared on
// readback" are the same number and the self-check does not report a false failure above 7 sections.
func dspCtrlBandsField() int {
	n := dspActiveBands()
	if n > 7 {
		n = 7
	}
	return n
}

func dspWriteCtrl() error {
	if !dspAvailable {
		return errDSPUnavailable
	}
	val := uint32(0)
	// bit0 = 1 means the DSP is not in the chain: explicit bypass, or the master switch is off
	if dspBypass || !dspEnabled {
		val |= 1
	}
	val |= uint32(dspCtrlBandsField()&0x7) << 1
	if !dspLimiterEnabled {
		val |= 1 << 4
	}
	// bit18 = use the true-peak lookahead limiter (0 = the old feedback form). See dsp_limiter_tp.go.
	if dspLimiterTP {
		val |= ctrlLimTP
	}
	// bit19 = in-chain headroom (only enabled when "this bitstream compiled it + the limiter catches peaks"; see dspHeadroomUsable).
	if dspHeadroomOn {
		val |= ctrlHeadroom
	}
	// ⚠️ bit17 (bank_sel) = "which bank the parameter window points at", **not chain configuration**,
	//    and must not be cleared here. This used to write the whole word `val` ⇒ every CTRL write
	//    knocked bank_sel back to 0; and after COMMIT the active bank is a coin flip ⇒ readbacks
	//    (`/api/dsp/check`, `dspDumpCoeffs`) read the **old bank**, measured on the board as
	//    "applying 8 presets, 4 times ok=false" (false failures).
	//    Fix: preserve the current bank_sel (whoever sets it owns it, see ⑦ of dspDownloadSlotPlan).
	val |= regRead(regCtrl) & ctrlBankSel
	regWrite(regCtrl, val)
	// Readback check: only the 0.2 bitstream implements bit17/18; writing them on a 0.1 bitstream
	// is writing into thin air (they read back 0), so the mask follows "what this hardware
	// generation implements" — verifying 0.2 bits on 0.1 hardware would report false errors.
	mask := dspCtrlVerifyMask()
	if back := regRead(regCtrl) & mask; back != val&mask {
		return fmt.Errorf("CTRL readback mismatch: wrote %#x read %#x (mask %#x)", val&mask, back, mask)
	}
	return nil
}

// dspCtrlVerifyMask returns the CTRL bits that **can be read back** on this hardware.
// bit17/18 belong to the 0.2 engine (BANK_SEL / LIM_TP); the 0.1 bitstream has neither, so
// they read back 0 and writing them has no effect — they must not be compared against the
// software's expectation, or falling back to 0.1 would report "CTRL readback mismatch" out of
// nowhere (0.1 must be able to run completely unchanged).
func dspCtrlVerifyMask() uint32 {
	if dspEngineGen == 1 {
		m := uint32(ctrlMask | ctrlBankSel | ctrlLimTP)
		if dspReadEngineCaps().HeadroomAvailable() {
			m |= ctrlHeadroom // writing it on a bitstream that did not compile it reads back 0, so it cannot be verified
		}
		return m
	}
	return ctrlMask
}

// ── Global switches ──────────────────────────────────────────────────

func dspSetEnable(en bool) error {
	dspEnabled = en
	return dspWriteCtrl()
}

func dspSetBypass(bp bool) error {
	dspBypass = bp
	return dspWriteCtrl()
}

// dspSetMasterVolume has no corresponding register in the new hardware: digital volume goes
// through ALSA Master (see handleVolume in api.go), and the FPGA no longer adds a gain stage.
func dspSetMasterVolume(vol float64) error {
	return nil
}

// dspSetPreamp sets the preamp (linear gain). The hardware has no separate preamp stage, so it is
// folded into the coefficients (see dspFinalCoeffs); the effective value =
// min(this value, -the EQ's largest boost), guaranteeing that the cascade output does not exceed 0 dBFS.
func dspSetPreamp(gain float64) error {
	if gain <= 0 {
		return nil
	}
	currentPreampDB = 20 * math.Log10(gain)
	return dspWriteAllBands()
}

// ── Limiter ───────────────────────────────────────────────────────────

// dspSetLimiter sets the limiter switch and threshold (dBFS, e.g. -1.0).
// limiterThrRegDB converts the "user-facing" threshold into the **register-facing** one:
// with in-chain headroom on, the in-chain signal is 18 dB lower overall and the limiter works
// before the restore (×8) ⇒ the threshold must also drop by 18 dB, so that the guarantee
// "actual output peak ≤ user threshold" is unchanged (see the HEADROOM note in dsp_engine.v).
func limiterThrRegDB() float64 {
	db := dspLimiterThrDB
	if dspHeadroomOn {
		db -= headroomDB
	}
	return db
}

// dspWriteLimiterThr writes the threshold register to the value it should currently have (user value + headroom offset).
func dspWriteLimiterThr() error {
	db := limiterThrRegDB()
	if db > 0 {
		db = 0
	}
	if db < -60 {
		db = -60
	}
	thrQ := uint32(math.Round(math.Pow(10, db/20) * qOne))
	if thrQ < 1 {
		thrQ = 1
	}
	if thrQ > qOne { // 1.0 FS = never triggers
		thrQ = qOne
	}
	regWrite(regLimThr, thrQ)
	if back := regRead(regLimThr) & coefMask; back != thrQ {
		return fmt.Errorf("limiter THR readback mismatch: wrote %d read %d", thrQ, back)
	}
	return nil
}

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
	// thrDB is the **user-facing** value (that −1 dBFS on the panel); the register-facing value is converted by limiterThrRegDB()
	wasUsable := dspHeadroomUsable()
	dspLimiterEnabled = enabled
	dspLimiterThrDB = thrDB
	if err := dspWriteLimiterThr(); err != nil {
		return err
	}
	// ⚠️ The limiter switch **changes the headroom policy** (headroom relies on the limiter as its
	//    catcher) ⇒ the whole chain's preamp and CTRL.bit19 change with it. Writing only the
	//    threshold without re-downloading leaves the hardware on the old coefficients, divorced
	//    from the software's expectation (on the board: `/api/dsp/check` suddenly reports ok=false,
	//    when in fact it was only "not re-downloaded").
	if wasUsable != dspHeadroomUsable() {
		if err := dspWriteAllBands(); err != nil {
			return err
		}
		markStateDirty()
		return nil
	}
	if err := dspWriteCtrl(); err != nil {
		return err
	}
	markStateDirty()
	return nil
}

// dspSetLimiterTimes sets the coefficients from the attack/release times (ms).
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
	// True-peak mode: first order in the log2 domain, both coefficients < 1 (approaching the
	// target), and it uses the **slot rate** of 96 kHz (the old mode uses sampleRate=48000, the
	// nominal convention already released in 0.1, and is not touched)
	if dspLimiterTP {
		attQ := limKFromMsTP(attMs)
		relQ := limKFromMsTP(relMs)
		regWrite(regLimAtt, attQ)
		regWrite(regLimRel, relQ)
		if back := regRead(regLimAtt) & coefMask; back != attQ {
			return fmt.Errorf("true-peak limiter ATT readback mismatch: wrote %d read %d", attQ, back)
		}
		if back := regRead(regLimRel) & coefMask; back != relQ {
			return fmt.Errorf("true-peak limiter REL readback mismatch: wrote %d read %d", relQ, back)
		}
		dspLimiterAttMs = attMs
		dspLimiterRelMs = relMs
		return nil
	}

	kAtt := math.Exp(-1.0 / (attMs / 1000.0 * sampleRate))
	kRel := math.Exp(1.0 / (relMs / 1000.0 * sampleRate))
	attQ := uint32(math.Round(kAtt * qOne))
	relQ := uint32(math.Round(kRel * qOne))
	if attQ > qOne-1 { // must be < 1.0, otherwise it cannot hold the gain down
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

// ── Coefficient writes ───────────────────────────────────────────────

func dspWriteBand(band int, b0, b1, b2, a1, a2 int32) {
	regWrite(regCidx, uint32(band*coefPerBand))
	for _, v := range [coefPerBand]int32{b0, b1, b2, a1, a2} {
		regWrite(regCdat, uint32(v)&coefMask)
	}
	regRead(regCtrl) // one read of a register on the same slave, ensuring the posted writes above have landed
}

// decodeCoefReadback converts the CDAT readback value back into a coefficient.
//
// In dsp_insert.v the hardware has already **sign-extended the 18-bit coefficient to 32 bits**:
//
//	cdat_rd = {{(32-COEF_W){coef_sel[COEF_W-1]}}, coef_sel}
//
// so here it can simply be interpreted as int32; it must **not** be extended as 18 bits a second
// time, otherwise a negative coefficient comes out 2^18 too small (-23907 read as -286051).
func decodeCoefReadback(v uint32) int32 {
	return int32(v)
}

func dspReadCoeff(idx int) int32 {
	if idx < 0 || idx >= coefWindow() {
		return 0
	}
	regWrite(regCidx, uint32(idx))
	return decodeCoefReadback(regRead(regCdat))
}

// dspDumpCoeffs reads back this configuration's coefficients (for self-check/debugging; the range is coefWritten()).
func dspDumpCoeffs() []int32 {
	out := make([]int32, coefWritten())
	for i := range out {
		out[i] = dspReadCoeff(i)
	}
	return out
}

// ── Single-section configuration ─────────────────────────────────────

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
		return 0, fmt.Errorf("coefficient %s=%d is outside the Q3.15 range [%d,%d] (frequency/gain too extreme)",
			name, v, coefMin, coefMax)
	}
	return v, nil
}

// designBiquad designs one section per the RBJ cookbook, returning Q3.15 integer coefficients.
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

// dspSetSlot configures one section. band is 0..5, matching the WebUI slider order.
func dspSetSlot(band int, sc slotConfig) error {
	if !dspAvailable {
		return errDSPUnavailable
	}
	if band < 0 || band >= bandLimit() {
		return fmt.Errorf("band %d out of range (0..%d)", band, bandLimit()-1)
	}

	if isBandOff(sc.Type) {
		currentSlots[band] = slotConfig{Type: "off"}
	} else {
		sc = sanitizeBand(sc)
		// try it once first: a coefficient out of range (frequency/gain too extreme) must be reported to the API, not silently written as pass-through
		if _, _, _, _, _, err := designBiquad(sc); err != nil {
			return fmt.Errorf("band %d: %w", band, err)
		}
		currentSlots[band] = sc
	}
	// one section changed, so the whole chain's headroom changed → rewrite everything (30 registers, negligible cost)
	return dspWriteAllBands()
}

// dspFinalCoeffs returns the **final** Q3.15 coefficients for the 6 sections that go into the
// hardware (automatic preamp already included), plus the preamp that actually takes effect
// (dB, ≤0, for status display and logging).
func dspFinalCoeffs() ([maxBands][coefPerBand]int32, float64) {
	var out [maxBands][coefPerBand]int32
	active := 0
	for i := 0; i < maxBands; i++ {
		sc := currentSlots[i]
		out[i] = [coefPerBand]int32{qOne, 0, 0, 0, 0}
		if isBandOff(sc.Type) {
			continue
		}
		if b0, b1, b2, a1, a2, err := designBiquad(sanitizeBand(sc)); err == nil {
			out[i] = [coefPerBand]int32{b0, b1, b2, a1, a2}
			active++
		}
	}

	maxGain := cascadeMaxGainDB(out)
	// ⚠️ The gain of the convolution (FIR) stage is **not in `out`** — `out` only holds the
	// EQ/DDC biquad cascade. Missing it: with EQ off, maxGain≈0 ⇒ "no headroom needed" is
	// concluded and the in-chain 18 dB is switched off, while the IR's worst case (L1 norm) can
	// reach +11.5 dB ⇒ full-scale music hits the 24-bit ceiling the moment it enters the
	// convolution (hard clipping). On the board: **"a hissing noise at high volume"**
	// (reported by the author on 2026-09-18).
	if g := firGainDBForHeadroom(); g > maxGain {
		maxGain = g
	}
	// With headroom available, "automatically reduce the overall level" does not have to work as
	// hard: the in-chain 18 dB comes for free, and only the part **beyond the headroom** actually
	// needs reducing (so that no biquad clips inside the chain).
	// ⚠️ The old rule (dead zone + 3 dB safety margin) still holds after the headroom is subtracted.
	dspHeadroomOn = dspHeadroomUsable() && maxGain > preampDeadZoneDB
	pre := effectivePreampDB(maxGain, currentPreampDB)
	if dspHeadroomOn {
		// ⚠️ **No automatic preamp** — this replicates the master section of the reference local JamesDSP
		// (~/.config/jamesdsp/audio.conf):
		//     master_limthreshold=0   master_limrelease=60   master_postgain=0
		// i.e.: the boost stays as it is, peaks are caught by the 0 dBFS limiter, there is no
		// postgain ⇒ **the perceived loudness does not change**.
		// (EqualizerAPO's "pre-attenuate the preamp by the largest boost" goes the other way: it
		//  never clips but the volume drops. JamesDSP is the reference player here,
		//  so JamesDSP is the reference.)
		// The in-chain 18 dB headroom guarantees no clipping inside the chain; the slight overflow
		// at the output is left to the limiter, not hard clipping.
		// If the user/preset explicitly asks for a deeper preamp (the Preamp: -X dB that comes with
		// an AutoEQ curve), that is honoured.
		pre = 0
		if currentPreampDB < pre {
			pre = currentPreampDB
		}
	}
	// The user wants it louder: give the headroom back (see the note on chainGainDB). Allowing a positive value ⇒ peaks will be clipped/limited.
	if chainGainDB != 0 {
		pre += chainGainDB
		if pre > 12 {
			pre = 12
		}
	}
	if pre > 0.005 || pre < -0.005 {
		// Only scale the numerator of the **first non-off section**: mathematically that equals
		// adding a preamp at the chain input. Spreading it over every stage is wrong — the first
		// stage's own gain barely drops, and a full-scale input still blows it up.
		first := -1
		for i := 0; i < maxBands; i++ {
			if !isBandOff(currentSlots[i].Type) {
				first = i
				break
			}
		}
		if first >= 0 {
			base := out
			apply := func(db float64) [maxBands][coefPerBand]int32 {
				c := base
				g := math.Pow(10, db/20)
				for k := 0; k < 3; k++ { // only the numerator; a1/a2 are poles and moving them changes the response shape
					v := int32(math.Round(float64(base[first][k]) * g))
					if v > coefMax {
						v = coefMax
					} else if v < coefMin {
						v = coefMin
					}
					c[first][k] = v
				}
				return c
			}
			out = apply(pre)
			// no iterative deepening: the 60 Hz low shelf's numerator is a cancellation term, and
			// rounding the scale makes the measured response non-monotonic (measured -7 dB is worse
			// than -6 dB), so iteration would be led astray. One exact computation suffices; the real
			// effect is verified by the offline fixed-point time-domain simulation (rock: clipping onset 10146 → 0).
		}
	}
	return out, pre
}

// dspWriteAllBands writes **the current chain** into the hardware as a whole and updates CTRL.
//
// The two paths follow the bitstream generation (the conclusion of `dspProbeEngine()`):
//
//	· 0.2 slot-table engine: only the **active sections** are compiled into a slot table (+ coefficients),
//	  and COMMIT at a frame boundary makes it all take effect at once;
//	· 0.1 old bitstream: keeps the original 30-coefficient path, behaviour unchanged (the old hardware window is legacyBands wide).
func dspWriteAllBands() error {
	if !dspAvailable {
		return errDSPUnavailable
	}
	coefs, pre := dspFinalCoeffs()
	currentPreampAppliedDB = pre
	// the headroom switch changed ⇒ the register-facing threshold changed too (by 18 dB), so it must be rewritten
	if dspLimiterEnabled {
		if err := dspWriteLimiterThr(); err != nil {
			return err
		}
	}

	// 0.2 slot-table engine: the whole chain is compiled into a slot table + coefficients, and
	// COMMIT at a **frame boundary** makes it all take effect (no longer "write coefficient by
	// coefficient while running" — which is exactly the flaw that gave 0.1 a half-old/half-new
	// intermediate state)
	if dspEngineGen == 1 {
		// only the **active sections** are downloaded (band 0..active-1): sections turned off in the
		// middle still enter the chain with unit coefficients (they must hold their place, otherwise
		// the later section numbers would shift and the frequencies would be misplaced), but the
		// useless sections at the tail need not be computed by the engine every frame — the engine's
		// per-frame budget is meant to be left for "more sections".
		//
		// If dynamic bass (DYN) is on, it goes **before the EQ**, following the reference implementation's chain order (see buildChainNodes).
		nodes, err := buildChainNodes(coefs[:dspActiveBands()], currentDynBass, currentCrossfeed, currentSurround)
		if err != nil {
			return err
		}
		plan, err := buildSlotPlanNodes(nodes)
		if err != nil {
			return err
		}
		// FIR (convolution) coefficients: they do not take EQ coefficient space and live from
		// coefFIRBase onwards. irPlanCoefs() is non-empty only when the IR changed — 8192 words
		// need not be moved on every download.
		plan.FirCoefs = irPlanCoefs()
		if err := dspDownloadSlotPlan(plan); err != nil {
			return err
		}
		if len(plan.FirCoefs) > 0 {
			irClearDirty() // clear the flag only once the coefficients really were written
		}
		// record "the coefficients just really written", for /api/dsp/check to reconcile word by word (see lastPlanCoefs)
		lastPlanCoefs = append(lastPlanCoefs[:0], plan.Coefs...)
		// ── antiPop: after the download that turns ViPERBass **from off to on**, start a full 1-second fade-in ramp ──
		// Placed after "the whole table is already in effect": the ramp is meant to modify exactly those two
		// weights that were just compiled, and the table has to exist before it can be modified.
		// In the steady state (ViPERBass is always in the table) this is a **no-op** and writes not one register.
		if err := dspAntiPopAfterDownload(plan); err != nil {
			// ⚠️ The error must **not** be propagated here: the whole table has already been COMMITted into
			//    effect, and returning an error would make the layer above roll back its **in-memory** state
			//    while the hardware stays on the new chain — exactly the old accident in chain.go of "the API
			//    reports an error while the effect is half in effect". The ramp failing to start only means
			//    that 1-second fade-in is missing (falling back to "a jump when the effect is switched on"),
			//    unrelated to the rest of the chain, so an honest log entry is enough.
			log.Printf("ViPERBass antiPop ramp could not start (the chain is in effect, only the 1-second fade-in is missing): %v", err)
		}
		return dspWriteCtrl()
	}

	// Old bitstream (0.1): keep the original 30-coefficient path, behaviour unchanged.
	// The window width is legacyBands (30 words) — writing beyond that on old hardware wraps around / tramples other sections.
	for i := 0; i < legacyBands; i++ {
		c := coefs[i]
		dspWriteBand(i, c[0], c[1], c[2], c[3], c[4])
	}
	return dspWriteCtrl()
}

// getBandConfig returns a section's current configuration (for export/status display).
func getBandConfig(band int) slotConfig {
	if band < 0 || band >= maxBands {
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
		if isBandOff(sc.Type) {
			currentSlots[i] = slotConfig{Type: "off"}
			continue
		}
		s := sanitizeBand(sc)
		if _, _, _, _, _, err := designBiquad(s); err != nil {
			return fmt.Errorf("preset %s section %d: %w", name, i+1, err)
		}
		currentSlots[i] = s
	}
	// The preset's own Preamp (common in REW/AutoEQ exports) now really takes part in the headroom computation
	currentPreampDB = p.PreampDB
	return dspWriteAllBands()
}

// ── Self-check ───────────────────────────────────────────────────────

// dspExpectedCoeffs computes, from the current software state, the values the coefficients
// **written by this configuration** should have, for the readback check (the range is
// coefWritten(): 30 on the old path, and on the slot-table path only up to the last active section).
func dspExpectedCoeffs() []int32 {
	// 0.2: what is in the hardware **is last time's downloaded plan.Coefs** (possibly including DYN's
	//      side chain and DYN parameters); compare it as it is, do not recompute the EQ — recomputing
	//      is answering a different question.
	if dspEngineGen == 1 && len(lastPlanCoefs) > 0 {
		return append([]int32(nil), lastPlanCoefs...)
	}
	coefs, _ := dspFinalCoeffs()
	out := make([]int32, coefWritten())
	for i := 0; i < coefWritten()/coefPerBand; i++ {
		copy(out[i*coefPerBand:], coefs[i][:])
	}
	return out
}

// dspSelfCheck reads back CTRL + all coefficients and compares them against the software's expectation.
func dspSelfCheck() error {
	if !dspAvailable {
		return errDSPUnavailable
	}
	want := dspExpectedCoeffs()
	got := dspDumpCoeffs()
	for i := range want {
		if want[i] != got[i] {
			return fmt.Errorf("coefficient %d (band %d, number %d) mismatch: expected %d read back %d",
				i, i/coefPerBand, i%coefPerBand, want[i], got[i])
		}
	}
	ctrl := regRead(regCtrl) & ctrlMask
	if nb := int((ctrl >> 1) & 0x7); nb != dspCtrlBandsField() {
		return fmt.Errorf("NR_BANDS mismatch: expected %d read back %d", dspCtrlBandsField(), nb)
	}
	return nil
}

// ── Q3.15 conversion ─────────────────────────────────────────────────

func floatToQ315(val float64) int32 {
	return int32(math.Round(val * float64(qOne)))
}

// ── Status snapshot ──────────────────────────────────────────────────

// dspStatusSnapshot assembles the DSP status (used by /api/status, the WebSocket push and the self-check).
func dspStatusSnapshot() DSPStatus {
	st := DSPStatus{
		Available:   dspAvailable,
		Enabled:     dspEnabled,
		Bypass:      dspBypass,
		Preset:      currentPreset,
		PreampDB:    currentPreampAppliedDB,
		BandsActive: dspActiveBands(),
		Limiter:     dspLimiterEnabled,
		LimiterMode: limiterModeName(),
		LimThrDB:    dspLimiterThrDB,
		LimAttMs:    dspLimiterAttMs,
		LimRelMs:    dspLimiterRelMs,
		Bands:       make([]BandStatus, maxBands),
	}
	for i := 0; i < maxBands; i++ {
		sc := currentSlots[i]
		st.Bands[i] = BandStatus{Type: sc.Type, Freq: sc.Freq, GainDB: sc.GainDB, Q: sc.Q}
	}
	return st
}
