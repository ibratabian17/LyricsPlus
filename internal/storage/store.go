package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/hashicorp/golang-lru/v2/expirable"
	_ "modernc.org/sqlite"

	"lyricsplus/backend/internal/config"
	"lyricsplus/backend/internal/domain"
	"lyricsplus/backend/internal/similarity"
)

var (
	errWritePanic = errors.New("storage: write-back panicked")
	errQueueFull  = errors.New("storage: write-back queue full")
)

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
CREATE INDEX IF NOT EXISTS idx_lyrics_isrc ON lyrics(isrc) WHERE isrc IS NOT NULL AND isrc != '';
CREATE INDEX IF NOT EXISTS idx_lyrics_platform ON lyrics(platform_id) WHERE platform_id IS NOT NULL AND platform_id != '';
CREATE INDEX IF NOT EXISTS idx_lyrics_isrc_source ON lyrics(isrc, source) WHERE isrc IS NOT NULL AND isrc != '';
CREATE INDEX IF NOT EXISTS idx_lyrics_platform_source ON lyrics(platform_id, source) WHERE platform_id IS NOT NULL AND platform_id != '';
CREATE INDEX IF NOT EXISTS idx_lyrics_title_artist ON lyrics(title COLLATE NOCASE, artist COLLATE NOCASE);
CREATE INDEX IF NOT EXISTS idx_lyrics_ta_covering ON lyrics(title COLLATE NOCASE, artist COLLATE NOCASE, id, filename, isrc, platform_id, source, duration_ms, created_at);
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

type Store struct {
	cfg config.Storage

	db  *sql.DB
	wdb *sql.DB

	exact      *lru.Cache[string, *Row]
	exactTitle *lru.Cache[string, []*Row]
	exist      *lru.Cache[string, []*Row]
	content    *lru.Cache[int64, []byte]
	miss       *expirable.LRU[string, struct{}]

	writes    *writeQueue
	closeOnce sync.Once
}

func NewStore(cfg config.Storage) (*Store, error) {
	path := cfg.DBPath
	if path == "" {
		path = filepath.Join("database", "lyrics_cache.db")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("storage: mkdir: %w", err)
	}

	readDSN := buildDSN(path, readCacheKB)
	writeDSN := buildDSN(path, writeCacheKB)

	db, err := sql.Open("sqlite", readDSN)
	if err != nil {
		return nil, fmt.Errorf("storage: open sqlite: %w", err)
	}
	readConns := cfg.ReadConns
	if readConns <= 0 {
		readConns = defaultReadConns
	}
	db.SetMaxOpenConns(readConns)
	db.SetMaxIdleConns(readConns)
	db.SetConnMaxIdleTime(5 * time.Minute)

	wdb, err := sql.Open("sqlite", writeDSN)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("storage: open sqlite writer: %w", err)
	}
	wdb.SetMaxOpenConns(1)
	wdb.SetMaxIdleConns(1)
	wdb.SetConnMaxLifetime(time.Hour)

	if err := initStoreSchema(db); err != nil {
		_ = db.Close()
		_ = wdb.Close()
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

	negSize := cfg.MissMemoSize
	if negSize <= 0 {
		negSize = size / 4
	}
	negTTL := cfg.NegativeTTL
	if negTTL <= 0 {
		negTTL = 30 * time.Second
	}
	miss := expirable.NewLRU[string, struct{}](negSize, nil, negTTL)

	st := &Store{
		cfg:        cfg,
		db:         db,
		wdb:        wdb,
		exact:      exact,
		exactTitle: exactTitle,
		exist:      exist,
		content:    content,
		miss:       miss,
	}
	st.writes = newWriteQueue(1, cfg.WriteQueueSize)
	return st, nil
}

const (
	defaultReadConns = 8
	readCacheKB      = 16384
	writeCacheKB     = 2000
)

func buildDSN(path string, cacheKB int) string {
	return fmt.Sprintf(
		"file:%s?_pragma=busy_timeout(10000)"+
			"&_pragma=journal_mode(WAL)"+
			"&_pragma=synchronous(NORMAL)"+
			"&_pragma=cache_size(%d)"+
			"&_pragma=mmap_size(1073741824)"+
			"&_pragma=temp_store(MEMORY)"+
			"&_pragma=foreign_keys(ON)",
		path, cacheKB,
	)
}

