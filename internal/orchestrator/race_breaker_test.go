package orchestrator

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"lyricsplus/backend/internal/domain"
)

type scriptedSource struct {
	name  string
	calls atomic.Int64
	resp  *domain.LyricsResponse
	err   error
}

func (c *scriptedSource) Name() string { return c.name }
func (c *scriptedSource) FetchLyrics(ctx context.Context, q domain.SearchQuery) (*domain.LyricsResponse, error) {
	c.calls.Add(1)
	return c.resp, c.err
}

// A provider that keeps failing (banned account / 429) must stop being called,
// must be flagged Degraded (so the miss is not negative-cached), and the
// healthy provider must keep serving.
func TestRacerBreakerStopsCallingDeadProvider(t *testing.T) {
	dead := &scriptedSource{name: "musixmatch", err: errors.New("musixmatch: login failed with status 401: bad")}
	good := &scriptedSource{name: "apple", resp: wordResp("apple")}
	br := NewBreaker(BreakerConfig{Threshold: 3, BaseCooldown: time.Minute, MaxCooldown: time.Minute})
	r := NewRacer([]Source{good, dead}, time.Second, WithBreaker(br), WithLimiter(NewLimiter(LimiterConfig{})))

	q := domain.SearchQuery{Title: "t", Artist: "a"}
	for i := 0; i < 10; i++ {
		res := r.Race(context.Background(), q, []string{"musixmatch"})
		if i >= 3 {
			if st := res.SourcesStatus["musixmatch"]; st.Status != "SKIP" || !st.Degraded {
				t.Fatalf("iter %d: want degraded SKIP once open, got %+v", i, st)
			}
		}
	}
	if got := dead.calls.Load(); got != 3 {
		t.Fatalf("dead provider should be called only until the breaker opens (3), got %d", got)
	}
	if !br.Status("musixmatch").Open {
		t.Fatal("breaker should be open")
	}

	res := r.Race(context.Background(), q, []string{"apple"})
	if res == nil || res.Resp == nil || len(res.Resp.Lyrics) == 0 {
		t.Fatal("healthy provider must still win")
	}
}

// A clean "no lyrics" answer is not a health failure and must never open the
// breaker, no matter how many misses there are.
func TestRacerBreakerIgnoresCleanMisses(t *testing.T) {
	miss := &scriptedSource{name: "apple", err: errors.New("status 404")}
	br := NewBreaker(BreakerConfig{Threshold: 3})
	r := NewRacer([]Source{miss}, time.Second, WithBreaker(br), WithLimiter(NewLimiter(LimiterConfig{})))
	for i := 0; i < 20; i++ {
		res := r.Race(context.Background(), domain.SearchQuery{Title: "t", Artist: "a"}, []string{"apple"})
		if st := res.SourcesStatus["apple"]; st.Degraded {
			t.Fatalf("clean miss flagged degraded: %+v", st)
		}
	}
	if br.Status("apple").Open {
		t.Fatal("breaker must stay closed on clean misses")
	}
	if miss.calls.Load() != 20 {
		t.Fatalf("expected 20 calls, got %d", miss.calls.Load())
	}
}
