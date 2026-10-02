package orchestrator

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lyricsplus/backend/internal/domain"
)

// countingSource simulates a provider that occupies a slot for a fixed time,
// then returns lyrics, so races take a predictable amount of wall time.
type countingSource struct {
	name    string
	delay   time.Duration
	calls   atomic.Int64
	live    atomic.Int32
	peak    atomic.Int32
	respond bool
}

func (c *countingSource) Name() string { return c.name }

func (c *countingSource) FetchLyrics(ctx context.Context, _ domain.SearchQuery) (*domain.LyricsResponse, error) {
	c.calls.Add(1)
	n := c.live.Add(1)
	for {
		p := c.peak.Load()
		if n <= p || c.peak.CompareAndSwap(p, n) {
			break
		}
	}
	defer c.live.Add(-1)

	select {
	case <-time.After(c.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if !c.respond {
		return nil, context.DeadlineExceeded
	}
	return &domain.LyricsResponse{
		Type:     domain.SyncTypeWord,
		Metadata: domain.LyricsMetadata{Source: c.name},
		Lyrics: []domain.Line{
			{Time: 0, Duration: 1000, Text: "x", Syllabus: []domain.Syllable{{Time: 0, Duration: 500, Text: "x "}}},
		},
	}, nil
}

// allSourceNames is the default SourceOrder, so every racing slot is populated.
var allSourceNames = []string{"apple", "lyricsplus", "deezer", "qq", "musixmatch-word", "musixmatch"}

func buildRacer(t *testing.T, limiter *Limiter, delay time.Duration, respond bool) (*Racer, []*countingSource) {
	t.Helper()
	var sources []Source
	var made []*countingSource
	for _, n := range allSourceNames {
		cs := &countingSource{name: n, delay: delay, respond: respond}
		sources = append(sources, cs)
		made = append(made, cs)
	}
	var opts []RacerOption
	if limiter != nil {
		opts = append(opts, WithLimiter(limiter))
	}
	return NewRacer(sources, 2*time.Second, opts...), made
}

// peakGoroutines samples the goroutine count while fn runs.
func peakGoroutines(t *testing.T, d time.Duration, fn func()) int {
	t.Helper()
	stop := make(chan struct{})
	var peak int64
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if n := int64(runtime.NumGoroutine()); n > peak {
				peak = n
			}
			time.Sleep(200 * time.Microsecond)
		}
	}()
	fn()
	close(stop)
	wg.Wait()
	return int(peak)
}

// TestRaceBoundedGoroutinesUnderLoad is the core memory-safety guarantee: peak
// concurrent provider fetches must stay at the limiter's global capacity rather
// than scaling with requests * sources.
//
// Without admission control, every request spawns one goroutine per source that
// lives for the full provider delay, so in-flight provider work grows linearly
// with request count.
func TestRaceBoundedGoroutinesUnderLoad(t *testing.T) {
	const (
		requests    = 400
		providerCap = 64
		delay       = 40 * time.Millisecond
	)

	limiter := NewLimiter(LimiterConfig{Global: providerCap, PerSource: 32, Wait: 50 * time.Millisecond})
	racer, sources := buildRacer(t, limiter, delay, true)

	var wg sync.WaitGroup
	gate := make(chan struct{})
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gate
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			racer.Race(ctx, domain.SearchQuery{
				Title:  "Bounded Song",
				Artist: "Load Test",
			}, nil)
		}()
	}

	// Sample the total number of provider fetches in flight. This is the
	// quantity the limiter bounds, and it is independent of the test's own
	// request-driver goroutines.
	var peakFetches atomic.Int64
	stop := make(chan struct{})
	var sampler sync.WaitGroup
	sampler.Add(1)
	go func() {
		defer sampler.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			var total int64
			for _, cs := range sources {
				total += int64(cs.live.Load())
			}
			for {
				p := peakFetches.Load()
				if total <= p || peakFetches.CompareAndSwap(p, total) {
					break
				}
			}
			time.Sleep(200 * time.Microsecond)
		}
	}()

	peakG := peakGoroutines(t, 4*time.Second, func() {
		close(gate)
		wg.Wait()
	})
	close(stop)
	sampler.Wait()

	// The hard invariant: in-flight provider fetches never exceed the global cap.
	if peakFetches.Load() > providerCap {
		t.Fatalf("peak concurrent provider fetches %d exceeded global cap %d (unbounded would reach ~%d)",
			peakFetches.Load(), providerCap, requests*len(allSourceNames))
	}

	// Secondary check: total goroutines must not scale with the fan-out. The
	// test itself holds `requests` driver goroutines, so the allowance is
	// requests + cap + slack; unbounded fan-out would need requests*sources.
	goroutineCeiling := requests + providerCap + 64
	if peakG > goroutineCeiling {
		t.Fatalf("peak goroutines %d exceeded %d (requests %d + cap %d + slack; unbounded fan-out would need ~%d)",
			peakG, goroutineCeiling, requests, providerCap, requests*len(allSourceNames))
	}

	t.Logf("peak provider fetches %d (cap %d); peak goroutines %d (ceiling %d, unbounded fan-out would need ~%d)",
		peakFetches.Load(), providerCap, peakG, goroutineCeiling, requests*len(allSourceNames))
}

// TestRacePerSourceCapIsEnforced verifies the per-provider cap holds under a
// fan-out much wider than the cap.
func TestRacePerSourceCapIsEnforced(t *testing.T) {
	const (
		requests = 200
		perSrc   = 8
		delay    = 30 * time.Millisecond
	)

	limiter := NewLimiter(LimiterConfig{Global: 512, PerSource: perSrc, Wait: 20 * time.Millisecond})
	racer, sources := buildRacer(t, limiter, delay, false)

	var wg sync.WaitGroup
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			racer.Race(ctx, domain.SearchQuery{Title: "Cap Test", Artist: "Load"}, nil)
		}()
	}
	wg.Wait()

	for _, cs := range sources {
		if got := cs.peak.Load(); got > perSrc {
			t.Errorf("provider %s peaked at %d concurrent fetches, cap is %d", cs.name, got, perSrc)
		}
	}
	t.Logf("per-provider peak concurrency stayed within cap %d across %d requests", perSrc, requests)
}

// TestRaceRejectionsRecordedAsSkip verifies overload is reported as a skip
// (admission rejection) rather than an upstream RTO, so capacity problems stay
// distinguishable in metrics.
func TestRaceRejectionsRecordedAsSkip(t *testing.T) {
	limiter := NewLimiter(LimiterConfig{Global: 1, PerSource: 1, Wait: time.Millisecond})
	racer, _ := buildRacer(t, limiter, 80*time.Millisecond, true)

	var skipped atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			res := racer.Race(ctx, domain.SearchQuery{Title: "Reject", Artist: "Load"}, nil)
			if res != nil {
				for _, o := range res.SourcesStatus {
					if o.Status == "SKIP" {
						skipped.Add(1)
					}
				}
			}
		}()
	}
	wg.Wait()

	if skipped.Load() == 0 {
		t.Fatal("expected admission rejections to be reported as SKIP under a global cap of 1")
	}
	if r := limiter.Rejected(); len(r) == 0 {
		t.Fatal("expected limiter to record rejections")
	}
	t.Logf("recorded %d SKIP outcomes, limiter rejections: %v", skipped.Load(), limiter.Rejected())
}
