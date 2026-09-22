// dsp_slot.go — register window and "effect chain → slot table" compiler for the 0.2 engine (slot table)
//
// Background: 0.1's hardware is a "fixed 6-section biquad cascade" and software can only write 30
// coefficients through CIDX/CDAT; the 0.2 engine (dsp_engine.v) is **slot-table driven**: a chain =
// a number of slot descriptors + the coefficient table, and it only takes effect at the frame
// boundary after COMMIT. The register window is documented in the DSP engine P1-b notes §14.
//
// The two paths must coexist (this is the only reason CAP0 exists):
//
//	· an old bitstream (0.1) has no CAP registers and reads back 0 ⇒ the original 30-coefficient path
//	  is taken, with byte-for-byte identical behaviour;
//	· a new bitstream (0.2) reports CAP0[31:16] == 0x5A44 ⇒ the slot table path is taken.
//
// So the conclusion of dspProbeEngine() decides which path every later write takes.
package main

import (
	"fmt"
	"log"
	"math"
	"strings"
	"time"
)

// ── 0.2 registers (word offsets, base 0x43C00000) ──────────────────────
const (
	regSlotAddr = 0x48 // word 18: slot word index (auto-increments on write; writes the header at NSLOT*8)
	regSlotData = 0x4C // word 19: slot descriptor word
	regStatus   = 0x50 // word 20: read-only status
	regCap0     = 0x54 // word 21: magic + version
	regCap1     = 0x58 // word 22: opcode bitmap
	regCap2     = 0x5C // word 23: limits (NSLOT / FIR / DELAY)
	regCap3     = 0x60 // word 24: per-opcode MAC cost
	// regCap4: word 25, added 2026-09-21 — the capacity and cost of the **second small FIR**
	// (OP_SFIR). Why a register of its own rather than squeezing it into CAP2/CAP3: those two words
	// use **all** 32 bits (CAP2 = NSLOT/FIR_LOG2/DELAY_LOG2/NSEC, CAP3 = NCOEF/DLYSLOT/POLY
	// cost/section cost), so squeezing anything else in would change the meaning of existing fields
	// ⇒ a new backend reading an old bitstream would decode it wrongly. On the hardware side see cap4
	// in dsp_engine.v and the `when 25 =>` readback branch in axi_i2s_adi_v1_2.vhd.
	// An old bitstream reads 0 from this word (that address used to be others ⇒ 0) ⇒ 0 must be
	// treated as "not reported".
	regCap4 = 0x64 // word 25: second small FIR capacity / MACs per cycle / fixed overhead
)

const (
	capMagic    = 0x5A44 // CAP0[31:16]: "ZD" (matches CAP_MAGIC in dsp_engine.v)
	ctrlCommit  = 1 << 16
	ctrlBankSel = 1 << 17

	// capOpcodeLimTP: CAP1 bitmap bit9 = "a true-peak lookahead limiter is available to switch in".
	// ⚠️ It is **not** a slot opcode but an optional stage at the end of the chain; the hardware puts
	//    it in the same bitmap as the opcodes to save a register (see the comment on the cap1 line in
	//    dsp_engine.v).
	//    The backend either reports truepeak_available from this hardware bit, or it is deciding on
	//    the hardware's behalf.
	capOpcodeLimTP = 1 << 9

	// capOpcodeHeadroom: CAP1 bitmap bit10 = "this bitstream implements in-chain headroom
	// (CTRL.bit19)". Same reasoning as bit9: **do not look at the "engine generation"** — whether a
	// bitstream of the same generation includes it is decided by the build, and using gen==1 as a
	// proxy diverges from the facts (falling back to a bitstream without it would enable it wrongly
	// and the sound would be plainly wrong).
	capOpcodeHeadroom = 1 << 10

	// capOpcodeMix2: CAP1 bitmap bit3 = "this bitstream implements the MIX2 slot (weighted sum of
	// two buses)". The hardware prerequisite for crossfeed (adding in the other channel's low-pass
	// branch).
	capOpcodeMix2 = 1 << 3

	// capOpcodeDyn: CAP1 bitmap bit11 = "this bitstream implements the DYN slot (dynamic gain)".
	// Same reasoning as bit9/bit10: **do not look at the engine generation**; software relies on the
	// hardware's self-report to decide whether dynamic bass can be downloaded.
	capOpcodeDyn = 1 << 11

	slotWordSize = 8              // 8 32-bit words per slot descriptor
	slotHdrAddr  = dspSlotMax * 8 // header address: number of active slots
	// dspSlotMax = NSLOT (dsp_engine.v). Raised from 8 to 16 on 2026-09-20 and from 16 to **24** on
	// 2026-09-21: the reference real chain (VSE + convolution + DDC(18 sections) + ViPERBass
	// + Clarity XHIFI) needs **22 slots** (exciter 5 + convolution 1 + DDC 6 + ViPERBass 2 + XHIFI 8),
	// and with 16 slots the board reported
	// `not enough slots: this chain needs 17 slots and the engine has 16`.
	// Why 24 rather than 20: this chain alone needs 22 slots and 20 cannot hold it (see
	// the realchain capacity test); 24 still leaves 2 slots for "one more stage" (e.g. also
	// enabling DYN dynamic bass = +2 slots = 24 slots, exactly full).
	dspSlotMax = 24
	// maxSections = NSEC (dsp_engine.v) = state-RAM sections per channel, and the single yardstick
	// for every "section" budget in this package (each EQ band **is** a section,
	// maxBands = maxSections).
	// Raised from 24 to 36 on 2026-09-20: the effects alone need 34 sections (DDC 20 + Cure 6
	// + tube flavour 1 + exciter 3 + Clarity 1 + SpeakerCorrection 3), and the old value could not
	// even "hold DDC's 20 sections unchanged".
	// Raised from 36 to **48** on 2026-09-21: the real chain needs **37 sections** (exciter 4 + DDC 18
	// + ViPERBass 3 + XHIFI 12), and at 36 the board reported
	// `chain too long: 37 sections (including DYN's detector band-pass) > engine limit of 36 sections`.
	// ⚠️ The limit is **not** set by RAM but by cycles per frame — the single yardstick is
	//    TestSectionFrameBudget in hdl_sync_test.go (plus the compile-time checkFrameBudget).
	//    On 2026-09-21 convolution went from 512 cycles down to 256 (MACS 8→16) ⇒ that frame-budget
	//    constraint was relaxed by half; the **worst case** for 48 sections + convolution is in the
	//    t.Logf of TestSectionFrameBudget (with margin left).
	// ⚠️ It must stay in step with NSEC **and** NCOEF (= 5×NSEC, see coefWordsPerBank); changing only
	//    this would run past the hardware coefficient RAM, and out-of-range coefficients are silently
	//    dropped by `cidx < NCOEF`.
	maxSections = 48
	secPerSlot  = 3 // packing: how many sections fit in one slot at most (multi-section slots verified by simulation H1)
	opNop       = 0 // empty slot / end of table
	opBiquad    = 1 // DF-I biquad
	opDyn       = 2 // dynamic gain section (side chain on in_b; see the dyn source and dsp_engine.v)
	opMix2      = 3 // weighted sum of two buses: out = c0·in_a + c1·in_b (see the crossfeed source)
	opDelay     = 4 // short delay: out = the input of this channel L samples ago (see OP_DELAY in dsp_engine.v)
	// capOpcodeDelay: CAP1 bit4 = "this bitstream implements DELAY (the short delay ring)"
	capOpcodeDelay = 1 << 4

	// capOpcodeFIR: CAP1 bit5 = "this bitstream implements FIR (parallel split convolution kernel)".
	// On the hardware side see the FIR bank RTL (fir_bank.v) and OP_FIR/S_FIR in dsp_engine.v.
	capOpcodeFIR = 1 << 5

	// capOpcodeSFIR: CAP1 bit7 = "this bitstream implements the **second small FIR** (OP_SFIR)".
	// The hardware prerequisite for ViPERBass's PURE_BASS_PLUS setting (its dry path is not a
	// pass-through but a 63-tap polyphase kernel; the 8192-tap convolution stage cannot provide that
	// structure — that one is reserved for user IRs).
	// Same reasoning as the other capability bits: **do not look at the engine generation**; whether
	// a bitstream of the same generation includes it is decided by the build (dropping the 8 DSPs for
	// a trimmed build is a perfectly reasonable build choice), and using the version number as a proxy
	// diverges from the facts.
	capOpcodeSFIR = 1 << 7

	// capOpcodeJointStereo: CAP1 bit12 = "this bitstream implements the **joint-stereo frame pass**"
	// (OP_JDST / OP_J3DS). The hardware prerequisite for V4A ColorfulMusic (its DepthSurround is a
	// joint-stereo processor: prev0/prev1 and both delay lines are shared by every instance and must
	// hold L and R in the same cycle).
	// Same reasoning as the other capability bits: **do not look at the engine generation** — whether
	// a bitstream of the same generation includes it is decided by the build.
	capOpcodeJointStereo = 1 << 12

	// opJDST / opJ3DS: the two slot opcodes of the joint-stereo frame pass (see the file header of
	// dsp_engine.v).
	//   opJDST = DepthSurround (19 cycles: joint recursion + two delays + HP800)
	//   opJ3DS = Stereo3DSurround (7 cycles: stateless 2×2 matrix)
	// Descriptor field semantics (**hard-wired to the RTL**):
	//   in_a  = for JDST the **L output bus** (its L input comes from a frame-capture register), for
	//           J3DS the L input bus
	//   in_b  = the **R input bus** (both slots)
	//   out_b = the **R output bus** (0xFFFF = unused)
	//   cfb   = coefficient base address; stb for JDST = **offset inside the ring** (the two delay
	//           lines, same semantics as OP_DELAY)
	opJDST = 9
	opJ3DS = 10

	// jointStateReserve: the joint stage's state is **one copy per frame** (prev0/prev1 + the 4 state
	// words of HP800 = 2 words) and is not split per channel, so it always lands in the **last two
	// words of the left channel's sections**: JS_STATE0 = NSEC−2 in the HDL (see dsp_engine.v).
	// ⇒ when planning a joint stage the per-channel section count must **leave these 2 sections
	// free** (otherwise the last few left-channel sections tread on the joint state — and that kind of
	// error does not sound at all like "state being trampled").
	// The check is TestJointStateBaseMatchesHDL in hdl_sync_test.go.
	jointStateReserve = 2

	// colorfulJointCycles: the joint stage's **cycles per frame** (the right-channel pass), counted
	// off the FSM:
	//   JDST = 19 cycles (S_JDST phases 0..18) + 7 descriptor cycles = 26
	//   J3DS =  7 cycles (S_J3DS phases 0..6) + 7 descriptor cycles = 14
	// 40 cycles in total. The 2 cycles per slot of slot scheduling are accounted for separately by
	// the slots*2 term in chainFrameCost.
	// ⚠️ Changing the phase table means changing this number (criterion ⑥ of tb_engine_js does not go
	// easy under real timing).
	colorfulJointCycles = 40

	// opSFIR: slot opcode 7 = the second, independent small FIR. The slot CFG's n has the **same
	// semantics as opFIR**: n = log2(number of MACS blocks), taps = sfirMACS << n (PBP uses
	// 64 taps = 8 blocks ⇒ n=3).
	opSFIR = 7

	// opFIR: slot opcode 5. The slot CFG's n = **log2(number of MACS blocks)**, taps = firMACS << n.
	// ⚠️ The meaning of n is **tied to firMACS**: blocks = total taps ÷ MACs per cycle, and 8 bits
	//    cannot hold 1024 blocks, so it is log2-encoded. With MACS=8, 8192 taps = 1024 blocks ⇒ n=10;
	//    with MACS=16 (2026-09-21), 8192 taps = 512 blocks ⇒ **n=9**.
	//    The same expression on the engine side must use FIR_MACS (not a literal) — otherwise changing
	//    n mismatches the two: an n that is too large makes the same taps count double (16384 taps
	//    ⇒ nblk saturates inside the kernel and it **looks normal**), while an n that is too small
	//    convolves only half the taps (losing half the high-frequency detail). Both sides are pinned
	//    down by TestFIRMACSAndCapLog2MatchGo in hdl_sync_test.go.
	opFIR = 5

	// firTaps / firMACS: consistent with TAPS/MACS in fir_bank.v / fir_bank_long.v and with
	// FIR_TAPS/FIR_MACS in dsp_engine.v (the three-way agreement is pinned down by hdl_sync_test.go).
	firTaps = 8192 // two parallel kernels with block decomposition: 4096×2 (see the long FIR bank RTL, fir_bank_long.v)
	// Raised from 8 to 16 on 2026-09-21: fir_bank.v is now genuinely parameterised as
	// LOGM=$clog2(MACS), giving 4096÷16 = **256 cycles** per kernel (was 512). The cost: 16 DSPs per
	// kernel ⇒ 32 for the two kernels.
	firMACS = 16

	// firCycles: **cycles per frame** for one convolution (per channel). The two parallel kernels run
	// **simultaneously** ⇒ cycles = one kernel's BLK_TAPS ÷ firMACS = 4096 ÷ 16 = 256 (**not**
	// firTaps/firMACS = 512, which is the cycle count for "one kernel covering all 8192 taps").
	// The identity firCycles·firMACS·2 == firTaps is pinned down by hdl_sync_test.go.
	firCycles = 256

	// ── Size and cost of the second small FIR (OP_SFIR) ──────────────────
	// Use: ViPERBass's PURE_BASS_PLUS setting. The official core's **dry path** is a 63-tap
	// polyphase kernel (POLYPHASE_COEFFICIENTS_2 in the community ViPER4Android rewrite
	// (viperfx-re): src/viper/utils/Polyphase.cpp); here we take **64 taps** with h[63]=0:
	//   · fir_bank requires taps to be a whole multiple of MACS and the block count (taps/MACS) to be
	//     a power of two;
	//   · one extra tap that is always 0 is **bit-for-bit equivalent** to 63 taps (phase A1 of
	//     tb_engine_sfir was compared bit by bit against a 63-tap reference model).
	// sfirTaps / sfirMACS / sfirOvh must match SFIR_TAPS / SFIR_MACS / SFIR_OVH in dsp_engine.v
	// (pinned down by TestSFIRConstantsMatchGo in hdl_sync_test.go).
	sfirTaps = 64
	sfirMACS = 8
	// sfirOvh: the **fixed overhead** of one small FIR (cycles/channel, excluding taps/sfirMACS).
	// Counted off the fir_bank FSM: S_DISP(1)+S_FIRW(1)+F_IDLE(1)+pipeline fill(3)
	// +F_SUM reduction(log2(8)=3)+done readback(2) = 11. The hardware self-reports the same number in
	// CAP4[23:16].
	sfirOvh = 11
	// sfirCycles: the cycles per frame of that one PBP pass (per channel) = 64/8 + 11 = 19.
	// ⚠️ Changing sfirTaps means this number must change with it — hdl_sync_test.go pins down this
	// relation.
	sfirCycles = sfirTaps/sfirMACS + sfirOvh

	// coefFIRBase: the base address of the FIR coefficients in the **engine coefficient address
	// space** = 2·NCOEF (dsp_engine.v). bank0 occupies 0..NCOEF-1, bank1 occupies
	// NCOEF..2·NCOEF-1, and FIR starts at 2·NCOEF: channel 0 occupies base..base+4095 and channel 1
	// occupies base+4096..base+8191.
	// ⚠️ **It must not be hard-coded**: it used to be hard-coded as 256 (in the NCOEF=128 days), and
	//    as soon as NCOEF grew it overlapped bank1; the symptom was "the EQ coefficients get trampled
	//    by FIR taps the moment convolution is switched on".
	coefFIRBase = 2 * coefWordsPerBank
	// coefSFIRBase: the base address of the **second small FIR**'s coefficients in the engine
	// coefficient address space = COEF_FIR_BASE + NCH·FIR_TAPS (COEF_SFIR_BASE in dsp_engine.v).
	// Layout: cidx 0..NCOEF-1 = bank0, NCOEF..2NCOEF-1 = bank1,
	//         2NCOEF..+16383 = the 8192-tap convolution (ch0 at +0, ch1 at +FIR_TAPS),
	//         coefSFIRBase..+127 = the small FIR (ch0 at +0, ch1 at +sfirTaps).
	// ⚠️ Unlike the small FIR's **slot**, these coefficients do **not** occupy the 240 words/bank of
	//    NCOEF (that is the double-bank space for biquad coefficients), nor the convolution region —
	//    the three layers each have their own address range and do not overlap.
	coefSFIRBase = coefFIRBase + 2*firTaps // = 480 + 16384 = 16864
	// opPoly: slot opcode 6 = the **non-linear** harmonic stage (V4A `Harmonic`: a 10th-order
	// polynomial + 0.999 leaky integration). On the hardware side see OP_POLY/S_POLY in dsp_engine.v;
	// for the parameter expansion see the harmonic source.
	// The slot CFG's n is 11 (the polynomial order); the engine always reads 12 words: c0..c10 + the
	// de-click length.
	opPoly = 6

	// capOpcodePoly: CAP1 bit6 = "this bitstream implements POLY (the non-linear harmonic stage)".
	// Same reasoning as the other capability bits: **do not look at the engine generation**; software
	// relies on the hardware's self-report to decide whether the exciter can be downloaded.
	capOpcodePoly = 1 << 6

	opSat    = 8      // clipping detection/counting
	flBypass = 1 << 0 // flags.bit0
)

