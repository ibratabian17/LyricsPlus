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
	"sync/atomic"
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

const ftsRepairEstimatedDuration = 2 * time.Minute

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

DROP TRIGGER IF EXISTS lyrics_fts_update;
CREATE TRIGGER IF NOT EXISTS lyrics_fts_update AFTER UPDATE OF title, artist ON lyrics
WHEN old.title IS NOT new.title OR old.artist IS NOT new.artist
BEGIN
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
	exactRows  *lru.Cache[string, []*Row]
	exactTitle *lru.Cache[string, []*Row]
	exist      *lru.Cache[string, []*Row]
	content    *lru.Cache[int64, []byte]
	miss       *expirable.LRU[string, struct{}]
	recentSave *expirable.LRU[string, struct{}]

	writes       *writeQueue
	writeCount   atomic.Int64
	ftsRepairing atomic.Bool
	closeOnce    sync.Once
	ftsDone      chan struct{}

	cbMu              sync.RWMutex
	cbOpen            bool
	cbOpenUntil       time.Time
	consecutiveErrors int
	cbThreshold       int
	cbCooldown        time.Duration
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
	db.SetConnMaxIdleTime(15 * time.Minute)

	wdb, err := sql.Open("sqlite", writeDSN)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("storage: open sqlite writer: %w", err)
	}
	wdb.SetMaxOpenConns(1)
	wdb.SetMaxIdleConns(1)
	wdb.SetConnMaxLifetime(0)

	if err := initStoreSchema(wdb); err != nil {
		_ = db.Close()
		_ = wdb.Close()
		return nil, fmt.Errorf("storage: init schema: %w", err)
	}

	size := cfg.LRUSize
	if size <= 0 {
		size = 16384
	}
	exact, _ := lru.New[string, *Row](size)
	exactRows, _ := lru.New[string, []*Row](size)
	exactTitle, _ := lru.New[string, []*Row](size)
	exist, _ := lru.New[string, []*Row](size)
	contentSize := size / 2
	if contentSize < 64 {
		contentSize = size
	}
	content, _ := lru.New[int64, []byte](contentSize)

	negSize := cfg.MissMemoSize
	if negSize <= 0 {
		negSize = size * 2
	}
	negTTL := cfg.NegativeTTL
	if negTTL <= 0 {
		negTTL = 3 * time.Minute
	}
	miss := expirable.NewLRU[string, struct{}](negSize, nil, negTTL)
	recentSave := expirable.NewLRU[string, struct{}](8192, nil, 5*time.Minute)

	cbThreshold := cfg.CircuitBreakerThreshold
	if cbThreshold <= 0 {
		cbThreshold = 8
	}
	cbCooldown := cfg.CircuitBreakerCooldown
	if cbCooldown <= 0 {
		cbCooldown = 15 * time.Second
	}

	st := &Store{
		cfg:         cfg,
		db:          db,
		wdb:         wdb,
		exact:       exact,
		exactRows:   exactRows,
		exactTitle:  exactTitle,
		exist:       exist,
		content:     content,
		miss:        miss,
		recentSave:  recentSave,
		cbThreshold: cbThreshold,
		cbCooldown:  cbCooldown,
		ftsDone:     make(chan struct{}),
	}
	st.writes = newWriteQueue(1, cfg.WriteQueueSize)

	go func() {
		defer close(st.ftsDone)
		st.BackfillFTS5()
	}()

	return st, nil
}

const (
	defaultReadConns = 16
	readCacheKB      = -16384
	writeCacheKB     = -8192
)

func buildDSN(path string, cacheKB int) string {
	return fmt.Sprintf(
		"file:%s?_pragma=busy_timeout(15000)"+
			"&_pragma=journal_mode(WAL)"+
			"&_pragma=synchronous(NORMAL)"+
			"&_pragma=cache_size(%d)"+
			"&_pragma=mmap_size(67108864)"+
			"&_pragma=journal_size_limit(67108864)"+
			"&_pragma=wal_autocheckpoint(1000)"+
			"&_pragma=temp_store(DEFAULT)"+
			"&_pragma=foreign_keys(ON)",
		path, cacheKB,
	)
}

func (s *Store) BackfillFTS5() {
	s.repairFTS5(false)
}

