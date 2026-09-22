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
const storeLightRowColumns = "id, filename, isrc, platform_id, source, title, artist, duration_ms, created_at"

// Store is the SQLite-backed two-tier lyrics cache sitting behind positive LRU caches.
type Store struct {
	cfg        config.Storage
	db         *sql.DB
	exact      *lru.Cache[string, *Row]
	exactTitle *lru.Cache[string, []*Row]
	exist      *lru.Cache[string, []*Row]
	content    *lru.Cache[int64, []byte]
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
	// In WAL mode, concurrent readers do not block each other or writers.
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)

	if err := initStoreSchema(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("storage: init schema: %w", err)
	}

	size := cfg.LRUSize
	if size <= 0 {
		size = 4096
	}
	exact, _ := lru.New[string, *Row](size)
	exactTitle, _ := lru.New[string, []*Row](size)
	exist, _ := lru.New[string, []*Row](size)
	content, _ := lru.New[int64, []byte](size)

	return &Store{
		cfg:        cfg,
		db:         db,
		exact:      exact,
		exactTitle: exactTitle,
		exist:      exist,
		content:    content,
	}, nil
}

func initStoreSchema(db *sql.DB) error {
	pragmas := []string{
		"PRAGMA journal_mode = WAL;",
		"PRAGMA synchronous = NORMAL;",
		"PRAGMA busy_timeout = 10000;",
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

// GetExactUser returns the most recent row matching an ISRC or platform ID with source = 'lyricsplus'.
func (s *Store) GetExactUser(ctx context.Context, isrc, platformID string) (*Row, bool) {
	if isrc == "" && platformID == "" {
		return nil, false
	}
	key := "exact_user::" + strings.ToLower(isrc) + "::" + strings.ToLower(platformID)
	if v, ok := s.exact.Get(key); ok {
		return v, v != nil
	}
	row, err := s.queryExactUser(ctx, isrc, platformID)
	if err != nil {
		return nil, false
	}
	if row != nil {
		s.exact.Add(key, row)
	}
	return row, row != nil
}

func (s *Store) queryExactUser(ctx context.Context, isrc, platformID string) (*Row, error) {
	const q = `SELECT ` + storeRowColumns + `
FROM lyrics
WHERE ((isrc = ? AND isrc <> '') OR (platform_id = ? AND platform_id <> ''))
  AND source = 'lyricsplus'
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

// GetByTitleArtist returns rows matching title and artist using idx_lyrics_title_artist index.
func (s *Store) GetByTitleArtist(ctx context.Context, title, artist string) ([]*Row, bool) {
	title = strings.TrimSpace(title)
	artist = strings.TrimSpace(artist)
	if title == "" || artist == "" {
		return nil, false
	}
	key := "ta::" + strings.ToLower(title) + "::" + strings.ToLower(artist)
	if v, ok := s.exactTitle.Get(key); ok {
		return v, len(v) > 0
	}
	rows, err := s.queryTitleArtist(ctx, title, artist)
	if err != nil {
		return nil, false
	}
	if len(rows) > 0 {
		s.exactTitle.Add(key, rows)
	}
	return rows, len(rows) > 0
}

func (s *Store) queryTitleArtist(ctx context.Context, title, artist string) ([]*Row, error) {
	const q = `SELECT ` + storeLightRowColumns + `
FROM lyrics
WHERE title = ? AND artist = ?
ORDER BY created_at DESC LIMIT 10`
	rows, err := s.db.QueryContext(ctx, q, title, artist)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []*Row
	for rows.Next() {
		row := &Row{}
		var createdAt int64
		if err := rows.Scan(
			&row.ID, &row.Filename, &row.ISRC, &row.PlatformID,
			&row.Source, &row.Title, &row.Artist, &row.DurationMS, &createdAt,
		); err != nil {
			return nil, err
		}
		row.CreatedAt = time.UnixMilli(createdAt)
		out = append(out, row)
	}
	return out, rows.Err()
}

// GetContent loads and decompresses content BLOB by primary key ID, caching in LRU.
func (s *Store) GetContent(ctx context.Context, id int64) ([]byte, error) {
	if id <= 0 {
		return nil, nil
	}
	if v, ok := s.content.Get(id); ok && len(v) > 0 {
		return v, nil
	}
	const q = `SELECT content FROM lyrics WHERE id = ?`
	var raw []byte
	err := s.db.QueryRowContext(ctx, q, id).Scan(&raw)
	if err != nil {
		return nil, err
	}
	content := DecompressContent(raw)
	if len(content) > 0 {
		s.content.Add(id, content)
	}
	return content, nil
}

func (s *Store) queryExisting(ctx context.Context, keywords []string) ([]*Row, error) {
	conds := make([]string, 0, len(keywords))
	args := make([]interface{}, 0, len(keywords)*2)
	for _, k := range keywords {
		pat := "%" + k + "%"
		conds = append(conds, "(title LIKE ? OR artist LIKE ?)")
		args = append(args, pat, pat)
	}
	q := `SELECT ` + storeLightRowColumns + ` FROM lyrics WHERE ` + strings.Join(conds, " AND ") +
		` LIMIT 100`
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
			&row.ID, &row.Filename, &row.ISRC, &row.PlatformID,
			&row.Source, &row.Title, &row.Artist, &row.DurationMS, &createdAt,
		); err != nil {
			return nil, err
		}
		row.CreatedAt = time.UnixMilli(createdAt)
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
	s.exact.Remove("exact_user::" + strings.ToLower(row.ISRC) + "::" + strings.ToLower(row.PlatformID))
	s.exactTitle.Remove("ta::" + strings.ToLower(row.Title) + "::" + strings.ToLower(row.Artist))
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