// Slot descriptor fields (the 8 words of the design document §2.1)
const (
	swCfg  = 0 // [7:0]opcode [15:8]flags [23:16]n
	swCfb  = 1 // coefficient start address
	swStb  = 2 // state start address
	swInA  = 3 // input bus
	swInB  = 4 // second input (used by MIX2; currently filled with 0xFFFF)
	swOutB = 5 // output bus
	swPrm  = 6 // dynamic parameter address (used by DYN/SLEW)
	swRsv  = 7 // reserved
)

// dspEngineGen: 0 = old bitstream (no slot table), 1 = the 0.2 slot-table engine
var dspEngineGen int

// dspEngineCaps holds the hardware's self-reported capabilities (read from CAP1..CAP3); used by
// /api/dsp/capabilities
type dspEngineCaps struct {
	Present    bool
	Version    uint16
	Opcodes    uint32 // bitmap: bit n = opcode n is supported
	NumSlots   int
	FirMaxLog2 int
	DelayLog2  int
	// Added 2026-09-20 (the HDL gained these two CAP fields at the same time):
	//   CAP2[31:24] = NSEC  state sections per channel = the real section-budget limit
	//   CAP3[31:24] = NCOEF coefficient RAM words/bank = 5·NSEC (FIR base = 2·NCOEF)
	// These two fields read 0 on an old bitstream (undriven) ⇒ callers must treat 0 as "not reported"
	// and fall back to the compile-time constants.
	NumSections  int
	NumCoefWords int
	// CAP3[23:16] = the number of **independent delay-slot pointers** per channel (since 2026-09-21;
	// 0 = old bitstream, which allows only one delay slot)
	DelaySlots int
	// The cost fields of CAP3 (since 2026-09-20 software **really uses them** to compute the frame
	// budget):
	//   [7:0]  = average cycles per section (9 after RTL-2; an old bitstream reports 5 = MAC cycles
	//            only ⇒ not trustworthy)
	//   [15:8] = total cycles of one POLY slot (26)
	CostPerSection int
	CostPoly       int
	// CAP4 (word 25, added 2026-09-21) = the capacity and cost of the **second small FIR** (OP_SFIR):
	//   [7:0]   = log2(maximum block count)  ⇒ taps limit = SFIRMACS << that value (same semantics as
	//             CAP2[15:8])
	//   [15:8]  = MACs per cycle (8)
	//   [23:16] = fixed overhead of one small FIR (cycles/channel, excluding taps/SFIRMACS)
	// These three fields read 0 on an old bitstream (that address used to read 0) ⇒ callers must treat
	// 0 as "this bitstream has no small FIR".
	SFIRMaxLog2  int
	SFIRMACS     int
	SFIROverhead int
}

// dspProbeEngine reads CAP0 once to determine which generation the bitstream is. Called from
// dspInit.
func dspProbeEngine() {
	dspEngineGen = 0
	if !dspAvailable {
		return
	}
	c0 := regRead(regCap0)
	if uint16(c0>>16) != capMagic {
		// Old bitstream: CAP0 reads back 0 (or any old value) ⇒ take the old 30-coefficient path
		log.Printf("DSP engine: old bitstream (no CAP magic, read back %#x) ⇒ using the fixed 6-section path", c0)
		return
	}
	dspEngineGen = 1
	caps := dspReadEngineCaps()
	log.Printf("DSP engine: 0.2 slot-table engine v%d (%d slots, opcode bitmap %#08x, FIR log2=%d, DELAY log2=%d)",
		caps.Version, caps.NumSlots, caps.Opcodes, caps.FirMaxLog2, caps.DelayLog2)
}

// dspReadEngineCaps reads CAP0..CAP4.
func dspReadEngineCaps() dspEngineCaps {
	c0 := regRead(regCap0)
	c1 := regRead(regCap1)
	c2 := regRead(regCap2)
	c3 := regRead(regCap3)
	c4 := regRead(regCap4)
	return dspEngineCaps{
		Present:        uint16(c0>>16) == capMagic,
		Version:        uint16(c0 & 0xFFFF),
		Opcodes:        c1,
		NumSlots:       int(c2 & 0xFF),
		FirMaxLog2:     int((c2 >> 8) & 0xFF),
		DelayLog2:      int((c2 >> 16) & 0xFF),
		NumSections:    int((c2 >> 24) & 0xFF),
		NumCoefWords:   int((c3 >> 24) & 0xFF),
		DelaySlots:     int((c3 >> 16) & 0xFF),
		CostPerSection: int(c3 & 0xFF),
		CostPoly:       int((c3 >> 8) & 0xFF),
		// ⚠️ On an **old bitstream** regCap4 (word 25) reads back 0 (that address used to be others
		//    ⇒ 0) — exactly the "not reported" semantics we want: SFIRMaxLog2 = 0 ⇒ no small FIR.
		SFIRMaxLog2:  int(c4 & 0xFF),
		SFIRMACS:     int((c4 >> 8) & 0xFF),
		SFIROverhead: int((c4 >> 16) & 0xFF),
	}
}

// ── Capacities in force at run time (fixed by the **hardware's self-report** after reading CAP at
// boot) ─────────────────────────────────────────────────────────────────
//
// Why compile-time constants alone are not enough: NSEC/NCOEF live in the HDL, while the Go side is
// written "against one particular bitstream". With a **new backend on an old bitstream** the software
// cannot read the hardware's real capacity and would download more sections than its own constants
// allow — and the hardware's `cidx < NCOEF` **silently drops** out-of-range coefficients (every
// interface looks fine, only that stage sounds wrong). Adding these two CAP fields on 2026-09-20 was
// meant to close exactly that hole. An old bitstream reads 0 ⇒ treat it as "not reported" and use the
// constants.
var (
	hwMaxSections = maxSections
	hwCoefWords   = coefWordsPerBank
	hwCoefFIRBase = coefFIRBase
	hwDelayWords  = maxDelayWords
	// How many **mutually independent** delay slots each channel can have. The default of 1 is the
	// most conservative (an old bitstream has only one write pointer, so two delay slots would share
	// it and tread on each other). After reading CAP3[23:16] at boot it is widened to the hardware's
	// self-report.
	hwDelaySlots = 1
	// Upper bound on the small FIR's taps (= how long OP_SFIR can run on this bitstream). The default
	// is the compile-time value of **this RTL**; after reading CAP4 at boot it is overwritten with the
	// hardware's self-report, and **if the hardware says there is none it is squashed to 0** — see
	// adoptCapsFrom.
	// Why this is necessary: with a new backend on an old bitstream, if the software assumed a small
	// FIR existed it would send an OP_SFIR slot, and the old engine **does not know this opcode**
	// (it takes `default: skip`) ⇒ the whole dry path disappears while the wet signal remains, so the
	// sound is wrong while every interface returns 200 OK. Once squashed to 0, PBP is explicitly
	// rejected (better not to do it at all).
	hwSFIRMaxTaps  = sfirTaps
	hwSFIROvh      = sfirOvh
	hwCoefSFIRBase = coefSFIRBase
)

// dspAdoptHardwareCaps is called once after dspInit has read the CAPs. Idempotent.
func dspAdoptHardwareCaps() {
	if dspEngineGen != 1 {
		return
	}
	adoptCapsFrom(dspReadEngineCaps())
}