func (s *Store) repairFTS5(forceReset bool) {
	if s == nil || s.db == nil || s.wdb == nil {
		return
	}
	if !s.ftsRepairing.CompareAndSwap(false, true) {
		return
	}
	defer s.ftsRepairing.Store(false)

	var maxLyricsID int64
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM lyrics`).Scan(&maxLyricsID); err != nil || maxLyricsID == 0 {
		return
	}

	var maxFTSID int64
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM lyrics_fts_docsize`).Scan(&maxFTSID); err != nil {
		forceReset = true
	}

	if !forceReset && maxFTSID > 0 {
		var probeID int64
		err := s.db.QueryRow(`SELECT rowid FROM lyrics_fts WHERE lyrics_fts MATCH '"a"*' LIMIT 1`).Scan(&probeID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) && isFTSCorruptError(err) {
			forceReset = true
		}
	}

	if forceReset || maxFTSID > maxLyricsID {
		_, _ = s.wdb.Exec(`DROP TRIGGER IF EXISTS lyrics_fts_insert; DROP TRIGGER IF EXISTS lyrics_fts_delete; DROP TRIGGER IF EXISTS lyrics_fts_update; DROP TABLE IF EXISTS lyrics_fts;`)
		if _, err := s.wdb.Exec(lyricsFTS5Setup); err != nil {
			return
		}
		maxFTSID = 0
	}

	if maxFTSID >= maxLyricsID {
		return
	}

	const batchSize = int64(100000)
	for cur := maxFTSID; cur < maxLyricsID; {
		next := cur + batchSize
		if next > maxLyricsID {
			next = maxLyricsID
		}
		if _, err := s.wdb.Exec(
			`INSERT INTO lyrics_fts(rowid, title, artist) SELECT id, title, artist FROM lyrics WHERE id > ? AND id <= ?`,
			cur, next,
		); err != nil {
			_, _ = s.wdb.Exec(`INSERT INTO lyrics_fts(lyrics_fts) VALUES('rebuild')`)
			break
		}
		cur = next
	}
	if s.miss != nil {
		s.miss.Purge()
	}
}

func isFTSCorruptError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "malformed") ||
		strings.Contains(msg, "corrupt") ||
		strings.Contains(msg, "vtable") ||
		strings.Contains(msg, "missing row") ||
		strings.Contains(msg, "no such table: main.lyrics_fts") ||
		strings.Contains(msg, "no such table: lyrics_fts") ||
		strings.Contains(msg, "fts5")
}

func initStoreSchema(db *sql.DB) error {
	pragmas := []string{
		"PRAGMA journal_mode = WAL;",
		"PRAGMA synchronous = NORMAL;",
		"PRAGMA busy_timeout = 15000;",
		"PRAGMA cache_size = -16384;",
		"PRAGMA mmap_size = 67108864;",
		"PRAGMA journal_size_limit = 67108864;",
		"PRAGMA wal_autocheckpoint = 1000;",
		"PRAGMA temp_store = DEFAULT;",
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
	_, _ = db.Exec("PRAGMA wal_checkpoint(PASSIVE);")
	_, _ = db.Exec("PRAGMA analysis_limit = 1000;")
	_, _ = db.Exec("ANALYZE;")
	_, _ = db.Exec("PRAGMA optimize;")
	return nil
}

func isDBBusyError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "busy") ||
		strings.Contains(msg, "locked") ||
		strings.Contains(msg, "timeout") ||
		strings.Contains(msg, "cannot start a transaction within a transaction") ||
		strings.Contains(msg, "bad connection")
}

func (s *Store) Shed() {
	if s == nil {
		return
	}
	shedLRU(s.content)
	shedLRU(s.exist)
	shedLRU(s.exactTitle)
	shedLRU(s.exactRows)
	shedLRU(s.exact)
}

func shedLRU[K comparable, V any](c *lru.Cache[K, V]) {
	if c == nil {
		return
	}
	toRemove := c.Len() / 2
	for i := 0; i < toRemove; i++ {
		c.RemoveOldest()
	}
}

func (s *Store) IsFTSRepairing() bool {
	if s == nil {
		return false
	}
	return s.ftsRepairing.Load()
}

func (s *Store) SetFTSRepairing(v bool) {
	if s == nil {
		return
	}
	s.ftsRepairing.Store(v)
}

func (s *Store) FTSRepairEstimate() time.Duration {
	if s == nil {
		return ftsRepairEstimatedDuration
	}
	return ftsRepairEstimatedDuration
}

func (s *Store) IsResting() bool {
	if s == nil {
		return false
	}
	s.cbMu.RLock()
	if !s.cbOpen {
		s.cbMu.RUnlock()
		return false
	}
	if time.Now().After(s.cbOpenUntil) {
		s.cbMu.RUnlock()
		s.cbMu.Lock()
		if s.cbOpen && time.Now().After(s.cbOpenUntil) {
			s.cbOpen = false
			s.consecutiveErrors = 0
		}
		s.cbMu.Unlock()
		return false
	}
	s.cbMu.RUnlock()
	return true
}

func (s *Store) CooldownRemaining() time.Duration {
	if s == nil {
		return 0
	}
	s.cbMu.RLock()
	defer s.cbMu.RUnlock()
	if !s.cbOpen {
		return 0
	}
	rem := time.Until(s.cbOpenUntil)
	if rem < 0 {
		return 0
	}
	return rem
}

func (s *Store) TripCooldown(d time.Duration) {
	if s == nil {
		return
	}
	s.cbMu.Lock()
	defer s.cbMu.Unlock()
	s.cbOpen = true
	if d <= 0 {
		d = s.cbCooldown
	}
	s.cbOpenUntil = time.Now().Add(d)
}

