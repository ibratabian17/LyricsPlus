package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"lyricsplus/backend/internal/config"
	"lyricsplus/backend/internal/storage"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	command := os.Args[1]
	switch command {
	case "merge":
		runMerge(os.Args[2:])
	case "stats":
		runStats(os.Args[2:])
	case "help", "-h", "--help":
		printUsage()
	default:
		// If first arg starts with a flag (e.g. --source), treat as merge
		if len(command) > 0 && command[0] == '-' {
			runMerge(os.Args[1:])
		} else {
			fmt.Printf("Unknown command %q\n\n", command)
			printUsage()
			os.Exit(1)
		}
	}
}

func printUsage() {
	fmt.Println("=================================================================")
	fmt.Println("             LyricsPlus - Database Merger & Tool                ")
	fmt.Println("=================================================================")
	fmt.Println("Usage:")
	fmt.Println("  db_merger merge [options]    Merge one or more databases into target DB")
	fmt.Println("  db_merger stats [options]    Display lyrics count, quality & source stats")
	fmt.Println("  db_merger help               Show this help message")
	fmt.Println()
	fmt.Println("Examples:")
	fmt.Println("  # Quality-first merge of Apple Music scraper DB into main cache:")
	fmt.Println("  db_merger merge --source=apple-music-scraper/apple_lyrics.db")
	fmt.Println()
	fmt.Println("  # Merge multiple external dump files with vacuum:")
	fmt.Println("  db_merger merge --source=\"data/dumps/*.db\" --target=database/lyrics_cache.db --vacuum")
	fmt.Println()
	fmt.Println("  # Inspect database metrics and sync type distribution:")
	fmt.Println("  db_merger stats --db=database/lyrics_cache.db")
	fmt.Println()
}

