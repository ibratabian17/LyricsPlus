package storage

import (
	"log"
	"runtime"
	"time"
)

// Watchdog sheds in-memory caches when process RSS memory exceeds the
// threshold, checked every interval.
type Watchdog struct {
	limitBytes int64
	interval   time.Duration
	caches     []interface{ Shed() }
	stop       chan struct{}
}

func NewWatchdog(limitBytes int64, interval time.Duration, caches ...interface{ Shed() }) *Watchdog {
	return &Watchdog{
		limitBytes: limitBytes,
		interval:   interval,
		caches:     caches,
		stop:       make(chan struct{}),
	}
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
	close(w.stop)
}

func (w *Watchdog) check() {
	used := rssBytes()
	if int64(used) > w.limitBytes {
		log.Printf("memory watchdog: RSS %dMB exceeds %dMB, shedding caches", used>>20, w.limitBytes>>20)
		for _, c := range w.caches {
			c.Shed()
		}
		runtime.GC()
	}
}

// rssBytes approximates process resident memory. runtime.MemStats.Sys tracks
// the total address space reserved by the runtime, a cheap conservative proxy.
func rssBytes() uint64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.Sys
}
