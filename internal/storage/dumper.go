package storage

import (
	"compress/gzip"
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"lyricsplus/backend/internal/config"
	"lyricsplus/backend/internal/logger"
)

// Dumper manages scheduled SQLite database backups and uploads to Google Drive.
type Dumper struct {
	dbPath    string
	dumpDir   string
	interval  time.Duration
	gdrive    *GDriveClient
	store     *Store
	logger    *logger.Logger
	stopCh    chan struct{}
	mu        sync.Mutex
	isRunning bool
}

// NewDumper constructs a database backup manager.
func NewDumper(cfg config.Config, store *Store, gdrive *GDriveClient, logger *logger.Logger) *Dumper {
	dumpDir := cfg.GDrive.DailyDumpDir
	if dumpDir == "" {
		dumpDir = "data/dumps"
	}
	interval := cfg.GDrive.DailyDumpInterval
	if interval <= 0 {
		interval = 24 * time.Hour
	}

	return &Dumper{
		dbPath:   cfg.Storage.DBPath,
		dumpDir:  dumpDir,
		interval: interval,
		gdrive:   gdrive,
		store:    store,
		logger:   logger,
		stopCh:   make(chan struct{}),
	}
}

// Start launches the daily backup background worker.
func (d *Dumper) Start() {
	d.mu.Lock()
	if d.isRunning {
		d.mu.Unlock()
		return
	}
	d.isRunning = true
	d.mu.Unlock()

	go d.loop()
}

// Stop cleanly terminates the backup worker.
func (d *Dumper) Stop() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.isRunning {
		return
	}
	close(d.stopCh)
	d.isRunning = false
}

func (d *Dumper) loop() {
	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()

	d.logf("daily dump worker started (interval: %v, dir: %s)", d.interval, d.dumpDir)

	for {
		select {
		case <-d.stopCh:
			d.logf("daily dump worker stopped")
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			if err := d.DumpOnce(ctx); err != nil {
				d.logf("daily dump failed: %v", err)
			}
			cancel()
		}
	}
}

// DumpOnce creates an online snapshot of SQLite and optionally uploads to Google Drive.
func (d *Dumper) DumpOnce(ctx context.Context) error {
	if err := os.MkdirAll(d.dumpDir, 0755); err != nil {
		return fmt.Errorf("create dump dir: %w", err)
	}

	timestamp := time.Now().UTC().Format("2006-01-02_150405")
	tempDBPath := filepath.Join(d.dumpDir, fmt.Sprintf("lyricsplus_%s.tmp.db", timestamp))
	gzPath := filepath.Join(d.dumpDir, fmt.Sprintf("lyricsplus_backup_%s.db.gz", timestamp))

	// 1. Snapshot SQLite using VACUUM INTO (crash-consistent, lock-free)
	vacuumSQL := fmt.Sprintf("VACUUM INTO '%s'", tempDBPath)
	if _, err := d.store.db.ExecContext(ctx, vacuumSQL); err != nil {
		// Fallback to manual file copy if VACUUM INTO fails (e.g. older SQLite)
		if copyErr := copyFile(d.dbPath, tempDBPath); copyErr != nil {
			return fmt.Errorf("vacuum into failed: %v, copy failed: %w", err, copyErr)
		}
	}
	defer func() { _ = os.Remove(tempDBPath) }()

	// 2. Compress the snapshot with gzip
	if err := compressToGz(tempDBPath, gzPath); err != nil {
		return fmt.Errorf("gzip dump: %w", err)
	}

	d.logf("local database snapshot created: %s", gzPath)

	// 3. Upload to Google Drive if configured
	if d.gdrive != nil && d.gdrive.IsConfigured() {
		gzBytes, err := os.ReadFile(gzPath)
		if err != nil {
			return fmt.Errorf("read compressed dump: %w", err)
		}

		fileName := filepath.Base(gzPath)
		item, err := d.gdrive.UploadBackupFile(ctx, fileName, gzBytes)
		if err != nil {
			d.logf("failed to upload backup to GDrive: %v", err)
		} else if item != nil {
			d.logf("successfully uploaded daily backup to Google Drive: %s (id: %s)", fileName, item.ID)
		}
	}

	// 4. Prune local backups older than 7 days
	d.pruneOldBackups(7 * 24 * time.Hour)

	return nil
}

func compressToGz(srcPath, dstPath string) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()

	dst, err := os.Create(dstPath)
	if err != nil {
		return err
	}
	defer func() { _ = dst.Close() }()

	gz := gzip.NewWriter(dst)
	defer func() { _ = gz.Close() }()

	_, err = io.Copy(gz, src)
	return err
}

func copyFile(srcPath, dstPath string) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()

	dst, err := os.Create(dstPath)
	if err != nil {
		return err
	}
	defer func() { _ = dst.Close() }()

	_, err = io.Copy(dst, src)
	return err
}

func (d *Dumper) pruneOldBackups(retention time.Duration) {
	entries, err := os.ReadDir(d.dumpDir)
	if err != nil {
		return
	}

	cutoff := time.Now().Add(-retention)
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "lyricsplus_backup_") {
			continue
		}
		info, err := e.Info()
		if err == nil && info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(d.dumpDir, e.Name()))
		}
	}
}

func (d *Dumper) logf(format string, args ...interface{}) {
	if d.logger != nil {
		d.logger.Infof("[Dumper] "+format, args...)
	}
}

// GetDB exposes underlying db for vacuum operations.
func (s *Store) GetDB() *sql.DB {
	return s.db
}