// adoptCapsFrom is the **pure logic** of "tighten the run-time limits to the hardware's
// self-reported capacities" (so that it can be unit-tested).
// 0 means "this bitstream does not report this field" (old bitstream) ⇒ leave the compile-time
// constant untouched.
func adoptCapsFrom(caps dspEngineCaps) {
	if n := caps.NumSections; n > 0 {
		if n < hwMaxSections {
			log.Printf("⚠️ the bitstream self-reports NSEC=%d, below the backend's section limit %d ⇒ **tightening to %d**"+
				" (this is the silent coefficient-loss scenario of 'new backend on an old bitstream'; the hardware wins)",
				n, hwMaxSections, n)
		}
		hwMaxSections = n
	}
	if n := caps.NumCoefWords; n > 0 {
		if n != hwCoefWords {
			log.Printf("bitstream self-reports NCOEF=%d (backend constant %d) ⇒ the coefficient space and FIR base follow the hardware",
				n, hwCoefWords)
		}
		hwCoefWords = n
		hwCoefFIRBase = 2 * n // COEF_FIR_BASE = 2·NCOEF (the same relation as in the HDL)
		// The small FIR's base = COEF_FIR_BASE + NCH·FIR_TAPS (the HDL's COEF_SFIR_BASE) — it
		// follows NCOEF and must not be hard-coded (hard-coding is the same species of bug as the
		// "EQ coefficients trampled when convolution is switched on" of 2026-09-20).
		hwCoefSFIRBase = hwCoefFIRBase + 2*firTaps
	}
	// Small FIR (OP_SFIR): **only accepted when the hardware reports both places** —
	// ① CAP1 bit7 (this bitstream really has OP_SFIR built in); ② CAP4 reports the MACs per cycle and
	// the block-count limit. Accepting only one of them diverges: with the bitmap bit set but CAP4
	// undriven (or the other way round) a bogus limit would be computed.
	if caps.SFIRAvailable() && caps.SFIRMACS > 0 && caps.SFIRMaxLog2 > 0 {
		taps := caps.SFIRMACS << caps.SFIRMaxLog2
		ovh := caps.SFIROverhead
		if taps != hwSFIRMaxTaps || ovh != hwSFIROvh {
			log.Printf("bitstream self-reports a small FIR: taps ≤%d (%d MACs/cycle, fixed overhead %d cycles) ⇒ following the hardware",
				taps, caps.SFIRMACS, ovh)
		}
		hwSFIRMaxTaps = taps
		hwSFIROvh = ovh
	} else if !caps.SFIRAvailable() {
		// This bitstream has **no** small FIR: squash the limit to 0 so that PBP is explicitly rejected
		// (rather than sending an OP_SFIR slot the old engine does not know ⇒ the dry path silently
		// disappearing).
		hwSFIRMaxTaps = 0
	}
	if n := caps.DelaySlots; n > 0 {
		if n != hwDelaySlots {
			log.Printf("bitstream self-reports %d independent delay-slot pointers per channel (CAP3[23:16]) ⇒ up to %d delays in one frame", n, n)
		}
		hwDelaySlots = n
	}
	if n := caps.DelayLog2; n > 0 {
		// The delay-ring depth follows the **hardware's self-report** (adopted in both directions): it
		// sets the largest L the chain compiler allows.
		hwDelayWords = 1 << n
	}
	log.Printf("run-time capacities: sections ≤%d, coefficients %d words/bank, FIR base %d, small FIR base %d (taps ≤%d), "+
		"delay ring %d words/channel (%.1f ms @48k)",
		hwMaxSections, hwCoefWords, hwCoefFIRBase, hwCoefSFIRBase, hwSFIRMaxTaps,
		hwDelayWords, float64(hwDelayWords)/48.0)
}

// ── Frame budget (after RTL-2 the software can finally work it out itself) ─────────────────
//
// Semantics: the budget for one sample of **one channel** is 100 MHz ÷ 48 kHz ÷ 2 = **1041 cycles**
// (the engine makes one pass per channel). The cost = sections×cycles per section + convolution
// + exciter + slot scheduling. Going over means **dropped samples** (an audible "pop", with the
// STATUS[31:24] counter non-zero). Measured on 2026-09-20: 36 sections + convolution dropped samples
// before RTL-2 (95.5% by the model) and that fell to 84% after RTL-2 ⇒ this check now "computes from
// the hardware's self-reported numbers" instead of guessing.
const frameBudgetCycles = 1041 // 100e6 / 48e3 / 2, rounded down

// costPerSectionRuntime returns the "average cycles per section". A self-reported value < 7 is
// **not trustworthy** (an old bitstream reports 5 = MAC cycles only), so it falls back to the
// conservative 12 (the pre-speed-up semantics).
func costPerSectionRuntime() int {
	if n := dspReadEngineCaps().CostPerSection; n >= 7 {
		return n
	}
	return 12
}

// costPolyRuntime returns the total cycles of one exciter (POLY) slot; if it is not reported, the
// documented 26 is used.
func costPolyRuntime() int {
	if n := dspReadEngineCaps().CostPoly; n >= 20 {
		return n
	}
	return 26
}

// sfirCyclesRuntime returns the **cycles per frame of one small FIR (OP_SFIR)** (per channel).
// Semantics = taps/sfirMACS + fixed overhead, with both components taken from the CAP4 self-report
// (only falling back to this RTL's compile-time constant when they cannot be computed). Why it must
// be computed from the hardware's self-report: changing sfirTaps/sfirMACS changes the cycle count of
// the same slot (the same argument as firCycles for convolution) — using the Go constant as a proxy
// makes the frame-budget decision for "new backend on an old bitstream" too optimistic, and going
// over budget shows up as **dropped samples** (occasional clicks).
func sfirCyclesRuntime() int {
	if !dspAvailable || dspEngineGen != 1 {
		return sfirCycles
	}
	c := dspReadEngineCaps()
	if c.SFIRMACS > 0 && c.SFIRAvailable() {
		taps := c.SFIRMACS << c.SFIRMaxLog2
		if taps > 0 && c.SFIROverhead > 0 {
			return taps/c.SFIRMACS + c.SFIROverhead
		}
	}
	return sfirCycles
}

// chainFrameCost estimates the chain's cycles per frame (per channel). The convolution term uses
// firCycles (= 256, the two parallel kernels covering 4096 taps ÷ firMACS(16)); in the MACS=8 days
// this number was 512.
// hasSFIR = the chain has the second small FIR (PBP's dry path) ⇒ add sfirCyclesRuntime() (= 19
// cycles).
// hasJoint = the chain has the joint-stereo frame pass (ColorfulMusic) ⇒ add colorfulJointCycles
// (40 cycles/frame).
// ⚠️ It is added to the **right-channel pass** (the left pass only skips and captures the L-side
// input), while the budget semantics are "one pass per channel"
//
//	⇒ adding it straight on is fine (the left pass is actually shorter). The check is criterion ⑥ of
//	tb_engine_js (ack=1041 with no dropped samples).
func chainFrameCost(sections int, hasFIR, hasPOLY, hasSFIR, hasJoint bool, slots int) int {
	cost := sections*costPerSectionRuntime() + slots*2 // slot scheduling ≈2 cycles/slot
	if hasFIR {
		cost += firCycles
	}
	if hasSFIR {
		cost += sfirCyclesRuntime()
	}
	if hasPOLY {
		cost += costPolyRuntime()
	}
	if hasJoint {
		cost += colorfulJointCycles
	}
	return cost
}

// dspReadStatus reads the engine status ([15:8] slots executed this frame, [23:16] sections, bit0
// commit pending, bit1 active bank).
func dspReadStatus() uint32 {
	return regRead(regStatus)
}

// TruepeakAvailable reports whether **this hardware** really has a true-peak limiter (CAP1 bit9).
// Why not let callers write `engine_gen == 1`: that is a proxy for "the bitstream is of the 0.2
// generation" and does not mean "the limiter is really built into this generation" — the two diverge
// (a trimmed gen2 in the future would diverge). The hardware already puts the answer in the bitmap,
// so just read it and report it.
func (c dspEngineCaps) TruepeakAvailable() bool {
	return c.Present && c.Opcodes&capOpcodeLimTP != 0
}

// HeadroomAvailable reports whether this bitstream implements in-chain headroom (CAP1 bit10).
func (c dspEngineCaps) HeadroomAvailable() bool {
	return c.Present && c.Opcodes&capOpcodeHeadroom != 0
}

// DynAvailable reports whether this bitstream implements the DYN slot (CAP1 bit11, used by
// "dynamic bass / dynamic loudness"). Again **without looking at the engine generation**: whether a
// bitstream of the same generation includes it is decided by the build, and software decides from
// that whether it can be downloaded.
// JointStereoAvailable: whether this bitstream has the **joint-stereo frame pass** (CAP1 bit12,
// OP_JDST/OP_J3DS). The hardware prerequisite for V4A ColorfulMusic — it must hold L and R in the
// same cycle.
func (c dspEngineCaps) JointStereoAvailable() bool {
	return c.Present && c.Opcodes&capOpcodeJointStereo != 0
}

// DelayAvailable: whether this bitstream has the DELAY slot (the short delay ring).
func (c dspEngineCaps) DelayAvailable() bool {
	return c.Present && c.Opcodes&capOpcodeDelay != 0
}

// Mix2Available: whether this bitstream has the MIX2 slot (the hardware prerequisite for
// crossfeed).
func (c dspEngineCaps) Mix2Available() bool {
	return c.Present && c.Opcodes&capOpcodeMix2 != 0
}

// firParams is the minimal description of "this stage wants convolution": how many taps, and what
// the IR is called.
// The coefficients themselves are not here (they are loaded by /api/dsp/ir into the address space
// starting at coefFIRBase).
type firParams struct {
	Taps int    `json:"taps"`
	Name string `json:"name,omitempty"`
}

// currentFIR is the convolution stage on the current chain (nil = no such stage). Same shape as
// currentCrossfeed / currentSurround.
var currentFIR *firParams

// firParamsBlocks folds taps into a MACS block count (it must be a power of two; the planner
// validates it).
// firGainDBForHeadroom returns the gain (dB) the current convolution stage can contribute in the
// **worst case**; it is 0 when there is no convolution stage.
//
// Why the L1 norm Σ|h[k]|: for any input with |x[n]|≤1 we have |y[n]| = |Σ h[k]·x[n−k]| ≤ Σ|h[k]|,
// so it is a **hard upper bound** on "can convolution blow up the in-chain level" (more conservative
// than the frequency-response peak, and more honest).
// Measured on the reference clear_bass IR: Σ|h| = 3.738 ⇒ **+11.5 dB** (the frequency-response
// peak is only +4.3 dB).
//
// Why it is essential: the headroom decision (`cascadeMaxGainDB(out)` in `dspFinalCoeffs`) only looks
// at the EQ biquad cascade and **cannot see the convolution stage**. So with EQ off (the local
// JamesDSP configuration has it off) maxGain≈0 ⇒ the decision is "no headroom needed" ⇒ the 18 dB
// in-chain headroom is switched off; while full-scale music through convolution then drives the
// output straight to 24 bits (hard clipping).
// On the board this showed up as **a "hissing" noise at high volume** (2026-09-18). Fixed-point
// simulation (0 dBFS 1 kHz sine): headroom off ⇒ **26795 of 48000 samples clipping**; headroom on
// (÷8) ⇒ 0.
func firGainDBForHeadroom() float64 {
	irMu.Lock()
	defer irMu.Unlock()
	if currentFIR == nil || len(irBank[0]) == 0 {
		return 0
	}
	var worst float64
	for _, ch := range irBank {
		var l1 float64
		for _, v := range ch {
			if v < 0 {
				l1 += -float64(v)
			} else {
				l1 += float64(v)
			}
		}
		if l1 > worst {
			worst = l1
		}
	}
	if worst <= 0 {
		return 0
	}
	return 20 * math.Log10(worst/32768.0) // irBank is Q3.15 (1.0 = 32768)
}

func firParamsBlocks(p *firParams) int {
	taps := firTaps
	if p != nil && p.Taps > 0 && p.Taps <= firTaps {
		taps = p.Taps
	}
	blocks := taps / firMACS
	if blocks < 1 {
		blocks = 1
	}
	lg := 0
	for (1 << lg) < blocks {
		lg++
	}
	return 1 << lg // round up to a power of two (staying within firTaps is guaranteed by the caller)
}

// firAvailable reports whether this bitstream has the FIR slot (the hardware prerequisite for
// convolution/DDC, CAP1 bit5).
func firAvailable() bool {
	return dspEngineGen == 1 && dspReadEngineCaps().FIRAvailable()
}

func (c dspEngineCaps) FIRAvailable() bool {
	return c.Present && c.Opcodes&capOpcodeFIR != 0
}

func (c dspEngineCaps) DynAvailable() bool {
	return c.Present && c.Opcodes&capOpcodeDyn != 0
}

// BiquadAvailable: BIQUAD (CAP1 bit1) — the engine's most basic capability bit, relied on by
// effects such as the tube flavour that "need only a biquad".
func (c dspEngineCaps) BiquadAvailable() bool {
	return c.Present && c.Opcodes&(1<<1) != 0
}

// PolyAvailable: whether this bitstream has the POLY slot (the non-linear harmonic stage, CAP1
// bit6).
// The hardware prerequisite for harmonic excitation (the V4A SpectrumExtend / AnalogX family).
func (c dspEngineCaps) PolyAvailable() bool {
	return c.Present && c.Opcodes&capOpcodePoly != 0
}

// SFIRAvailable: whether this bitstream has the **second small FIR** (OP_SFIR, CAP1 bit7).
// The hardware prerequisite for ViPERBass's PURE_BASS_PLUS setting (its dry path is a 63-tap
// polyphase kernel, with its own RAM/multipliers separate from the 8192-tap convolution stage).
// Again **without looking at the engine generation**.
func (c dspEngineCaps) SFIRAvailable() bool {
	return c.Present && c.Opcodes&capOpcodeSFIR != 0
}

// sfirAvailableNow: whether the current hardware can run the small FIR right now (the bitmap bit is
// set **and** CAP4 reports the capacity). Both conditions are required: accepting the bitmap alone
// would let a bitstream with "the bit set but CAP4 undriven" be planned against a bogus limit (taps
// beyond the hardware ⇒ fir_bank saturates nblk ⇒ a wrong convolution computed **silently**).
func sfirAvailableNow() bool {
	if !dspAvailable || dspEngineGen != 1 {
		return false
	}
	c := dspReadEngineCaps()
	return c.SFIRAvailable() && c.SFIRMACS > 0 && c.SFIRMaxLog2 > 0
}