func (s *Store) BackfillFTS5() {
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
		"PRAGMA cache_size = -16384;",
		"PRAGMA mmap_size = 1073741824;",
		"PRAGMA temp_store = MEMORY;",
		"PRAGMA foreign_keys = ON;",
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
	_, _ = db.Exec("ANALYZE;")
	_, _ = db.Exec("PRAGMA optimize;")
	return nil
}

func (s *Store) GetExact(ctx context.Context, isrc, platformID string) (*Row, bool) {
	if isrc == "" && platformID == "" {
		return nil, false
	}
	key := "exact::" + strings.ToLower(isrc) + "::" + strings.ToLower(platformID)
	if v, ok := s.exact.Get(key); ok {
		return v, v != nil
	}
	if s.wasMiss(key) {
		return nil, false
	}
	row, err := s.queryExact(ctx, isrc, platformID)
	if err != nil {
		return nil, false
	}
	if row != nil {
		s.exact.Add(key, row)
	} else {
		s.noteMiss(key)
	}
	return row, row != nil
}

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
	if s.wasMiss(key) {
		return nil, false
	}

	rows, err := s.queryFTS5(ctx, tokens)
	if err != nil {
		return nil, false
	}
	if len(rows) > 0 {
		s.exist.Add(key, rows)
	} else {
		s.noteMiss(key)
	}
	return rows, len(rows) > 0
}

func buildFTSQuery(title, artist string) string {
	cleanTitle := sanitizeFTSToken(title)
	cleanArtist := sanitizeFTSToken(artist)

	if cleanTitle == "" && cleanArtist == "" {
		return ""
	}

	var clauses []string

	if cleanTitle != "" {
		clauses = append(clauses, fmt.Sprintf(`title: "%s"`, cleanTitle))
		if strings.Contains(cleanTitle, " ") {
			clauses = append(clauses, fmt.Sprintf(`title: "%s"*`, cleanTitle))
		}
	}

	if cleanTitle != "" && cleanArtist != "" {
		clauses = append(clauses, fmt.Sprintf(`(title: "%s" AND artist: "%s")`, cleanTitle, cleanArtist))
	} else if cleanArtist != "" {
		clauses = append(clauses, fmt.Sprintf(`artist: "%s"`, cleanArtist))
	}

	return strings.Join(clauses, " OR ")
}

