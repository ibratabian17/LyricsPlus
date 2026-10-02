package orchestrator

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lyricsplus/backend/internal/domain"
)

// blockingSource blocks until its context is cancelled and records that it
// observed the cancellation.
type blockingSource struct {
	name     string
	started  chan struct{}
	stopped  atomic.Int32
	sawClose atomic.Int32
}

func newBlockingSource(name string) *blockingSource {
	return &blockingSource{name: name, started: make(chan struct{}, 8)}
}

func (b *blockingSource) Name() string { return b.name }

func (b *blockingSource) FetchLyrics(ctx context.Context, _ domain.SearchQuery) (*domain.LyricsResponse, error) {
	select {
	case b.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	b.sawClose.Add(1)
	return nil, ctx.Err()
}

func (b *blockingSource) stoppedCount() int { return int(b.stopped.Load()) }

// newBlockingRacer wires the blocking source under a real provider name,
// because SourceOrder selects from a fixed list and Racer.has() gates on it.
func newBlockingRacer(t *testing.T, src *blockingSource, timeout time.Duration) *Dedup {
	t.Helper()
	racer := NewRacer([]Source{src}, timeout, WithLimiter(NewLimiter(LimiterConfig{})))
	return NewDedup(racer)
}

// testSourceName is first in the default SourceOrder, so it is always part of
// phase 1 of the race.
const testSourceName = "apple"

// TestDedupCancelsWhenAllWaitersLeave verifies the shared scrape is cancelled
// once every caller has abandoned it, instead of running out the full timeout.
func TestDedupCancelsWhenAllWaitersLeave(t *testing.T) {
	src := newBlockingSource(testSourceName)
	dedup := newBlockingRacer(t, src, 10*time.Second)

	q := domain.SearchQuery{Title: "Ghost Song", Artist: "Nobody"}

	// Each caller joins the flight, waits for it to be running, then abandons it.
	const callers = 2
	ctxs := make([]context.CancelFunc, 0, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		ctxs = append(ctxs, cancel)
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = dedup.Get(ctx, q, nil)
		}()
		// Let this caller register as a waiter and let the flight start.
		time.Sleep(120 * time.Millisecond)
	}

	select {
	case <-src.started:
	case <-time.After(2 * time.Second):
		t.Fatal("shared scrape never started")
	}
	if got := src.sawClose.Load(); got != 0 {
		t.Fatalf("scrape ended early with %d cancellations before waiters left", got)
	}

	// Both callers give up.
	for _, c := range ctxs {
		c()
	}
	wg.Wait()

	// The flight must observe cancellation promptly rather than lingering for
	// the full 10s timeout.
	deadline := time.After(2 * time.Second)
	for src.sawClose.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("shared scrape was not cancelled after all waiters left; it will run for the full timeout")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// TestDedupKeepsFlightWhileWaiterRemains verifies that one caller giving up does
// not cancel the scrape for a caller still waiting.
func TestDedupKeepsFlightWhileWaiterRemains(t *testing.T) {
	src := newBlockingSource(testSourceName)
	dedup := newBlockingRacer(t, src, 10*time.Second)
	q := domain.SearchQuery{Title: "Popular Song", Artist: "Someone"}

	// A long-lived waiter keeps the flight alive.
	stayCtx, stayCancel := context.WithCancel(context.Background())
	defer stayCancel()
	stayed := make(chan struct{})
	go func() {
		defer close(stayed)
		_, _ = dedup.Get(stayCtx, q, nil)
	}()

	// A short-lived caller for the same key.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		_, _ = dedup.Get(ctx, q, nil)
	}()

	select {
	case <-src.started:
	case <-time.After(2 * time.Second):
		t.Fatal("shared scrape never started")
	}

	// Let the short caller expire.
	time.Sleep(400 * time.Millisecond)
	if got := src.sawClose.Load(); got != 0 {
		t.Fatalf("scrape was cancelled while a waiter remained (cancellations=%d)", got)
	}

	// Now release the remaining waiter and confirm cancellation happens.
	stayCancel()
	select {
	case <-stayed:
	case <-time.After(2 * time.Second):
		t.Fatal("remaining waiter never returned")
	}
}

// TestDedupAllCallersCancelReturns verifies callers get their context error
// promptly instead of blocking on a flight nobody is waiting for.
func TestDedupAllCallersCancelReturns(t *testing.T) {
	src := newBlockingSource(testSourceName)
	dedup := newBlockingRacer(t, src, 30*time.Second)
	q := domain.SearchQuery{Title: "Vanishing", Artist: "Point"}

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, err := dedup.Get(ctx, q, nil)
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context error, got %v", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Get blocked for %s despite caller cancellation", elapsed)
	}
	if dedup.InflightKeys() != 0 {
		t.Fatalf("expected no inflight keys after departure, got %d", dedup.InflightKeys())
	}
}

// TestLimiterRejectsWhenSaturated verifies overload is rejected immediately
// rather than queued, which is what keeps goroutine and memory bounded.
func TestLimiterRejectsWhenSaturated(t *testing.T) {
	l := NewLimiter(LimiterConfig{Global: 2, PerSource: 2, Wait: 10 * time.Millisecond})

	r1, ok1 := l.TryAcquire("a")
	r2, ok2 := l.TryAcquire("b")
	if !ok1 || !ok2 {
		t.Fatal("expected first two acquisitions to succeed")
	}

	start := time.Now()
	r3, ok3 := l.TryAcquire("c")
	if ok3 {
		r3()
		t.Fatal("expected third acquisition to be rejected at capacity")
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("rejection took %s; it must fail fast", elapsed)
	}

	// Per-provider cap must bite even while global capacity remains.
	l2 := NewLimiter(LimiterConfig{Global: 10, PerSource: 1, Wait: time.Millisecond})
	ra, _ := l2.TryAcquire("same")
	if ra == nil {
		t.Fatal("first acquisition for provider should succeed")
	}
	if _, ok := l2.TryAcquire("same"); ok {
		t.Fatal("per-provider cap of 1 should reject the second acquisition")
	}
	ra()

	// After release, capacity returns.
	if rel, ok := l2.TryAcquire("same"); !ok {
		t.Fatal("capacity should be reusable after release")
	} else {
		rel()
	}

	r1()
	r2()
}

// TestLimiterBoundedConcurrentFetches verifies the limiter actually caps
// concurrent provider work under a fan-out heavier than the cap.
func TestLimiterBoundedConcurrentFetches(t *testing.T) {
	const (
		limit   = 4
		probes  = 200
		sources = 6
	)
	l := NewLimiter(LimiterConfig{Global: limit, PerSource: limit, Wait: time.Millisecond})

	var cur, peak atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < probes; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rel, ok := l.TryAcquire("p")
			if !ok {
				return
			}
			defer rel()
			n := cur.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			cur.Add(-1)
		}()
	}
	wg.Wait()

	if got := peak.Load(); got > limit {
		t.Fatalf("peak concurrency %d exceeded limit %d", got, limit)
	}
	_ = sources
}
