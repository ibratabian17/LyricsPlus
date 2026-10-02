package orchestrator

import (
	"context"
	"time"

	"golang.org/x/sync/singleflight"

	"lyricsplus/backend/internal/domain"
)

// Dedup wraps the racing engine with singleflight key deduplication.
// Concurrent identical searches share one in-flight result.
type Dedup struct {
	racer *Racer
	group singleflight.Group
}

func NewDedup(racer *Racer) *Dedup {
	return &Dedup{racer: racer}
}

// Get races and deduplicates. The winner's per-source outcome map is captured
// in the Result; the shared result is deep-copied per caller to avoid mutation.
func (d *Dedup) Get(ctx context.Context, q domain.SearchQuery, preferredSources []string) (*Result, error) {
	if len(preferredSources) == 0 {
		preferredSources = SourceOrder(q, nil)
	}
	key := q.NormalizeKey()

	timeout := d.racer.timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	ch := d.group.DoChan(key, func() (interface{}, error) {
		fetchCtx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
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
		// Return a shallow copy so callers can decorate diagnostics safely.
		cp := *r
		if r.Resp != nil {
			rCopy := *r.Resp
			cp.Resp = &rCopy
		}
		return &cp, nil
	}
}
