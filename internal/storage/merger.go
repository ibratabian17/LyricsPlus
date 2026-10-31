package storage

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type MergeStrategy string

const (
	StrategyQuality    MergeStrategy = "quality"     // Syllable/Word > Line > Plain > Empty; newer timestamp breaks ties
	StrategyNewer      MergeStrategy = "newer"       // Newer created_at wins
	StrategyInsertOnly MergeStrategy = "insert-only" // Only insert missing songs, never overwrite
	StrategyForce      MergeStrategy = "force"       // Always overwrite target
)

const (
	RankNone     = 0
	RankEmpty    = 1
	RankPlain    = 2
	RankLine     = 3
	RankSyllable = 4
)

func QualityRankName(rank int) string {
	switch rank {
	case RankSyllable:
		return "Syllable"
	case RankLine:
		return "Line"
	case RankPlain:
		return "Plain"
	case RankEmpty:
		return "Empty"
	default:
		return "None"
	}
}

type MergeConfig struct {
	SourcePaths []string
	TargetDB    string
	Strategy    MergeStrategy
	BatchSize   int
	RebuildFTS  bool
	Vacuum      bool
	LogFunc     func(format string, args ...any)
}

type MergeStats struct {
	TotalSources  int
	TotalExamined int64
	Inserted      int64
	Upgraded      int64
	SkippedSame   int64
	SkippedLower  int64
	SkippedExists int64
	Errors        int64
	Duration      time.Duration
}

type DBStats struct {
	TotalRows    int64
	Sources      map[string]int64
	SyncTypes    map[string]int64
	FileSize     int64
	WALSize      int64
	DatabasePath string
}

const lyricsBaseTableOnlySQL = `
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
CREATE VIRTUAL TABLE IF NOT EXISTS lyrics_fts USING fts5(
  title,
  artist,
  content='lyrics',
  content_rowid='id',
  tokenize='unicode61 remove_diacritics 1'
);
`

const lyricsDropSecondaryIndexesSQL = `
DROP TRIGGER IF EXISTS lyrics_fts_insert;
DROP INDEX IF EXISTS idx_lyrics_filename;
DROP INDEX IF EXISTS idx_lyrics_isrc;
DROP INDEX IF EXISTS idx_lyrics_platform;
DROP INDEX IF EXISTS idx_lyrics_isrc_source;
DROP INDEX IF EXISTS idx_lyrics_platform_source;
DROP INDEX IF EXISTS idx_lyrics_title_artist;
DROP INDEX IF EXISTS idx_lyrics_ta_covering;
`

var (
	bytesTimingWord     = []byte(`itunes:timing="Word"`)
	bytesTimingSyllable = []byte(`itunes:timing="Syllable"`)
	bytesTimingLine     = []byte(`itunes:timing="Line"`)
	bytesSpanOpen       = []byte("<span")
	bytesBeginAttr      = []byte("begin=")
	bytesPOpen          = []byte("<p")
	bytesLyricsEmpty1   = []byte(`"lyrics":[]`)
	bytesLyricsEmpty2   = []byte(`"lyrics": []`)
	bytesLyricsNull1    = []byte(`"lyrics":null`)
	bytesLyricsNull2    = []byte(`"lyrics": null`)
	bytesKpoeTools      = []byte(`"KpoeTools"`)
	bytesTypeKey        = []byte(`"type"`)
	bytesSyllabusNonEmp = []byte(`"syllabus":[{`)
	bytesRichsyncBody   = []byte(`"richsync_body":"[`)
	bytesRichsyncEmpty  = []byte(`"richsync_body":"[]"`)
	bytesSubtitleBody   = []byte(`"subtitle_body":"`)
	bytesSubtitleEmpty  = []byte(`"subtitle_body":""`)
)

