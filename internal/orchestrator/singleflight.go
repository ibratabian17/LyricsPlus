package orchestrator

import (
	"context"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"lyricsplus/backend/internal/domain"
)

type Dedup struct {
	racer  *Racer
	group  singleflight.Group
	active *keyTracker
}

func NewDedup(racer *Racer) *Dedup {
	return &Dedup{racer: racer, active: newKeyTracker()}
}

type keyTracker struct {
	mu      sync.Mutex
	waiters map[string]int
	cancels map[string]context.CancelFunc
}

func newKeyTracker() *keyTracker {
	return &keyTracker{
		waiters: make(map[string]int),
		cancels: make(map[string]context.CancelFunc),
	}
}

func (t *keyTracker) enter(key string) (release func()) {
	t.mu.Lock()
	t.waiters[key]++
	t.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() { t.leave(key) })
	}
}

func (t *keyTracker) leave(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := t.waiters[key] - 1
	if n <= 0 {
		delete(t.waiters, key)
		if cancel, ok := t.cancels[key]; ok {
			delete(t.cancels, key)
			cancel()
		}
		return
	}
	t.waiters[key] = n
}

func (t *keyTracker) registerCancel(key string, cancel context.CancelFunc) {
	t.mu.Lock()
	if _, waiting := t.waiters[key]; !waiting {
		t.mu.Unlock()
		cancel()
		return
	}
	t.cancels[key] = cancel
	t.mu.Unlock()
}

func (t *keyTracker) clearCancel(key string) {
	t.mu.Lock()
	delete(t.cancels, key)
	t.mu.Unlock()
}

func (d *Dedup) InflightKeys() int {
	if d == nil {
		return 0
	}
	return d.active.inflight()
}

func (d *Dedup) Get(ctx context.Context, q domain.SearchQuery, preferredSources []string) (*Result, error) {
	if len(preferredSources) == 0 {
		preferredSources = SourceOrder(q, nil)
	}
	key := q.NormalizeKey()

	timeout := d.racer.timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	release := d.active.enter(key)
	defer release()

	ch := d.group.DoChan(key, func() (interface{}, error) {
		fetchCtx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		d.active.registerCancel(key, cancel)
		defer d.active.clearCancel(key)
		return d.racer.Race(fetchCtx, q, preferredSources), nil
	})

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-ch:
		if res.Err != nil {
			return nil, res.Err
		}
		r, ok := res.Val.(*Result)
		if !ok {
			return nil, errUnavailable
		}
		if r == nil {
			return nil, nil
		}
		cp := *r
		if r.Resp != nil {
			rCopy := *r.Resp
			cp.Resp = &rCopy
		}
		return &cp, nil
	}
}

func (t *keyTracker) inflight() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.waiters)
}
