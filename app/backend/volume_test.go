// SPDX-License-Identifier: GPL-2.0-only
// volume_test.go — offline tests for the ALSA Master read-back parser

package main

import "testing"

func TestParseMixerPercent(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{
			name: "stereo control at 80%",
			in: "Simple mixer control 'Master',0\n" +
				"  Capabilities: pvolume\n" +
				"  Playback channels: Front Left - Front Right\n" +
				"  Limits: Playback 0 - 127\n" +
				"  Mono:\n" +
				"  Front Left: Playback 102 [80%] [-20.00dB]\n" +
				"  Front Right: Playback 102 [80%] [-20.00dB]\n",
			want: 80,
		},
		{
			name: "muted / no percentage",
			in:   "  Front Left: Playback 0 [0%] [-99999.99dB]\n",
			want: 0,
		},
		{
			name: "empty output",
			in:   "",
			want: -1,
		},
		{
			name: "unrelated text",
			in:   "amixer: Mixer default load error: No such device\n",
			want: -1,
		},
	}
	for _, c := range cases {
		if got := parseMixerPercent(c.in); got != c.want {
			t.Errorf("%s: parseMixerPercent() = %d, want %d", c.name, got, c.want)
		}
	}
}
