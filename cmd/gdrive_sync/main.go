package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"lyricsplus/backend/internal/config"
	"lyricsplus/backend/internal/parsers"
	"lyricsplus/backend/internal/storage"
	_ "modernc.org/sqlite"
)

type Checkpoint struct {
	FolderTokens map[string]string `json:"folder_tokens"`
	TotalSaved   int64             `json:"total_saved"`
}

type fileJob struct {
	item   storage.FileItem
	source string
	folder string
}

func main() {
	configPath := flag.String("config", "", "Path to configuration file (.json or .env)")
	dbPath := flag.String("db", "", "Target SQLite database file")
	concurrency := flag.Int("concurrency", 150, "Number of concurrent download workers")
	checkpointPath := flag.String("checkpoint", "data/.gdrive_sync_checkpoint.json", "Checkpoint file path for resuming")
	resetCheckpoint := flag.Bool("reset", false, "Start migration from scratch, ignoring checkpoint")
	redoErrors := flag.Bool("redo", false, "Rescan folders and redo missing/errored files (skips existing)")
	flag.Parse()

	cfg := config.Load(*configPath)
	targetDB := cfg.Storage.DBPath
	if *dbPath != "" {
		targetDB = *dbPath
	}
	if targetDB == "" {
		targetDB = "database/lyrics_cache.db"
	}

	httpClient := &http.Client{Timeout: 60 * time.Second}
	gdrive := storage.NewGDriveClient(cfg.GDrive, httpClient)

	fmt.Println("=========================================================")
	fmt.Println("       LyricsPlus - Google Drive Bulk Importer           ")
	fmt.Println("=========================================================")

	if !gdrive.IsConfigured() {
		fmt.Println("\n[!] Google Drive is not configured.")
		fmt.Println("Please set the following environment variables:")
		fmt.Println("  AUTH_KEY_CLIENT_ID       (Google OAuth Client ID)")
		fmt.Println("  AUTH_KEY_CLIENT_SECRET   (Google OAuth Client Secret)")
		fmt.Println("  AUTH_KEY_REFRESH_TOKEN   (Google OAuth Refresh Token)")
		fmt.Println("  or GDRIVE_ACCOUNTS='CLIENT_ID|CLIENT_SECRET|REFRESH_TOKEN'")
		fmt.Println("\nAnd your folder IDs:")
		fmt.Println("  GDRIVE_CACHED_TTML       (Apple Music folder ID)")
		fmt.Println("  GDRIVE_CACHED_SPOTIFY    (Spotify folder ID)")
		fmt.Println("  GDRIVE_USERTML_JSON      (Lyrics+ UGC folder ID)")
		fmt.Println("  GDRIVE_CACHED_MUSIXMATCH (Musixmatch folder ID)")
		fmt.Println("  GDRIVE_CACHED_QQ         (QQ Music folder ID)")
		fmt.Println("  GDRIVE_CACHED_DEEZER     (Deezer folder ID)")
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fmt.Print("\nAuthenticating with Google Drive... ")
	if _, err := gdrive.Authenticate(ctx, false); err != nil {
		fmt.Printf("FAILED: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("OK!")

	dbDir := filepath.Dir(targetDB)
	_ = os.MkdirAll(dbDir, 0755)

	db, err := sql.Open("sqlite", targetDB)
	if err != nil {
		log.Fatalf("Failed to open SQLite database (%s): %v", targetDB, err)
	}
	defer func() { _ = db.Close() }()

	if err := initSchema(db); err != nil {
		log.Fatalf("Failed to init SQLite schema: %v", err)
	}

	// Load existing (source::filename) into an in-memory index for O(1) deduplication
	existingMap := make(map[string]struct{})
	var existingMu sync.RWMutex
	rows, err := db.Query("SELECT filename, source FROM lyrics")
	if err == nil {
		for rows.Next() {
			var fn, src string
			if err := rows.Scan(&fn, &src); err == nil {
				existingMap[src+"::"+fn] = struct{}{}
			}
		}
		_ = rows.Close()
	}
	fmt.Printf("[*] Loaded %d existing entries from database (%s).\n", len(existingMap), targetDB)

	cp := loadCheckpoint(*checkpointPath)
	if *resetCheckpoint || *redoErrors {
		cp = Checkpoint{FolderTokens: make(map[string]string)}
		if *redoErrors {
			fmt.Println("[*] Redo mode enabled: will scan folders and download only missing/failed files.")
		}
	}

	folderTasks := []struct {
		source  string
		folders []string
	}{
		{"lyricsplus", cfg.GDrive.FolderUserTML},
		{"apple", cfg.GDrive.FolderTTML},
		{"spotify", cfg.GDrive.FolderSpotify},
		{"musixmatch", cfg.GDrive.FolderMusixmatch},
		{"qq", cfg.GDrive.FolderQQ},
		{"deezer", cfg.GDrive.FolderDeezer},
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println("\n\n[!] Interrupt received! Flushing database and saving checkpoint...")
		cancel()
	}()

	var totalScanned atomic.Int64
	var totalSkipped atomic.Int64
	var totalDownloaded atomic.Int64
	var totalSaved atomic.Int64
	totalSaved.Store(cp.TotalSaved)
	var totalErrors atomic.Int64

	startTime := time.Now()

	jobQueue := make(chan fileJob, 2000)
	saveQueue := make(chan *storage.Row, 1000)

	var writerWg sync.WaitGroup
	writerWg.Add(1)
	go func() {
		defer writerWg.Done()
		batch := make([]*storage.Row, 0, 100)
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()

		flush := func() {
			if len(batch) == 0 {
				return
			}
			if err := insertBatch(db, batch); err != nil {
				log.Printf("Batch insert error: %v", err)
				totalErrors.Add(int64(len(batch)))
			} else {
				totalSaved.Add(int64(len(batch)))
				existingMu.Lock()
				for _, r := range batch {
					existingMap[r.Source+"::"+r.Filename] = struct{}{}
				}
				existingMu.Unlock()
			}
			batch = batch[:0]
		}

		for {
			select {
			case row, ok := <-saveQueue:
				if !ok {
					flush()
					return
				}
				batch = append(batch, row)
				if len(batch) >= 100 {
					flush()
				}
			case <-ticker.C:
				flush()
			}
		}
	}()

	var failedJobs []fileJob
	var failedMu sync.Mutex

	var workerWg sync.WaitGroup
	for w := 0; w < *concurrency; w++ {
		workerWg.Add(1)
		go func() {
			defer workerWg.Done()
			for job := range jobQueue {
				if ctx.Err() != nil {
					return
				}

				// Check again if already saved
				existingMu.RLock()
				_, alreadyExists := existingMap[job.source+"::"+job.item.Name]
				existingMu.RUnlock()
				if alreadyExists {
					totalSkipped.Add(1)
					continue
				}

				// Download with retry & backoff
				var content []byte
				var downloadErr error
				for attempt := 0; attempt < 5; attempt++ {
					if ctx.Err() != nil {
						return
					}
					rem := gdrive.CircuitBreakerRemaining()
					if rem > 0 {
						select {
						case <-time.After(rem + 300*time.Millisecond):
						case <-ctx.Done():
							return
						}
					}

					content, downloadErr = gdrive.FetchFile(ctx, job.item.ID)
					if downloadErr == nil {
						break
					}

					backoff := time.Duration(1<<attempt)*time.Second + time.Duration(rand.Intn(500))*time.Millisecond
					if backoff > 20*time.Second {
						backoff = 20 * time.Second
					}
					select {
					case <-time.After(backoff):
					case <-ctx.Done():
						return
					}
				}

				if downloadErr != nil {
					totalErrors.Add(1)
					failedMu.Lock()
					failedJobs = append(failedJobs, job)
					failedMu.Unlock()
					continue
				}

				totalDownloaded.Add(1)

				parsed := storage.ParseFilename(job.item.Name)
				contentToStore := content
				trimmed := strings.TrimSpace(string(content))
				if strings.HasPrefix(trimmed, "<") {
					if job.source == "apple" || strings.HasSuffix(strings.ToLower(job.item.Name), ".ttml") {
						if p, err := parsers.TTMLToJSON(content); err == nil && p != nil && len(p.Lyrics) > 0 {
							p.RawData = string(content)
							if b, err := json.Marshal(p); err == nil {
								contentToStore = b
							}
						}
					} else if job.source == "qq" || strings.HasSuffix(strings.ToLower(job.item.Name), ".qrc") {
						p := parsers.ParseQQQRC(trimmed, parsers.ExactMetadata{
							Title:      parsed.Title,
							Artist:     parsed.Artist,
							DurationMs: parsed.DurationMS,
							PlatformID: parsed.PlatformID,
						})
						if p != nil && len(p.Lyrics) > 0 {
							p.RawData = string(content)
							if b, err := json.Marshal(p); err == nil {
								contentToStore = b
							}
						}
					}
				}

				row := &storage.Row{
					Filename:    job.item.Name,
					ContentJSON: storage.CompressContent(contentToStore),
					ISRC:        parsed.ISRC,
					PlatformID:  parsed.PlatformID,
					Source:      job.source,
					Title:       parsed.Title,
					Artist:      parsed.Artist,
					DurationMS:  parsed.DurationMS,
					CreatedAt:   job.item.CreatedTime,
				}
				if row.CreatedAt.IsZero() {
					row.CreatedAt = time.Now()
				}

				saveQueue <- row
			}
		}()
	}

	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				elapsed := time.Since(startTime).Seconds()
				rate := float64(totalDownloaded.Load()) / elapsed
				fmt.Printf("\r  Scanned: %d | Skipped: %d | Downloaded: %d | Saved: %d | Speed: %.1f files/s | Errors: %d    ",
					totalScanned.Load(), totalSkipped.Load(), totalDownloaded.Load(), totalSaved.Load(), rate, totalErrors.Load())
			}
		}
	}()

	for _, task := range folderTasks {
		for _, folderID := range task.folders {
			if folderID == "" || ctx.Err() != nil {
				continue
			}

			pageToken := cp.FolderTokens[folderID]
			fmt.Printf("\n--> Processing source '%s' (Folder: %s)...\n", task.source, folderID)

			for ctx.Err() == nil {
				var res *storage.FileListResponse
				var err error

				for searchAttempt := 0; searchAttempt < 10; searchAttempt++ {
					if ctx.Err() != nil {
						break
					}
					rem := gdrive.CircuitBreakerRemaining()
					if rem > 0 {
						fmt.Printf("\n[*] Waiting %v for rate limit cooldown...\n", rem)
						select {
						case <-time.After(rem + 300*time.Millisecond):
						case <-ctx.Done():
							break
						}
					}

					res, err = gdrive.SearchFiles(ctx, []string{folderID}, "", 1000, pageToken)
					if err == nil {
						break
					}

					backoff := time.Duration(2<<searchAttempt) * time.Second
					if backoff > 45*time.Second {
						backoff = 45 * time.Second
					}
					fmt.Printf("\n[!] GDrive search error on folder %s: %v. Retrying in %v...\n", folderID, err, backoff)
					select {
					case <-time.After(backoff):
					case <-ctx.Done():
						break
					}
				}

				if err != nil || res == nil {
					fmt.Printf("\n[!] Failed to search folder %s after retries: %v\n", folderID, err)
					break
				}

				for _, item := range res.Files {
					totalScanned.Add(1)

					existingMu.RLock()
					_, exists := existingMap[task.source+"::"+item.Name]
					existingMu.RUnlock()

					if exists {
						totalSkipped.Add(1)
						continue
					}

					jobQueue <- fileJob{item: item, source: task.source, folder: folderID}
				}

				pageToken = res.NextPageToken
				cp.FolderTokens[folderID] = pageToken
				cp.TotalSaved = totalSaved.Load()
				saveCheckpoint(*checkpointPath, cp)

				if pageToken == "" {
					break
				}
			}
		}
	}

	close(jobQueue)
	workerWg.Wait()

	// Second pass: retry failed downloads with relaxed concurrency
	failedMu.Lock()
	numFailed := len(failedJobs)
	failedMu.Unlock()

	if numFailed > 0 && ctx.Err() == nil {
		fmt.Printf("\n\n[*] Attempting second pass for %d failed downloads...\n", numFailed)
		gdrive.ResetCircuitBreaker()
		time.Sleep(3 * time.Second)

		retryQueue := make(chan fileJob, numFailed)
		for _, j := range failedJobs {
			retryQueue <- j
		}
		close(retryQueue)

		var retryWg sync.WaitGroup
		retryWorkers := 10
		if retryWorkers > numFailed {
			retryWorkers = numFailed
		}

		for rw := 0; rw < retryWorkers; rw++ {
			retryWg.Add(1)
			go func() {
				defer retryWg.Done()
				for job := range retryQueue {
					if ctx.Err() != nil {
						return
					}
					existingMu.RLock()
					_, exists := existingMap[job.source+"::"+job.item.Name]
					existingMu.RUnlock()
					if exists {
						continue
					}

					content, err := gdrive.FetchFile(ctx, job.item.ID)
					if err != nil {
						time.Sleep(2 * time.Second)
						content, err = gdrive.FetchFile(ctx, job.item.ID)
					}
					if err != nil {
						continue
					}

					totalDownloaded.Add(1)
					totalErrors.Add(-1) // recovered!

					parsed := storage.ParseFilename(job.item.Name)
					contentToStore := content
					trimmed := strings.TrimSpace(string(content))
					if strings.HasPrefix(trimmed, "<") {
						if job.source == "apple" || strings.HasSuffix(strings.ToLower(job.item.Name), ".ttml") {
							if p, err := parsers.TTMLToJSON(content); err == nil && p != nil && len(p.Lyrics) > 0 {
								p.RawData = string(content)
								if b, err := json.Marshal(p); err == nil {
									contentToStore = b
								}
							}
						} else if job.source == "qq" || strings.HasSuffix(strings.ToLower(job.item.Name), ".qrc") {
							p := parsers.ParseQQQRC(trimmed, parsers.ExactMetadata{
								Title:      parsed.Title,
								Artist:     parsed.Artist,
								DurationMs: parsed.DurationMS,
								PlatformID: parsed.PlatformID,
							})
							if p != nil && len(p.Lyrics) > 0 {
								p.RawData = string(content)
								if b, err := json.Marshal(p); err == nil {
									contentToStore = b
								}
							}
						}
					}

					row := &storage.Row{
						Filename:    job.item.Name,
						ContentJSON: storage.CompressContent(contentToStore),
						ISRC:        parsed.ISRC,
						PlatformID:  parsed.PlatformID,
						Source:      job.source,
						Title:       parsed.Title,
						Artist:      parsed.Artist,
						DurationMS:  parsed.DurationMS,
						CreatedAt:   job.item.CreatedTime,
					}
					if row.CreatedAt.IsZero() {
						row.CreatedAt = time.Now()
					}
					saveQueue <- row
				}
			}()
		}
		retryWg.Wait()
	}

	close(saveQueue)
	writerWg.Wait()

	if err := createIndexes(db); err != nil {
		log.Printf("Failed to create indexes: %v", err)
	}
	// Fold the WAL into the main DB file and reclaim free pages.
	if _, err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE);"); err != nil {
		log.Printf("wal checkpoint failed: %v", err)
	}
	if _, err := db.Exec("VACUUM;"); err != nil {
		log.Printf("vacuum failed: %v", err)
	}

	cp.TotalSaved = totalSaved.Load()
	saveCheckpoint(*checkpointPath, cp)

	elapsed := time.Since(startTime)
	fmt.Printf("\n\n=========================================================\n")
	fmt.Printf("                   Sync Completed!                       \n")
	fmt.Printf("=========================================================\n")
	fmt.Printf("Total Elapsed:    %v\n", elapsed)
	fmt.Printf("Total Scanned:    %d\n", totalScanned.Load())
	fmt.Printf("Total Skipped:    %d (already up to date)\n", totalSkipped.Load())
	fmt.Printf("Total Downloaded: %d\n", totalDownloaded.Load())
	fmt.Printf("Total In DB:      %d\n", totalSaved.Load())
	fmt.Printf("Total Errors:     %d\n", totalErrors.Load())
	fmt.Printf("Database:         %s\n", targetDB)
	fmt.Printf("Checkpoint:       %s\n\n", *checkpointPath)
}

