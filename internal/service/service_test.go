package service

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"lyricsplus/backend/internal/config"
	"lyricsplus/backend/internal/domain"
	"lyricsplus/backend/internal/storage"
)

func TestFromStoreLyricsPlusSourceOverride(t *testing.T) {
	st, err := storage.NewStore(config.Storage{DBPath: filepath.Join(t.TempDir(), "cache.db"), LRUSize: 64})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer func() { _ = st.Close() }()

	stored := &domain.LyricsResponse{
		Type:      domain.SyncTypeLine,
		KpoeTools: "lyricsplus",
		Metadata: domain.LyricsMetadata{
			Title:       "Superstar",
			Artist:      "Jamelia",
			SongWriters: []string{},
		},
		Lyrics: []domain.Line{
			{Time: 0, Duration: 4000, Text: "Superstar", Element: domain.LineElement{Key: "L1"}},
		},
		Cached: domain.CacheNone,
	}
	raw, err := json.Marshal(stored)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	query := domain.SearchQuery{Title: "Superstar", Artist: "Jamelia"}
	if err := st.SaveUserLyrics(context.Background(), query, raw); err != nil {
		t.Fatalf("save: %v", err)
	}

	s := &Service{Store: st}
	resp, ok := s.fromStore(context.Background(), query)
	if !ok {
		t.Fatal("expected cache hit")
	}
	if resp.Metadata.Source != "Lyrics+" {
		t.Fatalf("expected forced source Lyrics+, got %q", resp.Metadata.Source)
	}
	if resp.ProcessingTime == nil {
		t.Fatal("expected processingTime on cache hit")
	}
	if resp.ProcessingTime.WinnerSource == nil || *resp.ProcessingTime.WinnerSource != "lyricsplus" {
		t.Fatalf("expected winnerSource lyricsplus, got %+v", resp.ProcessingTime.WinnerSource)
	}
	if resp.ProcessingTime.SelectedSongMetadata == nil ||
		resp.ProcessingTime.SelectedSongMetadata.Source != "Lyrics+" ||
		resp.ProcessingTime.SelectedSongMetadata.Title != "Superstar" {
		t.Fatalf("expected selectedSongMetadata, got %+v", resp.ProcessingTime.SelectedSongMetadata)
	}
}

func TestProviderNameForSource(t *testing.T) {
	cases := map[string]string{
		"Spotify":     "spotify",
		"Apple Music": "apple",
		"Apple":       "apple",
		"QQ Music":    "qq",
		"QQ":          "qq",
		"Musixmatch":  "musixmatch",
		"Deezer":      "deezer",
		"Lyrics+":     "lyricsplus",
		"":            "lyricsplus",
	}
	for in, want := range cases {
		if got := providerNameForSource(in); got != want {
			t.Errorf("providerNameForSource(%q) = %q, want %q", in, got, want)
		}
	}
}
