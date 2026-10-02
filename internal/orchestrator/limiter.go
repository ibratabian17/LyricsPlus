package orchestrator

import (
	"context"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"
)

type Limiter struct {
	global    *semaphore.Weighted
	perSource map[string]*semaphore.Weighted

	wait time.Duration

	perSourceLimit int64

	mu       sync.Mutex
	rejected map[string]int64
}

type LimiterConfig struct {
	Global           int64
	PerSource        int64
	Wait             time.Duration
	DefaultPerSource int64
}

func DefaultLimiterConfig() LimiterConfig {
	return LimiterConfig{
		Global:           4096,
		PerSource:        1024,
		DefaultPerSource: 256,
		Wait:             2 * time.Second,
	}
}

func NewLimiter(cfg LimiterConfig) *Limiter {
	d := DefaultLimiterConfig()
	if cfg.Global <= 0 {
		cfg.Global = d.Global
	}
	if cfg.PerSource <= 0 {
		cfg.PerSource = d.PerSource
	}
	if cfg.DefaultPerSource <= 0 {
		cfg.DefaultPerSource = d.DefaultPerSource
	}
	if cfg.Wait <= 0 {
		cfg.Wait = d.Wait
	}
	return &Limiter{
		global:         semaphore.NewWeighted(cfg.Global),
		perSource:      make(map[string]*semaphore.Weighted, 8),
		perSourceLimit: cfg.PerSource,
		wait:           cfg.Wait,
		rejected:       make(map[string]int64, 8),
	}
}

func (l *Limiter) Acquire(ctx context.Context, name string) (release func(), ok bool) {
	if l == nil {
		return func() {}, true
	}

	wait := l.wait
	if dl, ok := ctx.Deadline(); ok {
		if rem := time.Until(dl); rem < wait {
			wait = rem
		}
	}
	if wait <= 0 {
		wait = time.Millisecond
	}
	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()

	src := l.sourceSem(name)

	if err := src.Acquire(waitCtx, 1); err != nil {
		l.noteReject(name)
		return nil, false
	}
	if err := l.global.Acquire(waitCtx, 1); err != nil {
		src.Release(1)
		l.noteReject(name)
		return nil, false
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			l.global.Release(1)
			src.Release(1)
		})
	}, true
}

func (l *Limiter) TryAcquire(name string) (release func(), ok bool) {
	if l == nil {
		return func() {}, true
	}
	src := l.sourceSem(name)
	if !src.TryAcquire(1) {
		l.noteReject(name)
		return nil, false
	}
	if !l.global.TryAcquire(1) {
		src.Release(1)
		l.noteReject(name)
		return nil, false
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			l.global.Release(1)
			src.Release(1)
		})
	}, true
}

func (l *Limiter) sourceSem(name string) *semaphore.Weighted {
	l.mu.Lock()
	defer l.mu.Unlock()
	if s, ok := l.perSource[name]; ok {
		return s
	}
	s := semaphore.NewWeighted(l.perSourceLimit)
	l.perSource[name] = s
	return s
}

func (l *Limiter) noteReject(name string) {
	l.mu.Lock()
	l.rejected[name]++
	l.mu.Unlock()
}

func (l *Limiter) Rejected() map[string]int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]int64, len(l.rejected))
	for k, v := range l.rejected {
		out[k] = v
	}
	return out
}
