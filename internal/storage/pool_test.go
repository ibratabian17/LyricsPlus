package storage

import (
	"context"
	"database/sql"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"lyricsplus/backend/internal/config"
	"lyricsplus/backend/internal/domain"
)

func TestPooledConnectionsCarryTheirOwnPageCache(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "pool.db")
	st, err := NewStore(config.Storage{DBPath: dbPath, LRUSize: 8, ReadConns: 4})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer func() { _ = st.Close() }()

	ctx := context.Background()

	pinned := make([]*sql.Conn, 0, 4)
	defer func() {
		for _, c := range pinned {
			_ = c.Close()
		}
	}()
	for i := 0; i < 4; i++ {
		c, err := st.db.Conn(ctx)
		if err != nil {
			t.Fatalf("Conn %d: %v", i, err)
		}
		pinned = append(pinned, c)
	}

	for i, c := range pinned {
		var cacheSize, journalMode, busyTimeout string
		if err := c.QueryRowContext(ctx, "PRAGMA cache_size").Scan(&cacheSize); err != nil {
			t.Fatalf("conn %d cache_size: %v", i, err)
		}
		if err := c.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode); err != nil {
			t.Fatalf("conn %d journal_mode: %v", i, err)
		}
		if err := c.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
			t.Fatalf("conn %d busy_timeout: %v", i, err)
		}

		if got, want := journalMode, "wal"; got != want {
			t.Errorf("conn %d journal_mode = %q, want %q (WAL is what allows concurrent readers)", i, got, want)
		}
		size, err := strconv.Atoi(cacheSize)
		if err != nil {
			t.Fatalf("conn %d cache_size %q unparseable: %v", i, cacheSize, err)
		}

		perConnKB := size
		if perConnKB < 0 {
			perConnKB = -perConnKB
		}
		totalMB := perConnKB * len(pinned) / 1024
		if totalMB > 256 {
			t.Errorf("page-cache budget for %d connections is %d MiB; too high", len(pinned), totalMB)
		}
	}
}

func TestReaderPoolIsBounded(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "bounded.db")
	st, err := NewStore(config.Storage{DBPath: dbPath, LRUSize: 8, ReadConns: 6})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer func() { _ = st.Close() }()

	stats := st.db.Stats()
	if stats.MaxOpenConnections != 6 {
		t.Errorf("reader pool MaxOpenConnections = %d, want 6", stats.MaxOpenConnections)
	}

	if got := st.wdb.Stats().MaxOpenConnections; got != 1 {
		t.Errorf("writer pool MaxOpenConnections = %d, want 1", got)
	}
}

func TestMissMemoCollapsesDuplicateLookups(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "memo.db")
	st, err := NewStore(config.Storage{DBPath: dbPath, LRUSize: 8, NegativeTTL: time.Minute})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer func() { _ = st.Close() }()

	ctx := context.Background()
	const attempts = 50
	for i := 0; i < attempts; i++ {
		if _, ok := st.GetByTitleArtist(ctx, "Ghost Song", "Nobody"); ok {
			t.Fatal("unexpected hit on empty database")
		}
	}

	if memoized := st.wasMiss("ta::ghost song::nobody"); !memoized {
		t.Fatal("expected the miss to be memoized")
	}
	if got := st.miss.Len(); got != 1 {
		t.Fatalf("expected exactly 1 memoized miss, got %d", got)
	}
}

func TestSaveClearsMissMemo(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "memo2.db")
	st, err := NewStore(config.Storage{DBPath: dbPath, LRUSize: 8, NegativeTTL: time.Minute})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer func() { _ = st.Close() }()

	ctx := context.Background()
	q := domain.SearchQuery{Title: "New Song", Artist: "Fresh"}

	if _, ok := st.GetByTitleArtist(ctx, q.Title, q.Artist); ok {
		t.Fatal("expected miss before save")
	}
	if err := st.SaveUserLyrics(ctx, q, []byte(`{"type":"line","lyrics":[{"time":1,"text":"x"}]}`)); err != nil {
		t.Fatalf("SaveUserLyrics: %v", err)
	}
	rows, ok := st.GetByTitleArtist(ctx, q.Title, q.Artist)
	if !ok || len(rows) == 0 {
		t.Fatal("row is still hidden by a stale memoized miss after save")
	}
}

func TestSaveLyricsEvictsStaleContent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "stale.db")
	st, err := NewStore(config.Storage{DBPath: dbPath, LRUSize: 8})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer func() { _ = st.Close() }()

	ctx := context.Background()
	q := domain.SearchQuery{Title: "Evolving", Artist: "Song"}

	first := []byte(`{"type":"line","lyrics":[{"time":1,"text":"first"}]}`)
	if err := st.SaveUserLyrics(ctx, q, first); err != nil {
		t.Fatalf("first save: %v", err)
	}
	rows, ok := st.GetByTitleArtist(ctx, q.Title, q.Artist)
	if !ok || len(rows) == 0 {
		t.Fatal("expected row after first save")
	}
	id := rows[0].ID

	if got, _ := st.GetContent(ctx, id); !bytesEqual(got, first) {
		t.Fatalf("unexpected first content: %s", got)
	}

	second := []byte(`{"type":"line","lyrics":[{"time":2,"text":"second"}]}`)
	if err := st.SaveUserLyrics(ctx, q, second); err != nil {
		t.Fatalf("second save: %v", err)
	}

	got, err := st.GetContent(ctx, id)
	if err != nil {
		t.Fatalf("GetContent: %v", err)
	}
	if !bytesEqual(got, second) {
		t.Fatalf("stale content served after update: %s", got)
	}
}
