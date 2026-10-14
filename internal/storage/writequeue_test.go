package storage

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lyricsplus/backend/internal/config"
	"lyricsplus/backend/internal/domain"
)

func TestWriteQueueDrainsOnClose(t *testing.T) {
	q := newWriteQueue(2, 128)
	var done atomic.Int64
	for i := 0; i < 64; i++ {
		q.Submit(context.Background(), func(context.Context) {
			time.Sleep(time.Millisecond)
			done.Add(1)
		})
	}
	q.CloseWithin(5 * time.Second)
	if got := done.Load(); got != 64 {
		t.Fatalf("expected all 64 writes to drain, got %d", got)
	}
}

func TestWriteQueueBoundsConcurrency(t *testing.T) {
	const workers = 3
	q := newWriteQueue(workers, 512)

	var live, peak atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			q.Submit(context.Background(), func(context.Context) {
				n := live.Add(1)
				for {
					p := peak.Load()
					if n <= p || peak.CompareAndSwap(p, n) {
						break
					}
				}
				time.Sleep(200 * time.Microsecond)
				live.Add(-1)
			})
		}()
	}
	wg.Wait()
	q.CloseWithin(5 * time.Second)

	if got := peak.Load(); got > workers {
		t.Fatalf("peak concurrent writes %d exceeded worker count %d", got, workers)
	}
}

func TestWriteQueueShedsInsteadOfBlocking(t *testing.T) {
	q := newWriteQueue(1, 8)

	block := make(chan struct{})

	q.Submit(context.Background(), func(context.Context) { <-block })

	var completed atomic.Int64
	deadline := time.Now().Add(2 * time.Second)
	for i := 0; i < 500; i++ {
		q.Submit(context.Background(), func(context.Context) { completed.Add(1) })
		if time.Now().After(deadline) {
			t.Fatal("Submit blocked under saturation; it must shed instead")
		}
	}

	close(block)
	q.CloseWithin(5 * time.Second)

	shed, _, _ := q.Stats()
	if shed == 0 {
		t.Fatalf("expected shed jobs under saturation, got %d (completed %d)", shed, completed.Load())
	}

	if completed.Load() == 500 {
		t.Fatal("expected shedding to drop some jobs, but all 500 ran")
	}
	t.Logf("shed %d jobs, completed %d of 500 offered", shed, completed.Load())
}

func TestWriteQueueRecoversPanic(t *testing.T) {
	q := newWriteQueue(1, 16)
	var ran atomic.Int64
	q.Submit(context.Background(), func(context.Context) { panic("boom") })
	q.Submit(context.Background(), func(context.Context) { ran.Add(1) })
	q.CloseWithin(5 * time.Second)

	if ran.Load() != 1 {
		t.Fatalf("worker did not survive a panicking job; subsequent job ran=%d", ran.Load())
	}
}

func TestWriteQueueSubmitAfterClose(t *testing.T) {
	q := newWriteQueue(1, 8)
	q.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			q.Submit(context.Background(), func(context.Context) {})
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Submit panicked or blocked after Close")
	}
}

func TestSaveUserLyricsAsyncPersists(t *testing.T) {
	dbPath := t.TempDir() + "/async.db"
	st, err := NewStore(config.Storage{DBPath: dbPath, LRUSize: 32})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer func() { _ = st.Close() }()

	content := []byte(`{"type":"line","lyrics":[{"time":1,"text":"async"}]}`)
	st.SaveUserLyricsAsync(domain.SearchQuery{
		Title:  "Async Song",
		Artist: "Queue",
	}, content, 5*time.Second)

	copy(content, []byte("XXXXXXXXXXXXXXXXXXXXXXXX"))

	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	st2, err := NewStore(config.Storage{DBPath: dbPath, LRUSize: 32})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = st2.Close() }()

	rows, ok := st2.GetByTitleArtist(context.Background(), "Async Song", "Queue")
	if !ok || len(rows) == 0 {
		t.Fatal("async write did not persist")
	}
	got, err := st2.GetContent(context.Background(), rows[0].ID)
	if err != nil {
		t.Fatalf("GetContent: %v", err)
	}
	if !bytesEqual(got, []byte(`{"type":"line","lyrics":[{"time":1,"text":"async"}]}`)) {
		t.Fatalf("stored content corrupted by caller buffer reuse: %s", got)
	}
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestStoreClosesIdempotently(t *testing.T) {
	st, err := NewStore(config.Storage{DBPath: t.TempDir() + "/idem.db", LRUSize: 8})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = st.Close()
		}()
	}
	wg.Wait()
}

var _ = context.Background