// slotLimit is **how many slots can really be used right now**: the smaller of the software's
// compile-time constant and the hardware's self-reported NSLOT.
//
// Why a constant alone is not enough: `dspSlotMax` is written against **this RTL** (NSLOT=24), while
// the bitstream running on the board may be old (NSLOT=8). With new software + an old bitstream the
// software thinks it can fit 24 slots while the hardware table has only 8×8=64 words ⇒ the 65th word
// **wraps around and overwrites the first two slots** (every interface says ok, the table is
// scribbled over). The hardware self-reports NSLOT in CAP2[7:0], so it is read here and the smaller
// value is taken — this is the only guarantee that "falling back to an old bitstream still runs
// safely". With no hardware / an old bitstream (CAP unreadable) it falls back to the software
// constant (which is also the path offline tests take).
func slotLimit() int {
	if !dspAvailable || dspEngineGen != 1 {
		return dspSlotMax
	}
	if n := dspReadEngineCaps().NumSlots; n > 0 && n < dspSlotMax {
		return n
	}
	return dspSlotMax
}

// ── Effect chain → slot table (pure functions, testable offline) ────────

// slotPlan is the complete plan for one download: descriptor words + coefficients + active slot
// count.
// It is split out as pure data so that it can be **unit-tested offline**: the address arithmetic here
// is where P1-c is most likely to go wrong.
type slotPlan struct {
	// Words is the ordered "slot word index → value" list (ascending by address; the pointer
	// auto-increments as it is written)
	Words []slotWord
	// Coefs is the coefficient table (ascending by coef address)
	Coefs []int32
	// FirCoefs are the convolution kernel's coefficients (**not part of Coefs**): when written they
	// land in the address space starting at coefFIRBase, channel 0 first and channel 1 after. It is
	// non-empty only when the IR has changed (8192 words, so there is no need to move them on every
	// download).
	FirCoefs []int32
	// SFirCoefs are the **second small FIR**'s coefficients (likewise **not part of Coefs**): they
	// land in the address space starting at coefSFIRBase (2×sfirTaps = 128 words, channel 0 first).
	// It is non-empty only when the chain has PBP.
	// Why not stuff them into FirCoefs: the two have different base addresses (2·FIR_TAPS apart), and
	// mixing them would mean guessing from len "is this stretch convolution or the small FIR" —
	// guessing wrong writes the polyphase kernel into the user's IR.
	SFirCoefs []int32
	// Slots is the number of active slots (written into the header)
	Slots int
	// Sections is the **section-budget usage**: the per-channel biquad section count + (with a joint
	// stereo stage, the jointStateReserve reserved words). The semantics match the capacity self-check
	// in buildSlotPlanNodes — the joint state is **one copy per frame** and always occupies the last
	// two words of the left channel's sections, so it really does eat into the section budget.
	Sections int
	// StereoFrame means this table **carries a joint stereo stage**: the header must set bit8 (sf_en),
	// and the entry bus of the later per-channel slots is JBus (the hardware presets "this channel's
	// joint output from the previous frame" onto that bus in every S_IDLE pass, see the dsp_engine.v
	// file header).
	StereoFrame bool
	// JBus is the entry bus number of the later per-channel slots (meaningful only when StereoFrame is
	// set).
	JBus int
	// HasBassMix / BassMixCoef: true when the chain has ViPERBass's MIX2 slot; BassMixCoef is the
	// starting index of its **two weights** in Coefs (= Coefs[BassMixCoef], [BassMixCoef+1]).
	//
	// Why the compiler reports it instead of letting the user search the table: the slot table's cfg
	// word only has "opcode + section count" and **no effect id** — there are several MIX2 slots in a
	// table (crossfeed / exciter / the two XHIFI stages / AnalogX / ColorfulMusic all have them), and
	// outside the table software cannot tell which one is ViPERBass. Only the compiler knows which
	// node it just compiled. Use: ViPERBass's antiPop ramp has to change these two words (taking the
	// whole stage's gain from 0 up to 1, see antipop.go).
	HasBassMix  bool
	BassMixCoef int
}

type slotWord struct {
	Addr  int
	Value uint32
}

// buildSlotPlan compiles a cascade of "5 Q3.15 coefficients per section" into a slot table.
//
// Layout rules (consistent with the default chain semantics of dsp_engine.v, but packed into
// multi-section slots):
//
//	· slot i: in_a = i, out_bus = i+1, coef_base = the coefficient address of the slot's first
//	  section, state_base = the state group number of the slot's first section, n = sections in this
//	  slot;
//	· inside a section: section k's coefficients are at coef_base + k*5 and its state at
//	  state_base + k (independent per channel);
//	· the header (address dspSlotMax*8) holds the number of active slots — 0 means "keep using the old
//	  nbands semantics", so a non-zero value **must** be written here, otherwise the engine would go
//	  and read the old nbands.
//
// Going over a limit is **reported as an error**, never silently truncated (project rule: no quietly
// dropping data).
func buildSlotPlan(sections [][5]int32) (slotPlan, error) {
	var p slotPlan
	if len(sections) == 0 {
		// Empty chain = full bypass: no slots are written, only the header is set to 0 (the engine takes
		// the old nbands path = 0 sections = pass-through)
		p.Slots = 0
		return p, nil
	}
	if len(sections) > hwMaxSections {
		return p, fmt.Errorf("chain too long: %d sections > engine limit of %d sections (the coefficient/state RAM cannot hold them; nothing will be truncated)",
			len(sections), hwMaxSections)
	}

	sec := 0
	slot := 0
	for sec < len(sections) {
		n := len(sections) - sec
		if n > secPerSlot {
			n = secPerSlot
		}
		cfg := uint32(opBiquad) | uint32(n)<<16 // flags = 0
		p.Words = append(p.Words,
			slotWord{slot*slotWordSize + swCfg, cfg},
			slotWord{slot*slotWordSize + swCfb, uint32(sec * 5)},
			slotWord{slot*slotWordSize + swStb, uint32(sec)},
			slotWord{slot*slotWordSize + swInA, uint32(slot)},
			slotWord{slot*slotWordSize + swInB, 0xFFFF},
			slotWord{slot*slotWordSize + swOutB, uint32(slot + 1)},
			slotWord{slot*slotWordSize + swPrm, 0},
			slotWord{slot*slotWordSize + swRsv, 0},
		)
		for k := 0; k < n; k++ {
			c := sections[sec+k]
			p.Coefs = append(p.Coefs, c[0], c[1], c[2], c[3], c[4])
		}
		sec += n
		slot++
	}
	p.Slots = slot
	p.Sections = sec
	if p.StereoFrame {
		p.Sections += jointStateReserve // the 2 words the joint state always occupies count towards the section budget too
	}
	return p, nil
}

// ── Download ───────────────────────────────────────────────────────────

// dspActiveBankCtrl returns the bit to write into CTRL.bank_sel to "point the parameter window at
// the **active bank**".
// STATUS.bit1 is the active bank (see the status assembly in dsp_engine.v), and the bit value of
// bank_sel = the bank number.
func dspActiveBankCtrl() uint32 {
	if dspReadStatus()&0x2 != 0 {
		return ctrlBankSel
	}
	return 0
}

// dspWriteActiveCoef changes coefficient number idx of the **bank that is currently running**
// directly to v.
//
// ⚠️ This path **deliberately does not** use dspDownloadSlotPlan's "write the inactive bank + COMMIT
// to swap it all over"; the two are two uses of the same coefficient write port
// (dsp_engine.v:660-661 `coef[cfb_wr + cidx] <= cdat`, cfb_wr = bank_sel ? NCOEF : 0):
//
//	· whole-table download: changing topology / the whole chain ⇒ **must** take effect as a whole,
//	  using the inactive bank + a frame-boundary COMMIT (see the comment at the start of
//	  dspDownloadSlotPlan); there must be no "half new, half old".
//	· this function: changes only **two coefficients with the topology untouched word for word**
//	  (ViPERBass's antiPop ramp) ⇒ writing the active bank directly, word by word, is enough, and it
//	  **must** be done this way: the COMMIT path would mean re-moving the whole table (24 slots × 8
//	  words + all coefficients + the COMMIT handshake each time) on every step, which for a ramp of
//	  200 steps of 5 ms each is two orders of magnitude too expensive.
//
// Why "writing the active bank" is safe (the evidence): coefficients are **fetched every frame** —
// dsp_engine.v:1328 sets cf_a to cfb_act + sl_cfb_r, S_FETCH(:1355-1361) reads coef_rd cycle by
// cycle, and :1391-1398 enters S_MIX with the c0/c1 just read; there is no "shadow register loaded
// only at COMMIT".
// The only visible untidiness: when one step's two writes happen to fall between the c0/c1 fetches of
// the same slot, that frame (20.8 µs) gets "new c0 + old c1". A single ramp step is Δa ≤ 1/200 ⇒ the
// error in that frame is ≤ 0.5% in amplitude, inaudible.
//
// The price in readback consistency: writing the active bank makes "the coefficients in the hardware"
// briefly disagree with lastPlanCoefs, so during the ramp /api/dsp/check falsely reports a mismatch
// (it recovers once the ramp ends and everything is restored bit for bit). See antipop.go.
func dspWriteActiveCoef(idx int, v int32) error {
	if !dspAvailable {
		return errDSPUnavailable
	}
	if dspEngineGen != 1 {
		return fmt.Errorf("the bitstream is not the 0.2 slot-table engine (no CAP0 magic), so there is no coefficient RAM that can be rewritten at run time")
	}
	if idx < 0 || idx >= hwCoefWords {
		return fmt.Errorf("coefficient index %d out of range (this bitstream has only %d words per bank)", idx, hwCoefWords)
	}
	dspSetCtrlBits(ctrlBankSel, dspActiveBankCtrl())
	regWrite(regCidx, uint32(idx))
	regWrite(regCdat, uint32(v)&coefMask)
	return nil
}

