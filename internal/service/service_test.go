package service

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"lyricsplus/backend/internal/config"
	"lyricsplus/backend/internal/domain"
	"lyricsplus/backend/internal/orchestrator"
	"lyricsplus/backend/internal/providers"
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
		"Spotify":                     "spotify",
		"Apple Music":                 "apple",
		"Apple":                       "apple",
		"QQ Music":                    "qq",
		"QQ":                          "qq",
		"Musixmatch":                  "musixmatch",
		"Deezer":                      "deezer",
		"Qaple":                       "qaple",
		"qaple":                       "qaple",
		"Lyrics+ (via Apple with QQ)": "qaple",
		"Lyrics+":                     "lyricsplus",
		"":                            "lyricsplus",
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

	qqQuery := domain.SearchQuery{
		Title:   "Nyam Nyam Ketupat",
		Artist:  "Dinda Dania",
		Sources: []string{"qq"},
	}
	if _, ok := s.fromStore(context.Background(), qqQuery); ok {
		t.Error("expected fromStore to return false when requesting source=qq but cache has apple")
	}

	appleQuery := domain.SearchQuery{
		Title:   "Nyam Nyam Ketupat",
		Artist:  "Dinda Dania",
		Sources: []string{"apple"},
	}
	if resp, ok := s.fromStore(context.Background(), appleQuery); !ok || resp == nil {
		t.Error("expected fromStore to return true when requesting source=apple")
	}

	defaultQuery := domain.SearchQuery{
		Title:  "Nyam Nyam Ketupat",
		Artist: "Dinda Dania",
	}
	if resp, ok := s.fromStore(context.Background(), defaultQuery); !ok || resp == nil {
		t.Error("expected fromStore to return true when requesting without source filter")
	}

	qqAppleQuery := domain.SearchQuery{
		Title:   "Nyam Nyam Ketupat",
		Artist:  "Dinda Dania",
		Sources: []string{"qq", "apple"},
	}
	if _, ok := s.fromStore(context.Background(), qqAppleQuery); ok {
		t.Error("expected fromStore to return false when top preference qq is missing from cache")
	}

	appleQqQuery := domain.SearchQuery{
		Title:   "Nyam Nyam Ketupat",
		Artist:  "Dinda Dania",
		Sources: []string{"apple", "qq"},
	}
	if resp, ok := s.fromStore(context.Background(), appleQqQuery); !ok || resp == nil {
		t.Error("expected fromStore to return true when top preference apple is cached")
	}

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

	if resp, ok := s.fromStore(context.Background(), qqAppleQuery); !ok || resp == nil {
		t.Fatal("expected cache hit for qq,apple")
	} else if resp.Metadata.Source != "QQ Music" {
		t.Errorf("expected winner QQ Music, got %q", resp.Metadata.Source)
	}

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

	qStudio := domain.SearchQuery{
		Title:    "Warrior",
		Artist:   "Disturbed",
		Album:    "Asylum",
		Duration: 181000,
	}
	hitStudio, ok := s.fromStore(ctx, qStudio)
	if !ok || hitStudio == nil {
		t.Fatalf("expected hit for studio version")
	}
	if len(hitStudio.Lyrics) == 0 || hitStudio.Lyrics[0].Text != "Studio Warrior" {
		t.Fatalf("expected 'Studio Warrior', got %v", hitStudio.Lyrics)
	}

	qLive := domain.SearchQuery{
		Title:    "Warrior",
		Artist:   "Disturbed",
		Album:    "Live in London",
		Duration: 309000,
	}
	hitLive, ok := s.fromStore(ctx, qLive)
	if !ok || hitLive == nil {
		t.Fatalf("expected hit for live version")
	}
	if len(hitLive.Lyrics) == 0 || hitLive.Lyrics[0].Text != "Live Warrior" {
		t.Fatalf("expected 'Live Warrior', got %v", hitLive.Lyrics)
	}

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