// DetectLyricsQuality inspects the compressed or raw content blob and determines its sync quality rank.
// It uses a zero-allocation 512-byte header inspection fast path that avoids full decompression and
// JSON unmarshaling for >99% of TTML and normalized JSON payloads.
func DetectLyricsQuality(contentBlob []byte) (int, string) {
	if len(contentBlob) == 0 {
		return RankNone, "None"
	}

	var stackBuf [512]byte
	head, complete := DecompressContentPrefix(contentBlob, stackBuf[:])
	if len(head) == 0 {
		return RankNone, "None"
	}

	trimmedHead := bytes.TrimLeft(head, " \t\r\n")
	if len(trimmedHead) == 0 {
		if complete {
			return RankNone, "None"
		}
	} else if trimmedHead[0] == '<' {
		if bytes.Contains(trimmedHead, bytesTimingWord) ||
			bytes.Contains(trimmedHead, bytesTimingSyllable) ||
			(bytes.Contains(trimmedHead, bytesSpanOpen) && bytes.Contains(trimmedHead, bytesBeginAttr)) {
			return RankSyllable, "Syllable"
		}
		if bytes.Contains(trimmedHead, bytesTimingLine) {
			return RankLine, "Line"
		}
		if complete {
			if bytes.Contains(trimmedHead, bytesBeginAttr) || bytes.Contains(trimmedHead, bytesPOpen) {
				return RankLine, "Line"
			}
			return RankPlain, "Plain"
		}
		raw := DecompressContent(contentBlob)
		if bytes.Contains(raw, bytesSpanOpen) && bytes.Contains(raw, bytesBeginAttr) {
			return RankSyllable, "Syllable"
		}
		if bytes.Contains(raw, bytesBeginAttr) || bytes.Contains(raw, bytesPOpen) {
			return RankLine, "Line"
		}
		return RankPlain, "Plain"
	} else if trimmedHead[0] == '{' {
		if bytes.Contains(trimmedHead, bytesLyricsEmpty1) ||
			bytes.Contains(trimmedHead, bytesLyricsEmpty2) ||
			bytes.Contains(trimmedHead, bytesLyricsNull1) ||
			bytes.Contains(trimmedHead, bytesLyricsNull2) {
			return RankEmpty, "Empty"
		}
		if rank := detectJSONHeaderType(trimmedHead); rank == RankSyllable {
			return RankSyllable, "Syllable"
		} else if rank == RankLine && (complete || bytes.Contains(trimmedHead, bytesKpoeTools) || bytes.Contains(trimmedHead, bytesSyllabusNonEmp)) {
			if bytes.Contains(trimmedHead, bytesSyllabusNonEmp) {
				return RankSyllable, "Syllable"
			}
			return RankLine, "Line"
		}
	}

	raw := DecompressContent(contentBlob)
	if len(raw) == 0 {
		return RankNone, "None"
	}

	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return RankNone, "None"
	}
	if trimmed[0] == '<' {
		if bytes.Contains(trimmed, bytesSpanOpen) && bytes.Contains(trimmed, bytesBeginAttr) {
			return RankSyllable, "Syllable"
		}
		if bytes.Contains(trimmed, bytesBeginAttr) || bytes.Contains(trimmed, bytesPOpen) {
			return RankLine, "Line"
		}
		return RankPlain, "Plain"
	}

	var p struct {
		Type   string `json:"type"`
		Lyrics []struct {
			Time     int    `json:"time"`
			Duration int    `json:"duration"`
			Text     string `json:"text"`
			Syllabus []any  `json:"syllabus"`
		} `json:"lyrics"`
	}
	if err := json.Unmarshal(trimmed, &p); err != nil {
		if bytes.Contains(trimmed, bytesRichsyncBody) && !bytes.Contains(trimmed, bytesRichsyncEmpty) {
			return RankSyllable, "Syllable"
		}
		if bytes.Contains(trimmed, bytesSubtitleBody) && !bytes.Contains(trimmed, bytesSubtitleEmpty) {
			return RankLine, "Line"
		}
		return RankNone, "Error"
	}
	if len(p.Lyrics) == 0 {
		return RankEmpty, "Empty"
	}

	t := strings.ToLower(strings.TrimSpace(p.Type))
	if t == "syllable" || t == "word" {
		return RankSyllable, "Syllable"
	}
	for _, l := range p.Lyrics {
		if len(l.Syllabus) > 0 {
			return RankSyllable, "Syllable"
		}
	}
	if t == "line" {
		return RankLine, "Line"
	}
	for _, l := range p.Lyrics {
		if l.Time > 0 {
			return RankLine, "Line"
		}
	}
	return RankPlain, "Plain"
}

func detectJSONHeaderType(head []byte) int {
	searchWindow := head
	if len(searchWindow) > 160 {
		searchWindow = searchWindow[:160]
	}
	idx := bytes.Index(searchWindow, bytesTypeKey)
	if idx < 0 {
		return RankNone
	}
	rest := searchWindow[idx+len(bytesTypeKey):]
	for len(rest) > 0 && (rest[0] == ' ' || rest[0] == '\t' || rest[0] == '\r' || rest[0] == '\n') {
		rest = rest[1:]
	}
	if len(rest) == 0 || rest[0] != ':' {
		return RankNone
	}
	rest = rest[1:]
	for len(rest) > 0 && (rest[0] == ' ' || rest[0] == '\t' || rest[0] == '\r' || rest[0] == '\n') {
		rest = rest[1:]
	}
	if len(rest) == 0 || rest[0] != '"' {
		return RankNone
	}
	rest = rest[1:]
	endQuote := bytes.IndexByte(rest, '"')
	if endQuote <= 0 {
		return RankNone
	}
	val := rest[:endQuote]
	if bytes.EqualFold(val, []byte("word")) || bytes.EqualFold(val, []byte("syllable")) {
		return RankSyllable
	}
	if bytes.EqualFold(val, []byte("line")) {
		return RankLine
	}
	return RankNone
}