func sanitizeFTSToken(word string) string {
	var b strings.Builder
	for _, r := range word {
		if r == '"' || r == '(' || r == ')' || r == '^' || r == '*' || r == ':' || r == '{' || r == '}' || r == '[' || r == ']' {
			continue
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

func (s *Store) queryFTS5(ctx context.Context, matchExpr string) ([]*Row, error) {
	const q = `SELECT l.id, l.filename, l.isrc, l.platform_id, l.source, l.title, l.artist, l.duration_ms, l.created_at
FROM lyrics_fts
JOIN lyrics l ON lyrics_fts.rowid = l.id
WHERE lyrics_fts MATCH ?
LIMIT 30`
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

func (s *Store) scanSingleRow(ctx context.Context, query string, args ...any) (*Row, error) {
	row := &Row{}
	var createdAt int64
	err := s.db.QueryRowContext(ctx, query, args...).Scan(
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

func (s *Store) queryExact(ctx context.Context, isrc, platformID string) (*Row, error) {
	if isrc != "" {
		const q = `SELECT ` + storeLightRowColumns + ` FROM lyrics WHERE isrc = ? LIMIT 1`
		if row, err := s.scanLightRow(ctx, q, isrc); err != nil || row != nil {
			return row, err
		}
	}
	if platformID != "" {
		const q = `SELECT ` + storeLightRowColumns + ` FROM lyrics WHERE platform_id = ? LIMIT 1`
		return s.scanLightRow(ctx, q, platformID)
	}
	return nil, nil
}

func (s *Store) GetExactUser(ctx context.Context, isrc, platformID string) (*Row, bool) {
	if isrc == "" && platformID == "" {
		return nil, false
	}
	key := "exact_user::" + strings.ToLower(isrc) + "::" + strings.ToLower(platformID)
	if v, ok := s.exact.Get(key); ok {
		return v, v != nil
	}
	if s.wasMiss(key) {
		return nil, false
	}
	row, err := s.queryExactUser(ctx, isrc, platformID)
	if err != nil {
		return nil, false
	}
	if row != nil {
		s.exact.Add(key, row)
	} else {
		s.noteMiss(key)
	}
	return row, row != nil
}

func (s *Store) queryExactUser(ctx context.Context, isrc, platformID string) (*Row, error) {
	if isrc != "" {
		const q = `SELECT ` + storeLightRowColumns + ` FROM lyrics WHERE isrc = ? AND source = 'lyricsplus' LIMIT 1`
		if row, err := s.scanLightRow(ctx, q, isrc); err != nil || row != nil {
			return row, err
		}
	}
	if platformID != "" {
		const q = `SELECT ` + storeLightRowColumns + ` FROM lyrics WHERE platform_id = ? AND source = 'lyricsplus' LIMIT 1`
		return s.scanLightRow(ctx, q, platformID)
	}
	return nil, nil
}

func (s *Store) scanLightRow(ctx context.Context, query string, args ...any) (*Row, error) {
	row := &Row{}
	var createdAt int64
	err := s.db.QueryRowContext(ctx, query, args...).Scan(
		&row.ID, &row.Filename, &row.ISRC, &row.PlatformID,
		&row.Source, &row.Title, &row.Artist, &row.DurationMS, &createdAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	row.CreatedAt = time.UnixMilli(createdAt)
	return row, nil
}

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
	if s.wasMiss(key) {
		return nil, false
	}
	rows, err := s.queryTitleArtist(ctx, title, artist)
	if err != nil {
		return nil, false
	}
	if len(rows) > 0 {
		s.exactTitle.Add(key, rows)
	} else {
		s.noteMiss(key)
	}
	return rows, len(rows) > 0
}

func (s *Store) queryTitleArtist(ctx context.Context, title, artist string) ([]*Row, error) {
	const q = `SELECT ` + storeLightRowColumns + `
FROM lyrics
WHERE title = ? COLLATE NOCASE AND artist = ? COLLATE NOCASE
LIMIT 10`
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

func (s *Store) GetContent(ctx context.Context, id int64) ([]byte, error) {
	if id <= 0 {
		return nil, nil
	}
	if v, ok := s.content.Get(id); ok && len(v) > 0 {
		return v, nil
	}
	const q = `SELECT content FROM lyrics WHERE id = ?`
	var raw []byte
	if err := s.db.QueryRowContext(ctx, q, id).Scan(&raw); err != nil {
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
	  created_at = excluded.created_at
	RETURNING id`
	var id int64
	if err := s.wdb.QueryRowContext(ctx, upsert,
		row.Filename, row.ContentJSON, row.ISRC, row.PlatformID, row.Source,
		row.Title, row.Artist, row.DurationMS, row.CreatedAt.UnixMilli(),
	).Scan(&id); err != nil {
		return err
	}
	s.invalidate(row, id)
	return nil
}

func (s *Store) SaveLyricsAsync(row *Row, timeout time.Duration) {
	if row == nil {
		return
	}
	s.writes.Submit(context.Background(), func(ctx context.Context) {
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		_ = s.SaveLyrics(ctx, row)
	})
}

func (s *Store) SaveUserLyricsAsync(q domain.SearchQuery, content []byte, timeout time.Duration) {
	buf := make([]byte, len(content))
	copy(buf, content)
	s.writes.Submit(context.Background(), func(ctx context.Context) {
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		_ = s.SaveUserLyrics(ctx, q, buf)
	})
}

func (s *Store) invalidate(row *Row, id int64) {
	loISRC := strings.ToLower(row.ISRC)
	loPlat := strings.ToLower(row.PlatformID)
	s.exact.Remove("exact::" + loISRC + "::" + loPlat)
	s.exact.Remove("exact_user::" + loISRC + "::" + loPlat)
	s.exactTitle.Remove("ta::" + strings.ToLower(row.Title) + "::" + strings.ToLower(row.Artist))
	if id > 0 {
		s.content.Remove(id)
	}
	s.miss.Purge()
}

func (s *Store) noteMiss(key string) {
	if s.miss == nil {
		return
	}
	s.miss.Add(key, struct{}{})
}

func (s *Store) wasMiss(key string) bool {
	if s.miss == nil {
		return false
	}
	_, ok := s.miss.Get(key)
	return ok
}

func (s *Store) Close() error {
	var err error
	s.closeOnce.Do(func() {
		if s.writes != nil {
			s.writes.CloseWithin(5 * time.Second)
		}
		if s.wdb != nil {
			_ = s.wdb.Close()
		}
		if s.db != nil {
			err = s.db.Close()
		}
	})
	return err
}

func (s *Store) Ping(ctx context.Context) error {
	if s.db == nil {
		return fmt.Errorf("storage: store not initialized")
	}
	return s.db.PingContext(ctx)
}

func (s *Store) SearchCatalog(ctx context.Context, query string) ([]domain.SongCatalogItem, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	clean := strings.TrimSpace(query)
	if clean == "" {
		return nil, nil
	}

	var rows []*Row
	matchExpr := buildSearchFTS5Expr(clean)
	if matchExpr != "" {
		if r, err := s.queryFTS5(ctx, matchExpr); err == nil && len(r) > 0 {
			rows = r
		}
	}

	if len(rows) == 0 {
		likePattern := "%" + clean + "%"
		const q = `SELECT id, filename, isrc, platform_id, source, title, artist, duration_ms, created_at
FROM lyrics
WHERE title LIKE ? OR artist LIKE ? OR filename LIKE ?
LIMIT 20`
		dbRows, err := s.db.QueryContext(ctx, q, likePattern, likePattern, likePattern)
		if err == nil {
			defer func() { _ = dbRows.Close() }()
			for dbRows.Next() {
				row := &Row{}
				var createdAt int64
				if err := dbRows.Scan(
					&row.ID, &row.Filename, &row.ISRC, &row.PlatformID,
					&row.Source, &row.Title, &row.Artist, &row.DurationMS, &createdAt,
				); err == nil {
					row.CreatedAt = time.UnixMilli(createdAt)
					rows = append(rows, row)
				}
			}
		}
	}

	if len(rows) == 0 {
		return nil, nil
	}

	seen := make(map[string]bool)
	var items []domain.SongCatalogItem
	for _, r := range rows {
		key := strings.ToLower(r.Title) + "::" + strings.ToLower(r.Artist)
		if seen[key] {
			continue
		}
		seen[key] = true

		var isrcPtr *string
		if r.ISRC != "" {
			isrc := r.ISRC
			isrcPtr = &isrc
		}

		sourceName := r.Source
		if sourceName == "" {
			sourceName = "Database"
		}
		sourceKey := strings.ToLower(sourceName)

		idMap := map[string]string{}
		if r.PlatformID != "" {
			idMap[sourceKey] = r.PlatformID
		}
		idMap["db"] = fmt.Sprintf("%d", r.ID)

		item := domain.SongCatalogItem{
			ID:           idMap,
			SourceID:     r.PlatformID,
			Title:        r.Title,
			Artist:       r.Artist,
			DurationMs:   int64(r.DurationMS),
			ISRC:         isrcPtr,
			Availability: []string{"Database", sourceName},
			ExternalURLs: map[string]string{},
		}
		items = append(items, item)
	}
	return items, nil
}

func buildSearchFTS5Expr(q string) string {
	tokens := strings.Fields(q)
	var clean []string
	for _, tok := range tokens {
		t := sanitizeFTSToken(tok)
		if t != "" {
			clean = append(clean, t)
		}
	}
	if len(clean) == 0 {
		return ""
	}
	var clauses []string
	phrase := strings.Join(clean, " ")
	clauses = append(clauses, fmt.Sprintf(`"%s"`, phrase))
	clauses = append(clauses, fmt.Sprintf(`"%s"*`, phrase))
	for _, t := range clean {
		clauses = append(clauses, fmt.Sprintf(`title: "%s"*`, t))
		clauses = append(clauses, fmt.Sprintf(`artist: "%s"*`, t))
	}
	return strings.Join(clauses, " OR ")
}

func (s *Store) GetMetadata(ctx context.Context, title, artist, album string, durationSec float64) (map[string]interface{}, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	rows, ok := s.GetByTitleArtist(ctx, title, artist)
	if !ok || len(rows) == 0 {
		rows, ok = s.GetByFTS5(ctx, title, artist)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	var candidates []similarity.SongCandidate
	for _, r := range rows {
		candidates = append(candidates, similarity.SongCandidate{
			Title:      r.Title,
			Artist:     r.Artist,
			DurationMs: r.DurationMS,
			ISRC:       r.ISRC,
			PlatformID: r.PlatformID,
			Data:       r,
		})
	}
	best := similarity.FindBestSongMatch(candidates, title, artist, album, durationSec, "", "")
	if best == nil {
		return nil, nil
	}
	matched := best.Candidate.Data.(*Row)
	meta := map[string]interface{}{
		"name":       matched.Title,
		"artistName": matched.Artist,
		"durationMs": matched.DurationMS,
		"isrc":       matched.ISRC,
		"id":         matched.PlatformID,
		"source":     matched.Source,
	}
	return meta, nil
}
