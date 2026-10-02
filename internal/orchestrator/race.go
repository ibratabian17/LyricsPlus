// Package orchestrator races lyric providers concurrently and de-dupes results.
package orchestrator

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"lyricsplus/backend/internal/domain"
	"lyricsplus/backend/internal/logger"
	"lyricsplus/backend/internal/metrics"
)

var errUnavailable = errors.New("provider unavailable")

// Source is a lyrics provider abstraction consumed by the racing engine.
type Source interface {
	Name() string
	// FetchWord returns a normalized V2 payload for sync providers.
	FetchLyrics(ctx context.Context, q domain.SearchQuery) (*domain.LyricsResponse, error)
}

// Result is the outcome of a single provider fetch.
type Result struct {
	Source   string
	Resp     *domain.LyricsResponse
	Priority int
	Err      error
	Elapsed  time.Duration
	Status   string // "OK", "BAD", "RTO", "SKIP"
	Pipeline time.Duration
	// SourcesStatus maps provider name -> outcome for the whole race.
	SourcesStatus map[string]SourceOutcome
}

// SourceOutcome is the diagnosable trace of one fetch.
type SourceOutcome struct {
	Status    string
	ElapsedMs int64
}

// Racer performs the two-phase speculative fetch.
type Racer struct {
	sources map[string]Source
	timeout time.Duration
	logger  *logger.Logger
}

type raceSession struct {
	r         *Racer
	mu        sync.Mutex
	status    map[string]SourceOutcome
	raceStart time.Time
}

func (s *raceSession) record(name, status string, ms int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := SourceOutcome{Status: status, ElapsedMs: ms}
	if prev, ok := s.status[name]; ok && prev.Status == "OK" {
		return
	}
	s.status[name] = st
}

func (s *raceSession) snapshot() map[string]SourceOutcome {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]SourceOutcome, len(s.status))
	for k, v := range s.status {
		out[k] = v
	}
	return out
}

// RacerOption configures a Racer.
type RacerOption func(*Racer)

// WithLogger attaches a logger for per-source debug output.
func WithLogger(lg *logger.Logger) RacerOption {
	return func(r *Racer) { r.logger = lg }
}

