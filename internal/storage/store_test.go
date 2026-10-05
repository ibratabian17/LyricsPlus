package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"lyricsplus/backend/internal/config"
	"lyricsplus/backend/internal/domain"
)

func TestStoreGetByTitleArtistAndContent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "store_test.db")
	st, err := NewStore(config.Storage{DBPath: dbPath, LRUSize: 64})
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	defer func() { _ = st.Close() }()

	ctx := context.Background()

	rows, ok := st.GetByTitleArtist(ctx, "Warrior", "Disturbed")
	if ok || len(rows) > 0 {
		t.Fatalf("expected miss on empty DB, got %v", rows)
	}

	q := domain.SearchQuery{
		Title:    "Warrior",
		Artist:   "Disturbed",
		Duration: 180000,
	}
	contentData := []byte(`{"type":"line","lyrics":[{"time":1000,"text":"Warrior"}]}`)
	if err := st.SaveUserLyrics(ctx, q, contentData); err != nil {
		t.Fatalf("SaveUserLyrics failed: %v", err)
	}

	rows, ok = st.GetByTitleArtist(ctx, "Warrior", "Disturbed")
	if !ok || len(rows) == 0 {
		t.Fatalf("expected hit on GetByTitleArtist, got ok=%v, len=%d", ok, len(rows))
	}
	row := rows[0]
	if row.Title != "Warrior" || row.Artist != "Disturbed" {
		t.Fatalf("unexpected row metadata: %+v", row)
	}

	if len(row.ContentJSON) != 0 {
		t.Fatalf("expected deferred content to be empty, got %d bytes", len(row.ContentJSON))
	}

	content, err := st.GetContent(ctx, row.ID)
	if err != nil {
		t.Fatalf("GetContent failed: %v", err)
	}
	if string(content) != string(contentData) {
		t.Fatalf("content mismatch: got %s, want %s", string(content), string(contentData))
	}

	cachedContent, err := st.GetContent(ctx, row.ID)
	if err != nil || string(cachedContent) != string(contentData) {
		t.Fatalf("cached GetContent failed: %v", err)
	}

	q2 := domain.SearchQuery{Title: "Other Song", Artist: "Other Artist"}
	if err := st.SaveUserLyrics(ctx, q2, []byte(`{"type":"line"}`)); err != nil {
		t.Fatalf("save second song: %v", err)
	}

	key := "ta::warrior::disturbed"
	if _, found := st.exactTitle.Get(key); !found {
		t.Fatalf("exactTitle cache was unexpectedly wiped by second save")
	}
}

func TestStoreConnectionPoolSettings(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "pool_test.db")
	st, err := NewStore(config.Storage{DBPath: dbPath, LRUSize: 64})
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	defer func() { _ = st.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ch := make(chan error, 5)
	for i := 0; i < 5; i++ {
		go func() {
			_, err := st.queryTitleArtist(ctx, "Test", "Test")
			ch <- err
		}()
	}
	for i := 0; i < 5; i++ {
		if err := <-ch; err != nil {
			t.Fatalf("concurrent query failed: %v", err)
		}
	}
}

func TestFTS5Search(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "fts5_test.db")
	st, err := NewStore(config.Storage{DBPath: dbPath, LRUSize: 64})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()

	q := domain.SearchQuery{
		Title:  "Jadi Diri Sendiri (Kun Anta) [Bahasa/Malay Version]",
		Artist: "Humood Alkhudher",
	}
	if err := st.SaveUserLyrics(ctx, q, []byte(`{"type":"line"}`)); err != nil {
		t.Fatalf("SaveUserLyrics: %v", err)
	}

	rows, ok := st.GetByFTS5(ctx, "Jadi diri sendiri", "Humood AlKhuder")
	if !ok || len(rows) == 0 {
		t.Fatalf("GetByFTS5: expected hit, got ok=%v len=%d", ok, len(rows))
	}
	if rows[0].Artist != "Humood Alkhudher" {
		t.Errorf("unexpected artist: %q", rows[0].Artist)
	}
}

func TestBuildFTSQuery(t *testing.T) {
	cases := []struct {
		title, artist string
		wantEmpty     bool
	}{
		{"Jadi diri sendiri", "Humood", false},
		{"", "Humood", false},
		{"", "", true},

		{`"^*(::)`, `^*"`, true},

		{`"(bad)"`, `^*`, false},
	}
	for _, c := range cases {
		q := buildFTSQuery(c.title, c.artist)
		if c.wantEmpty && q != "" {
			t.Errorf("buildFTSQuery(%q,%q) = %q, want empty", c.title, c.artist, q)
		}
		if !c.wantEmpty && q == "" {
			t.Errorf("buildFTSQuery(%q,%q) = empty, want non-empty", c.title, c.artist)
		}
	}
}