// EnsureSchema initializes the lyrics schema and indexes on a target database.
func EnsureSchema(db *sql.DB) error {
	pragmas := []string{
		"PRAGMA page_size = 8192;",
		"PRAGMA journal_mode = WAL;",
		"PRAGMA synchronous = NORMAL;",
		"PRAGMA busy_timeout = 30000;",
		"PRAGMA temp_store = MEMORY;",
		"PRAGMA cache_size = -65536;",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			return fmt.Errorf("pragma %s: %w", p, err)
		}
	}

	if _, err := db.Exec(lyricsCreateTable); err != nil {
		return fmt.Errorf("create table: %w", err)
	}
	if _, err := db.Exec(lyricsFTS5Setup); err != nil {
		_ = err
	}
	return nil
}

// MergeDatabases merges one or more source SQLite databases into the target database.
// It uses native SQLite ATTACH DATABASE set operations, deferred secondary index/trigger creation,
// large in-memory page caches, and parallel multi-core quality conflict evaluation.
func MergeDatabases(ctx context.Context, cfg MergeConfig) (*MergeStats, error) {
	startTime := time.Now()
	stats := &MergeStats{TotalSources: len(cfg.SourcePaths)}

	logf := cfg.LogFunc
	if logf == nil {
		logf = func(format string, args ...any) {}
	}

	// Default to 100k rows per set-based chunk if using legacy 1000 default or unset.
	if cfg.BatchSize <= 1000 {
		cfg.BatchSize = 100000
	}
	if cfg.Strategy == "" {
		cfg.Strategy = StrategyQuality
	}

	if err := os.MkdirAll(filepath.Dir(cfg.TargetDB), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir target: %w", err)
	}

	targetDB, err := sql.Open("sqlite", cfg.TargetDB)
	if err != nil {
		return nil, fmt.Errorf("open target db: %w", err)
	}
	defer func() { _ = targetDB.Close() }()

	// Pin to a single connection so session-level PRAGMAs and ATTACH DATABASE
	// statements remain bound to the exact same SQLite connection.
	targetDB.SetMaxOpenConns(1)
	targetDB.SetMaxIdleConns(1)
	targetDB.SetConnMaxLifetime(0)

	bulkPragmas := []string{
		"PRAGMA page_size = 8192;",
		"PRAGMA journal_mode = WAL;",
		"PRAGMA synchronous = OFF;",
		"PRAGMA busy_timeout = 30000;",
		"PRAGMA temp_store = MEMORY;",
		"PRAGMA cache_size = -1048576;",  // ~1 GB page cache
		"PRAGMA mmap_size = 2147483648;", // 2 GB memory-mapped I/O
		"PRAGMA wal_autocheckpoint = 0;", // Defer checkpoint until merge completes
		"PRAGMA journal_size_limit = 268435456;",
	}
	for _, p := range bulkPragmas {
		if _, err := targetDB.ExecContext(ctx, p); err != nil {
			return nil, fmt.Errorf("target pragma %s: %w", p, err)
		}
	}

	if _, err := targetDB.ExecContext(ctx, lyricsBaseTableOnlySQL); err != nil {
		return nil, fmt.Errorf("init base table: %w", err)
	}

	var maxIDBeforeAll, targetRowsBeforeAll int64
	_ = targetDB.QueryRowContext(ctx, "SELECT COALESCE(MAX(id), 0), COUNT(*) FROM lyrics").Scan(&maxIDBeforeAll, &targetRowsBeforeAll)

	// Estimate total source size to decide whether to drop secondary indexes and FTS insert triggers
	var totalSourceBytes int64
	for _, srcPath := range cfg.SourcePaths {
		if fi, err := os.Stat(srcPath); err == nil {
			totalSourceBytes += fi.Size()
		}
	}

	bulkIndexDefer := targetRowsBeforeAll == 0 || totalSourceBytes >= 4*1024*1024
	if bulkIndexDefer {
		logf("[*] Preparing target DB for bulk merge (deferring secondary indexes & FTS insert trigger)...")
		_, _ = targetDB.ExecContext(ctx, lyricsDropSecondaryIndexesSQL)
	} else {
		_, _ = targetDB.ExecContext(ctx, lyricsCreateTable)
		_, _ = targetDB.ExecContext(ctx, lyricsFTS5Setup)
	}

	for srcIdx, srcPath := range cfg.SourcePaths {
		if ctx.Err() != nil {
			break
		}

		cleanSrc, err1 := filepath.Abs(srcPath)
		cleanTgt, err2 := filepath.Abs(cfg.TargetDB)
		if err1 == nil && err2 == nil && cleanSrc == cleanTgt {
			logf("[!] Skipping source identical to target: %s", srcPath)
			continue
		} else if filepath.Clean(srcPath) == filepath.Clean(cfg.TargetDB) {
			logf("[!] Skipping source identical to target: %s", srcPath)
			continue
		}

		if _, err := os.Stat(srcPath); err != nil {
			logf("[!] Source file not found: %s", srcPath)
			stats.Errors++
			continue
		}

		logf("[%d/%d] Merging source: %s ...", srcIdx+1, len(cfg.SourcePaths), srcPath)
		if err := mergeSingleSource(ctx, srcPath, targetDB, cfg, stats); err != nil {
			logf("[!] Error merging %s: %v", srcPath, err)
			stats.Errors++
		}
	}

	// Always restore indexes, FTS triggers, and normal pragmas using a non-canceled context
	// so the target database is never left in an unindexed state even on interrupt.
	restoreCtx := context.Background()
	if bulkIndexDefer {
		logf("[*] Restoring secondary indexes on target database...")
		_, _ = targetDB.ExecContext(restoreCtx, lyricsCreateTable)

		if cfg.RebuildFTS {
			logf("[*] Rebuilding FTS5 full-text index...")
			_, _ = targetDB.ExecContext(restoreCtx, `INSERT INTO lyrics_fts(lyrics_fts) VALUES('rebuild');`)
		} else if stats.Inserted > 0 {
			logf("[*] Syncing %d newly inserted records into FTS5 index...", stats.Inserted)
			_, _ = targetDB.ExecContext(restoreCtx, `
				INSERT INTO lyrics_fts(rowid, title, artist)
				SELECT id, title, artist FROM lyrics WHERE id > ?;
			`, maxIDBeforeAll)
		}
		_, _ = targetDB.ExecContext(restoreCtx, lyricsFTS5Setup)
	} else if cfg.RebuildFTS {
		logf("[*] Rebuilding FTS5 full-text index...")
		_, _ = targetDB.ExecContext(restoreCtx, `INSERT INTO lyrics_fts(lyrics_fts) VALUES('rebuild');`)
	}

	_, _ = targetDB.ExecContext(restoreCtx, "PRAGMA synchronous = NORMAL;")
	_, _ = targetDB.ExecContext(restoreCtx, "PRAGMA wal_autocheckpoint = 1000;")

	if cfg.Vacuum && ctx.Err() == nil {
		logf("[*] Running checkpoint and VACUUM on target...")
		_, _ = targetDB.ExecContext(restoreCtx, "PRAGMA wal_checkpoint(TRUNCATE);")
		_, _ = targetDB.ExecContext(restoreCtx, "VACUUM;")
	} else {
		_, _ = targetDB.ExecContext(restoreCtx, "PRAGMA wal_checkpoint(PASSIVE);")
	}

	_, _ = targetDB.ExecContext(restoreCtx, "ANALYZE;")
	_, _ = targetDB.ExecContext(restoreCtx, "PRAGMA optimize;")

	stats.Duration = time.Since(startTime)
	return stats, ctx.Err()
}