func NewRacer(sources []Source, timeout time.Duration, opts ...RacerOption) *Racer {
	m := make(map[string]Source, len(sources))
	for _, s := range sources {
		m[s.Name()] = s
	}
	r := &Racer{sources: m, timeout: timeout}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

func (r *Racer) debugf(format string, args ...any) {
	if r.logger == nil {
		return
	}
	r.logger.Debugf(format, args...)
}

// TryFetch runs a single provider with its own timeout already applied.
func (r *Racer) TryFetch(ctx context.Context, name string, q domain.SearchQuery) *Result {
	sess := &raceSession{r: r, status: make(map[string]SourceOutcome), raceStart: time.Now()}
	return sess.tryFetch(ctx, name, q)
}

func (s *raceSession) tryFetch(ctx context.Context, name string, q domain.SearchQuery) *Result {
	src, ok := s.r.sources[name]
	if !ok {
		s.record(name, "SKIP", 0)
		return &Result{Source: name, Status: "SKIP", Err: errUnavailable}
	}
	if c, ok := src.(interface{ Configured() bool }); ok && !c.Configured() {
		s.record(name, "SKIP", 0)
		return &Result{Source: name, Status: "SKIP"}
	}
	start := time.Now()
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Immediately record as BAD in case context cancels before return, matching JS trackedFetch
	s.record(name, "BAD", 0)

	resp, err := src.FetchLyrics(reqCtx, q)
	elapsed := time.Since(start)
	res := &Result{Source: name, Resp: resp, Err: err, Elapsed: elapsed}
	switch {
	case err == nil && resp != nil && len(resp.Lyrics) > 0:
		if resp.ProcessingTime != nil && resp.ProcessingTime.WinnerSource != nil && *resp.ProcessingTime.WinnerSource != "" {
			res.Source = *resp.ProcessingTime.WinnerSource
		} else if strings.Contains(resp.Metadata.Source, "with QQ") {
			res.Source = "qaple"
		}
		res.Priority = Grade(resp, res.Source)
		res.Status = "OK"
	case err == context.DeadlineExceeded || err == context.Canceled || reqCtx.Err() != nil:
		res.Status = "RTO"
	default:
		res.Status = "BAD"
	}
	s.record(name, res.Status, elapsed.Milliseconds())
	metrics.Default.RecordProviderCall(name, res.Status, elapsed.Milliseconds())
	switch res.Status {
	case "OK":
		s.r.debugf("source %s OK priority=%d lines=%d took=%s", name, res.Priority, len(resp.Lyrics), elapsed.Round(time.Millisecond))
	case "RTO":
		s.r.debugf("source %s RTO took=%s", name, elapsed.Round(time.Millisecond))
	case "BAD":
		if err != nil {
			s.r.debugf("source %s BAD took=%s err=%v", name, elapsed.Round(time.Millisecond), err)
		} else {
			s.r.debugf("source %s BAD took=%s (no lyrics)", name, elapsed.Round(time.Millisecond))
		}
	default:
		s.r.debugf("source %s %s", name, res.Status)
	}
	return res
}

func (r *Racer) has(name string) bool {
	_, ok := r.sources[name]
	return ok
}

// Race orchestrates the full two-phase algorithm.
func (r *Racer) Race(ctx context.Context, q domain.SearchQuery, preferredSources []string) *Result {
	sess := &raceSession{
		r:         r,
		status:    make(map[string]SourceOutcome),
		raceStart: time.Now(),
	}
	raceCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	order := SourceOrder(q, preferredSources)
	phase1 := order
	if len(phase1) > 2 {
		phase1 = phase1[:2]
	}
	remaining := order
	if len(remaining) > 2 {
		remaining = remaining[2:]
	}

	r.debugf("race title=%q artist=%q phase1=%v remaining=%v", q.Title, q.Artist, phase1, remaining)

	// Phase 1: first two sources, blocking semantics, P3 early exit.
	p1 := sess.runPhase(raceCtx, q, phase1)

	if p1 != nil && p1.Priority >= PriorityWord {
		return sess.finalize(p1)
	}

	if len(remaining) == 0 || raceCtx.Err() != nil {
		return sess.finalize(p1)
	}

	// Phase 2: P3-upgrade search if Phase 1 delivered line sync.
	if p1 != nil && p1.Priority == PriorityLine {
		p3Sources := make([]string, 0, len(remaining))
		for _, s := range remaining {
			if s != "musixmatch" && s != "spotify" {
				p3Sources = append(p3Sources, s)
			}
		}
		if len(p3Sources) > 0 {
			p3 := sess.raceForPriority(raceCtx, q, p3Sources, PriorityWord)
			if p3 != nil {
				return sess.finalize(p3)
			}
		}
		return sess.finalize(p1)
	}

	// Otherwise race all remaining concurrently and pick the highest priority result.
	p2 := sess.runPhase(raceCtx, q, remaining)
	if p2 != nil {
		if p1 == nil || p2.Priority > p1.Priority {
			return sess.finalize(p2)
		}
	}
	return sess.finalize(p1)
}

func (s *raceSession) finalize(res *Result) *Result {
	snap := s.snapshot()
	if res == nil {
		res = &Result{}
	}
	res.SourcesStatus = snap
	res.Pipeline = time.Since(s.raceStart)
	if res.Status == "OK" || (res.Resp != nil && len(res.Resp.Lyrics) > 0) {
		s.r.debugf("race winner source=%s priority=%d pipeline=%s", res.Source, res.Priority, res.Pipeline.Round(time.Millisecond))
	} else {
		s.r.debugf("race no winner pipeline=%s statuses=%v", res.Pipeline.Round(time.Millisecond), snap)
	}
	return res
}

// runPhase executes a set of sources concurrently, honoring the priority
// blocking rule: later P2 waits for earlier sources; any P3 wins immediately.
func (s *raceSession) runPhase(ctx context.Context, q domain.SearchQuery, names []string) *Result {
	if len(names) == 0 {
		return nil
	}
	phaseCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	results := make([]*Result, len(names))
	idxByName := map[string]int{}
	for i, n := range names {
		idxByName[n] = i
	}

	type phaseOutcome struct {
		name string
		res  *Result
	}
	ch := make(chan phaseOutcome, len(names))
	var wg sync.WaitGroup
	for _, n := range names {
		if !s.r.has(n) {
			results[idxByName[n]] = nil
			continue
		}
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			ch <- phaseOutcome{name: name, res: s.tryFetch(phaseCtx, name, q)}
		}(n)
	}

	// Early exit channel.
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	finished := 0
	complete := make([]bool, len(names))
	for {
		select {
		case out := <-ch:
			finished++
			i := idxByName[out.name]
			results[i] = out.res
			complete[i] = true
			if out.res != nil && out.res.Priority >= PriorityWord {
				cancel()
				return out.res
			}
			if finished == len(names) {
				return pickWinner(results, names)
			}
		case <-done:
			return pickWinner(results, names)
		case <-phaseCtx.Done():
			cancel()
			return pickWinner(results, names)
		case <-ctx.Done():
			cancel()
			return pickWinner(results, names)
		}
	}
}

// raceForPriority runs remaining sources looking for an exact priority match.
func (s *raceSession) raceForPriority(ctx context.Context, q domain.SearchQuery, names []string, want int) *Result {
	phaseCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	ch := make(chan *Result, len(names))
	var wg sync.WaitGroup
	for _, n := range names {
		if !s.r.has(n) {
			continue
		}
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			ch <- s.tryFetch(phaseCtx, name, q)
		}(n)
	}
	go func() {
		wg.Wait()
		close(ch)
	}()

	for {
		select {
		case res, ok := <-ch:
			if !ok {
				return nil
			}
			if res.Priority == want {
				cancel()
				return res
			}
		case <-phaseCtx.Done():
			return nil
		case <-ctx.Done():
			return nil
		}
	}
}

func pickWinner(results []*Result, names []string) *Result {
	bestPrio := -1
	var best []*Result
	for _, res := range results {
		if res == nil {
			continue
		}
		if res.Priority > bestPrio {
			bestPrio = res.Priority
			best = []*Result{res}
		} else if res.Priority == bestPrio {
			best = append(best, res)
		}
	}
	if bestPrio <= 0 {
		// Return first non-failed result for diagnostics even if priority 0.
		for _, res := range results {
			if res != nil && res.Resp != nil && len(res.Resp.Lyrics) > 0 {
				return res
			}
		}
		return nil
	}
	// Tie-break by source order.
	idx := map[string]int{}
	for i, n := range names {
		idx[n] = i
	}
	nameOrder := func(source string) int {
		if i, ok := idx[source]; ok {
			return i
		}
		if source == "qaple" {
			if i, ok := idx["lyricsplus"]; ok {
				return i
			}
		}
		return 999
	}
	winner := best[0]
	for _, res := range best[1:] {
		if nameOrder(res.Source) < nameOrder(winner.Source) {
			winner = res
		}
	}
	return winner
}
