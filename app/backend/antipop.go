// SPDX-License-Identifier: GPL-2.0-only
// antipop.go — ViPERBass's `antiPop` (**a 1-second fade-in of the whole stage** when the effect goes from off to on)
//
// ── Official semantics (primary source: the community ViPER4Android rewrite, viperfx-re) ──
//
//	src/viper/effects/ViPERBass.cpp:83     Reset(): antiPop = 0.0f
//	src/viper/effects/ViPERBass.cpp:93-98  SetEnable(true) → Reset()
//	src/viper/effects/ViPERBass.cpp:28-37  start of Process():
//	    if (antiPop < 1.0)
//	        for (every stereo frame) { samples[i] *= antiPop; samples[i+1] *= antiPop;
//	                             antiPop = min(1.0, antiPop + 1.0/fs); }
//
//	**The key step** (it decides whether our implementation can do this): those two lines scale **the buffer entering Process**,
//	while all three modes are **linear** in their input (NATURAL's biquad, PBP's polyphase,
//	and even SUBWOOFER's pole filter are all LTI) ⇒ "scale first, then filter" and "scale afterwards" are bit-identical:
//
//	    out = a·x + w·LP(a·x) = a·(x + w·LP(x)) = a · (the **whole stage**'s output without the fade-in)
//
//	So mathematically antiPop **is exactly "the whole-stage gain of this ViPERBass stage rising linearly from 0 to 1 within one second"**.
//	⚠️ It is **not** "fade the wet signal only" — the official TODO at :27 says precisely "perhaps only the effect could be attenuated",
//	   whereas what it does now is the entire sample (dry and wet both faded). This file does the same: the **whole stage**.
//
// ── How our implementation lands it (zero HDL / zero bitstream / zero new slots / zero new sections) ──
//
//	The ViPERBass node already carries one MIX2 slot:
//	  · NATURAL: dsp_slot.go:1409-1415
//	  · PBP    : dsp_slot.go:1494-1500
//	In the RTL, OP_MIX2 is out = c0·in_a + c1·in_b (S_MIX in the DSP engine RTL, dsp_engine.v:1560-1581).
//	NATURAL: in_a = the node's input bus (dry), in_b = the low-pass output (wet)
//	  ⇒ out = c0·x + c1·LP(x); multiplying c0 and c1 by a **at the same time** gives out = a·(whole-stage output) ✓ which is exactly antiPop.
//	PBP: in_a = the dry-path small FIR's output, in_b = the delayed+low-passed wet signal ⇒ the same argument holds ✓
//
//	So all that is needed is to rewrite **the two weighting coefficients of this one MIX2 slot** along the ramp. Why that works:
//
//	1) The coefficient RAM is **fetched every frame**; there is no "shadow register loaded only at COMMIT":
//	   dsp_engine.v:1328 sets cf_a to `cfb_act + sl_cfb_r`, S_FETCH at :1355-1361
//	   reads coef_rd into c0..c4 beat by beat, and :1391-1398 uses the c0/c1 just read to enter S_MIX
//	   ⇒ change one word and **the next frame** uses the new value.
//	2) The coefficient write port selects the bank from CTRL.bit17(bank_sel) (dsp_engine.v:660-661
//	   `coef[cfb_wr + cidx] <= cdat`, cfb_wr = bank_sel ? NCOEF : 0);
//	   pointing bank_sel at the **active bank** changes the coefficient set that is running right now — the hardware supports this path,
//	   and it is the same write port used two ways as "write the inactive bank + swap it all at COMMIT" (see dspWriteActiveCoef).
//	3) One ramp step = 4 register writes (CIDX/CDAT ×2; setting the index explicitly in axi_i2s_adi_v1_2.vhd:693-699
//	   cancels the pending auto-increment, so every word lands at the right address). No slot table, no header, no COMMIT,
//	   no slot and no section taken ⇒ **completely independent** of the frame budget and the slot budget (24 slots).
//
// ── The only difference from the official core: the ramp is a **staircase** in time ────
//
//	The official core advances **per stereo frame** (1/fs = 20.8 µs @48k). Software can only rewrite registers at millisecond granularity,
//	so our implementation uses antiPopSteps steps over a total of antiPopSeconds. steps=200 ⇒ 5 ms/step, Δa=0.005.
//	The absolute discontinuity per step is ≤ Δa, and the ramp **rises monotonically** ⇒ no clicks, and by ear it is
//	indistinguishable from a linear fade-in; the end values (0 → the two compiled weights, bit-identical), the duration and the monotonicity all match the official core.
//	This is the finest that is **in principle** possible on the software side: nothing can write a register every 20.8 µs from Go.
//	(The alternative "re-download the whole table + COMMIT per step" produces the same staircase, but each step moves the entire table —
//	  24 slots × 8 words + all coefficients + the COMMIT handshake — which costs two orders of magnitude more, see the docs §G.2.)
package main

