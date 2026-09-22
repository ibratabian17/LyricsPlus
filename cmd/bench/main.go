package main

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"net/http"
	"net/url"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"

	"lyricsplus/backend/internal/config"
	"lyricsplus/backend/internal/domain"
	"lyricsplus/backend/internal/orchestrator"
	"lyricsplus/backend/internal/service"
	"lyricsplus/backend/internal/storage"
)

type SongPair struct {
	Title  string
	Artist string
}

func loadDistinctSongs(dbPath string, limit int) ([]SongPair, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.Query("SELECT title, artist FROM lyrics WHERE title != '' AND artist != '' LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var songs []SongPair
	for rows.Next() {
		var s SongPair
		if err := rows.Scan(&s.Title, &s.Artist); err == nil {
			songs = append(songs, s)
		}
	}
	return songs, nil
}

func main() {
	dbPath := "database/lyrics_cache.db"
	numSongs := 5000
	fmt.Printf("Loading %d distinct songs from %s...\n", numSongs, dbPath)
	songs, err := loadDistinctSongs(dbPath, numSongs)
	if err != nil || len(songs) == 0 {
		fmt.Printf("Failed to load songs: %v\n", err)
		return
	}
	fmt.Printf("Loaded %d distinct songs for simulation.\n\n", len(songs))

	// -------------------------------------------------------------
	// PART 1: In-Process Service & Storage Engine (Testing 120K req/s)
	// -------------------------------------------------------------
	fmt.Println("==================================================================")
	fmt.Println("TEST 1: In-Process Service & Storage Engine Stress Test")
	fmt.Printf("CPUs: %d | Target Throughput: High Concurrency (120K+ req/s target)\n", runtime.NumCPU())
	fmt.Println("==================================================================")

	cfg := config.Load()
	cfg.Storage.DBPath = dbPath
	cfg.Storage.LRUSize = 10000
	cfg.Cache.MaxEntries = 10000
	st, err := storage.NewStore(cfg.Storage)
	if err != nil {
		fmt.Printf("NewStore failed: %v\n", err)
		return
	}
	defer st.Close()

	memCache := storage.NewMemoryCache(cfg.Cache)
	racer := orchestrator.NewRacer(nil, 2*time.Second)
	dedup := orchestrator.NewDedup(racer)

	svc := &service.Service{
		Dedup:    dedup,
		Store:    st,
		MemCache: memCache,
	}

	// Warm up cache with first 500 songs
	warmupCtx := context.Background()
	for i := 0; i < 500 && i < len(songs); i++ {
		_, _ = svc.FetchLyrics(warmupCtx, domain.SearchQuery{Title: songs[i].Title, Artist: songs[i].Artist}, nil, false)
	}

	testDirectService(svc, songs, 10*time.Second, 1000)

	// -------------------------------------------------------------
	// PART 2: HTTP Network Load Test (Across Diverse Songs)
	// -------------------------------------------------------------
	fmt.Println("\n==================================================================")
	fmt.Println("TEST 2: HTTP Endpoint Network Load Test (/v2/lyrics/get)")
	fmt.Println("==================================================================")
	testHTTPEndpoint(songs, 10*time.Second, 300)
}

func testDirectService(svc *service.Service, songs []SongPair, duration time.Duration, concurrency int) {
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()

	var (
		totalOps  atomic.Int64
		hitCount  atomic.Int64
		missCount atomic.Int64
		latencies []time.Duration
		latMu     sync.Mutex
	)

	start := time.Now()
	var wg sync.WaitGroup

	numSongs := len(songs)

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(time.Now().UnixNano() + int64(workerID)))
			var localLats []time.Duration

			for {
				select {
				case <-ctx.Done():
					latMu.Lock()
					if len(localLats) > 10000 {
						latencies = append(latencies, localLats[:10000]...)
					} else {
						latencies = append(latencies, localLats...)
					}
					latMu.Unlock()
					return
				default:
					idx := rng.Intn(numSongs)
					q := domain.SearchQuery{
						Title:  songs[idx].Title,
						Artist: songs[idx].Artist,
					}

					t0 := time.Now()
					resp, err := svc.FetchLyrics(ctx, q, nil, false)
					elapsed := time.Since(t0)

					localLats = append(localLats, elapsed)
					totalOps.Add(1)

					if err == nil && resp != nil && len(resp.Lyrics) > 0 {
						hitCount.Add(1)
					} else {
						missCount.Add(1)
					}
				}
			}
		}(i)
	}

	wg.Wait()
	actualDuration := time.Since(start)

	tot := totalOps.Load()
	opsPerSec := float64(tot) / actualDuration.Seconds()

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })

	var p50, p90, p99, max time.Duration
	if len(latencies) > 0 {
		p50 = latencies[len(latencies)*50/100]
		p90 = latencies[len(latencies)*90/100]
		p99 = latencies[len(latencies)*99/100]
		max = latencies[len(latencies)-1]
	}

	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	fmt.Printf("Total Queries Executed:  %d\n", tot)
	fmt.Printf("Elapsed Time:            %v\n", actualDuration.Round(time.Millisecond))
	fmt.Printf("THROUGHPUT:              %.2f queries/sec\n", opsPerSec)
	fmt.Printf("Cache/Store Hits:        %d (%.1f%%)\n", hitCount.Load(), float64(hitCount.Load())*100/float64(tot))
	fmt.Printf("Latency p50:             %v\n", p50.Round(time.Nanosecond))
	fmt.Printf("Latency p90:             %v\n", p90.Round(time.Nanosecond))
	fmt.Printf("Latency p99:             %v\n", p99.Round(time.Nanosecond))
	fmt.Printf("Latency Max:             %v\n", max.Round(time.Microsecond))
	fmt.Printf("Heap In-Use:             %.1f MB (Alloc: %.1f MB, Sys: %.1f MB)\n",
		float64(m.HeapInuse)/(1<<20), float64(m.Alloc)/(1<<20), float64(m.Sys)/(1<<20))
}