// dspDownloadSlotPlan writes the plan into the hardware: coefficients → descriptors → header →
// COMMIT.
// What is written is the **inactive bank**, and after COMMIT the engine swaps over as a whole at the
// next frame boundary ⇒ there is no "half new, half old" intermediate state (which is exactly the
// flaw of 0.1's "write coefficient by coefficient while it runs").
func dspDownloadSlotPlan(p slotPlan) error {
	if !dspAvailable {
		return errDSPUnavailable
	}
	if dspEngineGen != 1 {
		return fmt.Errorf("the bitstream is not the 0.2 slot-table engine (no CAP0 magic), so a slot table cannot be downloaded")
	}

	// ⓪ A whole-table download rewrites one of the two banks and flips the active bank ⇒ first let the
	//    running antiPop ramp finish cleanly (it restores the coefficients to the compiled values under
	//    the lock, and cannot write again after it returns — see antiPopCancelRamp). Skipping this step:
	//    the ramp's next step would write those two indices into a different section of the **new**
	//    table and spoil another effect.
	antiPopCancelRamp()

	// ① Choose the inactive bank: read STATUS.bit1 for the active bank and write the other one
	st := dspReadStatus()
	bank := uint32(0)
	if st&0x2 == 0 {
		bank = ctrlBankSel
	}
	dspSetCtrlBits(ctrlBankSel, bank)

	// ② Coefficients: **set CIDX explicitly for every word**; do not rely on CDAT
	// auto-incrementing.
	//
	// Why not take the easy way of "set CIDX to 0 and write 35 of them in a row": in the bitstream the
	// wrap point of the index auto-increment is the **old semantics** `DSP_NB*5 - 1 = 29` (see the
	// dsp_cidx_inc branch in axi_i2s_adi_v1_2.vhd). So the 31st word wraps around to address 0: on the
	// board, after downloading a 7-section chain (35 words) the coefficient RAM was
	//   [band6, band1, band2, band3, band4, band5, 0,0,0,0,0]
	// — section 0 (the one carrying the automatic preamp) was trampled by section 6, section 6 itself
	//   landed in 0..4, and addresses 30..34 were never written. Audibly section 7 was empty, and
	//   `/api/dsp/check` reported a false failure.
	// When CIDX is written explicitly the hardware **cancels** the pending auto-increment
	// (`dsp_cidx_inc <= '0'`), so the addressing limit of this path is the coefficient RAM's 128 words
	// (16 sections × 5 = 80), consistent with the engine's NSEC/NSLOT.
	// (The cleaner fix on the bitstream side is to change the wrap point to NCOEF-1; it is recorded in
	//   the RTL to-do list; the software path works right now and has no effect at all on the 0.1
	//   30-word path.)
	for i, c := range p.Coefs {
		regWrite(regCidx, uint32(i))
		regWrite(regCdat, uint32(c)&coefMask)
	}
	// ②b FIR coefficients (the convolution kernel's own address space). As with biquad, **set CIDX
	//     explicitly for every word**: the FIR addresses are already beyond 128, so relying on
	//     auto-increment would wrap back into the biquad space and trample the EQ coefficients.
	for i, c := range p.FirCoefs {
		regWrite(regCidx, uint32(hwCoefFIRBase+i))
		regWrite(regCdat, uint32(c)&coefMask)
	}
	// ②c The **second small FIR**'s coefficients (ViPERBass PBP's dry path, the third address space).
	//     CIDX is likewise set explicitly word by word: its addresses are even higher than the
	//     convolution region, so auto-increment would wrap back into the biquad space.
	for i, c := range p.SFirCoefs {
		regWrite(regCidx, uint32(hwCoefSFIRBase+i))
		regWrite(regCdat, uint32(c)&coefMask)
	}

	// ③ Descriptor words: the slot area is **contiguous** from address 0, so set SLOT_ADDR to 0 and
	//    write them one by one (the auto-increment wrap point is the header at 64 and there are at most
	//    48 slot words, which is enough; unlike CIDX, no per-word address is needed here)
	regWrite(regSlotAddr, 0)
	for _, w := range p.Words {
		regWrite(regSlotData, w.Value)
	}
	// The header is outside the slot area and is written once on its own: address = dspSlotMax*8.
	// [7:0] = active slot count | [8] = sf_en (carries a joint stereo stage) | [13:9] = jbus (entry bus
	// of the later stages).
	// ⚠️ The three fields must be written **together**: the header is one 16-bit register (hdr_bank),
	// and writing them separately would overwrite one another.
	hdr := uint32(p.Slots) & 0xFF
	if p.StereoFrame {
		hdr |= 1 << 8
		hdr |= (uint32(p.JBus) & 0x1F) << 9
	}
	regWrite(regSlotAddr, slotHdrAddr)
	regWrite(regSlotData, hdr)

	// ③b Log one line saying "how many slots/sections this version actually downloaded".
	// Why it is worth keeping permanently: `/api/status` only returns the number of slots the hardware
	// **executed in the previous frame**, while "the table the software compiled" and "the table the
	// hardware is really running" are two different things (a descriptor write wrapping around, a
	// COMMIT that did not take effect, being overwritten by a later download … all make the two
	// disagree). With this line, a reconciliation immediately shows which side the problem is on.
	// The **slot order** (each slot's opcode) goes into the log as well: a change such as chain order
	// has no other observable evidence on the board — the slot table has no readback port and STATUS
	// only reports "how many slots were executed". With this line one glance confirms that a structural
	// change such as "convolution(5) comes before DDC's biquad(1)" really took effect.
	var opseq []string
	for i := 0; i < p.Slots; i++ {
		for _, w := range p.Words {
			if w.Addr == i*slotWordSize+swCfg {
				opseq = append(opseq, fmt.Sprintf("%d", w.Value&0xFF))
			}
		}
	}
	log.Printf("slot table downloaded: %d slots / %d sections / %d coefficients (bank %d) slot order=[%s] slot order meaning=0NOP 1BIQUAD 2DYN 3MIX2 4DELAY 5FIR 6POLY 7SFIR 8SAT 9JDST 10J3DS",
		p.Slots, p.Sections, len(p.Coefs), 1-(bank>>17), strings.Join(opseq, ","))

	// ④ COMMIT: write CTRL.bit16 = 1 (the hardware turns it into a one-cycle pulse; the bit itself is
	// not latched)
	dspSetCtrlBits(ctrlCommit, ctrlCommit)

	// ⑥ Wait for it to land (STATUS.bit0 = commit pending), up to 50 ms
	deadline := time.Now().Add(50 * time.Millisecond)
	for dspReadStatus()&0x1 != 0 {
		if time.Now().After(deadline) {
			return fmt.Errorf("COMMIT timed out without taking effect (STATUS=%#x)", dspReadStatus())
		}
		time.Sleep(200 * time.Microsecond)
	}

	// ⑦ The parameter window follows the **new active bank**.
	// After COMMIT the active bank has already flipped, but bank_sel still points at the one just
	// **written** (= the old active bank) ⇒ a readback would return the previous data. Without this
	// step `/api/dsp/check` is right only half the time (measured on the board: applying 6 presets in a
	// row, 3 times ok=false, still reading the previous preset's coefficients).
	// The readback must be the same copy as "the one being played", otherwise the deployment
	// self-check is a random number.
	bank = 0
	if dspReadStatus()&0x2 != 0 {
		bank = ctrlBankSel
	}
	dspSetCtrlBits(ctrlBankSel, bank)
	return nil
}

// dspSetCtrlBits reads-modifies-writes some bits of CTRL (leaving the bypass/section-count/limiter
// bits untouched).
func dspSetCtrlBits(mask, val uint32) {
	cur := regRead(regCtrl)
	regWrite(regCtrl, (cur&^mask)|(val&mask))
}

// ── Slot table compilation with a DYN stage ──────────────────────────
//
// Why a separate path: `DYN` is not "just another biquad in the chain" — it needs **two buses**
// (the signal on in_a and the side chain on in_b), and the side chain's band-pass section must be
// computed first. The biquad 3-section packing logic is unchanged (the old path's output is identical
// word for word), while DYN becomes two slots of its own.
const (
	// dynScratchBus is the bus number the side chain occupies. On the RTL side NBUS went from 16 to 24
	// on 2026-09-21 and the reserved numbers shifted up accordingly: cross-channel = 22, DYN side
	// chain = **23** (see crossLoBus and dsp_engine.v).
	dynScratchBus = 23
	// crossLoBus is the bus "this channel's lo" is written to: in the RTL bus **22** is special-cased
	// as a **cross-channel register** (read = the other channel's lo, write = this channel's lo,
	// latched at the end of the pass), so crossfeed's lo section must be written to this bus. See
	// BUS_XF_LO in dsp_engine.v.
	// Why it moved from 14 up to 22 on 2026-09-21 (measured on the real chain):
	// VSE→convolution→DDC→ViPERBass→XHIFI compiled to bus numbers that went all the way up to **16**
	// (inside XHIFI base..base+4 with base=12), while 14/15 were reserved numbers ⇒ before the
	// expansion XHIFI's intermediate results would be written into the "cross-channel bus" and the
	// "DYN side-chain bus".
	// ⚠️ The final self-check `bus >= crossLoBus` only looks at the **final** bus (=13) and cannot see
	//    the 16 used inside XHIFI ⇒ this is not something "the self-check would catch"; the reserved
	//    numbers must be left generous enough (22/23).
	crossLoBus = 22
	// maxDelayWords = the delay ring's words per channel (= DLY_WORDS in dsp_engine.v =
	// 2^CAP2.delay_log2). This is the **compile-time default**; after reading CAP at boot
	// hwDelayWords takes over (a smaller bitstream tightens it automatically).
	// Raised from 256 (5.33 ms) to **8192 (170.7 ms)** on 2026-09-20, unlocking V4A
	// DiffSurround(≈92.9 ms), ColorfulMusic(20 ms) and ViPERBass-PBP(23 ms). The ring is a separate
	// array and does not interact with the EQ state sections.
	maxDelayWords = 8192

	// flDlyRight = descriptor flags bit2: this DELAY slot affects only the **second channel** (the
	// right channel is passed through / delayed), consistent with FL_DLY_R in dsp_engine.v. This is how
	// V4A's DiffSurround uses it.
	flDlyRight = 0x04

	// planKindBiquad / planKindDyn / planKindCross / planKindDelay are the values of planNode.Kind
	planKindBiquad = "biquad"
	planKindDyn    = "dyn"
	planKindCross  = "cross"
	planKindDelay  = "delay"
	planKindFIR    = "fir"
	// planKindExciter: a whole "harmonic exciter" block (four slots: HPF + POLY + LPF + MIX2). See
	// harmonic.go.
	planKindExciter = "exciter"
	// planKindAnalogX: V4A AnalogX (the "analog flavour" harmonic unit). The same family of shape as
	// exciter but with a **different slot order**:
	//   HP240 → POLY → **MIX2 (dry + wet×setting gain)** → LP(setting frequency) → Peak633
	//   Note that MIX2 comes **before** the low-pass (exciter is HPF→POLY→LPF→MIX2); copying the order
	//   wrongly changes the sound.
	planKindAnalogX = "analogx"
	// planKindXHIFI: Clarity's XHIFI setting (V4A `HiFi`): three **parallel** branches + two summing
	// stages.
	//   HP(1200)×3[×1.2g] + BP(120–1200, 6 first-order sections)[×g, delay 120] + LP(120)×1[delay 240]
	// See xhifi.go.
	planKindXHIFI = "xhifi"
	// planKindDynBass: the **simplified branch** of V4A `DynamicBass` (see dynbass.go): 4 slots / 3
	// sections / 15 coefficients.
	// ① a carrier slot (n=0, a bypass copy that occupies no section) writes this channel's dry signal
	// to cross-channel bus 22; ② the side-signal MIX2; ③ the 55 Hz low-pass BIQUAD (numerator ×K);
	// ④ the MIX2 that adds it back to the dry signal.
	planKindDynBass = "dynbass"
	// planKindViPERBass: V4A ViPERBass's NATURAL_BASS setting (see viperbass.go):
	//   scaling(×1/K) → low-pass(numerator×k, Q0.53, fc = the panel cutoff) → MIX2(dry + wet×bassFactor)
	// The two biquads are **packed into the same slot** (they are in series, which is exactly the
	// compiler's default packing shape), plus one MIX2 slot ⇒ 2 slots / 2 buses / 3 sections.
	// ⚠️ The scaling section is not a "redundant stage": without amplifying the numerator by K, the
	//    40 Hz low-pass's b0/b1/b2 would be **quantised to 0 entirely** in Q3.15 (the low-pass output
	//    would be constantly 0 and the effect would simply vanish). See the fixed-point precision
	//    section of viperbass.go.
	planKindViPERBass = "viperbass"
	// planKindViPERBassPBP: V4A ViPERBass's **PURE_BASS_PLUS setting** (see viperbass.go).
	// Its structural difference from NATURAL is that "the dry path is not a pass-through":
	//   dry —— small FIR (63-tap polyphase kernel, OP_SFIR) ─────────────┐
	//   x[n−64] → scaling(×1/K) → low-pass(numerator×k, Q0.53) → MIX2(dry + wet) ──┴→ output
	// ⇒ **4 slots** (delay / the two packed biquads / small FIR / MIX2), 4 buses at peak,
	//   and 3 sections (the accounting semantics of scaling + low-pass + MIX2); neither the small FIR
	//   nor the delay takes an EQ section or coefficient.
	planKindViPERBassPBP = "viperbass_pbp"
	// planKindColorfulMusic: V4A ColorfulMusic (= DepthSurround + Stereo3DSurround).
	// It is the **only** effect that needs the joint-stereo frame pass: it generates **two slots** at
	// once (JDST + J3DS), which really run only in the right-channel pass while the left pass only
	// captures the L-side input (see colorful.go and dsp_engine.v).
	planKindColorfulMusic = "colorfulmusic"

	// coefWordsPerBank = NCOEF (dsp_engine.v) = coefficient words per bank = **5·NSEC**.
	// The compiler must keep an eye on it itself: going over means **quietly writing someone else's
	// coefficients** (the hardware's `cidx < NCOEF` drops them outright).
	// Raised from 128 to 180 (= 5 × NSEC 36) on 2026-09-20, and from 180 to **240** (= 5 × 48) on
	// 2026-09-21: the real chain needs 202~207 words, and XHIFI's 70 words fall in 132..201 (at 180 the
	// board reported
	// `coefficient RAM does not fit: Clarity XHIFI needs 117..186 and each bank has only 180 words`).
	coefWordsPerBank = 240
)

// planNode is one node of the slot table compilation: a biquad section, or a DYN stage (with its
// detector band-pass).
type planNode struct {
	Kind  string
	Coefs [5]int32
	Side  [5]int32 // only for Kind=="dyn": the 5 coefficients of the detector band-pass
	// only for Kind=="cross": the hi-section coefficients and MIX2's two weights
	Hi  [5]int32
	Mix [2]int32
	// only for Kind=="delay": the delay length (**this channel's sample count**, 1..1024) and the
	// descriptor flag bits
	Len   int
	Flags int
	// only for Kind=="fir": the MACS block count (0 = the default firTaps/firMACS = 512 blocks
	// ⇒ 8192 taps). It must be a power of two (the slot CFG's n is log2-encoded).
	Blocks int
	// only for Kind=="exciter": POLY's 12 words (c0..c10 + the de-click length).
	// Coefs is reused for the "input high-pass", Hi for the "output low-pass", and Mix = {dry, wet}.
	Poly [12]int32
	// only for Kind=="xhifi": the 4th coefficient group (XHIFI's four branches have one each: HP /
	// the LP half of BP / the HP half of BP / the standalone LP). The first three groups reuse
	// Coefs / Hi / Side.
	Extra [5]int32
	// only for Kind=="xhifi": the weights of the **second** MIX2 stage (the first stage uses Mix).
	// XHIFI's branch gains are folded into the weights (see xhifi.go), so each of the two mixing stages
	// has its own weights.
	MixB [2]int32
	// only for Kind=="colorfulmusic": DepthSurround's 9 coefficients (Q3.15), in an order hard-wired to
	// the RTL's fetch phases: [g, g1, 960, 672, b0, b1, b2, a1, a2] (see colorfulJointCoefs).
	Joint [9]int32
	// only for Kind=="colorfulmusic": Stereo3DSurround's 2 matrix coefficients {ca, cb}.
	Mix3D [2]int32
	// only for Kind=="viperbass_pbp": the **dry-path small FIR's taps** (Q3.15, sfirTaps of them, the
	// last one always 0). They are not written into p.Coefs — they go to their own address range
	// (coefSFIRBase). Used together with Len: Len = the wet signal's delay (the official core measured
	// 64 samples).
	SFir []int32
}