import (
	"fmt"
	"log"
	"math"
	"sync"
	"time"
)

const (
	// antiPopSeconds is the total fade-in duration (the official core = 1 second, see the per-sample accumulation at ViPERBass.cpp:33).
	antiPopSeconds = 1.0
	// antiPopSteps is the number of ramp steps: 1 second / 200 = **5 ms per step**, Δa = 0.005.
	// The case for 200: a per-step discontinuity of 0.005 ≈ -46 dBFS (far below the audibility threshold),
	// while each step costs only 4 mmap register writes + one 5 ms sleep — no burden on the backend at all.
	antiPopSteps = 200
)

// antiPopRamp is one pending ramp: which slot's two weights to change, and which two values to restore at the end.
//
// c0/c1 are not "recomputed" but **the very values the compiler just downloaded** (the two in slotPlan.Coefs).
// That way the end point (a=1) equals the compiled value bit for bit, and steady state is character-for-character the same as with no antiPop path at all.
type antiPopRamp struct {
	coefIdx int   // start index of the MIX2's two weights in the coefficient RAM
	c0, c1  int32 // the original compiled weights
}

var (
	// antiPopMu guards the three ramp states below and **at the same time** acts as the mutex between "ramp writes coefficients" and "whole-table download":
	// every ramp step must take it before writing, and cancellation (= the precondition of a new download) takes it too — so once cancellation returns,
	// a ramp write can never land on the new table (see the note on antiPopCancelRamp).
	antiPopMu sync.Mutex
	// antiPopActive non-nil = a ramp is running; antiPopDone closed = it is asked to stop.
	antiPopActive *antiPopRamp
	antiPopDone   chan struct{}
	// antiPopBassWasOn records "did the last **successfully downloaded** table contain the ViPERBass stage".
	// It identifies the off-to-on transition: only that one starts a ramp, steady state never does.
	antiPopBassWasOn bool

	// antiPopStepsN / antiPopSleeper / antiPopWriteActive are seams left for unit tests:
	// by swapping sleep for a fake clock and the coefficient write for a recording stub, a test can verify "step count and duration,
	// monotonicity, end-point restoration, failure rollback" **without a board**.
	antiPopStepsN      = antiPopSteps
	antiPopSleeper     = time.Sleep
	antiPopWriteActive = dspWriteActiveCoef
)

// antiPopScale returns the gain-scaled coefficient for step k (k = 0..steps): a_k = k/steps, scaling the original value directly.
//
// Why the two end points are special-cased: the end point must equal the compiled value **bit for bit** (if k=steps still went through floating-point
// multiply/divide, round(v·steps/steps) is mathematically v, but there is no reason to stake "steady state character-for-character" on float rounding).
// Monotonicity: for v ≥ 0, round(v·k/steps) is non-decreasing in k ⇒ the ramp never turns back.
func antiPopScale(v int32, k, steps int) int32 {
	if k <= 0 {
		return 0
	}
	if k >= steps {
		return v
	}
	return int32(math.Round(float64(v) * float64(k) / float64(steps)))
}

// antiPopWritePair writes the two weight coefficients of one MIX2 slot (= this step's whole-stage gain).
func antiPopWritePair(r antiPopRamp, c0, c1 int32) error {
	if err := antiPopWriteActive(r.coefIdx, c0); err != nil {
		return err
	}
	if err := antiPopWriteActive(r.coefIdx+1, c1); err != nil {
		return err
	}
	return nil
}

// antiPopShouldStart decides whether this download should start a ramp: only "the last downloaded table had no ViPERBass
// and this one does" — i.e. the effect going **off → on** — starts one.
//
// Why "the parameters changed" is not the trigger: the official core calls Reset() only on SetEnable(true) (ViPERBass.cpp:93-98),
// so changing the gain/cutoff does **not** re-fade (SetBassFactor :86-91 and SetProcessMode are what Reset).
// So "off → on" is the only trigger; steady-state downloads (applying one preset after another) must never fire it.
func antiPopShouldStart(prevHadBass, nowHasBass bool) bool {
	return nowHasBass && !prevHadBass
}

// antiPopNotePlan updates "did the last downloaded table contain ViPERBass" and answers "should this one start a ramp".
// It is factored out as a pure state function so the trigger sequence can be unit-tested offline (no hardware, no register writes).
func antiPopNotePlan(hasBass bool) bool {
	start := antiPopShouldStart(antiPopBassWasOn, hasBass)
	antiPopBassWasOn = hasBass
	return start
}