type conflictCandidate struct {
	targetID      int64
	tgtContent    []byte
	tgtCreatedAt  int64
	srcContent    []byte
	srcISRC       string
	srcPlatformID string
	srcTitle      string
	srcArtist     string
	srcDurationMS int
	srcCreatedAt  int64
	decision      int // 0 = skipSame, 1 = upgrade, 2 = skipLower
}

const (
	decisionSkipSame  = 0
	decisionUpgrade   = 1
	decisionSkipLower = 2
)

func mergeSingleSource(ctx context.Context, srcPath string, targetDB *sql.DB, cfg MergeConfig, stats *MergeStats) error {
	logf := cfg.LogFunc
	if logf == nil {
		logf = func(format string, args ...any) {}
	}

	if _, err := targetDB.ExecContext(ctx, "ATTACH DATABASE ? AS src", srcPath); err != nil {
		return fmt.Errorf("attach source: %w", err)
	}
	defer func() {
		_, _ = targetDB.ExecContext(context.Background(), "DETACH DATABASE src")
	}()

	var minRowID, maxRowID, totalSrcRows int64
	if err := targetDB.QueryRowContext(ctx,
		"SELECT COALESCE(MIN(rowid), 0), COALESCE(MAX(rowid), 0), COUNT(*) FROM src.lyrics",
	).Scan(&minRowID, &maxRowID, &totalSrcRows); err != nil {
		return fmt.Errorf("inspect source lyrics: %w", err)
	}
	if totalSrcRows == 0 {
		logf("    [i] Source database is empty (0 records).")
		return nil
	}

	var targetRowsBefore, maxTargetIDBefore int64
	if err := targetDB.QueryRowContext(ctx,
		"SELECT COUNT(*), COALESCE(MAX(id), 0) FROM main.lyrics",
	).Scan(&targetRowsBefore, &maxTargetIDBefore); err != nil {
		return fmt.Errorf("inspect target lyrics: %w", err)
	}

	chunkSize := int64(cfg.BatchSize)
	if chunkSize <= 1000 {
		chunkSize = 100000
	}

	srcStart := time.Now()
	var totalConflictsResolved int64
	var candidateConflictsExamined int64

	// Phase 1: Resolve conflicts on existing rows if the target DB is non-empty
	// and the strategy can update existing rows.
	if targetRowsBefore > 0 && cfg.Strategy != StrategyInsertOnly {
		srcHasFilenameIdx := hasLeadingFilenameIndex(ctx, targetDB, "src", "lyrics")
		driveFromMain := srcHasFilenameIdx && targetRowsBefore <= totalSrcRows

		switch cfg.Strategy {
		case StrategyForce:
			upgraded, err := resolveConflictsForceOrNewer(ctx, targetDB, false, driveFromMain, maxTargetIDBefore, minRowID, maxRowID, chunkSize)
			if err != nil {
				return err
			}
			stats.Upgraded += upgraded
			totalConflictsResolved = upgraded

		case StrategyNewer:
			upgraded, err := resolveConflictsForceOrNewer(ctx, targetDB, true, driveFromMain, maxTargetIDBefore, minRowID, maxRowID, chunkSize)
			if err != nil {
				return err
			}
			stats.Upgraded += upgraded
			totalConflictsResolved = upgraded

		case StrategyQuality:
			examined, err := resolveConflictsQuality(ctx, targetDB, driveFromMain, maxTargetIDBefore, minRowID, maxRowID, stats, logf)
			if err != nil {
				return err
			}
			candidateConflictsExamined = examined
		}
	}

	// Phase 2: Set-based bulk insert of all non-conflicting source rows in rowid chunks.
	contiguous := (maxRowID - minRowID + 1) == totalSrcRows
	var srcInserted int64
	var srcExamined int64

	tx, err := targetDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin bulk insert tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	insertStmt, err := tx.PrepareContext(ctx, `
		INSERT OR IGNORE INTO main.lyrics (filename, content, isrc, platform_id, source, title, artist, duration_ms, created_at)
		SELECT filename, content, COALESCE(isrc, ''), COALESCE(platform_id, ''), source, COALESCE(title, ''), COALESCE(artist, ''), COALESCE(duration_ms, 0), COALESCE(created_at, 0)
		FROM src.lyrics
		WHERE rowid > ? AND rowid <= ?
	`)
	if err != nil {
		return fmt.Errorf("prepare bulk insert: %w", err)
	}
	defer func() { _ = insertStmt.Close() }()

	for curID := minRowID - 1; curID < maxRowID; {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		nextID := curID + chunkSize
		if nextID > maxRowID {
			nextID = maxRowID
		}

		var chunkRows int64
		if contiguous {
			chunkRows = nextID - curID
		} else {
			if err := tx.QueryRowContext(ctx,
				"SELECT COUNT(*) FROM src.lyrics WHERE rowid > ? AND rowid <= ?",
				curID, nextID,
			).Scan(&chunkRows); err != nil {
				return fmt.Errorf("count chunk: %w", err)
			}
		}

		res, err := insertStmt.ExecContext(ctx, curID, nextID)
		if err != nil {
			stats.Errors++
			return fmt.Errorf("bulk insert chunk (%d..%d]: %w", curID, nextID, err)
		}
		ins, _ := res.RowsAffected()
		srcInserted += ins
		srcExamined += chunkRows
		stats.TotalExamined += chunkRows
		stats.Inserted += ins

		elapsed := time.Since(srcStart).Seconds()
		rate := float64(srcExamined)
		if elapsed > 0 {
			rate = float64(srcExamined) / elapsed
		}
		pct := float64(srcExamined) / float64(totalSrcRows) * 100.0
		logf("    ... processed %d / %d records (%.1f%% | %.0f rows/s) — inserted: %d, upgraded: %d",
			srcExamined, totalSrcRows, pct, rate, stats.Inserted, stats.Upgraded)

		curID = nextID
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit bulk insert: %w", err)
	}

	// Compute exact conflict skip statistics from total conflicts (totalSrcRows - srcInserted)
	totalConflicts := totalSrcRows - srcInserted
	if totalConflicts < 0 {
		totalConflicts = 0
	}

	switch cfg.Strategy {
	case StrategyInsertOnly:
		stats.SkippedExists += totalConflicts
	case StrategyNewer:
		skipped := totalConflicts - totalConflictsResolved
		if skipped > 0 {
			stats.SkippedSame += skipped
		}
	case StrategyQuality:
		identicalSkipped := totalConflicts - candidateConflictsExamined
		if identicalSkipped > 0 {
			stats.SkippedSame += identicalSkipped
		}
	}

	return nil
}

