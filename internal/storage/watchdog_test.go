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
	// Threshold set very low (1 byte) so current memory exceeds it
	w := NewWatchdog(1, 10*time.Millisecond, c)
	w.SetCooldown(100 * time.Millisecond)

	w.check()
	if c.Count() != 1 {
		t.Fatalf("expected 1 shed call, got %d", c.Count())
	}

	// Immediately calling check() should respect cooldown
	w.check()
	if c.Count() != 1 {
		t.Fatalf("expected still 1 shed call due to cooldown, got %d", c.Count())
	}

	// After cooldown expires
	time.Sleep(120 * time.Millisecond)
	w.check()
	if c.Count() != 2 {
		t.Fatalf("expected 2 shed calls after cooldown, got %d", c.Count())
	}
}

func TestWatchdogStopIdempotent(t *testing.T) {
	w := NewWatchdog(1000, 10*time.Millisecond)
	w.Start()
	// Stop multiple times shouldn't panic
	w.Stop()
	w.Stop()
}
