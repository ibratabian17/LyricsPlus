package orchestrator

import (
	"context"

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

	v, err, _ := d.group.Do(key, func() (interface{}, error) {
		return d.racer.Race(ctx, q, preferredSources), nil
	})
	if err != nil {
		return nil, err
	}
	res, ok := v.(*Result)
	if !ok {
		return nil, errUnavailable
	}
	if res == nil {
		return nil, nil
	}
	// Return a shallow copy so callers can decorate diagnostics safely.
	cp := *res
	if res.Resp != nil {
		r := *res.Resp
		cp.Resp = &r
	}
	return &cp, nil
}