func initSchema(db *sql.DB) error {
	pragmas := []string{
		"PRAGMA journal_mode = WAL;",
		"PRAGMA synchronous = OFF;",
		"PRAGMA busy_timeout = 10000;",
		"PRAGMA temp_store = MEMORY;",
		"PRAGMA cache_size = -262144;",   // 256MB memory page cache
		"PRAGMA wal_autocheckpoint = 0;", // defer checkpoints; done manually at the end
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			return err
		}
	}

	schema := `
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
`
	_, err := db.Exec(schema)
	return err
}

func insertBatch(db *sql.DB, rows []*storage.Row) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.Prepare(`
INSERT OR IGNORE INTO lyrics (filename, content, isrc, platform_id, source, title, artist, duration_ms, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
`)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()

	for _, r := range rows {
		_, err := stmt.Exec(r.Filename, r.ContentJSON, r.ISRC, r.PlatformID, r.Source, r.Title, r.Artist, r.DurationMS, r.CreatedAt.UnixMilli())
		if err != nil {
			return err
		}
	}

	return tx.Commit()
}

func createIndexes(db *sql.DB) error {
	_, err := db.Exec(`
CREATE INDEX IF NOT EXISTS idx_lyrics_filename ON lyrics(filename);
CREATE INDEX IF NOT EXISTS idx_lyrics_isrc ON lyrics(isrc);
CREATE INDEX IF NOT EXISTS idx_lyrics_platform ON lyrics(platform_id);
CREATE INDEX IF NOT EXISTS idx_lyrics_title_artist ON lyrics(title, artist);
`)
	return err
}

func loadCheckpoint(path string) Checkpoint {
	data, err := os.ReadFile(path)
	if err != nil {
		return Checkpoint{FolderTokens: make(map[string]string)}
	}
	var cp Checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return Checkpoint{FolderTokens: make(map[string]string)}
	}
	if cp.FolderTokens == nil {
		cp.FolderTokens = make(map[string]string)
	}
	return cp
}

func saveCheckpoint(path string, cp Checkpoint) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return
	}
	data, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0644)
}
