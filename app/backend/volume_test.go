// SPDX-License-Identifier: GPL-2.0-only
// volume_test.go — offline tests for the ALSA Master read-back and dB calibration
//
// Background: on this codec the control's percentage and its dB are not the same
// thing (measured 2026-09-15):
//
//	raw  0..47 : -99999.99dB (no dB mapping) -> dead zone
//	raw 48     : -74 dB
//	raw 102    : -20 dB      (what ALSA calls 80%)
//	raw 122    :   0 dB      (what ALSA calls 96%)
//	raw 127    :  +5 dB      (what ALSA calls 100%) -> clipping
//
// Writing a raw percentage is therefore silent at the bottom and clipping at the
// top; this file calibrates in dB instead: 0% = mute, 1..100% = -60..0 dB.

package main

import "testing"

func TestPctToDB(t *testing.T) {
	cases := []struct {
		pct  int
		want float64
	}{
		{0, -60}, {1, -59.4}, {50, -30}, {80, -12}, {96, -2.4}, {100, 0},
	}
	for _, c := range cases {
		if got := pctToDB(c.pct); got < c.want-0.01 || got > c.want+0.01 {
			t.Errorf("pctToDB(%d) = %.2f, want %.2f", c.pct, got, c.want)
		}
	}
}

func TestDBToPctRoundTrip(t *testing.T) {
	for _, pct := range []int{1, 10, 25, 37, 50, 80, 96, 100} {
		if got := dbToPct(pctToDB(pct)); got != pct {
			t.Errorf("dbToPct(pctToDB(%d)) = %d", pct, got)
		}
	}
}

func TestParseMixerDB(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want float64
		ok   bool
	}{
		{
			name: "80% = -20dB (real amixer output)",
			in: "Simple mixer control 'Master',0\n" +
				"  Capabilities: pvolume\n" +
				"  Playback channels: Front Left - Front Right\n" +
				"  Limits: Playback 0 - 127\n" +
				"  Mono:\n" +
				"  Front Left: Playback 102 [80%] [-20.00dB]\n" +
				"  Front Right: Playback 102 [80%] [-20.00dB]\n",
			want: -20, ok: true,
		},
		{
			// 37% (raw 47) falls in this dead zone: the driver cannot report a dB value
			name: "dead zone (unmapped) counts as silence",
			in:   "  Front Left: Playback 47 [37%] [-99999.99dB]\n",
			want: volumeMinDB, ok: true,
		},
		{
			name: "top of the range is +5 dB but we never ask for it",
			in:   "  Front Left: Playback 127 [100%] [5.00dB]\n",
			want: 5, ok: true,
		},
		{
			name: "garbage",
			in:   "amixer: Mixer default load error: No such device\n",
			want: 0, ok: false,
		},
	}
	for _, c := range cases {
		got, ok := parseMixerDB(c.in)
		if ok != c.ok {
			t.Errorf("%s: ok = %v, want %v", c.name, ok, c.ok)
			continue
		}
		if ok && (got < c.want-0.01 || got > c.want+0.01) {
			t.Errorf("%s: parseMixerDB() = %.2f, want %.2f", c.name, got, c.want)
		}
	}
}

func TestDeadZoneReadsAsZero(t *testing.T) {
	// 37% (raw 47) is in the dead zone, so it must read as 0%, not 37%
	db, _ := parseMixerDB("  Front Left: Playback 47 [37%] [-99999.99dB]\n")
	if pct := dbToPct(db); pct != 0 {
		t.Errorf("dead zone should map to 0%%, got %d%%", pct)
	}
}
