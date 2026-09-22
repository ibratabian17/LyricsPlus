package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"runtime"
	"sort"
	"strings"
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

func runProbe(dbPath string, songs []SongPair) {
	cfg := config.Load()
	cfg.Storage.DBPath = dbPath
	cfg.Storage.LRUSize = 4096
	st, err := storage.NewStore(cfg.Storage)
	if err != nil {
		fmt.Printf("NewStore failed: %v\n", err)
		return
	}
	defer st.Close()
	racer := orchestrator.NewRacer(nil, 30*time.Second)
	svc := &service.Service{
		Dedup:    orchestrator.NewDedup(racer),
		Store:    st,
		MemCache: storage.NewMemoryCache(cfg.Cache),
	}

	var (
		svcMiss   int
		rowAbsent int
		contentF  int
	)
	for _, s := range songs {
		ctx := context.Background()
		resp, sErr := svc.FetchLyrics(ctx, domain.SearchQuery{Title: s.Title, Artist: s.Artist}, nil, false)
		if sErr == nil && resp != nil && len(resp.Lyrics) > 0 {
			continue
		}
		svcMiss++
		rows, ok := st.GetByTitleArtist(ctx, s.Title, s.Artist)
		var (
			status  = "ROW_ABSENT"
			content []byte
		)
		if ok && len(rows) > 0 {
			c, cErr := st.GetContent(ctx, rows[0].ID)
			switch {
			case cErr != nil:
				status = "CONTENT_ERR"
			case len(c) == 0:
				status = "CONTENT_EMPTY"
			default:
				status = "CONTENT_FAIL"
				contentF++
			}
			content = c
		} else {
			rowAbsent++
		}
		if svcMiss <= 40 {
			fmt.Printf("%-13s title=%q artist=%q err=%v %s\n",
				status, s.Title, s.Artist, sErr, previewOf(content))
		}
	}
	fmt.Printf("\nProbe summary: %d/%d songs miss the service path (row_absent=%d, content_fail=%d)\n",
		svcMiss, len(songs), rowAbsent, contentF)
}

func previewOf(content []byte) string {
	if len(content) == 0 {
		return ""
	}
	s := strings.ReplaceAll(strings.ReplaceAll(string(content), "\n", " "), "\t", " ")
	if len(s) > 140 {
		s = s[:140]
	}
	return "content: " + s
}