func resolveConflictsForceOrNewer(
	ctx context.Context,
	targetDB *sql.DB,
	onlyNewer bool,
	driveFromMain bool,
	maxTargetIDBefore int64,
	minSrcRowID, maxSrcRowID, chunkSize int64,
) (int64, error) {
	tx, err := targetDB.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin conflict update tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var totalUpgraded int64
	newerClause := ""
	if onlyNewer {
		newerClause = " AND COALESCE(s.created_at, 0) > COALESCE(main.lyrics.created_at, 0)"
	}

	if driveFromMain {
		q := `
			UPDATE main.lyrics SET
				content = s.content,
				isrc = COALESCE(s.isrc, ''),
				platform_id = COALESCE(s.platform_id, ''),
				title = COALESCE(s.title, ''),
				artist = COALESCE(s.artist, ''),
				duration_ms = COALESCE(s.duration_ms, 0),
				created_at = COALESCE(s.created_at, 0)
			FROM src.lyrics AS s
			WHERE main.lyrics.id > ? AND main.lyrics.id <= ?
			  AND main.lyrics.filename = s.filename AND main.lyrics.source = s.source` + newerClause
		stmt, err := tx.PrepareContext(ctx, q)
		if err != nil {
			return 0, err
		}
		defer func() { _ = stmt.Close() }()

		for curID := int64(0); curID < maxTargetIDBefore; {
			if ctx.Err() != nil {
				return totalUpgraded, ctx.Err()
			}
			nextID := curID + chunkSize
			if nextID > maxTargetIDBefore {
				nextID = maxTargetIDBefore
			}
			res, err := stmt.ExecContext(ctx, curID, nextID)
			if err != nil {
				return totalUpgraded, err
			}
			aff, _ := res.RowsAffected()
			totalUpgraded += aff
			curID = nextID
		}
	} else {
		q := `
			UPDATE main.lyrics SET
				content = s.content,
				isrc = COALESCE(s.isrc, ''),
				platform_id = COALESCE(s.platform_id, ''),
				title = COALESCE(s.title, ''),
				artist = COALESCE(s.artist, ''),
				duration_ms = COALESCE(s.duration_ms, 0),
				created_at = COALESCE(s.created_at, 0)
			FROM src.lyrics AS s
			WHERE s.rowid > ? AND s.rowid <= ?
			  AND main.lyrics.filename = s.filename AND main.lyrics.source = s.source` + newerClause
		stmt, err := tx.PrepareContext(ctx, q)
		if err != nil {
			return 0, err
		}
		defer func() { _ = stmt.Close() }()

		for curID := minSrcRowID - 1; curID < maxSrcRowID; {
			if ctx.Err() != nil {
				return totalUpgraded, ctx.Err()
			}
			nextID := curID + chunkSize
			if nextID > maxSrcRowID {
				nextID = maxSrcRowID
			}
			res, err := stmt.ExecContext(ctx, curID, nextID)
			if err != nil {
				return totalUpgraded, err
			}
			aff, _ := res.RowsAffected()
			totalUpgraded += aff
			curID = nextID
		}
	}

	if err := tx.Commit(); err != nil {
		return totalUpgraded, err
	}
	return totalUpgraded, nil
}