// buildSlotPlanNodes compiles a node sequence into a slot table.
// Layout: consecutive biquads are packed up to secPerSlot; a dyn emits two slots
// (① detector band-pass: in_a=current bus → out_b=dynScratchBus; ② DYN: in_a=current bus, in_b=15
// → out_b=current+1).
func buildSlotPlanNodes(nodes []planNode) (slotPlan, error) {
	var p slotPlan
	if len(nodes) == 0 {
		p.Slots = 0
		return p, nil
	}
	// work out the section count first (biquad=1 section; dyn=2 sections: the detector band-pass + DYN
	// itself)
	sections := 0
	for _, n := range nodes {
		switch n.Kind {
		case planKindDyn:
			sections += 2 // detector band-pass + DYN itself
		case planKindCross:
			sections += 2 // lo section + hi section (MIX2 takes no state)
		case planKindDelay:
			// The delay ring is in a **separate array** (dl_ring in dsp_engine.v, a fixed 16 words per
			// channel) and takes no EQ state section ⇒ no section is added here.
		case planKindXHIFI:
			// 10 sections: HP 3 + BP's LP 3 + BP's HP 3 + LP 1 (there are also 2 DELAY and 2 MIX2
			// slots; the delays use a separate ring and MIX2 has no real state use, but the engine
			// writes state for every slot ⇒ account 12 to leave margin)
			sections += 12
		case planKindAnalogX:
			// AnalogX = the four sections HP + POLY + LP + Peak **plus one MIX2 slot**.
			// ⚠️ The engine's S_FIN writes state for **every slot** (regardless of opcode), so MIX2
			//    really does take a state index ⇒ account **5** here (better to count one too many than
			//    to undercount and run the state out of range).
			sections += 5
		case planKindExciter:
			// Harmonic exciter = HPF section + POLY state section + LPF section = 3 sections.
			// POLY needs one section of state (p[n−1] / y[n−1] / the de-click counter); it does not use
			// the biquad's four-word grouping, but it **still eats into the NSEC quota**, so it must be
			// counted truthfully.
			// ⚠️ When the wet gain exceeds MIX2's range, a "gain stage" is added (see below), and that
			// is a section too.
			sections += 3
			if n.MixB[0] != 0 {
				sections++
			}
		case planKindViPERBass:
			// The scaling biquad 1 section + the low-pass biquad 1 section + the MIX2 slot 1 section
			// (the engine writes state for **every slot**, so MIX2 really takes a state index, the same
			// accounting as AnalogX) = 3 sections.
			sections += 3
		case planKindViPERBassPBP:
			// PBP's section count is **also 3**, like NATURAL: scaling + low-pass (packed into one
			// slot) + the MIX2 accounting. The dry-path small FIR has its own coefficient/history RAM
			// (no sections), and the wet delay's ring is also in dl_ring (no section is added, for the
			// same reason as planKindDelay).
			sections += 3
		case planKindDynBass:
			// 3 sections: side-signal MIX2 + 55 Hz low-pass + add-back MIX2. The carrier slot with
			// n=0 takes the bypass branch and writes no state (dsp_engine.v:1216-1220) ⇒ it is not
			// counted. MIX2's accounting matches AnalogX/ViPERBass (conservative: the engine actually
			// writes no state for MIX2, yet a slot is still reserved here).
			sections += dynamicBassSections
		case planKindColorfulMusic:
			// The joint stage itself is not among the per-channel sections, but its state **always
			// occupies** the last 2 words of the left channel's sections (JS_STATE0 = NSEC−2) ⇒ the
			// per-channel chain's section budget is 2 smaller. Counted truthfully here (better to
			// overcount).
			sections += jointStateReserve
		case planKindFIR:
			// The convolution kernel has its own coefficient/history RAM (fir_bank.v) and **takes no**
			// EQ coefficients or state sections, nor does it count towards the "16 sections" limit — it
			// is an independent parallel stage of the chain.
		default:
			sections++
		}
	}
	if sections > hwMaxSections {
		return p, fmt.Errorf("chain too long: %d sections (including DYN's detector band-pass) > engine limit of %d sections; nothing will be truncated",
			sections, hwMaxSections)
	}

	sec := 0     // state section number (state_base)
	coefIdx := 0 // coefficient word index
	slot := 0    // slot number
	bus := 0     // the bus the signal currently flows on
	// peakBus = the highest bus number the chain itself **touched** (every emit's in_a/in_b/out_b
	// counts).
	// Why the final `bus` alone is not enough: XHIFI uses base..base+4 internally but returns to
	// base+1 at the end, so looking only at the end misses the ones in between (measured on
	// 2026-09-21: VSE→convolution→DDC→ViPERBass→XHIFI ended with bus=13, which looked fine, while
	// internally it had already used **16**, and 14/15 were reserved at the time ⇒ the self-check was
	// all green and the intermediate results were silently written into the "cross-channel bus" and
	// the "DYN side-chain bus"). The reserved numbers themselves (crossLoBus / dynScratchBus) may be
	// referenced; they are not part of the "chain itself" budget.
	peakBus := 0
	noteBus := func(b int) {
		if b == 0xFFFF || b == crossLoBus || b == dynScratchBus {
			return
		}
		if b > peakBus {
			peakBus = b
		}
	}
	i := 0
	emitF := func(op int, n int, flags int, cfb int, stb int, inA int, inB int, outB int) error {
		// ⚠️ **The slot count must stay within the hardware RAM** (NSLOT=8). There used to be no check
		//    here because "at most 7 slots" (convolution + 6 DDC slots) happened not to exceed it — but
		//    one POLY stage did. What happens when it is exceeded: the descriptors are written with
		//    **contiguous auto-increment**, the hardware table has only 8×8=64 words, and from the 65th
		//    word on it **wraps around and overwrites the first slots** ⇒ the table the engine reads is
		//    not the one the software compiled; the symptom is "slot/section counts do not match and the
		//    sound is wrong" while every interface says ok.
		//    Measured (2026-09-20): after a 10-slot plan really went to the board, STATUS reported
		//    8 slots and 19 sections (the table was scribbled over).
		if slot >= slotLimit() {
			return fmt.Errorf("not enough slots: this chain needs %d slots and the engine has only %d"+
				" (the smaller of CAP2's self-reported NSLOT and the software constant is used;"+
				" going over wraps around and overwrites the earlier slots, so it is rejected here)", slot+1, slotLimit())
		}
		cfg := uint32(op) | uint32(n)<<16 | uint32(flags&0xFF)<<8
		noteBus(inA)
		noteBus(inB)
		noteBus(outB)
		p.Words = append(p.Words,
			slotWord{slot*slotWordSize + swCfg, cfg},
			slotWord{slot*slotWordSize + swCfb, uint32(cfb)},
			slotWord{slot*slotWordSize + swStb, uint32(stb)},
			slotWord{slot*slotWordSize + swInA, uint32(inA)},
			slotWord{slot*slotWordSize + swInB, uint32(inB)},
			slotWord{slot*slotWordSize + swOutB, uint32(outB)},
			slotWord{slot*slotWordSize + swPrm, 0},
			slotWord{slot*slotWordSize + swRsv, 0},
		)
		slot++
		return nil
	}
	emit := func(op int, n int, cfb int, stb int, inA int, inB int, outB int) error {
		return emitF(op, n, 0, cfb, stb, inA, inB, outB)
	}
	// ── Delay ring allocator ─────────────────────────────────────────
	// There is only **one** ring per channel (hwDelayWords words), while a frame may have several
	// DELAY slots (Clarity XHIFI has two: the BP branch at fs/400 and the LP branch at fs/200).
	// The hardware has one write pointer per **slot number**, and the offset inside the ring comes from
	// the descriptor's STB word (the HDL's dl_off) ⇒ software must guarantee that the slots'
	// [offset, offset+length) ranges do not overlap and do not run past the ring.
	// An old bitstream has only one write pointer (CAP3[23:16]=0 ⇒ hwDelaySlots=1): a second delay
	// slot must be **rejected** rather than "sent down and see" — that would make two slots share one
	// pointer with different wrap lengths, and the sound would be broken up.
	delayUsed := 0
	delaySlots := 0
	allocDelay := func(L int) (int, error) {
		if delaySlots >= hwDelaySlots {
			return 0, fmt.Errorf("this frame needs %d delay slots and the bitstream supports only %d"+
				" (CAP3[23:16]; an old bitstream's delay ring has just one write pointer, so two delay slots would tread on each other)",
				delaySlots+1, hwDelaySlots)
		}
		if delayUsed+L > hwDelayWords {
			return 0, fmt.Errorf("delay ring too small: %d words already allocated + %d words for this slot > the ring capacity of %d words per channel"+
				" (%.1f ms @48k) — several delay slots **share the same ring**",
				delayUsed, L, hwDelayWords, float64(hwDelayWords)/48.0)
		}
		off := delayUsed
		delayUsed += L
		delaySlots++
		return off, nil
	}

	for i < len(nodes) {
		if nodes[i].Kind == planKindXHIFI {
			n := nodes[i]
			// V4A `HiFi::Process`: three **parallel** branches each start from the slot input, and two
			// MIX2 stages add them up.
			//   HP(1200)×3[×1.2g] ─────────────┐
			//   LP(1200)×3 → HP(120)×3[×g] → delay L1 ─┴→ MIX2 → MIX2 → output
			//   LP(120)×1[×1] → delay L2 ────────────────┘
			// The branch gains are folded into MIX2's **weights** (not into the filter numerators): the
			// whole chain has a single saturation point, consistent with V4A's "sum in floating point
			// and round once", and nothing gets flattened in the middle of a filter.
			base := bus
			dBP, dLP := n.Flags, n.Len // the delay lengths of the two branches (samples)
			for k, d := range []int{dBP, dLP} {
				if d < 1 || d > hwDelayWords {
					return p, fmt.Errorf("Clarity XHIFI branch %d has a delay of %d samples, beyond the %d-word delay ring",
						k+1, d, hwDelayWords)
				}
			}
			offBP, err := allocDelay(dBP)
			if err != nil {
				return p, fmt.Errorf("Clarity XHIFI BP branch delay: %w", err)
			}
			offLP, err := allocDelay(dLP)
			if err != nil {
				return p, fmt.Errorf("Clarity XHIFI LP branch delay: %w", err)
			}
			// Coefficients: 3+3+3+1 sections (5 words each) + 2 delay-length words + 2 sets of mixing
			// weights = 70 words
			if coefIdx+70 > hwCoefWords {
				return p, fmt.Errorf("coefficient RAM does not fit: Clarity XHIFI needs %d..%d and each bank has only %d words",
					coefIdx, coefIdx+69, hwCoefWords)
			}
			// ① HP(1200)×3 — the three sections' coefficients are **exactly the same** (V4A simply
			//    cascades the same set three times), but each section needs its own copy of them (the
			//    engine fetches at cfb + k·5)
			hb := coefIdx
			for k := 0; k < 3; k++ {
				p.Coefs = append(p.Coefs, n.Coefs[:]...)
			}
			coefIdx += 15
			if err := emit(opBiquad, 3, hb, sec, base, 0xFFFF, base+1); err != nil {
				return p, err
			}
			sec += 3
			// ⚠️ **Bus reuse** (a bug caught by measuring on the board on 2026-09-21): originally every
			//    stage took a fresh bus (base..base+8 = 9 of them), while 14/15 are engine reserved
			//    numbers (cross-channel / DYN side chain), so a real chain like "DDC + convolution
			//    + XHIFI" reported outright
			//    `not enough buses: this chain needs bus 15` — **the feature was written but could not
			//    be downloaded**.
			//    Now the assignment reuses whichever bus is already free: HP→base+1, BP-LP→base+2,
			//    BP-HP→base+3, BP delay→base+4, LP→base+2 (BP-LP has been consumed), LP delay→base+3,
			//    mix1→base+4 (the slot reads before it writes, and the engine latches the input before
			//    writing the bus), final→base+1. Five in total, saving four. The check is the bus-count
			//    assertion in xhifi_test.go.
			// ②a first half of BP: LP(1200)×3
			lpb := coefIdx
			for k := 0; k < 3; k++ {
				p.Coefs = append(p.Coefs, n.Hi[:]...)
			}
			coefIdx += 15
			if err := emit(opBiquad, 3, lpb, sec, base, 0xFFFF, base+2); err != nil {
				return p, err
			}
			sec += 3
			// ②b second half of BP: HP(120)×3
			hpb := coefIdx
			for k := 0; k < 3; k++ {
				p.Coefs = append(p.Coefs, n.Side[:]...)
			}
			coefIdx += 15
			if err := emit(opBiquad, 3, hpb, sec, base+2, 0xFFFF, base+3); err != nil {
				return p, err
			}
			sec += 3
			// ②c the BP branch's delay (fs/400)
			db := coefIdx
			p.Coefs = append(p.Coefs, int32(dBP), 0, 0, 0, 0)
			coefIdx += 5
			if err := emitF(opDelay, 1, 0, db, offBP, base+3, 0xFFFF, base+4); err != nil {
				return p, err
			}
			// ③ LP(120)×1 → delay (fs/200)
			lb := coefIdx
			p.Coefs = append(p.Coefs, n.Extra[:]...)
			coefIdx += 5
			if err := emit(opBiquad, 1, lb, sec, base, 0xFFFF, base+2); err != nil {
				return p, err
			}
			sec++
			dl := coefIdx
			p.Coefs = append(p.Coefs, int32(dLP), 0, 0, 0, 0)
			coefIdx += 5
			if err := emitF(opDelay, 1, 0, dl, offLP, base+2, 0xFFFF, base+3); err != nil {
				return p, err
			}
			// ④ two summing stages: first 1.2g·HP + g·BP, then + LP
			m1 := coefIdx
			p.Coefs = append(p.Coefs, n.Mix[0], n.Mix[1], 0, 0, 0)
			coefIdx += 5
			if err := emit(opMix2, 1, m1, sec, base+1, base+4, base+4); err != nil {
				return p, err
			}
			sec++
			m2 := coefIdx
			p.Coefs = append(p.Coefs, n.MixB[0], n.MixB[1], 0, 0, 0)
			coefIdx += 5
			if err := emit(opMix2, 1, m2, sec, base+4, base+3, base+1); err != nil {
				return p, err
			}
			sec++
			bus = base + 1
			i++
			continue
		}
		if nodes[i].Kind == planKindAnalogX {
			n := nodes[i]
			// ① input high-pass HP(240, Q0.717): bus → bus+1
			hb := coefIdx
			for _, v := range n.Coefs {
				p.Coefs = append(p.Coefs, v)
			}
			coefIdx += 5
			if err := emit(opBiquad, 1, hb, sec, bus, 0xFFFF, bus+1); err != nil {
				return p, err
			}
			sec++
			// ② POLY (12 words + one state section): bus+1 → bus+2
			pb := coefIdx
			if coefIdx+12 > hwCoefWords {
				return p, fmt.Errorf("coefficient RAM does not fit: AnalogX's POLY needs %d..%d and each bank has only %d words",
					coefIdx, coefIdx+11, hwCoefWords)
			}
			for _, v := range n.Poly {
				p.Coefs = append(p.Coefs, v)
			}
			coefIdx += 12
			if err := emitF(opPoly, 11, 0, pb, sec, bus+1, 0xFFFF, bus+2); err != nil {
				return p, err
			}
			sec++
			// ③ dry/wet mixing: out = in·c0 + poly·c1 (**before the low-pass**): bus(dry) + bus+2(wet)
			// → bus+3
			mb := coefIdx
			p.Coefs = append(p.Coefs, n.Mix[0], n.Mix[1], 0, 0, 0)
			coefIdx += 5
			if err := emit(opMix2, 1, mb, sec, bus, bus+2, bus+3); err != nil {
				return p, err
			}
			sec++
			// ④ output low-pass LP(setting frequency, Q0.717): bus+3 → bus+4
			lb := coefIdx
			for _, v := range n.Hi {
				p.Coefs = append(p.Coefs, v)
			}
			coefIdx += 5
			if err := emit(opBiquad, 1, lb, sec, bus+3, 0xFFFF, bus+4); err != nil {
				return p, err
			}
			sec++
			// ⑤ 633 Hz peak (+0.58 dB, the param_7 branch; 0.8 is already folded into the numerator):
			// bus+4 → bus+5
			kb := coefIdx
			for _, v := range n.Side {
				p.Coefs = append(p.Coefs, v)
			}
			coefIdx += 5
			if err := emit(opBiquad, 1, kb, sec, bus+4, 0xFFFF, bus+5); err != nil {
				return p, err
			}
			sec++
			bus += 5
			i++
			continue
		}
		if nodes[i].Kind == planKindExciter {
			n := nodes[i]
			// ① input high-pass (only high frequencies enter the harmonic generator)
			hb := coefIdx
			for _, v := range n.Coefs {
				p.Coefs = append(p.Coefs, v)
			}
			coefIdx += 5
			if err := emit(opBiquad, 1, hb, sec, bus, 0xFFFF, bus+1); err != nil {
				return p, err
			}
			sec++
			// ② POLY: 12 coefficients + one state section
			pb := coefIdx
			if coefIdx+12 > hwCoefWords {
				return p, fmt.Errorf("coefficient RAM does not fit: the harmonic exciter needs %d..%d and each bank has only %d words",
					coefIdx, coefIdx+11, hwCoefWords)
			}
			for _, v := range n.Poly {
				p.Coefs = append(p.Coefs, v)
			}
			coefIdx += 12
			// The CFG's n = 11 (the polynomial order). It **must be non-zero**: n=0 means bypassing this
			// slot in the engine.
			if err := emitF(opPoly, 11, 0, pb, sec, bus+1, 0xFFFF, bus+2); err != nil {
				return p, err
			}
			sec++
			// ③ output low-pass (limits the harmonics to the band)
			lb := coefIdx
			for _, v := range n.Hi {
				p.Coefs = append(p.Coefs, v)
			}
			coefIdx += 5
			if err := emit(opBiquad, 1, lb, sec, bus+2, 0xFFFF, bus+3); err != nil {
				return p, err
			}
			sec++
			// ③b optional: the wet-signal gain stage (in_a = in_b = the wet bus, with the output
			//     **written back to the same bus**).
			//     Why "a slot reading the bus it writes" is allowed: the engine first latches the input
			//     into s_in in S_DISP and only then writes bus[out] (xhifi's mix1 is used the same way).
			if n.MixB[0] != 0 {
				gb := coefIdx
				p.Coefs = append(p.Coefs, n.MixB[0], 0, 0, 0, 0)
				coefIdx += 5
				if err := emit(opMix2, 1, gb, sec, bus+3, bus+3, bus+3); err != nil {
					return p, err
				}
				sec++
			}
			// ④ dry/wet mixing: out = dry·c0 + wet·c1 (in_b is an ordinary bus number, not a
			// cross-channel special case)
			mb := coefIdx
			p.Coefs = append(p.Coefs, n.Mix[0], n.Mix[1], 0, 0, 0)
			coefIdx += 5
			if err := emit(opMix2, 1, mb, sec, bus, bus+3, bus+4); err != nil {
				return p, err
			}
			bus += 4
			i++
			continue
		}
		if nodes[i].Kind == planKindFIR {
			n := nodes[i]
			blocks := n.Blocks
			if blocks == 0 {
				blocks = firTaps / firMACS
			}
			lg := 0
			for (1 << lg) < blocks {
				lg++
			}
			if (1 << lg) != blocks {
				return p, fmt.Errorf("the FIR's MACS block count must be a power of two (got %d): the slot CFG's n is log2-encoded", blocks)
			}
			if (firMACS << lg) > firTaps {
				return p, fmt.Errorf("the FIR needs %d taps, above this RTL's limit of %d", firMACS<<lg, firTaps)
			}
			// The coefficients are **not written into p.Coefs**: they live in their own address space
			// (starting at coefFIRBase) and are loaded separately by /api/dsp/ir (the slot is bypassed
			// while loading). CFB merely records the base address for reference.
			// While an IR is loading, make the convolution stage step aside first (its coefficient RAM
			// has no double bank, so it must not be sounding while it is replaced)
			irMu.Lock()
			loading := irLoading
			irMu.Unlock()
			flags := n.Flags
			if loading {
				flags |= flBypass
			}
			if err := emitF(opFIR, lg, flags, hwCoefFIRBase, 0, bus, 0xFFFF, bus+1); err != nil {
				return p, err
			}
			bus++
			i++
			continue
		}
		if nodes[i].Kind == planKindDynBass {
			// V4A DynamicBass's simplified branch (see the file header of dynbass.go: timing, scaling
			// and error are all there).
			// 4 slots:
			//   ⓪ n=0 bypass copy      : base   → 22 (the cross-channel bus; **no section, no MAC**)
			//   ① MIX2 1/K,1/K         : base + 22 → base+1 (side signal/K; 1/K folded into the weights)
			//   ② BIQUAD 55Hz (numerator×K): base+1 → base+2 (avg)
			//   ③ MIX2 1.0,1.0         : base + base+2 → base+3 (dry + avg)
			n := nodes[i]
			base := bus
			// ⓪ the carrier slot: n=0 ⇒ the first condition of S_DISP matches and all it does is
			//    `bus[out] <= bus[in]`.
			//    ⚠️ It **must be n=0**: a normal slot (n≥1) fetches coefficients and spends MACs, while
			//    all that is needed here is one copy. cfb is never fetched (n=0 does not enter S_FETCH),
			//    so 0 is fine.
			if err := emit(opNop, 0, 0, 0, base, 0xFFFF, crossLoBus); err != nil {
				return p, err
			}
			// ① side signal: out = (1/K)·this channel + (1/K)·the other channel = (L+R)/K
			//    in_b = 22 goes through the cross-channel register xf_rd in the RTL
			//    (dsp_engine.v:1325).
			sb := coefIdx
			p.Coefs = append(p.Coefs, dynamicBassPreScaleWeight(), dynamicBassPreScaleWeight(), 0, 0, 0)
			coefIdx += 5
			if err := emit(opMix2, 1, sb, sec, base, crossLoBus, base+1); err != nil {
				return p, err
			}
			sec++
			// ② 55 Hz low-pass (the numerator is already multiplied by K ⇒ it cancels ①'s 1/K)
			lb := coefIdx
			p.Coefs = append(p.Coefs, n.Coefs[:]...)
			coefIdx += 5
			if err := emit(opBiquad, 1, lb, sec, base+1, 0xFFFF, base+2); err != nil {
				return p, err
			}
			sec++
			// ③ dry + avg
			ab := coefIdx
			p.Coefs = append(p.Coefs, q315Round(1.0), q315Round(1.0), 0, 0, 0)
			coefIdx += 5
			if err := emit(opMix2, 1, ab, sec, base, base+2, base+3); err != nil {
				return p, err
			}
			sec++
			bus = base + 3
			i++
			continue
		}
		if nodes[i].Kind == planKindViPERBass {
			n := nodes[i]
			// ViPERBass NATURAL_BASS: scaling(×1/K) → low-pass(numerator×k) → MIX2(dry + wet).
			// The two biquads are packed into one slot (in series, the default packing shape), saving a
			// slot and a bus.
			base := bus
			pb := coefIdx
			p.Coefs = append(p.Coefs, n.Extra[:]...) // scaling section (b0 = 1/K)
			p.Coefs = append(p.Coefs, n.Coefs[:]...) // low-pass section
			coefIdx += 10
			if err := emit(opBiquad, 2, pb, sec, base, 0xFFFF, base+1); err != nil {
				return p, err
			}
			sec += 2
			// Dry/wet summing: the dry signal is taken from the **original bus** (the low-pass is a
			// parallel branch and must not be chained after it), and the result is **written back to the
			// low-pass's bus** (in_b == out_b ⇒ only 1 bus used).
			// Why in-place writing is allowed: the engine first latches the input into s_in in S_DISP and
			// only then writes bus[out]; xhifi's first mixing stage also has out == in_b (see the comment
			// above). The bus budget is tight (14/15 are reserved numbers), so every bus saved counts.
			mb := coefIdx
			p.Coefs = append(p.Coefs, n.Mix[0], n.Mix[1], 0, 0, 0)
			coefIdx += 5
			// These two weights are ViPERBass stage's **dry/wet gains** (in_a = the node's input, in_b =
			// the low-pass output). Scaling both by a ⇒ the whole stage's output is multiplied by a ⇒
			// exactly the official antiPop semantics (see antipop.go).
			p.HasBassMix, p.BassMixCoef = true, mb
			if err := emit(opMix2, 1, mb, sec, base, base+1, base+1); err != nil {
				return p, err
			}
			sec++
			bus++
			i++
			continue
		}
		if nodes[i].Kind == planKindViPERBassPBP {
			n := nodes[i]
			// ViPERBass PURE_BASS_PLUS: the dry path is the **small FIR** (a 63-tap polyphase kernel)
			// and the wet signal = bassFactor · LP(x[n−64]). Four slots, 4 buses at peak:
			//   slot0 DELAY  c0=64      : base   → base+1   (the wet signal's 64-sample delay)
			//   slot1 BIQUAD n=2        : base+1 → base+2   (scaling ×1/K → low-pass, the wet signal)
			//   slot2 SFIR   n=log2(blocks) : base   → base+3   (the dry path: the 63-tap kernel)
			//   slot3 MIX2              : dry base+3 + wet base+2 → base+2
			// Order-wise "delay then filter" is **bit-for-bit equivalent** to the official core's
			// "filter then delay": both are LTI, and a fixed-point filter's state evolution is merely
			// shifted as a whole (not an approximation).
			if hwSFIRMaxTaps < sfirTaps {
				return p, fmt.Errorf("ViPERBass's Pure Bass+ needs a **%d-tap small FIR**"+
					" (that is the official core's dry path, not a pass-through), but this bitstream's small FIR limit is %d:"+
					" both CAP1 bit7 (OP_SFIR present) and CAP4 (capacity) must be set", sfirTaps, hwSFIRMaxTaps)
			}
			if len(n.SFir) != sfirTaps {
				return p, fmt.Errorf("ViPERBass PBP's dry-path taps must number %d, got %d",
					sfirTaps, len(n.SFir))
			}
			L := n.Len
			if L < 1 || L > hwDelayWords {
				return p, fmt.Errorf("ViPERBass PBP's wet-signal delay of %d samples exceeds the %d-word delay ring",
					L, hwDelayWords)
			}
			base := bus
			// ① the wet signal's delay (using the ready-made DELAY slot: the ring is a separate dl_ring
			// and takes no EQ section)
			db := coefIdx
			p.Coefs = append(p.Coefs, int32(L), 0, 0, 0, 0)
			coefIdx += 5
			off, err := allocDelay(L)
			if err != nil {
				return p, fmt.Errorf("ViPERBass PBP wet-signal delay: %w", err)
			}
			if err := emitF(opDelay, 1, 0, db, off, base, 0xFFFF, base+1); err != nil {
				return p, err
			}
			// ② scaling + low-pass: the two biquads are **packed into one slot** (in series, the default
			// packing shape)
			pb := coefIdx
			p.Coefs = append(p.Coefs, n.Extra[:]...) // scaling section (b0 = 1/K)
			p.Coefs = append(p.Coefs, n.Coefs[:]...) // low-pass section (numerator ×k)
			coefIdx += 10
			if err := emit(opBiquad, 2, pb, sec, base+1, 0xFFFF, base+2); err != nil {
				return p, err
			}
			sec += 2
			// ③ dry-path small FIR: the coefficients are in **its own** address space (starting at
			//    coefSFIRBase) and are moved separately by dspDownloadSlotPlan (the same routine as the
			//    convolution's FirCoefs).
			//    The slot CFG's n = log2(block count) = log2(sfirTaps/sfirMACS) (same semantics as
			//    opFIR).
			sfirBlocks := sfirTaps / sfirMACS
			lg := 0
			for (1 << lg) < sfirBlocks {
				lg++
			}
			if (1 << lg) != sfirBlocks {
				return p, fmt.Errorf("the small FIR's block count %d is not a power of two (the slot CFG's n is log2-encoded)", sfirBlocks)
			}
			// CFB merely "records the base address for reference" (the engine's OP_SFIR fetches
			// coefficients through cidx routing and does not use this word), the same semantics as
			// passing hwCoefFIRBase for the convolution slot.
			if err := emitF(opSFIR, lg, 0, hwCoefSFIRBase, 0, base, 0xFFFF, base+3); err != nil {
				return p, err
			}
			// ⚠️ The taps must be written **twice** (channel 0 at +0 and channel 1 at +sfirTaps) — the
			//    small FIR's coefficient RAM, like the convolution kernel's, is "split per channel"
			//    (`caddr/TAPS` in fir_bank is the channel number). Writing only one copy leaves the
			//    **right channel's dry path constantly 0** (the wet signal is still there) ⇒ audibly
			//    "there is no such boost on the right and the left/right phases are asymmetric", while
			//    every interface returns 200 OK (which is exactly the "looks like the download succeeded"
			//    failure mode).
			//    The official core uses **the same** polyphase table for both channels (in
			//    Polyphase.cpp both fir1 and fir2 LoadCoefficients from the same array), so the two
			//    copies are identical.
			p.SFirCoefs = append([]int32(nil), n.SFir...)
			p.SFirCoefs = append(p.SFirCoefs, n.SFir...)
			// ④ dry/wet summing: the dry signal comes from the small FIR's bus and the wet from the
			// low-pass's bus, with the result written back to the wet bus (in_b == out_b is an in-place
			// form the engine allows; xhifi's first mixing stage uses it).
			mb := coefIdx
			p.Coefs = append(p.Coefs, n.Mix[0], n.Mix[1], 0, 0, 0)
			coefIdx += 5
			// Same semantics as NATURAL: these two weights are PBP stage's dry/wet gains
			// (in_a = the dry-path small FIR's bus, in_b = the delay+low-pass wet signal's bus).
			p.HasBassMix, p.BassMixCoef = true, mb
			if err := emit(opMix2, 1, mb, sec, base+3, base+2, base+2); err != nil {
				return p, err
			}
			sec++
			bus = base + 2
			i++
			continue
		}
		if nodes[i].Kind == planKindColorfulMusic {
			// Joint-stereo frame pass (V4A ColorfulMusic): **two slots**.
			// Bus layout (the preceding stage's output is at base):
			//   JDST: R input = base, L output → base+1, R output → base+2
			//   J3DS: L input = base+1, R input = base+2, output goes to a register only
			//         (out_b = 0xFFFF)
			//   the later stages' entry = base+3 (jbus) — the hardware presets "this channel's joint
			//   output from the previous frame" onto base+3 in every S_IDLE pass, and later slots read it
			//   as an ordinary bus (no opcode needs a special case).
			n := nodes[i]
			base := bus
			db := coefIdx
			// The coefficient block is 10-word aligned (9 valid + 1 padding): as with other slots,
			// blocks are 5 words, so that no later slot's coefficients land on "half a block" (the
			// planner's address arithmetic always divides by 5).
			for _, v := range n.Joint {
				p.Coefs = append(p.Coefs, v)
			}
			p.Coefs = append(p.Coefs, 0)
			coefIdx += 10
			// The two delay lines (960 / 672 samples) use the **per-channel delay ring with the same
			// in-ring offset**: D0 in the left ring and D1 in the right ring (the hardware's js_a0 /
			// js_ad1) share one offset ⇒ allocate for the longer of the two.
			off, err := allocDelay(colorfulJointDelayWords(sampleRate))
			if err != nil {
				return p, fmt.Errorf("ColorfulMusic's two delays: %w", err)
			}
			if err := emitF(opJDST, 1, 0, db, off, base+1, base, base+2); err != nil {
				return p, err
			}
			mb := coefIdx
			p.Coefs = append(p.Coefs, n.Mix3D[0], n.Mix3D[1], 0, 0, 0)
			coefIdx += 5
			if err := emitF(opJ3DS, 1, 0, mb, 0, base+1, base+2, 0xFFFF); err != nil {
				return p, err
			}
			bus = base + 3
			noteBus(bus)
			p.StereoFrame = true
			p.JBus = bus
			i++
			continue
		}
		if nodes[i].Kind == planKindDelay {
			n := nodes[i]
			L := n.Len
			if L < 1 {
				return p, fmt.Errorf("the delay length must be ≥1 (got %d)", L)
			}
			if L > hwDelayWords {
				return p, fmt.Errorf("a delay of %d samples exceeds the limit of %d (the per-channel delay ring has only %d words = %.1f ms @48k)",
					L, hwDelayWords, hwDelayWords, float64(hwDelayWords)/48.0)
			}
			base := coefIdx
			p.Coefs = append(p.Coefs, int32(L), 0, 0, 0, 0) // only c0 is used
			coefIdx += 5
			// For ordinary slots STB is the "state base address", but for a delay slot it becomes the
			// **starting offset inside the ring** (the ring takes no state section, so this word is
			// spare anyway) — several delay slots each take a non-overlapping stretch of the ring, see
			// allocDelay.
			off, err := allocDelay(L)
			if err != nil {
				return p, err
			}
			// flags come from the node (DiffSurround uses bit2 = delay the right channel only)
			if err := emitF(opDelay, 1, n.Flags, base, off, bus, 0xFFFF, bus+1); err != nil {
				return p, err
			}
			bus++
			i++
			continue
		}
		if nodes[i].Kind == planKindCross {
			// crossfeed: lo and hi **must each take a slot** (they read the same input but write
			// different buses, so they cannot be packed into one chain like ordinary biquads — packing
			// would connect lo's output to hi's input).
			n := nodes[i]
			loBase := coefIdx
			for _, v := range n.Coefs {
				p.Coefs = append(p.Coefs, v)
			}
			coefIdx += 5
			emit(opBiquad, 1, loBase, sec, bus, 0xFFFF, crossLoBus) // lo → the cross-channel bus
			sec++

			hiBase := coefIdx
			for _, v := range n.Hi {
				p.Coefs = append(p.Coefs, v)
			}
			coefIdx += 5
			emit(opBiquad, 1, hiBase, sec, bus, 0xFFFF, bus+1) // hi → bus+1
			sec++

			mixBase := coefIdx
			p.Coefs = append(p.Coefs, n.Mix[0], n.Mix[1], 0, 0, 0)
			coefIdx += 5
			// out = mix0·hi + mix1·(**the other channel's** lo) — in_b=14 goes through the
			// cross-channel register in the RTL
			if err := emit(opMix2, 1, mixBase, sec, bus+1, crossLoBus, bus+2); err != nil {
				return p, err
			}
			bus += 2
			i++
			continue
		}
		if nodes[i].Kind == planKindDyn {
			n := nodes[i]
			// ① detector band-pass (takes a slot of its own, writes the scratch bus, not part of the
			// chain)
			sideBase := coefIdx
			for _, v := range n.Side {
				p.Coefs = append(p.Coefs, v)
			}
			coefIdx += 5
			if err := emit(opBiquad, 1, sideBase, sec, bus, 0xFFFF, dynScratchBus); err != nil {
				return p, err
			}
			sec++
			// ② the DYN stage
			dynBase := coefIdx
			for _, v := range n.Coefs {
				p.Coefs = append(p.Coefs, v)
			}
			coefIdx += 5
			if err := emit(opDyn, 1, dynBase, sec, bus, dynScratchBus, bus+1); err != nil {
				return p, err
			}
			sec++
			bus++
			i++
			continue
		}
		// consecutive biquads: packed up to secPerSlot sections
		n := 0
		base := coefIdx
		stb := sec
		for i+n < len(nodes) && nodes[i+n].Kind == planKindBiquad && n < secPerSlot {
			for _, v := range nodes[i+n].Coefs {
				p.Coefs = append(p.Coefs, v)
			}
			coefIdx += 5
			n++
		}
		if err := emit(opBiquad, n, base, stb, bus, 0xFFFF, bus+1); err != nil {
			return p, err
		}
		sec += n
		bus++
		i += n
	}
	// ⚠️ Final self-check: exceeding a bus number or a coefficient word count **must be an error**;
	//    it must not be allowed to quietly run out of range.
	//    · the buses crossLoBus(22)/dynScratchBus(23) are engine reserved numbers (cross-channel lo /
	//      DYN side chain), so the chain itself may only use 0..21 — the criterion is the **highest
	//      number touched** (peakBus), not the final one;
	//    · the coefficient RAM has NCOEF words per bank, and going over means overwriting other
	//      effects' coefficients.
	//    These two used to be "conventions" with no check — adding one effect stage could hit them, and
	//    an error beats hearing a strange noise.
	if peakBus >= crossLoBus || bus >= crossLoBus {
		return p, fmt.Errorf("not enough buses: this chain needs bus %d, while %d/%d are engine reserved numbers"+
			" (cross-channel / DYN side chain) — please drop one effect stage that needs its own bus",
			peakBus, crossLoBus, dynScratchBus)
	}
	if coefIdx > hwCoefWords {
		return p, fmt.Errorf("not enough coefficient RAM: this chain needs %d words and each bank has only %d",
			coefIdx, hwCoefWords)
	}
	// The joint stage's state always occupies the last `jointStateReserve` words of the left channel's
	// sections (the HDL's JS_STATE0 = NSEC−2) ⇒ with a joint stage the per-channel sections must
	// **leave them free**, otherwise the last few left-channel sections tread on the joint state.
	// (The sections accounting above already includes them; `sec` is checked once more here because it
	// is the index that is really occupied.)
	if p.StereoFrame && sec > hwMaxSections-jointStateReserve {
		return p, fmt.Errorf("not enough sections: this chain uses %d sections, while with a joint stereo stage only %d sections remain per channel"+
			" (the joint state always occupies the last %d words of the left channel)", sec, hwMaxSections-jointStateReserve,
			jointStateReserve)
	}
	p.Slots = slot
	p.Sections = sec
	if p.StereoFrame {
		p.Sections += jointStateReserve // the 2 words the joint state always occupies count towards the section budget too
	}
	return p, nil
}