func runMerge(args []string) {
	fs := flag.NewFlagSet("merge", flag.ExitOnError)
	sourceArg := fs.String("source", "", "Source SQLite DB path or glob pattern (e.g. apple_lyrics.db, dumps/*.db)")
	targetArg := fs.String("target", "", "Target SQLite DB path (default from config or database/lyrics_cache.db)")
	strategyArg := fs.String("strategy", "quality", "Conflict strategy: quality (syllable > line > plain), newer, insert-only, force")
	batchSize := fs.Int("batch-size", 1000, "Number of rows per transaction batch")
	vacuum := fs.Bool("vacuum", false, "Run WAL checkpoint & VACUUM on target after merge")
	rebuildFTS := fs.Bool("rebuild-fts", false, "Rebuild FTS5 full-text index after merge")
	configPath := fs.String("config", "", "Path to configuration file (.env or config.json)")
	_ = fs.Parse(args)

	cfg := config.Load(*configPath)
	targetDB := *targetArg
	if targetDB == "" {
		targetDB = cfg.Storage.DBPath
	}
	if targetDB == "" {
		targetDB = "database/lyrics_cache.db"
	}

	if *sourceArg == "" {
		fmt.Println("[!] Error: --source is required (e.g. --source=apple-music-scraper/apple_lyrics.db)")
		os.Exit(1)
	}

	// Expand glob patterns or comma-separated lists
	var sources []string
	matches, err := filepath.Glob(*sourceArg)
	if err == nil && len(matches) > 0 {
		sources = matches
	} else {
		sources = []string{*sourceArg}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	fmt.Println("=================================================================")
	fmt.Println("             LyricsPlus - High Performance DB Merger             ")
	fmt.Println("=================================================================")
	fmt.Printf("Target Database: %s\n", targetDB)
	fmt.Printf("Merge Strategy:  %s\n", *strategyArg)
	fmt.Printf("Batch Size:      %d\n", *batchSize)
	fmt.Printf("Sources Found:   %d\n", len(sources))
	for i, s := range sources {
		fmt.Printf("  [%d] %s\n", i+1, s)
	}
	fmt.Println("-----------------------------------------------------------------")

	logFunc := func(format string, args ...any) {
		fmt.Printf("[%s] %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
	}

	mCfg := storage.MergeConfig{
		SourcePaths: sources,
		TargetDB:    targetDB,
		Strategy:    storage.MergeStrategy(*strategyArg),
		BatchSize:   *batchSize,
		RebuildFTS:  *rebuildFTS,
		Vacuum:      *vacuum,
		LogFunc:     logFunc,
	}

	stats, err := storage.MergeDatabases(ctx, mCfg)
	if err != nil {
		fmt.Printf("\n[!] Merge failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("\n=================================================================")
	fmt.Println("                       Merge Completed!                          ")
	fmt.Println("=================================================================")
	fmt.Printf("Elapsed Time:    %v\n", stats.Duration.Round(time.Millisecond))
	fmt.Printf("Total Examined:  %d records\n", stats.TotalExamined)
	fmt.Printf("Inserted (New):  %d\n", stats.Inserted)
	fmt.Printf("Upgraded:        %d (better lyrics quality or newer timestamp)\n", stats.Upgraded)
	fmt.Printf("Skipped (Same):  %d (identical quality / older)\n", stats.SkippedSame)
	fmt.Printf("Skipped (Lower): %d (source was lower quality than target)\n", stats.SkippedLower)
	if stats.SkippedExists > 0 {
		fmt.Printf("Skipped (Exist): %d\n", stats.SkippedExists)
	}
	fmt.Printf("Errors:          %d\n", stats.Errors)
	fmt.Printf("Target Database: %s\n", targetDB)
	fmt.Println("=================================================================")
}

func runStats(args []string) {
	fs := flag.NewFlagSet("stats", flag.ExitOnError)
	dbArg := fs.String("db", "", "Path to SQLite database (default: from config or database/lyrics_cache.db)")
	configPath := fs.String("config", "", "Path to configuration file (.env or config.json)")
	_ = fs.Parse(args)

	cfg := config.Load(*configPath)
	targetDB := *dbArg
	if targetDB == "" {
		targetDB = cfg.Storage.DBPath
	}
	if targetDB == "" {
		targetDB = "database/lyrics_cache.db"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	fmt.Printf("[*] Analyzing database: %s ...\n", targetDB)
	stats, err := storage.GetDatabaseStats(ctx, targetDB)
	if err != nil {
		fmt.Printf("[!] Failed to analyze database: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("=================================================================")
	fmt.Println("                 Lyrics Database Inspection                      ")
	fmt.Println("=================================================================")
	fmt.Printf("Database:        %s\n", stats.DatabasePath)
	fmt.Printf("File Size:       %.2f MB\n", float64(stats.FileSize)/(1024*1024))
	if stats.WALSize > 0 {
		fmt.Printf("WAL Size:        %.2f MB\n", float64(stats.WALSize)/(1024*1024))
	}
	fmt.Printf("Total Lyrics:    %d\n", stats.TotalRows)
	fmt.Println("-----------------------------------------------------------------")
	fmt.Println("Source Breakdown:")
	if len(stats.Sources) == 0 {
		fmt.Println("  (No rows found)")
	} else {
		for src, count := range stats.Sources {
			pct := float64(count) / float64(stats.TotalRows) * 100.0
			fmt.Printf("  • %-16s : %7d (%5.1f%%)\n", src, count, pct)
		}
	}
	fmt.Println("-----------------------------------------------------------------")
	fmt.Println("Sync Quality Breakdown:")
	if len(stats.SyncTypes) == 0 {
		fmt.Println("  (No rows found)")
	} else {
		for qType, count := range stats.SyncTypes {
			pct := float64(count) / float64(stats.TotalRows) * 100.0
			fmt.Printf("  • %-16s : %7d (%5.1f%%)\n", qType, count, pct)
		}
	}
	fmt.Println("=================================================================")
}