func TestLyricsPlusProviderIgnoresCachedNonLyricsPlusRows(t *testing.T) {
	st, err := storage.NewStore(config.Storage{DBPath: filepath.Join(t.TempDir(), "cache.db"), LRUSize: 64})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer func() { _ = st.Close() }()

	ctx := context.Background()

	stored := &domain.LyricsResponse{
		Type: domain.SyncTypeLine,
		Metadata: domain.LyricsMetadata{
			Source: "Apple",
			Title:  "Anti-Hero",
			Artist: "Taylor Swift",
		},
		Lyrics: []domain.Line{
			{Time: 1000, Duration: 2000, Text: "It's me, hi"},
		},
	}
	raw, _ := json.Marshal(stored)
	appleRow := &storage.Row{
		Filename:    "Taylor Swift - Anti-Hero.ttml",
		ContentJSON: raw,
		Source:      "apple",
		Title:       "Anti-Hero",
		Artist:      "Taylor Swift",
		ISRC:        "USUG12204998",
	}
	if err := st.SaveLyrics(ctx, appleRow); err != nil {
		t.Fatalf("save apple row: %v", err)
	}

	lp := providers.NewLyricsPlus(nil)
	lp.SetStore(st)

	q := domain.SearchQuery{
		Title:  "Anti-Hero",
		Artist: "Taylor Swift",
		ISRC:   "USUG12204998",
	}
	resp, err := lp.FetchLyrics(ctx, q)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != nil {
		t.Fatalf("expected nil from LyricsPlusProvider for apple cached row, got %+v", resp)
	}

	userResp := &domain.LyricsResponse{
		Type: domain.SyncTypeLine,
		Metadata: domain.LyricsMetadata{
			Source: "Lyrics+",
			Title:  "Anti-Hero",
			Artist: "Taylor Swift",
		},
		Lyrics: []domain.Line{
			{Time: 1000, Duration: 2000, Text: "User synced line"},
		},
	}
	userRaw, _ := json.Marshal(userResp)
	if err := st.SaveUserLyrics(ctx, q, userRaw); err != nil {
		t.Fatalf("save user lyrics: %v", err)
	}

	respUser, err := lp.FetchLyrics(ctx, q)
	if err != nil {
		t.Fatalf("unexpected error on user query: %v", err)
	}
	if respUser == nil {
		t.Fatal("expected hit for genuine user submission")
	}
	if respUser.Metadata.Source != "Lyrics+" {
		t.Fatalf("expected Source Lyrics+, got %q", respUser.Metadata.Source)
	}
	if respUser.Lyrics[0].Text != "User synced line" {
		t.Fatalf("expected user text, got %q", respUser.Lyrics[0].Text)
	}
}

type fakeQapleSource struct{}

func (f *fakeQapleSource) Name() string { return "lyricsplus" }
func (f *fakeQapleSource) FetchLyrics(ctx context.Context, q domain.SearchQuery) (*domain.LyricsResponse, error) {
	winner := "qaple"
	return &domain.LyricsResponse{
		Type:    domain.SyncTypeWord,
		RawData: "test-raw",
		Metadata: domain.LyricsMetadata{
			Source: "Lyrics+ (via Apple with QQ)",
			Title:  q.Title,
			Artist: q.Artist,
		},
		Lyrics: []domain.Line{
			{Time: 1000, Duration: 2000, Text: "word sync", Syllabus: []domain.Syllable{{Text: "word", Time: 1000, Duration: 1000}}},
		},
		ProcessingTime: &domain.ProcessTiming{
			WinnerSource: &winner,
		},
	}, nil
}

func TestQapleResultSavedToStore(t *testing.T) {
	st, err := storage.NewStore(config.Storage{DBPath: filepath.Join(t.TempDir(), "cache.db"), LRUSize: 64})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer func() { _ = st.Close() }()

	racer := orchestrator.NewRacer([]orchestrator.Source{&fakeQapleSource{}}, 5*time.Second)
	dedup := orchestrator.NewDedup(racer)
	s := &Service{
		Dedup: dedup,
		Store: st,
	}

	q := domain.SearchQuery{Title: "Qaple Song", Artist: "Qaple Artist"}
	resp, err := s.FetchLyrics(context.Background(), q, nil, false)
	if err != nil {
		t.Fatalf("FetchLyrics failed: %v", err)
	}
	if resp == nil {
		t.Fatal("expected response")
	}
	if resp.ProcessingTime.WinnerSource == nil || *resp.ProcessingTime.WinnerSource != "qaple" {
		t.Fatalf("expected winnerSource qaple, got %+v", resp.ProcessingTime.WinnerSource)
	}

	time.Sleep(100 * time.Millisecond)

	rows, ok := st.GetByTitleArtist(context.Background(), "Qaple Song", "Qaple Artist")
	if !ok || len(rows) == 0 {
		t.Fatalf("expected Qaple results to be saved to Store, but found 0 rows")
	}
}

type countingSource struct {
	calls atomic.Int64
}

func (c *countingSource) Name() string { return "apple" }
func (c *countingSource) FetchLyrics(ctx context.Context, q domain.SearchQuery) (*domain.LyricsResponse, error) {
	c.calls.Add(1)
	return &domain.LyricsResponse{
		Type:    domain.SyncTypeLine,
		RawData: "upstream-raw",
		Metadata: domain.LyricsMetadata{
			Source: "Apple",
			Title:  q.Title,
			Artist: q.Artist,
		},
		Lyrics: []domain.Line{
			{Time: 1000, Duration: 2000, Text: "Live line from upstream"},
		},
	}, nil
}

