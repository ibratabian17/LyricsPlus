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

func TestMatchSource(t *testing.T) {
	if !matchSource("apple", nil) {
		t.Error("expected true when allowed is empty")
	}
	if !matchSource("apple", []string{"apple"}) {
		t.Error("expected true for exact match")
	}
	if !matchSource("Apple Music", []string{"apple"}) {
		t.Error("expected true for Apple Music -> apple")
	}
	if !matchSource("qq", []string{"qqmusic"}) {
		t.Error("expected true for qq -> qqmusic")
	}
	if matchSource("apple", []string{"qq"}) {
		t.Error("expected false for apple when only qq is allowed")
	}
	if !matchSource("musixmatch-word", []string{"musixmatch"}) {
		t.Error("expected true for musixmatch-word matching musixmatch")
	}
}

func TestExtFor(t *testing.T) {
	cases := map[string]string{
		"Apple":                       "ttml",
		"Apple Music":                 "ttml",
		"apple":                       "ttml",
		"QQ":                          "qrc",
		"QQ Music":                    "qrc",
		"qq":                          "qrc",
		"qqmusic":                     "qrc",
		"Spotify":                     "json",
		"Musixmatch":                  "json",
		"Lyrics+":                     "json",
		"Lyrics+ (via Apple with QQ)": "json",
		"":                            "json",
	}
	for in, want := range cases {
		if got := extFor(in); got != want {
			t.Errorf("extFor(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFromStoreSourceFiltering(t *testing.T) {
	st, err := storage.NewStore(config.Storage{DBPath: filepath.Join(t.TempDir(), "cache.db"), LRUSize: 64})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer func() { _ = st.Close() }()

	stored := &domain.LyricsResponse{
		Type: domain.SyncTypeLine,
		Metadata: domain.LyricsMetadata{
			Source: "Apple",
			Title:  "Nyam Nyam Ketupat",
			Artist: "Dinda Dania",
		},
		Lyrics: []domain.Line{
			{Time: 0, Duration: 3000, Text: "Nyam-nyam", Element: domain.LineElement{Key: "L1"}},
		},
	}
	raw, _ := json.Marshal(stored)
	row := &storage.Row{
		Filename:    "test.xml",
		ContentJSON: raw,
		Source:      "apple",
		Title:       "Nyam Nyam Ketupat",
		Artist:      "Dinda Dania",
	}
	if err := st.SaveLyrics(context.Background(), row); err != nil {
		t.Fatalf("save: %v", err)
	}

	s := &Service{Store: st}

	// Requesting qq: should NOT hit the apple cache
	qqQuery := domain.SearchQuery{
		Title:   "Nyam Nyam Ketupat",
		Artist:  "Dinda Dania",
		Sources: []string{"qq"},
	}
	if _, ok := s.fromStore(context.Background(), qqQuery); ok {
		t.Error("expected fromStore to return false when requesting source=qq but cache has apple")
	}

	// Requesting apple: should hit the cache
	appleQuery := domain.SearchQuery{
		Title:   "Nyam Nyam Ketupat",
		Artist:  "Dinda Dania",
		Sources: []string{"apple"},
	}
	if resp, ok := s.fromStore(context.Background(), appleQuery); !ok || resp == nil {
		t.Error("expected fromStore to return true when requesting source=apple")
	}

	// Requesting default (no source filter): should hit the cache
	defaultQuery := domain.SearchQuery{
		Title:  "Nyam Nyam Ketupat",
		Artist: "Dinda Dania",
	}
	if resp, ok := s.fromStore(context.Background(), defaultQuery); !ok || resp == nil {
		t.Error("expected fromStore to return true when requesting without source filter")
	}

	// Requesting qq,apple when ONLY apple is cached:
	// Top preference qq is not in cache, so it should return false to allow racing live
	qqAppleQuery := domain.SearchQuery{
		Title:   "Nyam Nyam Ketupat",
		Artist:  "Dinda Dania",
		Sources: []string{"qq", "apple"},
	}
	if _, ok := s.fromStore(context.Background(), qqAppleQuery); ok {
		t.Error("expected fromStore to return false when top preference qq is missing from cache")
	}

	// Requesting apple,qq when ONLY apple is cached:
	// Top preference apple is in cache, so it should hit cache
	appleQqQuery := domain.SearchQuery{
		Title:   "Nyam Nyam Ketupat",
		Artist:  "Dinda Dania",
		Sources: []string{"apple", "qq"},
	}
	if resp, ok := s.fromStore(context.Background(), appleQqQuery); !ok || resp == nil {
		t.Error("expected fromStore to return true when top preference apple is cached")
	}

	// Now also save a QQ record for the same song
	storedQQ := &domain.LyricsResponse{
		Type: domain.SyncTypeLine,
		Metadata: domain.LyricsMetadata{
			Source: "QQ Music",
			Title:  "Nyam Nyam Ketupat",
			Artist: "Dinda Dania",
		},
		Lyrics: []domain.Line{
			{Time: 0, Duration: 3000, Text: "QQ Nyam-nyam", Element: domain.LineElement{Key: "L1"}},
		},
	}
	rawQQ, _ := json.Marshal(storedQQ)
	rowQQ := &storage.Row{
		Filename:    "test_qq.xml",
		ContentJSON: rawQQ,
		Source:      "qq",
		Title:       "Nyam Nyam Ketupat",
		Artist:      "Dinda Dania",
	}
	if err := st.SaveLyrics(context.Background(), rowQQ); err != nil {
		t.Fatalf("save qq: %v", err)
	}

	// Now both Apple and QQ exist in SQLite.
	// When requesting source=qq,apple, it must pick QQ:
	if resp, ok := s.fromStore(context.Background(), qqAppleQuery); !ok || resp == nil {
		t.Fatal("expected cache hit for qq,apple")
	} else if resp.Metadata.Source != "QQ Music" {
		t.Errorf("expected winner QQ Music, got %q", resp.Metadata.Source)
	}

	// When requesting source=apple,qq, it must pick Apple:
	if resp, ok := s.fromStore(context.Background(), appleQqQuery); !ok || resp == nil {
		t.Fatal("expected cache hit for apple,qq")
	} else if resp.Metadata.Source != "Apple" {
		t.Errorf("expected winner Apple, got %q", resp.Metadata.Source)
	}
}

func TestFromStoreDuplicateTitleArtistDisambiguationByDurationAndAlbum(t *testing.T) {
	st, err := storage.NewStore(config.Storage{DBPath: filepath.Join(t.TempDir(), "dup_cache.db"), LRUSize: 64})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer func() { _ = st.Close() }()

	ctx := context.Background()

	// Version 1: Original Studio version (3m00s = 180000ms, Album: "Asylum")
	respStudio := &domain.LyricsResponse{
		Type: domain.SyncTypeLine,
		Metadata: domain.LyricsMetadata{
			Source: "apple",
			Title:  "Warrior",
			Artist: "Disturbed",
			Album:  "Asylum",
		},
		Lyrics: []domain.Line{{Text: "Studio Warrior"}},
	}
	rawStudio, _ := json.Marshal(respStudio)
	rowStudio := &storage.Row{
		Filename:    storage.CanonicalFilename("Disturbed", "Warrior", "Asylum", 180000, "", "", "json"),
		ContentJSON: rawStudio,
		Source:      "apple",
		Title:       "Warrior",
		Artist:      "Disturbed",
		DurationMS:  180000,
	}
	if err := st.SaveLyrics(ctx, rowStudio); err != nil {
		t.Fatalf("save studio: %v", err)
	}

	// Version 2: Live Extended version (5m10s = 310000ms, Album: "Live in London")
	respLive := &domain.LyricsResponse{
		Type: domain.SyncTypeLine,
		Metadata: domain.LyricsMetadata{
			Source: "apple",
			Title:  "Warrior",
			Artist: "Disturbed",
			Album:  "Live in London",
		},
		Lyrics: []domain.Line{{Text: "Live Warrior"}},
	}
	rawLive, _ := json.Marshal(respLive)
	rowLive := &storage.Row{
		Filename:    storage.CanonicalFilename("Disturbed", "Warrior", "Live in London", 310000, "", "", "json"),
		ContentJSON: rawLive,
		Source:      "apple",
		Title:       "Warrior",
		Artist:      "Disturbed",
		DurationMS:  310000,
	}
	if err := st.SaveLyrics(ctx, rowLive); err != nil {
		t.Fatalf("save live: %v", err)
	}

	s := &Service{Store: st}

	// 1. Query targeting Studio version by duration (~180s) and Album ("Asylum")
	qStudio := domain.SearchQuery{
		Title:    "Warrior",
		Artist:   "Disturbed",
		Album:    "Asylum",
		Duration: 181000, // 181s (~1s difference)
	}
	hitStudio, ok := s.fromStore(ctx, qStudio)
	if !ok || hitStudio == nil {
		t.Fatalf("expected hit for studio version")
	}
	if len(hitStudio.Lyrics) == 0 || hitStudio.Lyrics[0].Text != "Studio Warrior" {
		t.Fatalf("expected 'Studio Warrior', got %v", hitStudio.Lyrics)
	}

	// 2. Query targeting Live version by duration (~310s) and Album ("Live in London")
	qLive := domain.SearchQuery{
		Title:    "Warrior",
		Artist:   "Disturbed",
		Album:    "Live in London",
		Duration: 309000, // 309s (~1s difference)
	}
	hitLive, ok := s.fromStore(ctx, qLive)
	if !ok || hitLive == nil {
		t.Fatalf("expected hit for live version")
	}
	if len(hitLive.Lyrics) == 0 || hitLive.Lyrics[0].Text != "Live Warrior" {
		t.Fatalf("expected 'Live Warrior', got %v", hitLive.Lyrics)
	}

	// 3. Query with wild duration mismatch (e.g. 10 minutes = 600000ms)
	// Must NOT falsely return an out-of-sync studio or live version
	qMismatch := domain.SearchQuery{
		Title:    "Warrior",
		Artist:   "Disturbed",
		Duration: 600000,
	}
	_, okMismatch := s.fromStore(ctx, qMismatch)
	if okMismatch {
		t.Fatalf("expected rejection on severe duration mismatch, but got hit")
	}
}
