package orchestrator

import (
	"context"
	"errors"
	"net"
	"regexp"
	"strings"
	"sync"
	"time"
)

type Breaker struct {
	cfg BreakerConfig
	mu  sync.Mutex
	st  map[string]*breakerState
	now func() time.Time
}

type BreakerConfig struct {
	Threshold    int
	BaseCooldown time.Duration
	MaxCooldown  time.Duration
	ProbeTimeout time.Duration
}

func DefaultBreakerConfig() BreakerConfig {
	return BreakerConfig{
		Threshold:    6,
		BaseCooldown: 25 * time.Second,
		MaxCooldown:  5 * time.Minute,
		ProbeTimeout: 12 * time.Second,
	}
}

type breakerState struct {
	failures  int
	trips     int
	open      bool
	openUntil time.Time
	probing   bool
	probeAt   time.Time
}

// BreakerStatus is a read-only snapshot used for diagnostics.
type BreakerStatus struct {
	Open          bool
	ConsecutiveFa int
	RetryIn       time.Duration
}

func NewBreaker(cfg BreakerConfig) *Breaker {
	d := DefaultBreakerConfig()
	if cfg.Threshold <= 0 {
		cfg.Threshold = d.Threshold
	}
	if cfg.BaseCooldown <= 0 {
		cfg.BaseCooldown = d.BaseCooldown
	}
	if cfg.MaxCooldown <= 0 {
		cfg.MaxCooldown = d.MaxCooldown
	}
	if cfg.MaxCooldown < cfg.BaseCooldown {
		cfg.MaxCooldown = cfg.BaseCooldown
	}
	if cfg.ProbeTimeout <= 0 {
		cfg.ProbeTimeout = d.ProbeTimeout
	}
	return &Breaker{cfg: cfg, st: make(map[string]*breakerState, 8), now: time.Now}
}

func (b *Breaker) state(name string) *breakerState {
	s, ok := b.st[name]
	if !ok {
		s = &breakerState{}
		b.st[name] = s
	}
	return s
}

func (b *Breaker) cooldown(trips int) time.Duration {
	d := b.cfg.BaseCooldown
	for i := 1; i < trips && d < b.cfg.MaxCooldown; i++ {
		d *= 2
	}
	if d > b.cfg.MaxCooldown {
		d = b.cfg.MaxCooldown
	}
	return d
}

// Allow reports whether a call to the provider may be made right now. It is
// safe to call on a nil Breaker (always allows).
func (b *Breaker) Allow(name string) bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.state(name)
	if !s.open {
		return true
	}
	now := b.now()
	if now.Before(s.openUntil) {
		return false
	}
	// Half-open: let a single probe through.
	if s.probing && now.Sub(s.probeAt) < b.cfg.ProbeTimeout {
		return false
	}
	s.probing = true
	s.probeAt = now
	return true
}

// Report records the outcome of a call that Allow admitted.
func (b *Breaker) Report(name string, ok bool) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.state(name)
	if ok {
		s.failures = 0
		s.trips = 0
		s.open = false
		s.probing = false
		return
	}
	s.failures++
	if s.open {
		// Only a failed probe extends the cooldown; stragglers that were
		// already in flight when the breaker opened are ignored.
		if s.probing {
			s.trips++
			s.openUntil = b.now().Add(b.cooldown(s.trips))
			s.probing = false
		}
		return
	}
	if s.failures >= b.cfg.Threshold {
		s.open = true
		s.trips = 1
		s.openUntil = b.now().Add(b.cooldown(1))
		s.probing = false
	}
}

// Neutral clears a pending probe without counting it either way (used when a
// call was cancelled by the caller, which says nothing about provider health).
func (b *Breaker) Neutral(name string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if s, ok := b.st[name]; ok {
		s.probing = false
	}
}

func (b *Breaker) Status(name string) BreakerStatus {
	if b == nil {
		return BreakerStatus{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.st[name]
	if !ok {
		return BreakerStatus{}
	}
	out := BreakerStatus{Open: s.open, ConsecutiveFa: s.failures}
	if s.open {
		if rem := s.openUntil.Sub(b.now()); rem > 0 {
			out.RetryIn = rem
		}
	}
	return out
}

var healthStatusRe = regexp.MustCompile(`(?:status|error|code)\s*\(?\s*(?:401|402|403|407|429|5\d\d)\b`)

var healthMarkers = []string{
	"cooldown",
	"no account available",
	"account available",
	"login failed",
	"invalid user token",
	"empty android auth token",
	"empty access token",
	"returned no token",
	"empty jwt",
	"token error",
	"token request failed",
	"requires email",
	"no user token",
	"unauthorized",
	"forbidden",
	"too many requests",
	"rate limit",
	"captcha",
	"connection refused",
	"connection reset",
	"no such host",
	"tls:",
	"unexpected eof",
	"proxy",
	"circuit breaker is open",
	"i/o timeout",
	"context deadline exceeded",
}

// IsHealthFailure reports whether err says something about the provider being
// unhealthy (timeouts, transport errors, auth/rate-limit/5xx responses, dead
// accounts) as opposed to a normal "no lyrics for this song" outcome.
func IsHealthFailure(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	msg := strings.ToLower(err.Error())
	if healthStatusRe.MatchString(msg) {
		return true
	}
	for _, m := range healthMarkers {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}