func resolveConflictsQuality(
	ctx context.Context,
	targetDB *sql.DB,
	driveFromMain bool,
	maxTargetIDBefore int64,
	minSrcRowID, maxSrcRowID int64,
	stats *MergeStats,
	logf func(format string, args ...any),
) (int64, error) {
	tx, err := targetDB.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin quality conflict tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	const conflictRangeStep = int64(25000)
	var totalExaminedConflicts int64

	var selectQuery string
	var startID, endID int64
	if driveFromMain {
		selectQuery = `
			SELECT m.id, m.content, COALESCE(m.created_at, 0),
			       s.content, COALESCE(s.isrc, ''), COALESCE(s.platform_id, ''),
			       COALESCE(s.title, ''), COALESCE(s.artist, ''),
			       COALESCE(s.duration_ms, 0), COALESCE(s.created_at, 0)
			FROM main.lyrics m
			CROSS JOIN src.lyrics s ON s.filename = m.filename AND s.source = m.source
			WHERE m.id > ? AND m.id <= ?
			  AND (s.content != m.content OR COALESCE(s.created_at, 0) > COALESCE(m.created_at, 0))
		`
		startID = 0
		endID = maxTargetIDBefore
	} else {
		selectQuery = `
			SELECT m.id, m.content, COALESCE(m.created_at, 0),
			       s.content, COALESCE(s.isrc, ''), COALESCE(s.platform_id, ''),
			       COALESCE(s.title, ''), COALESCE(s.artist, ''),
			       COALESCE(s.duration_ms, 0), COALESCE(s.created_at, 0)
			FROM src.lyrics s
			CROSS JOIN main.lyrics m ON s.filename = m.filename AND s.source = m.source
			WHERE s.rowid > ? AND s.rowid <= ?
			  AND (s.content != m.content OR COALESCE(s.created_at, 0) > COALESCE(m.created_at, 0))
		`
		startID = minSrcRowID - 1
		endID = maxSrcRowID
	}

	selectStmt, err := tx.PrepareContext(ctx, selectQuery)
	if err != nil {
		return 0, fmt.Errorf("prepare conflict select: %w", err)
	}
	defer func() { _ = selectStmt.Close() }()

	updateStmt, err := tx.PrepareContext(ctx, `
		UPDATE main.lyrics SET
			content = ?,
			isrc = ?,
			platform_id = ?,
			title = ?,
			artist = ?,
			duration_ms = ?,
			created_at = ?
		WHERE id = ?
	`)
	if err != nil {
		return 0, fmt.Errorf("prepare conflict update: %w", err)
	}
	defer func() { _ = updateStmt.Close() }()

	workers := runtime.NumCPU()
	if workers < 1 {
		workers = 1
	}

	candidates := make([]conflictCandidate, 0, 1024)

	for curID := startID; curID < endID; {
		if ctx.Err() != nil {
			return totalExaminedConflicts, ctx.Err()
		}
		nextID := curID + conflictRangeStep
		if nextID > endID {
			nextID = endID
		}

		rows, err := selectStmt.QueryContext(ctx, curID, nextID)
		if err != nil {
			return totalExaminedConflicts, fmt.Errorf("query quality conflicts: %w", err)
		}

		candidates = candidates[:0]
		for rows.Next() {
			var c conflictCandidate
			if err := rows.Scan(
				&c.targetID, &c.tgtContent, &c.tgtCreatedAt,
				&c.srcContent, &c.srcISRC, &c.srcPlatformID,
				&c.srcTitle, &c.srcArtist, &c.srcDurationMS, &c.srcCreatedAt,
			); err != nil {
				stats.Errors++
				continue
			}
			candidates = append(candidates, c)
		}
		_ = rows.Close()

		if len(candidates) > 0 {
			totalExaminedConflicts += int64(len(candidates))
			evaluateQualityConflictsParallel(candidates, workers)

			for i := range candidates {
				c := &candidates[i]
				switch c.decision {
				case decisionUpgrade:
					if _, err := updateStmt.ExecContext(ctx,
						c.srcContent, c.srcISRC, c.srcPlatformID, c.srcTitle,
						c.srcArtist, c.srcDurationMS, c.srcCreatedAt, c.targetID,
					); err != nil {
						stats.Errors++
					} else {
						stats.Upgraded++
					}
				case decisionSkipSame:
					stats.SkippedSame++
				case decisionSkipLower:
					stats.SkippedLower++
				}
			}
		}

		curID = nextID
	}

	if err := tx.Commit(); err != nil {
		return totalExaminedConflicts, fmt.Errorf("commit quality conflicts: %w", err)
	}
	if totalExaminedConflicts > 0 {
		logf("    [i] Evaluated %d modified/newer conflict records (upgraded: %d, skipped lower: %d)",
			totalExaminedConflicts, stats.Upgraded, stats.SkippedLower)
	}
	return totalExaminedConflicts, nil
}