// antiPopCancelRamp stops the running ramp and **restores the coefficients to the compiled values**.
//
// Two guarantees (this is the whole basis for "a new download does not fight the ramp over coefficients"):
//   - every ramp step takes antiPopMu before writing, and this function takes it too ⇒ when it returns,
//     no further ramp write can land after it (either that step already finished, or it sees done closed when it wakes).
//   - the restore is done by the **cancelling side** inside the lock, not by letting the ramp wake up and do it — otherwise the ramp's restore
//     would come after the new table's COMMIT and write those two indices into a different section of the **new** table (which really would corrupt other effects' coefficients).
func antiPopCancelRamp() {
	antiPopMu.Lock()
	defer antiPopMu.Unlock()
	if antiPopDone != nil {
		close(antiPopDone)
		antiPopDone = nil
	}
	if antiPopActive != nil {
		r := *antiPopActive
		antiPopActive = nil
		if err := antiPopWritePair(r, r.c0, r.c1); err != nil {
			log.Printf("ViPERBass antiPop failed to restore coefficients on cancel (the next whole-table download will overwrite them): %v", err)
		}
	}
}

// startAntiPopRamp starts a ramp: write a=0 synchronously first (the very first frame should start silent), then hand it to the background.
//
// Why the first step is written synchronously: the official core's **first** buffer after enabling is already multiplied by 0. If the first step
// were handed to the background too, a few milliseconds of full-level signal would come out first — exactly the "jump" antiPop exists to remove.
func startAntiPopRamp(r antiPopRamp) error {
	antiPopCancelRamp() // finish the previous one (if any) first: it restores the gain

	antiPopMu.Lock()
	defer antiPopMu.Unlock()
	if err := antiPopWritePair(r, 0, 0); err != nil {
		// A half-written 0 must not be left behind (dry gone while wet remains = broken timbre and a sudden drop in level)
		_ = antiPopWritePair(r, r.c0, r.c1)
		return fmt.Errorf("ViPERBass antiPop failed to start (setting the gain to 0): %w", err)
	}
	done := make(chan struct{})
	antiPopActive, antiPopDone = &r, done
	go antiPopRun(r, done)
	return nil
}

// antiPopRun drives step 1 through step steps (step steps writes the compiled values back, restoring bit for bit).
//
// On error it **restores immediately**: the first thing the ramp does is write the gain to 0 (silence). If a failing step were simply ignored,
// the user would be left with "enabling ViPERBass makes it silent" while the API still reports success — that state must never be left behind.
func antiPopRun(r antiPopRamp, done chan struct{}) {
	steps := antiPopStepsN
	if steps < 1 {
		steps = 1
	}
	interval := time.Duration(float64(antiPopSeconds) * float64(time.Second) / float64(steps))
	for k := 1; k <= steps; k++ {
		antiPopSleeper(interval)
		antiPopMu.Lock()
		select {
		case <-done:
			// Already cancelled: the cancelling side restored and cleared the state inside the lock, so nothing is written here.
			antiPopMu.Unlock()
			return
		default:
		}
		err := antiPopWritePair(r, antiPopScale(r.c0, k, steps), antiPopScale(r.c1, k, steps))
		if err != nil {
			_ = antiPopWritePair(r, r.c0, r.c1) // restore the compiled values (gain = 1)
			antiPopActive, antiPopDone = nil, nil
			antiPopMu.Unlock()
			log.Printf("ViPERBass antiPop ramp failed at step %d/%d; the gain was restored to the compiled values: %v",
				k, steps, err)
			return
		}
		if k == steps {
			antiPopActive, antiPopDone = nil, nil
		}
		antiPopMu.Unlock()
	}
}

// dspAntiPopAfterDownload is called **after a successful whole-table download**: it decides whether this was "ViPERBass going off → on"
// and, if so, starts a 1-second ramp.
//
// ⚠️ Side effect (which must be known): during the 1 second the ramp runs, those two coefficients in the hardware disagree with `lastPlanCoefs`,
// so calling `/api/dsp/check` within that second reports "coefficients do not match" (a false failure). The ramp restores them bit for bit when it ends
// and the self-check passes again. That window is unavoidable for any "runtime coefficient update"; it is not a defect.
func dspAntiPopAfterDownload(p slotPlan) error {
	if !antiPopNotePlan(p.HasBassMix) {
		return nil
	}
	if p.BassMixCoef < 0 || p.BassMixCoef+1 >= len(p.Coefs) {
		return fmt.Errorf("ViPERBass antiPop: the MIX2 coefficient index %d reported by the compiler is out of range (Coefs holds only %d)"+
			" — no ramp is downloaded (better to keep that one jump than to write to a guessed address)",
			p.BassMixCoef, len(p.Coefs))
	}
	return startAntiPopRamp(antiPopRamp{
		coefIdx: p.BassMixCoef,
		c0:      p.Coefs[p.BassMixCoef],
		c1:      p.Coefs[p.BassMixCoef+1],
	})
}
