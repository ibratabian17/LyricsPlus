package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// DetectLyricsQuality inspects the compressed or raw content blob and determines its sync quality rank.
func DetectLyricsQuality(contentBlob []byte) (int, string) {
	if len(contentBlob) == 0 {
		return RankNone, "None"
	}
	raw := DecompressContent(contentBlob)
	if len(raw) == 0 {
		return RankNone, "None"
	}

	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, "<") {
		// Raw TTML or QRC XML
		if strings.Contains(trimmed, "<span") && strings.Contains(trimmed, "begin=") {
			return RankSyllable, "Syllable"
		}
		if strings.Contains(trimmed, "begin=") || strings.Contains(trimmed, "<p") {
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
	if err := json.Unmarshal(raw, &p); err != nil {
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

// EnsureSchema initializes the lyrics schema and indexes on a target database.
func EnsureSchema(db *sql.DB) error {
	pragmas := []string{
		"PRAGMA journal_mode = WAL;",
		"PRAGMA synchronous = NORMAL;",
		"PRAGMA busy_timeout = 15000;",
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
		// FTS5 might already exist or not be strictly fatal for merger
		_ = err
	}
	return nil
}

// MergeDatabases merges one or more source SQLite databases into the target database.
func MergeDatabases(ctx context.Context, cfg MergeConfig) (*MergeStats, error) {
	startTime := time.Now()
	stats := &MergeStats{TotalSources: len(cfg.SourcePaths)}

	logf := cfg.LogFunc
	if logf == nil {
		logf = func(format string, args ...any) {}
	}

	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 1000
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

	if err := EnsureSchema(targetDB); err != nil {
		return nil, fmt.Errorf("init target schema: %w", err)
	}

	for srcIdx, srcPath := range cfg.SourcePaths {
		if ctx.Err() != nil {
			break
		}

		cleanSrc := filepath.Clean(srcPath)
		cleanTgt := filepath.Clean(cfg.TargetDB)
		if cleanSrc == cleanTgt {
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

	if cfg.RebuildFTS {
		logf("[*] Rebuilding FTS5 full-text index...")
		_, _ = targetDB.ExecContext(ctx, `
			INSERT INTO lyrics_fts(lyrics_fts) VALUES('rebuild');
		`)
	}

	if cfg.Vacuum {
		logf("[*] Running checkpoint and VACUUM on target...")
		_, _ = targetDB.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE);")
		_, _ = targetDB.ExecContext(ctx, "VACUUM;")
	} else {
		_, _ = targetDB.ExecContext(ctx, "PRAGMA wal_checkpoint(PASSIVE);")
	}

	stats.Duration = time.Since(startTime)
	return stats, nil
}

func mergeSingleSource(ctx context.Context, srcPath string, targetDB *sql.DB, cfg MergeConfig, stats *MergeStats) error {
	logf := cfg.LogFunc
	if logf == nil {
		logf = func(format string, args ...any) {}
	}

	srcDB, err := sql.Open("sqlite", srcPath+"?mode=ro")
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer func() { _ = srcDB.Close() }()

	rows, err := srcDB.QueryContext(ctx, `
		SELECT filename, content, isrc, platform_id, source, title, artist, duration_ms, created_at
		FROM lyrics
	`)
	if err != nil {
		return fmt.Errorf("query source lyrics: %w", err)
	}
	defer func() { _ = rows.Close() }()

	tx, err := targetDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin target tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	selectStmt, err := tx.PrepareContext(ctx, `
		SELECT id, content, created_at FROM lyrics WHERE filename = ? AND source = ?
	`)
	if err != nil {
		return fmt.Errorf("prepare select: %w", err)
	}
	defer func() { _ = selectStmt.Close() }()

	insertStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO lyrics (filename, content, isrc, platform_id, source, title, artist, duration_ms, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return fmt.Errorf("prepare insert: %w", err)
	}
	defer func() { _ = insertStmt.Close() }()

	updateStmt, err := tx.PrepareContext(ctx, `
		UPDATE lyrics SET
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
		return fmt.Errorf("prepare update: %w", err)
	}
	defer func() { _ = updateStmt.Close() }()

	var uncommitted int
	batchCount := 0

	for rows.Next() {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		stats.TotalExamined++
		var (
			filename    string
			content     []byte
			isrc        sql.NullString
			platformID  sql.NullString
			source      string
			title       sql.NullString
			artist      sql.NullString
			durationMS  int
			createdAtMs int64
		)

		if err := rows.Scan(
			&filename, &content, &isrc, &platformID, &source,
			&title, &artist, &durationMS, &createdAtMs,
		); err != nil {
			stats.Errors++
			continue
		}

		// Check if target already has this (filename, source)
		var (
			existingID        int64
			existingContent   []byte
			existingCreatedAt int64
		)
		err := selectStmt.QueryRowContext(ctx, filename, source).Scan(&existingID, &existingContent, &existingCreatedAt)
		if err == sql.ErrNoRows {
			// Not present -> INSERT
			_, err = insertStmt.ExecContext(ctx,
				filename, content, isrc.String, platformID.String, source,
				title.String, artist.String, durationMS, createdAtMs,
			)
			if err != nil {
				stats.Errors++
			} else {
				stats.Inserted++
				uncommitted++
			}
		} else if err != nil {
			stats.Errors++
			continue
		} else {
			// Already exists -> Apply strategy
			shouldUpdate := false

			switch cfg.Strategy {
			case StrategyForce:
				shouldUpdate = true
			case StrategyInsertOnly:
				stats.SkippedExists++
				shouldUpdate = false
			case StrategyNewer:
				if createdAtMs > existingCreatedAt {
					shouldUpdate = true
				} else {
					stats.SkippedSame++
				}
			case StrategyQuality:
				srcRank, _ := DetectLyricsQuality(content)
				tgtRank, _ := DetectLyricsQuality(existingContent)

				if srcRank > tgtRank {
					// Higher quality lyrics (e.g. syllable over line or line over plain)
					shouldUpdate = true
					stats.Upgraded++
				} else if srcRank == tgtRank {
					if createdAtMs > existingCreatedAt {
						shouldUpdate = true
						stats.Upgraded++
					} else {
						stats.SkippedSame++
					}
				} else {
					// Target already has superior quality!
					stats.SkippedLower++
				}
			}

			if shouldUpdate {
				_, err = updateStmt.ExecContext(ctx,
					content, isrc.String, platformID.String, title.String,
					artist.String, durationMS, createdAtMs, existingID,
				)
				if err != nil {
					stats.Errors++
				} else {
					uncommitted++
				}
			}
		}

		if uncommitted >= cfg.BatchSize {
			if err := tx.Commit(); err != nil {
				return fmt.Errorf("commit batch: %w", err)
			}
			batchCount++
			uncommitted = 0

			// Start next transaction
			tx, err = targetDB.BeginTx(ctx, nil)
			if err != nil {
				return fmt.Errorf("begin next tx: %w", err)
			}
			selectStmt, err = tx.PrepareContext(ctx, `SELECT id, content, created_at FROM lyrics WHERE filename = ? AND source = ?`)
			if err != nil {
				return err
			}
			insertStmt, err = tx.PrepareContext(ctx, `INSERT INTO lyrics (filename, content, isrc, platform_id, source, title, artist, duration_ms, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
			if err != nil {
				return err
			}
			updateStmt, err = tx.PrepareContext(ctx, `UPDATE lyrics SET content = ?, isrc = ?, platform_id = ?, title = ?, artist = ?, duration_ms = ?, created_at = ? WHERE id = ?`)
			if err != nil {
				return err
			}

			if stats.TotalExamined%10000 == 0 {
				logf("    ... processed %d records (inserted: %d, upgraded: %d, skipped: %d)",
					stats.TotalExamined, stats.Inserted, stats.Upgraded, stats.SkippedSame+stats.SkippedLower+stats.SkippedExists)
			}
		}
	}

	if uncommitted > 0 {
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit final batch: %w", err)
		}
	}

	return nil
}

// GetDatabaseStats inspects an SQLite database and aggregates statistics.
func GetDatabaseStats(ctx context.Context, dbPath string) (*DBStats, error) {
	db, err := sql.Open("sqlite", dbPath+"?mode=ro")
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

	// Total rows
	_ = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM lyrics").Scan(&stats.TotalRows)

	// Sources breakdown
	srcRows, err := db.QueryContext(ctx, "SELECT source, COUNT(*) FROM lyrics GROUP BY source")
	if err == nil {
		defer func() { _ = srcRows.Close() }()
		for srcRows.Next() {
			var src string
			var count int64
			if err := srcRows.Scan(&src, &count); err == nil {
				stats.Sources[src] = count
			}
		}
	}

	// Quality breakdown
	contentRows, err := db.QueryContext(ctx, "SELECT content FROM lyrics")
	if err == nil {
		defer func() { _ = contentRows.Close() }()
		for contentRows.Next() {
			var blob []byte
			if err := contentRows.Scan(&blob); err == nil {
				_, qName := DetectLyricsQuality(blob)
				stats.SyncTypes[qName]++
			}
		}
	}

	return stats, nil
}