func evaluateQualityConflictsParallel(candidates []conflictCandidate, workers int) {
	n := len(candidates)
	if n == 0 {
		return
	}
	if n < 64 || workers <= 1 {
		for i := range candidates {
			evaluateSingleCandidate(&candidates[i])
		}
		return
	}

	if workers > n {
		workers = n
	}
	var wg sync.WaitGroup
	chunk := (n + workers - 1) / workers
	for w := 0; w < workers; w++ {
		start := w * chunk
		if start >= n {
			break
		}
		end := start + chunk
		if end > n {
			end = n
		}
		wg.Add(1)
		go func(slice []conflictCandidate) {
			defer wg.Done()
			for i := range slice {
				evaluateSingleCandidate(&slice[i])
			}
		}(candidates[start:end])
	}
	wg.Wait()
}

func evaluateSingleCandidate(c *conflictCandidate) {
	if bytes.Equal(c.srcContent, c.tgtContent) {
		if c.srcCreatedAt > c.tgtCreatedAt {
			c.decision = decisionUpgrade
		} else {
			c.decision = decisionSkipSame
		}
		return
	}
	srcRank, _ := DetectLyricsQuality(c.srcContent)
	tgtRank, _ := DetectLyricsQuality(c.tgtContent)
	if srcRank > tgtRank {
		c.decision = decisionUpgrade
	} else if srcRank == tgtRank {
		if c.srcCreatedAt > c.tgtCreatedAt {
			c.decision = decisionUpgrade
		} else {
			c.decision = decisionSkipSame
		}
	} else {
		c.decision = decisionSkipLower
	}
}

