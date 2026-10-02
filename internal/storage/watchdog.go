package storage

import (
	"log"
	"runtime/debug"
	"sync"
	"time"
)

// Watchdog sheds in-memory caches when process RSS memory exceeds the
// threshold, checked every interval with a cooldown period.
type Watchdog struct {
	limitBytes int64
	interval   time.Duration
	cooldown   time.Duration
	lastShed   time.Time
	caches     []interface{ Shed() }
	stop       chan struct{}
	stopOnce   sync.Once
}

func NewWatchdog(limitBytes int64, interval time.Duration, caches ...interface{ Shed() }) *Watchdog {
	return &Watchdog{
		limitBytes: limitBytes,
		interval:   interval,
		cooldown:   60 * time.Second,
		caches:     caches,
		stop:       make(chan struct{}),
	}
}

// SetCooldown customizes the minimum time between cache shedding events.
func (w *Watchdog) SetCooldown(d time.Duration) {
	w.cooldown = d
}

// Start begins periodic RSS monitoring.
func (w *Watchdog) Start() {
	go func() {
		t := time.NewTicker(w.interval)
		defer t.Stop()
		for {
			select {
			case <-w.stop:
				return
			case <-t.C:
				w.check()
			}
		}
	}()
}

func (w *Watchdog) Stop() {
	w.stopOnce.Do(func() {
		close(w.stop)
	})
}

func (w *Watchdog) check() {
	if w.limitBytes <= 0 {
		return
	}
	used := rssBytes()
	if int64(used) <= w.limitBytes {
		return
	}
	if time.Since(w.lastShed) < w.cooldown {
		return
	}
	w.lastShed = time.Now()

	log.Printf("memory watchdog: RSS %dMB exceeds %dMB, shedding caches", used>>20, w.limitBytes>>20)
	for _, c := range w.caches {
		c.Shed()
	}
	debug.FreeOSMemory()
	after := rssBytes()
	log.Printf("memory watchdog: RSS after shed %dMB", after>>20)
}