func (s *Store) ResetCircuitBreaker() {
	if s == nil {
		return
	}
	s.cbMu.Lock()
	defer s.cbMu.Unlock()
	s.cbOpen = false
	s.consecutiveErrors = 0
}

func (s *Store) RecordDBBusy(err error) {
	if s == nil || !isDBBusyError(err) {
		return
	}
	s.cbMu.Lock()
	defer s.cbMu.Unlock()
	s.consecutiveErrors++
	if s.consecutiveErrors >= s.cbThreshold {
		s.cbOpen = true
		s.cbOpenUntil = time.Now().Add(s.cbCooldown)
	}
}

func (s *Store) recordQueryErr(ctx context.Context, err error) {
	if err == nil {
		return
	}
	if ctx != nil && ctx.Err() != nil {
		return
	}
	s.RecordDBBusy(err)
}

func (s *Store) RecordDBSuccess() {
	if s == nil {
		return
	}
	s.cbMu.RLock()
	if s.consecutiveErrors == 0 {
		s.cbMu.RUnlock()
		return
	}
	s.cbMu.RUnlock()
	s.cbMu.Lock()
	s.consecutiveErrors = 0
	s.cbMu.Unlock()
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
	if s.IsResting() {
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

func (s *Store) GetExactRows(ctx context.Context, isrc, platformID string) ([]*Row, bool) {
	isrc = strings.TrimSpace(isrc)
	platformID = strings.TrimSpace(platformID)
	if isrc == "" && platformID == "" {
		return nil, false
	}
	key := "exact_rows::" + strings.ToLower(isrc) + "::" + strings.ToLower(platformID)
	if s.exactRows != nil {
		if v, ok := s.exactRows.Get(key); ok {
			return v, len(v) > 0
		}
	}
	if s.wasMiss(key) {
		return nil, false
	}
	if s.IsResting() {
		return nil, false
	}
	rows, err := s.queryExactRows(ctx, isrc, platformID)
	if err != nil {
		return nil, false
	}
	if len(rows) > 0 {
		if s.exactRows != nil {
			s.exactRows.Add(key, rows)
		}
	} else {
		s.noteMiss(key)
	}
	return rows, len(rows) > 0
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
	if s.wasMiss(key) {
		return nil, false
	}
	if s.IsResting() {
		return nil, false
	}
	rows, err := s.queryExisting(ctx, clean)
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
	if s.IsResting() {
		return nil, false
	}

	rows, err := s.queryFTS5Tiered(ctx, title, artist, tokens)
	if len(rows) == 0 && (err != nil || s.ftsRepairing.Load()) {
		if fbRows, fbErr := s.queryTitlePrefixFallback(ctx, title); fbErr == nil && len(fbRows) > 0 {
			s.exist.Add(key, fbRows)
			return fbRows, true
		}
	}
	if err != nil {
		return nil, false
	}
	if len(rows) > 0 {
		s.exist.Add(key, rows)
	} else if !s.ftsRepairing.Load() {
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

	if cleanTitle != "" && cleanArtist != "" {
		clauses = append(clauses, fmt.Sprintf(`(title: "%s" AND artist: "%s")`, cleanTitle, cleanArtist))
		if strings.Contains(cleanTitle, " ") {
			clauses = append(clauses, fmt.Sprintf(`(title: "%s"* AND artist: "%s"*)`, cleanTitle, cleanArtist))
		}
		artistWords := strings.Fields(cleanArtist)
		if len(artistWords) > 0 && len(artistWords[0]) >= 2 {
			clauses = append(clauses, fmt.Sprintf(`(title: "%s" AND artist: "%s"*)`, cleanTitle, artistWords[0]))
		}
		if len(cleanTitle) >= 5 || strings.Contains(cleanTitle, " ") {
			clauses = append(clauses, fmt.Sprintf(`title: "%s"`, cleanTitle))
		}
	} else if cleanTitle != "" {
		clauses = append(clauses, fmt.Sprintf(`title: "%s"`, cleanTitle))
		if strings.Contains(cleanTitle, " ") {
			clauses = append(clauses, fmt.Sprintf(`title: "%s"*`, cleanTitle))
		}
	} else if cleanArtist != "" {
		clauses = append(clauses, fmt.Sprintf(`artist: "%s"`, cleanArtist))
	}

	return strings.Join(clauses, " OR ")
}

func buildFTSTiers(title, artist string) []string {
	cleanTitle := sanitizeFTSToken(title)
	cleanArtist := sanitizeFTSToken(artist)
	if cleanTitle == "" && cleanArtist == "" {
		return nil
	}

	var tiers []string
	addTier := func(expr string) {
		if expr == "" {
			return
		}
		for _, existing := range tiers {
			if existing == expr {
				return
			}
		}
		tiers = append(tiers, expr)
	}

	if cleanTitle != "" && cleanArtist != "" {
		addTier(fmt.Sprintf(`(title: "%s" AND artist: "%s")`, cleanTitle, cleanArtist))

		baseTitle := sanitizeFTSToken(similarity.AnalyzeTitle(title).BaseTitle)
		if baseTitle != "" && !strings.EqualFold(baseTitle, cleanTitle) {
			addTier(fmt.Sprintf(`(title: "%s" AND artist: "%s")`, baseTitle, cleanArtist))
		}

		artistWords := strings.Fields(cleanArtist)
		if len(artistWords) > 0 && len(artistWords[0]) >= 2 {
			addTier(fmt.Sprintf(`(title: "%s" AND artist: "%s"*)`, cleanTitle, artistWords[0]))
			if baseTitle != "" && !strings.EqualFold(baseTitle, cleanTitle) {
				addTier(fmt.Sprintf(`(title: "%s" AND artist: "%s"*)`, baseTitle, artistWords[0]))
			}
		}

		if len(cleanTitle) >= 5 || strings.Contains(cleanTitle, " ") {
			addTier(fmt.Sprintf(`title: "%s"`, cleanTitle))
		}
	} else if cleanTitle != "" {
		addTier(fmt.Sprintf(`title: "%s"`, cleanTitle))
		if strings.Contains(cleanTitle, " ") {
			addTier(fmt.Sprintf(`title: "%s"*`, cleanTitle))
		}
	} else if cleanArtist != "" {
		addTier(fmt.Sprintf(`artist: "%s"`, cleanArtist))
	}

	return tiers
}

func sanitizeFTSToken(word string) string {
	var b strings.Builder
	b.Grow(len(word))
	for _, r := range word {
		if r == '"' || r == '(' || r == ')' || r == '^' || r == '*' || r == ':' || r == '{' || r == '}' || r == '[' || r == ']' {
			continue
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

func (s *Store) queryFTS5Tiered(ctx context.Context, title, artist, fallbackExpr string) ([]*Row, error) {
	tiers := buildFTSTiers(title, artist)
	if len(tiers) == 0 && fallbackExpr != "" {
		tiers = []string{fallbackExpr}
	}
	var lastErr error
	for _, expr := range tiers {
		if ctx != nil && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		rows, err := s.queryFTS5(ctx, expr)
		if err != nil {
			lastErr = err
			continue
		}
		if len(rows) > 0 {
			return rows, nil
		}
	}
	return nil, lastErr
}

func (s *Store) queryFTS5(ctx context.Context, matchExpr string) ([]*Row, error) {
	const q = `SELECT l.id, l.filename, l.isrc, l.platform_id, l.source, l.title, l.artist, l.duration_ms, l.created_at
FROM lyrics_fts
JOIN lyrics l ON lyrics_fts.rowid = l.id
WHERE lyrics_fts MATCH ?
LIMIT 30`
	rows, err := s.db.QueryContext(ctx, q, matchExpr)
	if err != nil {
		if isFTSCorruptError(err) {
			go s.repairFTS5(true)
		}
		s.recordQueryErr(ctx, err)
		return nil, err
	}
	s.RecordDBSuccess()
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

func (s *Store) queryTitlePrefixFallback(ctx context.Context, title string) ([]*Row, error) {
	base := strings.TrimSpace(similarity.AnalyzeTitle(title).BaseTitle)
	if base == "" {
		base = strings.TrimSpace(title)
	}
	if base == "" {
		return nil, nil
	}
	const q = `SELECT ` + storeLightRowColumns + ` FROM lyrics WHERE title >= ? COLLATE NOCASE AND title < ? COLLATE NOCASE LIMIT 30`
	return s.scanLightRows(ctx, q, base, base+"￿")
}

func (s *Store) scanSingleRow(ctx context.Context, query string, args ...any) (*Row, error) {
	row := &Row{}
	var createdAt int64
	err := s.db.QueryRowContext(ctx, query, args...).Scan(
		&row.ID, &row.Filename, &row.ContentJSON, &row.ISRC, &row.PlatformID,
		&row.Source, &row.Title, &row.Artist, &row.DurationMS, &createdAt,
	)
	if err == sql.ErrNoRows {
		s.RecordDBSuccess()
		return nil, nil
	}
	if err != nil {
		s.recordQueryErr(ctx, err)
		return nil, err
	}
	s.RecordDBSuccess()
	row.CreatedAt = time.UnixMilli(createdAt)
	row.ContentJSON = DecompressContent(row.ContentJSON)
	return row, nil
}

func (s *Store) queryExact(ctx context.Context, isrc, platformID string) (*Row, error) {
	if isrc != "" {
		const q = `SELECT ` + storeLightRowColumns + ` FROM lyrics WHERE isrc = ? AND isrc != '' LIMIT 1`
		if row, err := s.scanLightRow(ctx, q, isrc); err != nil || row != nil {
			return row, err
		}
		if upper := strings.ToUpper(strings.TrimSpace(isrc)); upper != isrc && upper != "" {
			if row, err := s.scanLightRow(ctx, q, upper); err != nil || row != nil {
				return row, err
			}
		}
	}
	if platformID != "" {
		const q = `SELECT ` + storeLightRowColumns + ` FROM lyrics WHERE platform_id = ? AND platform_id != '' LIMIT 1`
		return s.scanLightRow(ctx, q, platformID)
	}
	return nil, nil
}

func (s *Store) queryExactRows(ctx context.Context, isrc, platformID string) ([]*Row, error) {
	var out []*Row
	seen := make(map[int64]struct{}, 8)
	appendRows := func(rows []*Row) {
		for _, r := range rows {
			if r == nil {
				continue
			}
			if _, ok := seen[r.ID]; ok {
				continue
			}
			seen[r.ID] = struct{}{}
			out = append(out, r)
		}
	}

	if isrc != "" {
		const q = `SELECT ` + storeLightRowColumns + ` FROM lyrics WHERE isrc = ? AND isrc != '' LIMIT 10`
		rows, err := s.scanLightRows(ctx, q, isrc)
		if err != nil {
			return nil, err
		}
		appendRows(rows)
		if len(out) == 0 {
			if upper := strings.ToUpper(strings.TrimSpace(isrc)); upper != isrc && upper != "" {
				rows, err := s.scanLightRows(ctx, q, upper)
				if err != nil {
					return nil, err
				}
				appendRows(rows)
			}
		}
	}
	if platformID != "" && len(out) == 0 {
		const q = `SELECT ` + storeLightRowColumns + ` FROM lyrics WHERE platform_id = ? AND platform_id != '' LIMIT 10`
		rows, err := s.scanLightRows(ctx, q, platformID)
		if err != nil {
			return nil, err
		}
		appendRows(rows)
	}
	return out, nil
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
	if s.IsResting() {
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
		const q = `SELECT ` + storeLightRowColumns + ` FROM lyrics WHERE isrc = ? AND isrc != '' AND source = 'lyricsplus' LIMIT 1`
		if row, err := s.scanLightRow(ctx, q, isrc); err != nil || row != nil {
			return row, err
		}
		if upper := strings.ToUpper(strings.TrimSpace(isrc)); upper != isrc && upper != "" {
			if row, err := s.scanLightRow(ctx, q, upper); err != nil || row != nil {
				return row, err
			}
		}
	}
	if platformID != "" {
		const q = `SELECT ` + storeLightRowColumns + ` FROM lyrics WHERE platform_id = ? AND platform_id != '' AND source = 'lyricsplus' LIMIT 1`
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
		s.RecordDBSuccess()
		return nil, nil
	}
	if err != nil {
		s.recordQueryErr(ctx, err)
		return nil, err
	}
	s.RecordDBSuccess()
	row.CreatedAt = time.UnixMilli(createdAt)
	return row, nil
}

func (s *Store) scanLightRows(ctx context.Context, query string, args ...any) ([]*Row, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		s.recordQueryErr(ctx, err)
		return nil, err
	}
	s.RecordDBSuccess()
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

func primaryArtistName(artist string) string {
	for _, sep := range []string{",", " & ", " feat.", " feat ", " ft.", " ft ", " featuring ", " with ", " x ", " X "} {
		if idx := strings.Index(strings.ToLower(artist), sep); idx > 0 {
			return strings.TrimSpace(artist[:idx])
		}
	}
	return strings.TrimSpace(artist)
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
	if s.IsResting() {
		return nil, false
	}
	rows, err := s.queryTitleArtist(ctx, title, artist)
	if err != nil {
		return nil, false
	}
	if len(rows) == 0 {
		baseTitle := strings.TrimSpace(similarity.AnalyzeTitle(title).BaseTitle)
		primArtist := primaryArtistName(artist)
		if baseTitle != "" && (!strings.EqualFold(baseTitle, title) || !strings.EqualFold(primArtist, artist)) {
			if baseRows, baseErr := s.queryTitleArtist(ctx, baseTitle, artist); baseErr == nil && len(baseRows) > 0 {
				rows = baseRows
			} else if primArtist != "" && !strings.EqualFold(primArtist, artist) {
				if primRows, primErr := s.queryTitleArtist(ctx, title, primArtist); primErr == nil && len(primRows) > 0 {
					rows = primRows
				} else if !strings.EqualFold(baseTitle, title) {
					if bpRows, bpErr := s.queryTitleArtist(ctx, baseTitle, primArtist); bpErr == nil && len(bpRows) > 0 {
						rows = bpRows
					}
				}
			}
		}
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
LIMIT 15`
	return s.scanLightRows(ctx, q, title, artist)
}

func (s *Store) GetContent(ctx context.Context, id int64) ([]byte, error) {
	if id <= 0 {
		return nil, nil
	}
	if v, ok := s.content.Get(id); ok && len(v) > 0 {
		return v, nil
	}
	if s.IsResting() {
		return nil, fmt.Errorf("storage: database resting")
	}
	const q = `SELECT content FROM lyrics WHERE id = ?`
	var raw []byte
	if err := s.db.QueryRowContext(ctx, q, id).Scan(&raw); err != nil {
		s.recordQueryErr(ctx, err)
		return nil, err
	}
	s.RecordDBSuccess()
	content := DecompressContent(raw)
	if len(content) > 0 {
		s.content.Add(id, content)
	}
	return content, nil
}

func (s *Store) queryExisting(ctx context.Context, keywords []string) ([]*Row, error) {
	var ftsTerms []string
	for _, k := range keywords {
		if tok := sanitizeFTSToken(k); tok != "" {
			ftsTerms = append(ftsTerms, fmt.Sprintf(`"%s"*`, tok))
		}
	}
	if len(ftsTerms) > 0 {
		if ftsRows, err := s.queryFTS5(ctx, strings.Join(ftsTerms, " AND ")); err == nil && len(ftsRows) > 0 {
			return ftsRows, nil
		}
	}

	conds := make([]string, 0, len(keywords))
	args := make([]interface{}, 0, len(keywords)*2)
	for _, k := range keywords {
		pat := "%" + k + "%"
		conds = append(conds, "(title LIKE ? OR artist LIKE ?)")
		args = append(args, pat, pat)
	}
	q := `SELECT ` + storeLightRowColumns + ` FROM lyrics WHERE ` + strings.Join(conds, " AND ") +
		` LIMIT 100`
	return s.scanLightRows(ctx, q, args...)
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
	if s.IsResting() {
		return fmt.Errorf("storage: database resting")
	}
	if row == nil {
		return nil
	}
	if row.Filename == "" {
		row.Filename = CanonicalFilename(row.Artist, row.Title, "", row.DurationMS, row.ISRC, row.PlatformID, "json")
	}
	if row.CreatedAt.IsZero() {
		row.CreatedAt = time.Now()
	}
	compressed := CompressContent(row.ContentJSON)
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
		row.Filename, compressed, row.ISRC, row.PlatformID, row.Source,
		row.Title, row.Artist, row.DurationMS, row.CreatedAt.UnixMilli(),
	).Scan(&id); err != nil {
		s.recordQueryErr(ctx, err)
		return err
	}
	row.ID = id
	s.RecordDBSuccess()
	s.invalidate(row, id)
	if len(row.ContentJSON) > 0 && id > 0 {
		s.content.Add(id, row.ContentJSON)
	}
	if s.writeCount.Add(1)%250 == 0 {
		_, _ = s.wdb.ExecContext(ctx, "PRAGMA wal_checkpoint(PASSIVE);")
	}
	return nil
}

// SaveLyricsBatch inserts or updates multiple lyric rows inside a single database transaction.
func (s *Store) SaveLyricsBatch(ctx context.Context, rows []*Row) (int64, error) {
	if s.IsResting() {
		return 0, fmt.Errorf("storage: database resting")
	}
	if len(rows) == 0 {
		return 0, nil
	}

	tx, err := s.wdb.BeginTx(ctx, nil)
	if err != nil {
		s.recordQueryErr(ctx, err)
		return 0, fmt.Errorf("begin batch tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

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

	stmt, err := tx.PrepareContext(ctx, upsert)
	if err != nil {
		s.recordQueryErr(ctx, err)
		return 0, fmt.Errorf("prepare batch stmt: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	var successCount int64
	for _, row := range rows {
		if row == nil {
			continue
		}
		if row.Filename == "" {
			row.Filename = CanonicalFilename(row.Artist, row.Title, "", row.DurationMS, row.ISRC, row.PlatformID, "json")
		}
		if row.CreatedAt.IsZero() {
			row.CreatedAt = time.Now()
		}
		if row.Source == "" {
			row.Source = "lyricsplus"
		}
		compressed := CompressContent(row.ContentJSON)

		var id int64
		if err := stmt.QueryRowContext(ctx,
			row.Filename, compressed, row.ISRC, row.PlatformID, row.Source,
			row.Title, row.Artist, row.DurationMS, row.CreatedAt.UnixMilli(),
		).Scan(&id); err != nil {
			s.recordQueryErr(ctx, err)
			return successCount, fmt.Errorf("batch insert row %s: %w", row.Filename, err)
		}
		row.ID = id
		s.invalidate(row, id)
		if len(row.ContentJSON) > 0 && id > 0 {
			s.content.Add(id, row.ContentJSON)
		}
		successCount++
	}

	if err := tx.Commit(); err != nil {
		s.recordQueryErr(ctx, err)
		return 0, fmt.Errorf("commit batch tx: %w", err)
	}

	s.RecordDBSuccess()
	if s.writeCount.Add(successCount)%250 < successCount {
		_, _ = s.wdb.ExecContext(ctx, "PRAGMA wal_checkpoint(PASSIVE);")
	}

	return successCount, nil
}

// MarkRecentSave checks whether (source, filename) was recently queued for persistence,
// and if not, marks it as queued. This allows callers to skip expensive payload marshaling
// or validation before submitting to the async write queue.
func (s *Store) MarkRecentSave(source, filename string) bool {
	if s == nil || s.recentSave == nil {
		return true
	}
	dedupKey := strings.ToLower(strings.TrimSpace(source)) + "::" + filename
	if _, exists := s.recentSave.Get(dedupKey); exists {
		return false
	}
	s.recentSave.Add(dedupKey, struct{}{})
	return true
}

func (s *Store) SaveLyricsAsync(row *Row, timeout time.Duration) {
	if row == nil || s.IsResting() {
		return
	}
	if row.Filename == "" {
		row.Filename = CanonicalFilename(row.Artist, row.Title, "", row.DurationMS, row.ISRC, row.PlatformID, "json")
	}
	if s.recentSave != nil {
		dedupKey := strings.ToLower(row.Source) + "::" + row.Filename
		if _, exists := s.recentSave.Get(dedupKey); exists {
			return
		}
		s.recentSave.Add(dedupKey, struct{}{})
	}
	s.submitSaveLyrics(row, timeout)
}

func (s *Store) SubmitLyricsPreDeduped(row *Row, timeout time.Duration) {
	if row == nil || s.IsResting() {
		return
	}
	if row.Filename == "" {
		row.Filename = CanonicalFilename(row.Artist, row.Title, "", row.DurationMS, row.ISRC, row.PlatformID, "json")
	}
	s.submitSaveLyrics(row, timeout)
}

func (s *Store) submitSaveLyrics(row *Row, timeout time.Duration) {
	s.writes.Submit(context.Background(), func(ctx context.Context) {
		if s.IsResting() {
			return
		}
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		_ = s.SaveLyrics(ctx, row)
	})
}

func (s *Store) SaveUserLyricsAsync(q domain.SearchQuery, content []byte, timeout time.Duration) {
	if s.IsResting() {
		return
	}
	buf := make([]byte, len(content))
	copy(buf, content)
	s.writes.Submit(context.Background(), func(ctx context.Context) {
		if s.IsResting() {
			return
		}
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		_ = s.SaveUserLyrics(ctx, q, buf)
	})
}

func (s *Store) invalidate(row *Row, id int64) {
	loISRC := strings.ToLower(row.ISRC)
	loPlat := strings.ToLower(row.PlatformID)
	loTitle := strings.ToLower(strings.TrimSpace(row.Title))
	loArtist := strings.ToLower(strings.TrimSpace(row.Artist))

	exactKey := "exact::" + loISRC + "::" + loPlat
	exactRowsKey := "exact_rows::" + loISRC + "::" + loPlat
	exactUserKey := "exact_user::" + loISRC + "::" + loPlat
	taKey := "ta::" + loTitle + "::" + loArtist

	s.exact.Remove(exactKey)
	s.exact.Remove(exactUserKey)
	s.exact.Remove("exact::" + loISRC + "::")
	s.exact.Remove("exact::::" + loPlat)
	s.exact.Remove("exact_user::" + loISRC + "::")
	s.exact.Remove("exact_user::::" + loPlat)
	if s.exactRows != nil {
		s.exactRows.Remove(exactRowsKey)
		s.exactRows.Remove("exact_rows::" + loISRC + "::")
		s.exactRows.Remove("exact_rows::::" + loPlat)
	}
	s.exactTitle.Remove(taKey)
	if id > 0 {
		s.content.Remove(id)
	}
	if tokens := buildFTSQuery(row.Title, row.Artist); tokens != "" {
		ftsKey := "fts5::" + tokens
		s.exist.Remove(ftsKey)
		if s.miss != nil {
			s.miss.Remove(ftsKey)
		}
	}
	if s.miss != nil {
		s.miss.Remove(exactKey)
		s.miss.Remove(exactRowsKey)
		s.miss.Remove(exactUserKey)
		s.miss.Remove("exact::" + loISRC + "::")
		s.miss.Remove("exact::::" + loPlat)
		s.miss.Remove("exact_rows::" + loISRC + "::")
		s.miss.Remove("exact_rows::::" + loPlat)
		s.miss.Remove("exact_user::" + loISRC + "::")
		s.miss.Remove("exact_user::::" + loPlat)
		s.miss.Remove(taKey)
	}
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
		if s.ftsDone != nil {
			<-s.ftsDone
		}
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

// Reachable checks that the database answers, independent of the circuit
// breaker. Health probes use this so an open breaker (a short back-off after
// real lock contention) is not reported as the database being down.
func (s *Store) Reachable(ctx context.Context) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("storage: store not initialized")
	}
	return s.db.PingContext(ctx)
}

func (s *Store) Ping(ctx context.Context) error {
	if s.db == nil {
		return fmt.Errorf("storage: store not initialized")
	}
	if s.IsResting() {
		return fmt.Errorf("storage: database resting (cooldown remaining: %s)", s.CooldownRemaining().Round(time.Second))
	}
	err := s.db.PingContext(ctx)
	if err != nil {
		s.recordQueryErr(ctx, err)
		return err
	}
	s.RecordDBSuccess()
	return nil
}

func (s *Store) SearchCatalog(ctx context.Context, query string) ([]domain.SongCatalogItem, error) {
	if s == nil || s.db == nil || s.IsResting() {
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
		prefixPattern := clean + "%"
		const q = `SELECT id, filename, isrc, platform_id, source, title, artist, duration_ms, created_at
FROM lyrics
WHERE title >= ? COLLATE NOCASE AND title LIKE ? COLLATE NOCASE
LIMIT 20`
		if r, err := s.scanLightRows(ctx, q, clean, prefixPattern); err == nil && len(r) > 0 {
			rows = r
		}
	}

	if len(rows) == 0 {
		return nil, nil
	}

	seen := make(map[string]bool, len(rows))
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
	if len(clean) == 1 {
		return fmt.Sprintf(`title: "%s"* OR artist: "%s"*`, clean[0], clean[0])
	}
	phrase := strings.Join(clean, " ")
	var andTerms []string
	for _, t := range clean {
		andTerms = append(andTerms, fmt.Sprintf(`"%s"*`, t))
	}
	return fmt.Sprintf(`"%s" OR (%s)`, phrase, strings.Join(andTerms, " AND "))
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
	candidates := make([]similarity.SongCandidate, len(rows))
	for i, r := range rows {
		candidates[i] = similarity.SongCandidate{
			Title:      r.Title,
			Artist:     r.Artist,
			DurationMs: r.DurationMS,
			ISRC:       r.ISRC,
			PlatformID: r.PlatformID,
			Data:       r,
		}
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

func (s *Store) GetRowForSources(ctx context.Context, q domain.SearchQuery, targetSources ...string) (*Row, []byte, error) {
	if s == nil {
		return nil, nil, nil
	}
	matchSource := func(src string) bool {
		if len(targetSources) == 0 {
			return true
		}
		for _, ts := range targetSources {
			if strings.EqualFold(src, ts) {
				return true
			}
			if strings.EqualFold(ts, "apple") && (strings.EqualFold(src, "apple music") || strings.EqualFold(src, "apple")) {
				return true
			}
			if strings.EqualFold(ts, "qq") && (strings.EqualFold(src, "qq music") || strings.EqualFold(src, "qq") || strings.EqualFold(src, "qqmusic")) {
				return true
			}
			if strings.EqualFold(ts, "musixmatch") && (strings.EqualFold(src, "musixmatch") || strings.EqualFold(src, "musixmatch-word")) {
				return true
			}
		}
		return false
	}

	var candidateRows []*Row
	if q.ISRC != "" || q.PlatformID != "" {
		if rows, ok := s.GetExactRows(ctx, q.ISRC, q.PlatformID); ok && len(rows) > 0 {
			for _, r := range rows {
				if matchSource(r.Source) {
					candidateRows = append(candidateRows, r)
				}
			}
		}
	}

	if q.Title != "" && q.Artist != "" {
		if rows, ok := s.GetByTitleArtist(ctx, q.Title, q.Artist); ok && len(rows) > 0 {
			for _, r := range rows {
				if matchSource(r.Source) {
					candidateRows = append(candidateRows, r)
				}
			}
		}
	}

	if len(candidateRows) == 0 && (q.Title != "" || q.Artist != "") {
		if rows, ok := s.GetByFTS5(ctx, q.Title, q.Artist); ok && len(rows) > 0 {
			for _, r := range rows {
				if matchSource(r.Source) {
					candidateRows = append(candidateRows, r)
				}
			}
		}
	}

	if len(candidateRows) == 0 {
		return nil, nil, nil
	}

	var bestRow *Row
	if len(candidateRows) == 1 {
		bestRow = candidateRows[0]
	} else {
		candidates := make([]similarity.SongCandidate, len(candidateRows))
		for i, r := range candidateRows {
			candidates[i] = similarity.SongCandidate{
				Title:      r.Title,
				Artist:     r.Artist,
				DurationMs: r.DurationMS,
				ISRC:       r.ISRC,
				PlatformID: r.PlatformID,
				Data:       r,
			}
		}
		durationSec := float64(q.Duration) / 1000.0
		best := similarity.FindBestSongMatch(candidates, q.Title, q.Artist, q.Album, durationSec, q.ISRC, q.PlatformID)
		if best != nil {
			bestRow = best.Candidate.Data.(*Row)
		} else {
			bestRow = candidateRows[0]
		}
	}

	if bestRow == nil {
		return nil, nil, nil
	}

	content := bestRow.ContentJSON
	if len(content) == 0 {
		c, err := s.GetContent(ctx, bestRow.ID)
		if err == nil {
			content = c
		}
	}

	return bestRow, content, nil
}
