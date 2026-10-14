package storage

import "testing"

func TestCanonicalFilename(t *testing.T) {
	name := CanonicalFilename("Taylor Swift", "Shake It Off", "1989", 242000, "USUM71412345", "spotify:track:abc", "json")
	want := "Taylor Swift - Shake It Off [1989] (242.00) <USUM71412345::spotify:track:abc>.json"
	if name != want {
		t.Errorf("filename = %q, want %q", name, want)
	}

	p := ParseFilename(name)
	if p.Artist != "Taylor Swift" || p.Title != "Shake It Off" {
		t.Errorf("artist/title parse: %q / %q", p.Artist, p.Title)
	}
	if p.Album != "1989" {
		t.Errorf("album parse: %q", p.Album)
	}
	if p.DurationMS != 242000 {
		t.Errorf("duration parse: %d", p.DurationMS)
	}
	if p.ISRC != "USUM71412345" || p.PlatformID != "spotify:track:abc" {
		t.Errorf("isrc/platform parse: %q / %q", p.ISRC, p.PlatformID)
	}
}

func TestParseFilenameLoose(t *testing.T) {
	p := ParseFilename("Artist - Title (200.500).json")
	if p.Artist != "Artist" || p.Title != "Title" {
		t.Errorf("loose parse: %+v", p)
	}
	if p.DurationMS != 200500 {
		t.Errorf("loose duration: %d", p.DurationMS)
	}
}

func TestExtractKeywords(t *testing.T) {
	kw := ExtractKeywords("Taylor Swift - Shake It Off (Live)")
	got := map[string]bool{}
	for _, k := range kw {
		got[k] = true
	}
	if !got["Taylor"] || !got["Swift"] {
		t.Errorf("keywords = %v", kw)
	}
	if len(kw) > 2 {
		t.Errorf("keywords len = %d", len(kw))
	}
}
