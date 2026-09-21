// Package storage provides SQLite-backed caching, submissions, and backups.
package storage

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
	_ "modernc.org/sqlite"

	"lyricsplus/backend/internal/config"
	"lyricsplus/backend/internal/domain"
)

// Row mirrors a single row of the lyrics cache table.
type Row struct {
	ID          int64
	Filename    string
	ContentJSON []byte
	ISRC        string
	PlatformID  string
	Source      string
	Title       string
	Artist      string
	DurationMS  int
	CreatedAt   time.Time
}

const lyricsCreateTable = `
CREATE TABLE IF NOT EXISTS lyrics (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  filename TEXT NOT NULL,
  content BLOB NOT NULL,
  isrc TEXT,
  platform_id TEXT,
  source TEXT,
  title TEXT,
  artist TEXT,
  duration_ms INTEGER DEFAULT 0,
  created_at INTEGER NOT NULL,
  UNIQUE(filename, source)
);
CREATE INDEX IF NOT EXISTS idx_lyrics_filename ON lyrics(filename);
CREATE INDEX IF NOT EXISTS idx_lyrics_isrc ON lyrics(isrc);
CREATE INDEX IF NOT EXISTS idx_lyrics_platform ON lyrics(platform_id);
CREATE INDEX IF NOT EXISTS idx_lyrics_title_artist ON lyrics(title, artist);
`

const storeRowColumns = "id, filename, content, isrc, platform_id, source, title, artist, duration_ms, created_at"

// Store is the SQLite-backed two-tier lyrics cache sitting behind positive LRU caches.
type Store struct {
	cfg     config.Storage
	db      *sql.DB
	exact   *lru.Cache[string, *Row]
	exist   *lru.Cache[string, []*Row]
	content *lru.Cache[string, []byte]
}

// NewStore opens (creating if needed) the SQLite cache at cfg.DBPath.
func NewStore(cfg config.Storage) (*Store, error) {
	path := cfg.DBPath
	if path == "" {
		path = filepath.Join("database", "lyrics_cache.db")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("storage: mkdir: %w", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("storage: open sqlite: %w", err)
	}
	// Serialize access through one connection to avoid SQLITE_BUSY under WAL.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err := initStoreSchema(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("storage: init schema: %w", err)
	}

	size := cfg.LRUSize
	if size <= 0 {
		size = 4096
	}
	exact, _ := lru.New[string, *Row](size)
	exist, _ := lru.New[string, []*Row](size)
	content, _ := lru.New[string, []byte](size)

	return &Store{cfg: cfg, db: db, exact: exact, exist: exist, content: content}, nil
}

func initStoreSchema(db *sql.DB) error {
	pragmas := []string{
		"PRAGMA journal_mode = WAL;",
		"PRAGMA synchronous = NORMAL;",
		"PRAGMA busy_timeout = 5000;",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			return err
		}
	}
	_, err := db.Exec(lyricsCreateTable)
	return err
}

// GetExact returns the most recent row matching an ISRC or platform ID.
func (s *Store) GetExact(ctx context.Context, isrc, platformID string) (*Row, bool) {
	if isrc == "" && platformID == "" {
		return nil, false
	}
	key := "exact::" + strings.ToLower(isrc) + "::" + strings.ToLower(platformID)
	if v, ok := s.exact.Get(key); ok {
		return v, v != nil
	}
	row, err := s.queryExact(ctx, isrc, platformID)
	if err != nil {
		return nil, false
	}
	if row != nil {
		s.exact.Add(key, row)
	}
	return row, row != nil
}

// GetExisting returns rows matching the extracted keywords (title/artist LIKE).
func (s *Store) GetExisting(ctx context.Context, keywords []string) ([]*Row, bool) {
	var clean []string
	for _, k := range keywords {
		if strings.TrimSpace(k) != "" {
			clean = append(clean, k)
		}
	}
	if len(clean) == 0 {
		return nil, false
	}
	key := "existing::" + strings.Join(clean, " ")
	if v, ok := s.exist.Get(key); ok {
		return v, len(v) > 0
	}
	rows, err := s.queryExisting(ctx, clean)
	if err != nil {
		return nil, false
	}
	if len(rows) > 0 {
		s.exist.Add(key, rows)
	}
	return rows, len(rows) > 0
}

