package providers

import "testing"

func TestIsValidMxmToken(t *testing.T) {
	good := []string{"261010c08a8e0025eb382466efefd0831581d43d120c3995302c2d"}
	bad := []string{"", "   ", "short", "0000000000000000000000000000", "UpgradeOnlyUpgradeOnly12345", "  000000000000  "}
	for _, g := range good {
		if !isValidMxmToken(g) {
			t.Errorf("expected valid: %q", g)
		}
	}
	for _, b := range bad {
		if isValidMxmToken(b) {
			t.Errorf("expected invalid: %q", b)
		}
	}
}

func TestMxmHoneypots(t *testing.T) {
	if !isMxmHoneypotLyrics("blah Wob gopini den blah") {
		t.Error("decoy lyrics not detected")
	}
	if isMxmHoneypotLyrics("hello from the other side") {
		t.Error("false positive on real lyrics")
	}
	tracks := []mxmTrack{
		{TrackName: "Casual", ArtistName: "Doja Cat"},
		{TrackName: "Sleep", ArtistName: "Calming Cat Music"},
		{TrackName: "Hello", ArtistName: "Adele"},
	}
	got := filterMxmHoneypotTracks(tracks, "hello adele")
	if len(got) != 1 || got[0].TrackName != "Hello" {
		t.Fatalf("unexpected filter result: %+v", got)
	}
	got = filterMxmHoneypotTracks(tracks, "casual doja cat")
	if len(got) != 2 {
		t.Fatalf("explicitly requested track must be kept: %+v", got)
	}
}
