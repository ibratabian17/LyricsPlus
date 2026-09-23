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

const lyricsFTS5Setup = `
CREATE VIRTUAL TABLE IF NOT EXISTS lyrics_fts USING fts5(
  title,
  artist,
  content='lyrics',
  content_rowid='id',
  tokenize='unicode61 remove_diacritics 1'
);

-- Keep FTS index in sync with the main table.
CREATE TRIGGER IF NOT EXISTS lyrics_fts_insert AFTER INSERT ON lyrics BEGIN
  INSERT INTO lyrics_fts(rowid, title, artist) VALUES (new.id, new.title, new.artist);
END;

CREATE TRIGGER IF NOT EXISTS lyrics_fts_delete AFTER DELETE ON lyrics BEGIN
  INSERT INTO lyrics_fts(lyrics_fts, rowid, title, artist) VALUES ('delete', old.id, old.title, old.artist);
END;

CREATE TRIGGER IF NOT EXISTS lyrics_fts_update AFTER UPDATE ON lyrics BEGIN
  INSERT INTO lyrics_fts(lyrics_fts, rowid, title, artist) VALUES ('delete', old.id, old.title, old.artist);
  INSERT INTO lyrics_fts(rowid, title, artist) VALUES (new.id, new.title, new.artist);
END;
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

	dsn := fmt.Sprintf(
		"file:%s?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=cache_size(-4000)&_pragma=mmap_size(2147483648)&_pragma=temp_store(MEMORY)",
		path,
	)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("storage: open sqlite: %w", err)
	}
	// In WAL mode, concurrent readers do not block each other or writers.
	db.SetMaxOpenConns(200)
	db.SetMaxIdleConns(50)
	db.SetConnMaxLifetime(time.Hour)
	db.SetConnMaxIdleTime(10 * time.Minute)

	if err := initStoreSchema(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("storage: init schema: %w", err)
	}

	size := cfg.LRUSize
	if size <= 0 {
		size = 16384
	}
	exact, _ := lru.New[string, *Row](size)
	exactTitle, _ := lru.New[string, []*Row](size)
	exist, _ := lru.New[string, []*Row](size)
	content, _ := lru.New[int64, []byte](size)

	st := &Store{
		cfg:        cfg,
		db:         db,
		exact:      exact,
		exactTitle: exactTitle,
		exist:      exist,
		content:    content,
	}
	go st.backfillFTS5()
	return st, nil
}

func (s *Store) backfillFTS5() {
	var count int64
	if err := s.db.QueryRow(`SELECT count(*) FROM lyrics_fts`).Scan(&count); err == nil && count > 0 {
		return
	}
	_, _ = s.db.Exec(`INSERT INTO lyrics_fts(lyrics_fts) VALUES('rebuild')`)
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
	if _, err := db.Exec(lyricsCreateTable); err != nil {
		return err
	}
	if _, err := db.Exec(lyricsFTS5Setup); err != nil {
		return err
	}
	_, _ = db.Exec("PRAGMA optimize=0x10002;")
	return nil
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
// Prefer GetByFTS5 for performance; this is kept as a legacy fallback.
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

func (s *Store) GetByFTS5(ctx context.Context, title, artist string) ([]*Row, bool) {
	title = strings.TrimSpace(title)
	artist = strings.TrimSpace(artist)
	if title == "" && artist == "" {
		return nil, false
	}

	tokens := buildFTSQuery(title, artist)
	if tokens == "" {
		return nil, false
	}

	key := "fts5::" + tokens
	if v, ok := s.exist.Get(key); ok {
		return v, len(v) > 0
	}

	rows, err := s.queryFTS5(ctx, tokens)
	if err != nil {
		return nil, false
	}
	if len(rows) > 0 {
		s.exist.Add(key, rows)
	}
	return rows, len(rows) > 0
}

// buildFTSQuery builds a FTS5 match expression from title and artist.
// Each word becomes a term; all are OR-combined for broad recall.
// The title is required (prefix on first token) and artist is optional.
func buildFTSQuery(title, artist string) string {
	var terms []string
	for _, word := range strings.Fields(title) {
		if w := sanitizeFTSToken(word); w != "" {
			terms = append(terms, w)
		}
	}
	for _, word := range strings.Fields(artist) {
		if w := sanitizeFTSToken(word); w != "" {
			terms = append(terms, w)
		}
	}
	if len(terms) == 0 {
		return ""
	}
	return strings.Join(terms, " OR ")
}

// sanitizeFTSToken removes characters that would break an FTS5 query.
func sanitizeFTSToken(word string) string {
	var b strings.Builder
	for _, r := range word {
		// Keep letters, digits, apostrophes; strip FTS5 special chars.
		if r == '"' || r == '(' || r == ')' || r == '^' || r == '*' || r == ':' {
			continue
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

func (s *Store) queryFTS5(ctx context.Context, matchExpr string) ([]*Row, error) {
	// Join FTS5 virtual table with main table via rowid to get metadata.
	const q = `SELECT l.id, l.filename, l.isrc, l.platform_id, l.source, l.title, l.artist, l.duration_ms, l.created_at
FROM lyrics_fts
JOIN lyrics l ON lyrics_fts.rowid = l.id
WHERE lyrics_fts MATCH ?
ORDER BY rank
LIMIT 50`
	rows, err := s.db.QueryContext(ctx, q, matchExpr)
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