func TestFetchLyrics_CacheOnly(t *testing.T) {
	st, err := storage.NewStore(config.Storage{DBPath: filepath.Join(t.TempDir(), "cache.db"), LRUSize: 64})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer func() { _ = st.Close() }()

	src := &countingSource{}
	racer := orchestrator.NewRacer([]orchestrator.Source{src}, 5*time.Second)
	dedup := orchestrator.NewDedup(racer)
	s := &Service{
		Dedup: dedup,
		Store: st,
	}
	racer.SetSaver(s.SaveResultAsync)

	q := domain.SearchQuery{Title: "Uncached Song", Artist: "Unknown Artist"}
	cacheOnlyCtx := domain.WithCacheOnly(context.Background(), true)

	// 1. Cache-only miss: should return NotFoundError immediately without invoking upstream
	resp, err := s.FetchLyrics(cacheOnlyCtx, q, nil, false)
	if resp != nil {
		t.Fatalf("expected nil response on cache-only miss, got %+v", resp)
	}
	if err == nil {
		t.Fatal("expected NotFoundError on cache-only miss, got nil")
	}
	var nf *NotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("expected NotFoundError, got %v", err)
	}
	if src.calls.Load() != 0 {
		t.Fatalf("expected 0 upstream calls on cache-only miss, got %d", src.calls.Load())
	}

	// 2. Normal fetch: invokes upstream and caches it
	normalResp, err := s.FetchLyrics(context.Background(), q, nil, false)
	if err != nil || normalResp == nil {
		t.Fatalf("normal fetch failed: %v", err)
	}
	if src.calls.Load() != 1 {
		t.Fatalf("expected 1 upstream call, got %d", src.calls.Load())
	}

	// Wait for async write to store
	time.Sleep(100 * time.Millisecond)

	// 3. Cache-only hit: should return cached lyrics without calling upstream
	cachedResp, err := s.FetchLyrics(cacheOnlyCtx, q, nil, false)
	if err != nil {
		t.Fatalf("expected cached hit, got error: %v", err)
	}
	if cachedResp == nil || len(cachedResp.Lyrics) == 0 {
		t.Fatalf("expected cached response lyrics, got %+v", cachedResp)
	}
	if src.calls.Load() != 1 {
		t.Fatalf("expected upstream call count to stay 1, got %d", src.calls.Load())
	}

	// 4. Cache-only with forceReload=true: forceReload should be overridden / ignored
	cachedReloadResp, err := s.FetchLyrics(cacheOnlyCtx, q, nil, true)
	if err != nil || cachedReloadResp == nil {
		t.Fatalf("expected forceReload in cache-only to be ignored and return cache, got err=%v", err)
	}
	if src.calls.Load() != 1 {
		t.Fatalf("expected upstream call count to remain 1, got %d", src.calls.Load())
	}
}

func TestFetchRaw_CacheOnly(t *testing.T) {
	st, err := storage.NewStore(config.Storage{DBPath: filepath.Join(t.TempDir(), "cache.db"), LRUSize: 64})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer func() { _ = st.Close() }()

	src := &countingSource{}
	racer := orchestrator.NewRacer([]orchestrator.Source{src}, 5*time.Second)
	dedup := orchestrator.NewDedup(racer)
	s := &Service{
		Dedup: dedup,
		Store: st,
	}
	racer.SetSaver(s.SaveResultAsync)

	q := domain.SearchQuery{Title: "Raw Song", Artist: "Raw Artist"}
	cacheOnlyCtx := domain.WithCacheOnly(context.Background(), true)

	// 1. Raw cache-only miss
	raw, err := s.FetchRaw(cacheOnlyCtx, q, nil, false)
	if raw != nil || err == nil {
		t.Fatalf("expected not found on raw cache-only miss, got raw=%+v, err=%v", raw, err)
	}
	if src.calls.Load() != 0 {
		t.Fatalf("expected 0 calls, got %d", src.calls.Load())
	}

	// 2. Populate via normal FetchLyrics
	_, _ = s.FetchLyrics(context.Background(), q, nil, false)
	time.Sleep(100 * time.Millisecond)

	// 3. Raw cache-only hit
	rawHit, err := s.FetchRaw(cacheOnlyCtx, q, nil, false)
	if err != nil || rawHit == nil || rawHit.Raw == "" {
		t.Fatalf("expected raw cache hit, got raw=%+v, err=%v", rawHit, err)
	}
}
