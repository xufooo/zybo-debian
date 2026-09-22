// SPDX-License-Identifier: GPL-2.0-only
// playing_test.go -- offline tests for "is it playing?" (no board needed)
//
// Background (measured by the author on 2026-09-15): the panel showed
// "no source connected" and "airplay connected" at the same time.
// The root cause was that playing came from an in-memory flag -- set to true when a
// source was selected and set to true again when metadata arrived, and it was **never
// set back to false**; restarting the backend made it false again. So "music is playing"
// showed as not playing, or "nothing is playing" showed as connected.
//
// Now playing reads the sound card's PCM status directly (state: RUNNING = really
// outputting), which holds for every source. These tests use fixture files and do not
// depend on a real board.

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSystemIsPlaying(t *testing.T) {
	cases := []struct {
		name string
		body string // file content; an empty string means the file does not exist
		want bool
	}{
		{
			name: "playing (real content measured on the board)",
			body: "state: RUNNING\n" +
				"owner_pid   : 4537\n" +
				"trigger_time: 1749.668276857\n" +
				"tstamp      : 1845.837296166\n" +
				"delay       : 19025\n" +
				"avail       : 46511\n",
			want: true,
		},
		{name: "the driver writes closed when the device is not open", body: "closed\n", want: false},
		{name: "SETUP / PREPARED are not outputting yet", body: "state: PREPARED\nowner_pid   : 100\n", want: false},
		{name: "no such file (no sound card / wrong path)", body: "", want: false},
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

// TestMetadataClearsWhenGone: when the file is gone, the previous track's info must not
// be kept (the old implementation parsed only when err == nil => once the metadata
// disappeared the panel stayed on the old track forever).
func TestMetadataClearsWhenGone(t *testing.T) {
	origPath, origSrc := metadataPath, currentSource
	defer func() { metadataPath, currentSource = origPath, origSrc }()

	currentSource = "airplay"
	metadataPath = filepath.Join(t.TempDir(), "does-not-exist")
	currentTitle, currentArtist, currentAlbum = "old track", "old artist", "old album"
	collectMetadata()
	if currentTitle != "" || currentArtist != "" || currentAlbum != "" {
		t.Errorf("metadata must be cleared when the file is gone, got = %q/%q/%q", currentTitle, currentArtist, currentAlbum)
	}
}

// TestParseMetadataDropsMissingFields: fields absent from the file must be cleared
// instead of keeping the previous value
func TestParseMetadataDropsMissingFields(t *testing.T) {
	origSrc := currentSource
	defer func() { currentSource = origSrc }()
	currentSource = "airplay"

	f := filepath.Join(t.TempDir(), "md")
	metadataPath = f

	if err := os.WriteFile(f, []byte("title=track one\nartist=artist a\nalbum=album one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	collectMetadata()
	if currentTitle != "track one" || currentArtist != "artist a" || currentAlbum != "album one" {
		t.Fatalf("parse failed: %q/%q/%q", currentTitle, currentArtist, currentAlbum)
	}

	// The next track has only title => artist/album must become empty, not keep the
	// previous track's values
	if err := os.WriteFile(f, []byte("title=track two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	collectMetadata()
	if currentTitle != "track two" || currentArtist != "" || currentAlbum != "" {
		t.Errorf("missing fields must be cleared: %q/%q/%q", currentTitle, currentArtist, currentAlbum)
	}
}
