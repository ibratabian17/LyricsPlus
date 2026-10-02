package storage

import (
	"context"
	"path/filepath"
	"testing"

	"lyricsplus/backend/internal/config"
	"lyricsplus/backend/internal/domain"
)

// BenchmarkStoreHitPath measures allocations for a warm title+artist lookup
// followed by a content fetch. This is the dominant read path in production.
func BenchmarkStoreHitPath(b *testing.B) {
	dbPath := filepath.Join(b.TempDir(), "perf.db")
	st, err := NewStore(config.Storage{DBPath: dbPath, LRUSize: 512})
	if err != nil {
		b.Fatalf("NewStore: %v", err)
	}
	defer func() { _ = st.Close() }()

	ctx := context.Background()
	payload := make([]byte, 64<<10)
	for i := range payload {
		payload[i] = byte('a' + i%26)
	}
	if err := st.SaveUserLyrics(ctx, domain.SearchQuery{
		Title: "Bohemian Rhapsody", Artist: "Queen", Duration: 354000,
	}, payload); err != nil {
		b.Fatalf("SaveUserLyrics: %v", err)
	}

	// Warm the LRUs so we measure the steady-state hit path, not cold misses.
	if _, ok := st.GetByTitleArtist(ctx, "Bohemian Rhapsody", "Queen"); !ok {
		b.Fatal("expected warm hit")
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, ok := st.GetByTitleArtist(ctx, "Bohemian Rhapsody", "Queen")
		if !ok || len(rows) == 0 {
			b.Fatal("miss")
		}
		if _, err := st.GetContent(ctx, rows[0].ID); err != nil {
			b.Fatalf("GetContent: %v", err)
		}
	}
}

// BenchmarkStoreMissPath measures the repeat-miss path. Without negative
// caching this re-queries SQLite on every single request.
func BenchmarkStoreMissPath(b *testing.B) {
	dbPath := filepath.Join(b.TempDir(), "perfmiss.db")
	st, err := NewStore(config.Storage{DBPath: dbPath, LRUSize: 512})
	if err != nil {
		b.Fatalf("NewStore: %v", err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = st.GetByTitleArtist(ctx, "Nonexistent Song", "Nobody")
		_, _ = st.GetByFTS5(ctx, "Nonexistent Song", "Nobody")
	}
}