func hasLeadingFilenameIndex(ctx context.Context, db *sql.DB, schemaName, tableName string) bool {
	rows, err := db.QueryContext(ctx, fmt.Sprintf("PRAGMA %s.index_list(%s)", schemaName, tableName))
	if err != nil {
		return false
	}
	var indexNames []string
	for rows.Next() {
		var seq int
		var name, origin string
		var unique, partial int
		if err := rows.Scan(&seq, &name, &unique, &origin, &partial); err == nil {
			indexNames = append(indexNames, name)
		}
	}
	_ = rows.Close()

	for _, idxName := range indexNames {
		infoRows, err := db.QueryContext(ctx, fmt.Sprintf("PRAGMA %s.index_info(%q)", schemaName, idxName))
		if err != nil {
			continue
		}
		hasFilenameFirst := false
		for infoRows.Next() {
			var seqno, cid int
			var colName string
			if err := infoRows.Scan(&seqno, &cid, &colName); err == nil {
				if seqno == 0 && strings.EqualFold(colName, "filename") {
					hasFilenameFirst = true
				}
			}
		}
		_ = infoRows.Close()
		if hasFilenameFirst {
			return true
		}
	}
	return false
}

// GetDatabaseStats inspects an SQLite database and aggregates statistics using parallel workers.
func GetDatabaseStats(ctx context.Context, dbPath string) (*DBStats, error) {
	db, err := sql.Open("sqlite", dbPath+"?mode=ro&_pragma=cache_size(-262144)&_pragma=mmap_size(1073741824)&_pragma=temp_store(MEMORY)")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = db.Close() }()

	stats := &DBStats{
		DatabasePath: dbPath,
		Sources:      make(map[string]int64),
		SyncTypes:    make(map[string]int64),
	}

	if fi, err := os.Stat(dbPath); err == nil {
		stats.FileSize = fi.Size()
	}
	if fi, err := os.Stat(dbPath + "-wal"); err == nil {
		stats.WALSize = fi.Size()
	}

	// Total rows & Sources breakdown in a single query
	srcRows, err := db.QueryContext(ctx, "SELECT COALESCE(source, ''), COUNT(*) FROM lyrics GROUP BY source")
	if err == nil {
		for srcRows.Next() {
			var src string
			var count int64
			if err := srcRows.Scan(&src, &count); err == nil {
				stats.Sources[src] += count
				stats.TotalRows += count
			}
		}
		_ = srcRows.Close()
	} else {
		_ = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM lyrics").Scan(&stats.TotalRows)
	}

	// Parallel quality breakdown
	contentRows, err := db.QueryContext(ctx, "SELECT content FROM lyrics")
	if err == nil {
		defer func() { _ = contentRows.Close() }()

		workers := runtime.NumCPU()
		if workers < 1 {
			workers = 1
		}
		blobCh := make(chan [][]byte, workers*2)
		localMaps := make([]map[string]int64, workers)
		var wg sync.WaitGroup

		for w := 0; w < workers; w++ {
			localMaps[w] = make(map[string]int64, 8)
			wg.Add(1)
			go func(workerIdx int) {
				defer wg.Done()
				m := localMaps[workerIdx]
				for batch := range blobCh {
					for _, blob := range batch {
						_, qName := DetectLyricsQuality(blob)
						m[qName]++
					}
				}
			}(w)
		}

		batch := make([][]byte, 0, 2048)
		for contentRows.Next() {
			var blob []byte
			if err := contentRows.Scan(&blob); err == nil {
				cp := make([]byte, len(blob))
				copy(cp, blob)
				batch = append(batch, cp)
				if len(batch) >= 2048 {
					blobCh <- batch
					batch = make([][]byte, 0, 2048)
				}
			}
		}
		if len(batch) > 0 {
			blobCh <- batch
		}
		close(blobCh)
		wg.Wait()

		for _, m := range localMaps {
			for k, v := range m {
				stats.SyncTypes[k] += v
			}
		}
	}

	return stats, nil
}
