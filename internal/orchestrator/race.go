package orchestrator

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"lyricsplus/backend/internal/domain"
	"lyricsplus/backend/internal/logger"
	"lyricsplus/backend/internal/metrics"
)

var errUnavailable = errors.New("provider unavailable")
var errOverloaded = errors.New("provider overloaded")
var errBreakerOpen = errors.New("provider circuit breaker open")

const siblingGracePeriod = 120 * time.Millisecond

type Source interface {
	Name() string
	FetchLyrics(ctx context.Context, q domain.SearchQuery) (*domain.LyricsResponse, error)
}

type Result struct {
	Source        string
	Resp          *domain.LyricsResponse
	Priority      int
	Err           error
	Elapsed       time.Duration
	Status        string
	Pipeline      time.Duration
	SourcesStatus map[string]SourceOutcome
}

type SourceOutcome struct {
	Status    string
	ElapsedMs int64
	Degraded  bool
}

type ResultSaver func(q domain.SearchQuery, res *Result)

type Racer struct {
	sources map[string]Source
	timeout time.Duration
	logger  *logger.Logger
	limiter *Limiter
	breaker *Breaker
	saver   ResultSaver
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

func (s *raceSession) markDegraded(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.status[name]; ok && st.Status != "OK" {
		st.Degraded = true
		s.status[name] = st
	}
}

func (s *raceSession) skipOverloaded(name string) {
	s.record(name, "SKIP", 0)
	s.markDegraded(name)
	metrics.Default.RecordProviderOverload(name)
}

func (s *raceSession) breakerAllows(name string) bool {
	if s.r.breaker.Allow(name) {
		return true
	}
	s.record(name, "SKIP", 0)
	s.markDegraded(name)
	return false
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

type RacerOption func(*Racer)

func WithLogger(lg *logger.Logger) RacerOption {
	return func(r *Racer) { r.logger = lg }
}

func WithLimiter(l *Limiter) RacerOption {
	return func(r *Racer) { r.limiter = l }
}

func WithBreaker(b *Breaker) RacerOption {
	return func(r *Racer) { r.breaker = b }
}

func WithSaver(s ResultSaver) RacerOption {
	return func(r *Racer) { r.saver = s }
}

func (r *Racer) SetSaver(s ResultSaver) {
	if r != nil {
		r.saver = s
	}
}

func (r *Racer) saveAsync(q domain.SearchQuery, res *Result) {
	if r != nil && r.saver != nil && res != nil && res.Resp != nil && len(res.Resp.Lyrics) > 0 {
		r.saver(q, res)
	}
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

func (r *Racer) TryFetch(ctx context.Context, name string, q domain.SearchQuery) *Result {
	sess := &raceSession{r: r, status: make(map[string]SourceOutcome), raceStart: time.Now()}
	return sess.tryFetch(ctx, name, q, nil)
}

func (s *raceSession) tryFetch(ctx context.Context, name string, q domain.SearchQuery, release func()) *Result {
	src, ok := s.r.sources[name]
	if !ok {
		if release != nil {
			release()
		}
		s.record(name, "SKIP", 0)
		return &Result{Source: name, Status: "SKIP", Err: errUnavailable}
	}
	if c, ok := src.(interface{ Configured() bool }); ok && !c.Configured() {
		if release != nil {
			release()
		}
		s.record(name, "SKIP", 0)
		return &Result{Source: name, Status: "SKIP"}
	}

	if release == nil {
		if !s.breakerAllows(name) {
			return &Result{Source: name, Status: "SKIP", Err: errBreakerOpen}
		}
		r, admitted := s.r.limiter.Acquire(ctx, name)
		if !admitted {
			s.skipOverloaded(name)
			s.r.breaker.Neutral(name)
			return &Result{Source: name, Status: "SKIP", Err: errOverloaded}
		}
		release = r
	}
	defer release()

	start := time.Now()
	timeout := s.r.timeout
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	s.record(name, "WAIT", 0)

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
	case errors.Is(err, context.Canceled) || ctx.Err() == context.Canceled || reqCtx.Err() == context.Canceled:
		res.Status = "CANCEL"
	case errors.Is(err, context.DeadlineExceeded) || reqCtx.Err() == context.DeadlineExceeded:
		res.Status = "RTO"
	default:
		res.Status = "BAD"
	}
	s.record(name, res.Status, elapsed.Milliseconds())
	switch {
	case res.Status == "OK":
		s.r.breaker.Report(name, true)
	case res.Status == "CANCEL":
		s.r.breaker.Neutral(name)
	case res.Status == "RTO" || IsHealthFailure(err):
		s.markDegraded(name)
		s.r.breaker.Report(name, false)
	default:
		s.r.breaker.Report(name, true)
	}
	if res.Status != "CANCEL" {
		metrics.Default.RecordProviderCall(name, res.Status, elapsed.Milliseconds())
	}
	switch res.Status {
	case "OK":
		s.r.debugf("source %s OK priority=%d lines=%d took=%s", name, res.Priority, len(resp.Lyrics), elapsed.Round(time.Millisecond))
	case "RTO":
		s.r.debugf("source %s RTO took=%s", name, elapsed.Round(time.Millisecond))
	case "CANCEL":
		s.r.debugf("source %s CANCEL took=%s", name, elapsed.Round(time.Millisecond))
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

func (r *Racer) Race(ctx context.Context, q domain.SearchQuery, preferredSources []string) *Result {
	sess := &raceSession{
		r:         r,
		status:    make(map[string]SourceOutcome),
		raceStart: time.Now(),
	}

	order := SourceOrder(q, preferredSources)
	phase1 := order
	if len(phase1) > 2 {
		phase1 = phase1[:2]
	}
	var remaining []string
	if len(order) > 2 {
		remaining = order[2:]
	}

	r.debugf("race title=%q artist=%q phase1=%v remaining=%v", q.Title, q.Artist, phase1, remaining)

	p1 := sess.runPhase(ctx, q, phase1)

	if p1 != nil && p1.Priority >= PriorityWord {
		return sess.finalize(p1)
	}

	if len(remaining) == 0 || ctx.Err() != nil {
		return sess.finalize(p1)
	}

	if p1 != nil && p1.Priority == PriorityLine {
		p3Sources := make([]string, 0, len(remaining))
		for _, s := range remaining {
			if s != "musixmatch" && s != "spotify" {
				p3Sources = append(p3Sources, s)
			}
		}
		if len(p3Sources) > 0 {
			p3 := sess.raceForPriority(ctx, q, p3Sources, PriorityWord)
			if p3 != nil {
				return sess.finalize(p3)
			}
		}
		return sess.finalize(p1)
	}

	p2 := sess.runPhase(ctx, q, remaining)
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

func (s *raceSession) runPhase(ctx context.Context, q domain.SearchQuery, names []string) *Result {
	if len(names) == 0 {
		return nil
	}

	results := make([]*Result, len(names))
	idxByName := make(map[string]int, len(names))
	for i, n := range names {
		idxByName[n] = i
	}

	launched := make([]string, 0, len(names))
	releases := make([]func(), 0, len(names))
	for _, n := range names {
		src, ok := s.r.sources[n]
		if !ok {
			results[idxByName[n]] = nil
			continue
		}
		if c, ok := src.(interface{ Configured() bool }); ok && !c.Configured() {
			s.record(n, "SKIP", 0)
			continue
		}
		if !s.breakerAllows(n) {
			continue
		}
		release, ok := s.r.limiter.TryAcquire(n)
		if !ok {
			s.skipOverloaded(n)
			s.r.breaker.Neutral(n)
			continue
		}
		launched = append(launched, n)
		releases = append(releases, release)
	}
	if len(launched) == 0 {
		return nil
	}

	fetchTimeout := s.r.timeout
	if fetchTimeout <= 0 {
		fetchTimeout = 8 * time.Second
	}

	var winnerFound atomic.Bool
	winnerCh := make(chan struct{})
	var closeWinnerOnce sync.Once
	markWinner := func() {
		winnerFound.Store(true)
		closeWinnerOnce.Do(func() { close(winnerCh) })
	}

	type phaseOutcome struct {
		name string
		res  *Result
	}
	ch := make(chan phaseOutcome, len(launched))
	for i, n := range launched {
		rel := releases[i]
		go func(name string, release func()) {
			fetchCtx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
			defer cancel()

			stopMonitor := make(chan struct{})
			go func() {
				select {
				case <-ctx.Done():
					if !winnerFound.Load() {
						cancel()
						return
					}
					t := time.NewTimer(siblingGracePeriod)
					defer t.Stop()
					select {
					case <-t.C:
						cancel()
					case <-stopMonitor:
					case <-fetchCtx.Done():
					}
				case <-winnerCh:
					t := time.NewTimer(siblingGracePeriod)
					defer t.Stop()
					select {
					case <-t.C:
						cancel()
					case <-stopMonitor:
					case <-fetchCtx.Done():
					}
				case <-stopMonitor:
				case <-fetchCtx.Done():
				}
			}()
			defer close(stopMonitor)

			res := s.tryFetch(fetchCtx, name, q, release)
			ch <- phaseOutcome{name: name, res: res}
			if res != nil && res.Resp != nil && len(res.Resp.Lyrics) > 0 {
				s.r.saveAsync(q, res)
			}
		}(n, rel)
	}

	finished := 0
	for finished < len(launched) {
		var out phaseOutcome
		select {
		case out = <-ch:
		case <-ctx.Done():
			return pickWinner(results, names)
		}
		finished++
		results[idxByName[out.name]] = out.res
		if out.res != nil && out.res.Priority >= PriorityWord {
			markWinner()
			return out.res
		}
	}
	if w := pickWinner(results, names); w != nil {
		markWinner()
		return w
	}
	return nil
}

func (s *raceSession) raceForPriority(ctx context.Context, q domain.SearchQuery, names []string, want int) *Result {
	launched := make([]string, 0, len(names))
	releases := make([]func(), 0, len(names))
	for _, n := range names {
		src, ok := s.r.sources[n]
		if !ok {
			continue
		}
		if c, ok := src.(interface{ Configured() bool }); ok && !c.Configured() {
			s.record(n, "SKIP", 0)
			continue
		}
		if !s.breakerAllows(n) {
			continue
		}
		release, ok := s.r.limiter.TryAcquire(n)
		if !ok {
			s.skipOverloaded(n)
			s.r.breaker.Neutral(n)
			continue
		}
		launched = append(launched, n)
		releases = append(releases, release)
	}
	if len(launched) == 0 {
		return nil
	}

	fetchTimeout := s.r.timeout
	if fetchTimeout <= 0 {
		fetchTimeout = 8 * time.Second
	}

	var winnerFound atomic.Bool
	winnerCh := make(chan struct{})
	var closeWinnerOnce sync.Once
	markWinner := func() {
		winnerFound.Store(true)
		closeWinnerOnce.Do(func() { close(winnerCh) })
	}

	ch := make(chan *Result, len(launched))
	for i, n := range launched {
		rel := releases[i]
		go func(name string, release func()) {
			fetchCtx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
			defer cancel()

			stopMonitor := make(chan struct{})
			go func() {
				select {
				case <-ctx.Done():
					if !winnerFound.Load() {
						cancel()
						return
					}
					t := time.NewTimer(siblingGracePeriod)
					defer t.Stop()
					select {
					case <-t.C:
						cancel()
					case <-stopMonitor:
					case <-fetchCtx.Done():
					}
				case <-winnerCh:
					t := time.NewTimer(siblingGracePeriod)
					defer t.Stop()
					select {
					case <-t.C:
						cancel()
					case <-stopMonitor:
					case <-fetchCtx.Done():
					}
				case <-stopMonitor:
				case <-fetchCtx.Done():
				}
			}()
			defer close(stopMonitor)

			res := s.tryFetch(fetchCtx, name, q, release)
			ch <- res
			if res != nil && res.Resp != nil && len(res.Resp.Lyrics) > 0 {
				s.r.saveAsync(q, res)
			}
		}(n, rel)
	}

	for finished := 0; finished < len(launched); finished++ {
		var res *Result
		select {
		case res = <-ch:
		case <-ctx.Done():
			return nil
		}
		if res != nil && res.Priority >= want {
			markWinner()
			return res
		}
	}
	return nil
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
		for _, res := range results {
			if res != nil && res.Resp != nil && len(res.Resp.Lyrics) > 0 {
				return res
			}
		}
		return nil
	}
	idx := make(map[string]int, len(names))
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
