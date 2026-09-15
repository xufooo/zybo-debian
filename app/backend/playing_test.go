// SPDX-License-Identifier: GPL-2.0-only
// playing_test.go -- offline tests for "is it playing?" (no board needed)
//
// Background (reported on hardware, 2026-09-15): the panel showed "No source
// connected" and "airplay connected" at the same time. The cause was that
// `playing` came from an in-memory flag -- set when a source was selected, set
// again when metadata arrived, NEVER cleared, and reset to false by a restart.
// So it claimed nothing was playing while music played, or claimed a connection
// when nothing was going on.
//
// Playing is now read from the sound card's PCM state (state: RUNNING means
// audio is really flowing), which holds for every source. These tests drive it
// with fixture files instead of a board.

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSystemIsPlaying(t *testing.T) {
	cases := []struct {
		name string
		body string // empty means the file does not exist
		want bool
	}{
		{
			name: "playing (real content read off the board)",
			body: "state: RUNNING\n" +
				"owner_pid   : 4537\n" +
				"trigger_time: 1749.668276857\n" +
				"tstamp      : 1845.837296166\n" +
				"delay       : 19025\n" +
				"avail       : 46511\n",
			want: true,
		},
		{name: "driver writes closed when the device is not open", body: "closed\n", want: false},
		{name: "PREPARED is not yet producing audio", body: "state: PREPARED\nowner_pid   : 100\n", want: false},
		{name: "no such file (no card / wrong path)", body: "", want: false},
		{name: "empty file", body: "   \n", want: false},
	}

	orig := pcmStatusPath
	defer func() { pcmStatusPath = orig }()

	for _, c := range cases {
		if c.body == "" {
			pcmStatusPath = filepath.Join(t.TempDir(), "does-not-exist")
		} else {
			f := filepath.Join(t.TempDir(), "status")
			if err := os.WriteFile(f, []byte(c.body), 0o644); err != nil {
				t.Fatal(err)
			}
			pcmStatusPath = f
		}
		if got := systemIsPlaying(); got != c.want {
			t.Errorf("%s: systemIsPlaying() = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestMetadataClearsWhenGone: when the file disappears the previous track's info
// must be dropped (the old code only parsed on success, so the panel stayed
// stuck on the last title forever).
func TestMetadataClearsWhenGone(t *testing.T) {
	origPath, origSrc := metadataPath, currentSource
	defer func() { metadataPath, currentSource = origPath, origSrc }()

	currentSource = "airplay"
	metadataPath = filepath.Join(t.TempDir(), "does-not-exist")
	currentTitle, currentArtist, currentAlbum = "old title", "old artist", "old album"
	collectMetadata()
	if currentTitle != "" || currentArtist != "" || currentAlbum != "" {
		t.Errorf("metadata must be cleared when the file is gone, got %q/%q/%q",
			currentTitle, currentArtist, currentAlbum)
	}
}

// TestParseMetadataDropsMissingFields: fields absent from the file must be
// cleared instead of keeping the previous track's values.
func TestParseMetadataDropsMissingFields(t *testing.T) {
	origSrc := currentSource
	defer func() { currentSource = origSrc }()
	currentSource = "airplay"

	f := filepath.Join(t.TempDir(), "md")
	metadataPath = f

	if err := os.WriteFile(f, []byte("title=First\nartist=Someone\nalbum=Record One\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	collectMetadata()
	if currentTitle != "First" || currentArtist != "Someone" || currentAlbum != "Record One" {
		t.Fatalf("parse failed: %q/%q/%q", currentTitle, currentArtist, currentAlbum)
	}

	// The next track has only a title: artist/album must go empty, not linger.
	if err := os.WriteFile(f, []byte("title=Second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	collectMetadata()
	if currentTitle != "Second" || currentArtist != "" || currentAlbum != "" {
		t.Errorf("missing fields must be cleared: %q/%q/%q", currentTitle, currentArtist, currentAlbum)
	}
}