func testHTTPEndpoint(songs []SongPair, duration time.Duration, concurrency int) {
	targetURL := "http://127.0.0.1:3000/v2/lyrics/get"
	tr := &http.Transport{
		MaxIdleConns:        20000,
		MaxIdleConnsPerHost: 5000,
		IdleConnTimeout:     30 * time.Second,
	}
	client := &http.Client{Transport: tr, Timeout: 2 * time.Second}

	// Verify server is reachable
	resp, err := client.Get("http://127.0.0.1:3000/health")
	if err != nil {
		fmt.Printf("HTTP server not running on port 3000 (%v). Skipping HTTP test.\n", err)
		return
	}
	_ = resp.Body.Close()

	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()

	var totalReqs, okReqs, errReqs atomic.Int64
	numSongs := len(songs)
	start := time.Now()
	var wg sync.WaitGroup

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(time.Now().UnixNano() + int64(workerID)))

			for {
				select {
				case <-ctx.Done():
					return
				default:
					s := songs[rng.Intn(numSongs)]
					u := fmt.Sprintf("%s?title=%s&artist=%s", targetURL, url.QueryEscape(s.Title), url.QueryEscape(s.Artist))
					req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
					req.Header.Set("X-Forwarded-For", fmt.Sprintf("192.168.%d.%d", (workerID)%254+1, rng.Intn(254)+1))

					r, err := client.Do(req)
					if err != nil {
						errReqs.Add(1)
						continue
					}
					totalReqs.Add(1)
					if r.StatusCode == 200 {
						okReqs.Add(1)
					}
					_ = r.Body.Close()
				}
			}
		}(i)
	}

	wg.Wait()
	actual := time.Since(start)
	tot := totalReqs.Load()
	fmt.Printf("Total HTTP Requests:     %d\n", tot)
	fmt.Printf("Elapsed Time:            %v\n", actual.Round(time.Millisecond))
	fmt.Printf("HTTP THROUGHPUT:         %.2f req/sec\n", float64(tot)/actual.Seconds())
	fmt.Printf("200 OK:                  %d\n", okReqs.Load())
	fmt.Printf("Network/Other Errors:    %d\n", errReqs.Load())
}