func (s *Store) queryExact(ctx context.Context, isrc, platformID string) (*Row, error) {
	const q = `SELECT ` + storeRowColumns + `
FROM lyrics
WHERE (isrc = ? AND isrc <> '') OR (platform_id = ? AND platform_id <> '')
ORDER BY created_at DESC LIMIT 1`
	row := &Row{}
	var createdAt int64
	err := s.db.QueryRowContext(ctx, q, isrc, platformID).Scan(
		&row.ID, &row.Filename, &row.ContentJSON, &row.ISRC, &row.PlatformID,
		&row.Source, &row.Title, &row.Artist, &row.DurationMS, &createdAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	row.CreatedAt = time.UnixMilli(createdAt)
	row.ContentJSON = DecompressContent(row.ContentJSON)
	return row, nil
}

func (s *Store) queryExisting(ctx context.Context, keywords []string) ([]*Row, error) {
	conds := make([]string, 0, len(keywords))
	args := make([]interface{}, 0, len(keywords)*2)
	for _, k := range keywords {
		pat := "%" + k + "%"
		conds = append(conds, "(title LIKE ? OR artist LIKE ?)")
		args = append(args, pat, pat)
	}
	q := `SELECT ` + storeRowColumns + ` FROM lyrics WHERE ` + strings.Join(conds, " AND ") +
		` ORDER BY created_at DESC LIMIT 20`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []*Row
	for rows.Next() {
		row := &Row{}
		var createdAt int64
		if err := rows.Scan(
			&row.ID, &row.Filename, &row.ContentJSON, &row.ISRC, &row.PlatformID,
			&row.Source, &row.Title, &row.Artist, &row.DurationMS, &createdAt,
		); err != nil {
			return nil, err
		}
		row.CreatedAt = time.UnixMilli(createdAt)
		row.ContentJSON = DecompressContent(row.ContentJSON)
		out = append(out, row)
	}
	return out, rows.Err()
}

// SaveUserLyrics persists a user-generated submission.
func (s *Store) SaveUserLyrics(ctx context.Context, query domain.SearchQuery, content []byte) error {
	row := &Row{
		Filename:    CanonicalFilename(query.Artist, query.Title, query.Album, query.Duration, query.ISRC, query.PlatformID, "json"),
		ContentJSON: content,
		ISRC:        query.ISRC,
		PlatformID:  query.PlatformID,
		Source:      "lyricsplus",
		Title:       query.Title,
		Artist:      query.Artist,
		DurationMS:  query.Duration,
		CreatedAt:   time.Now(),
	}
	return s.SaveLyrics(ctx, row)
}

// SaveLyrics upserts a provider result row into the cache.
func (s *Store) SaveLyrics(ctx context.Context, row *Row) error {
	if row == nil {
		return nil
	}
	if row.Filename == "" {
		row.Filename = CanonicalFilename(row.Artist, row.Title, "", row.DurationMS, row.ISRC, row.PlatformID, "json")
	}
	if row.CreatedAt.IsZero() {
		row.CreatedAt = time.Now()
	}
	row.ContentJSON = CompressContent(row.ContentJSON)
	const upsert = `INSERT INTO lyrics (filename, content, isrc, platform_id, source, title, artist, duration_ms, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(filename, source) DO UPDATE SET
  content = excluded.content,
  isrc = excluded.isrc,
  platform_id = excluded.platform_id,
  title = excluded.title,
  artist = excluded.artist,
  duration_ms = excluded.duration_ms,
  created_at = excluded.created_at`
	_, err := s.db.ExecContext(ctx, upsert,
		row.Filename, row.ContentJSON, row.ISRC, row.PlatformID, row.Source,
		row.Title, row.Artist, row.DurationMS, row.CreatedAt.UnixMilli(),
	)
	if err != nil {
		return err
	}
	s.exact.Remove("exact::" + strings.ToLower(row.ISRC) + "::" + strings.ToLower(row.PlatformID))
	s.exist.Purge()
	return nil
}

// Close releases the underlying database handle.
func (s *Store) Close() error {
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}

// Ping verifies the underlying database is reachable.
func (s *Store) Ping(ctx context.Context) error {
	if s.db == nil {
		return fmt.Errorf("storage: store not initialized")
	}
	return s.db.PingContext(ctx)
}