func main() {
	dbPath := flag.String("db", "database/lyrics_cache.db", "path to SQLite cache DB")
	numSongs := flag.Int("songs", 5000, "number of distinct songs to load for the workload")
	skipDirect := flag.Bool("skip-direct", false, "skip the in-process service test (TEST 1)")
	skipHTTP := flag.Bool("skip-http", false, "skip the HTTP endpoint test (TEST 2)")
	baseURL := flag.String("url", "http://127.0.0.1:3000", "base URL of the running server")
	httpDur := flag.Duration("duration", 10*time.Second, "duration of the HTTP load test")
	httpConc := flag.Int("concurrency", 300, "concurrent HTTP workers for the load test")
	httpTimeout := flag.Duration("timeout", 2*time.Second, "per-request HTTP timeout")
	warmup := flag.Bool("warmup", false, "prime server caches by requesting each song once before the timed test")
	probe := flag.Bool("probe", false, "replay each loaded song through service+store and classify misses")
	flag.Parse()

	fmt.Printf("Loading %d distinct songs from %s...\n", *numSongs, *dbPath)
	songs, err := loadDistinctSongs(*dbPath, *numSongs)
	if err != nil || len(songs) == 0 {
		fmt.Printf("Failed to load songs: %v\n", err)
		return
	}
	fmt.Printf("Loaded %d distinct songs for simulation.\n\n", len(songs))

	if *probe {
		runProbe(*dbPath, songs)
		return
	}

	// -------------------------------------------------------------
	// PART 1: In-Process Service & Storage Engine (Testing 120K req/s)
	// -------------------------------------------------------------
	if !*skipDirect {
		fmt.Println("==================================================================")
		fmt.Println("TEST 1: In-Process Service & Storage Engine Stress Test")
		fmt.Printf("CPUs: %d | Target Throughput: High Concurrency (120K+ req/s target)\n", runtime.NumCPU())
		fmt.Println("==================================================================")

		cfg := config.Load()
		cfg.Storage.DBPath = *dbPath
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
	}

	// -------------------------------------------------------------
	// PART 2: HTTP Network Load Test (Across Diverse Songs)
	// -------------------------------------------------------------
	if !*skipHTTP {
		fmt.Println("\n==================================================================")
		fmt.Println("TEST 2: HTTP Endpoint Network Load Test (/v2/lyrics/get)")
		fmt.Printf("URL: %s | concurrency: %d | duration: %s\n", *baseURL, *httpConc, *httpDur)
		fmt.Println("==================================================================")
		testHTTPEndpoint(songs, *baseURL, *httpDur, *httpConc, *httpTimeout, *warmup)
	}
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

func testHTTPEndpoint(songs []SongPair, baseURL string, duration time.Duration, concurrency int, timeout time.Duration, warmup bool) {
	targetURL := strings.TrimRight(baseURL, "/") + "/v2/lyrics/get"
	tr := &http.Transport{
		MaxIdleConns:        20000,
		MaxIdleConnsPerHost: 5000,
		IdleConnTimeout:     30 * time.Second,
	}
	client := &http.Client{Transport: tr, Timeout: timeout}

	// Verify server is reachable
	resp, err := client.Get(strings.TrimRight(baseURL, "/") + "/health")
	if err != nil {
		fmt.Printf("HTTP server not running on port 3000 (%v). Skipping HTTP test.\n", err)
		return
	}
	_ = resp.Body.Close()

	urls := make([]string, len(songs))
	for i, s := range songs {
		urls[i] = fmt.Sprintf("%s?title=%s&artist=%s", targetURL, url.QueryEscape(s.Title), url.QueryEscape(s.Artist))
	}

	if warmup {
		const warmWorkers = 100
		fmt.Printf("Warming server caches with all %d songs (%d workers)...\n", len(urls), warmWorkers)
		var wg sync.WaitGroup
		warmIdx := make(chan int, 256)
		var doneCount atomic.Int64
		for i := 0; i < warmWorkers; i++ {
			wg.Add(1)
			go func(workerID int) {
				defer wg.Done()
				rng := rand.New(rand.NewSource(time.Now().UnixNano() + int64(workerID)))
				for idx := range warmIdx {
					req, _ := http.NewRequest("GET", urls[idx], nil)
					req.Header.Set("X-Forwarded-For", fmt.Sprintf("192.168.%d.%d", rng.Intn(254)+1, rng.Intn(254)+1))
					r, err := client.Do(req)
					if err != nil {
						fmt.Printf("  warmup #%d failed: %v\n", idx, err)
					} else {
						_, _ = io.Copy(io.Discard, r.Body)
						_ = r.Body.Close()
					}
					n := doneCount.Add(1)
					if n%1000 == 0 {
						fmt.Printf("  warmed %d/%d\n", n, len(urls))
					}
				}
			}(i)
		}
		for i := range urls {
			warmIdx <- i
		}
		close(warmIdx)
		wg.Wait()
		fmt.Printf("Warmup complete.\n\n")
	}

	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()

	var totalReqs, okReqs, errReqs atomic.Int64
	var code2xx, code4xx, code5xx, rateLimited atomic.Int64
	numSongs := len(urls)
	start := time.Now()
	var wg sync.WaitGroup

	stopTick := make(chan struct{})
	var tickWg sync.WaitGroup
	tickWg.Add(1)
	go func() {
		defer tickWg.Done()
		last := int64(0)
		lastT := time.Now()
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-stopTick:
				return
			case <-t.C:
				nowT := time.Now()
				cur := totalReqs.Load()
				fmt.Printf("  [%s] %8d req  (%.0f req/s)  err=%d\n",
					time.Since(start).Round(time.Second), cur,
					float64(cur-last)/nowT.Sub(lastT).Seconds(), errReqs.Load())
				last = cur
				lastT = nowT
			}
		}
	}()

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
					u := urls[rng.Intn(numSongs)]
					req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
					req.Header.Set("X-Forwarded-For", fmt.Sprintf("192.168.%d.%d", (workerID)%254+1, rng.Intn(254)+1))

					r, err := client.Do(req)
					if err != nil {
						errReqs.Add(1)
						continue
					}
					totalReqs.Add(1)
					switch {
					case r.StatusCode == 200:
						okReqs.Add(1)
					case r.StatusCode == 429:
						rateLimited.Add(1)
						code4xx.Add(1)
					case r.StatusCode >= 500:
						code5xx.Add(1)
					case r.StatusCode >= 400:
						code4xx.Add(1)
					default:
						code2xx.Add(1)
					}
					_ = r.Body.Close()
				}
			}
		}(i)
	}

	wg.Wait()
	close(stopTick)
	tickWg.Wait()
	actual := time.Since(start)
	tot := totalReqs.Load()
	fmt.Printf("Total HTTP Requests:     %d\n", tot)
	fmt.Printf("Elapsed Time:            %v\n", actual.Round(time.Millisecond))
	fmt.Printf("HTTP THROUGHPUT:         %.2f req/sec\n", float64(tot)/actual.Seconds())
	fmt.Printf("200 OK:                  %d\n", okReqs.Load())
	fmt.Printf("2xx (non-200):           %d\n", code2xx.Load())
	fmt.Printf("4xx:                     %d (429=%d)\n", code4xx.Load(), rateLimited.Load())
	fmt.Printf("5xx:                     %d\n", code5xx.Load())
	fmt.Printf("Network/Other Errors:    %d\n", errReqs.Load())
	fmt.Printf("Avg request rate /worker: %.0f req/s\n", float64(tot)/actual.Seconds()/float64(concurrency))
}
