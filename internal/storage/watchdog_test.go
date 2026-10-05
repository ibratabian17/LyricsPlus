package storage

import (
	"sync"
	"testing"
	"time"
)

type dummyCache struct {
	mu     sync.Mutex
	sheded int
}

func (d *dummyCache) Shed() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sheded++
}

func (d *dummyCache) Count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.sheded
}

func TestWatchdogThresholdAndCooldown(t *testing.T) {
	c := &dummyCache{}

	w := NewWatchdog(1, 10*time.Millisecond, c)
	w.SetCooldown(100 * time.Millisecond)

	w.check()
	if c.Count() != 1 {
		t.Fatalf("expected 1 shed call, got %d", c.Count())
	}

	w.check()
	if c.Count() != 1 {
		t.Fatalf("expected still 1 shed call due to cooldown, got %d", c.Count())
	}

	time.Sleep(120 * time.Millisecond)
	w.check()
	if c.Count() != 2 {
		t.Fatalf("expected 2 shed calls after cooldown, got %d", c.Count())
	}
}

func TestWatchdogStopIdempotent(t *testing.T) {
	w := NewWatchdog(1000, 10*time.Millisecond)
	w.Start()

	w.Stop()
	w.Stop()
}
