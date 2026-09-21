package similarity

import (
	"math"
	"testing"
)

func TestSorensenDice(t *testing.T) {
	if SorensenDice("hello", "hello") < 0.99 {
		t.Error("identical strings should score ~1")
	}
	if SorensenDice("hello", "world") > SorensenDice("hello", "hella") {
		t.Error("similar strings should outscore distant ones")
	}
	if SorensenDice("", "x") != 0 {
		t.Error("empty vs non-empty should be 0")
	}
	if SorensenDice("", "") != 1.0 {
		t.Error("both empty should be 1")
	}
}

func TestLevenshteinNorm(t *testing.T) {
	if LevenshteinNorm("kitten", "kitten") != 1.0 {
		t.Error("identical strings should normalize to 1")
	}
	if LevenshteinNorm("kitten", "sitting") >= 1.0 {
		t.Error("different strings should score < 1")
	}
}

func TestTitleSimilarity(t *testing.T) {
	// Live vs acoustic: different critical tags on equal base => 0.65.
	if got := TitleSimilarity("Song Name (Live)", "Song Name (Acoustic)"); math.Abs(got-0.65) > 1e-9 {
		t.Errorf("live/acoustic conflict should be 0.65, got %f", got)
	}
	// Same critical tag on one side only => 0.72.
	if got := TitleSimilarity("Song Name", "Song Name (Live)"); math.Abs(got-0.72) > 1e-9 {
		t.Errorf("one-sided critical should be 0.72, got %f", got)
	}
	if TitleSimilarity("Hello", "Hello") < 0.95 {
		t.Errorf("identical title should be near 1: %f", TitleSimilarity("Hello", "Hello"))
	}
}

func TestDurationScore(t *testing.T) {
	cases := []struct {
		delta float64
		want  float64
	}{
		{0, 1.00},
		{1, 0.98},
		{2, 0.95},
		{4, 0.85},
		{7, 0.70},
		{12, 0.50},
		{20, 0.30},
		{35, 0.15},
		{60, 0.05},
		{61, 0.00},
	}
	for _, c := range cases {
		if got := DurationScore(c.delta); got != c.want {
			t.Errorf("DurationScore(%v) = %v, want %v", c.delta, got, c.want)
		}
	}
	if DurationSimilarity(0, 0) != 0.7 {
		t.Error("missing duration should be 0.7")
	}
}

func TestMatchScoreThreshold(t *testing.T) {
	score := MatchScore("Shape of You", "Ed Sheeran", "÷", 233000,
		"Shape of You", "Ed Sheeran", "÷", 233000, true, true)
	if score < 0.70 {
		t.Errorf("identical match should exceed threshold, got %f", score)
	}

	bad := MatchScore("Shape of You", "Ed Sheeran", "÷", 233000,
		"Shape of You", "Ed Sheeran", "Deluxe", 60000, true, true)
	if bad > 0.70 {
		t.Errorf("mismatched duration/album should fail threshold, got %f", bad)
	}
}

func TestAnalyzeTitleFeatured(t *testing.T) {
	at := AnalyzeTitle("Song (Remix) feat. Drake")
	if at.Tags["remix"] != true {
		t.Error("remix tag not detected")
	}
	if len(at.FeatArtists) != 1 || !contains(at.FeatArtists, "drake") {
		t.Errorf("featured artist extraction: %v", at.FeatArtists)
	}
}

func TestSongSimilarityWeights(t *testing.T) {
	// hasAlbum && hasDuration -> .30/.30/.20/.20
	info := SongSimilarity("Shape of You", "Ed Sheeran", "÷", 233,
		"Shape of You", "Ed Sheeran", "÷", 233)
	if info.Score < 0.95 {
		t.Errorf("exact match should be ~1, got %f", info.Score)
	}
	if info.Weights.Title != 0.30 || info.Weights.Album != 0.20 || info.Weights.Duration != 0.20 {
		t.Errorf("unexpected weights for album+duration: %+v", info.Weights)
	}

	// Neither album nor duration -> .52/.42/.06/0.00
	info2 := SongSimilarity("Shape of You", "Ed Sheeran", "", 0,
		"Shape of You", "Ed Sheeran", "", 0)
	if info2.Weights.Album != 0.06 || info2.Reason == "" {
		t.Errorf("unexpected weights without album/duration: %+v", info2.Weights)
	}
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
