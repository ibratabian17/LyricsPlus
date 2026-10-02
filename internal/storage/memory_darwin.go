//go:build darwin

package storage

import (
	"runtime"
	"syscall"
)

// rssBytes returns the process RSS on macOS (Darwin).
func rssBytes() uint64 {
	var r syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &r); err == nil && r.Maxrss > 0 {
		// On Darwin/macOS, r.Maxrss is reported in bytes.
		return uint64(r.Maxrss)
	}

	// Fallback to active in-use memory
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapInuse + m.StackInuse + m.MSpanInuse + m.MCacheInuse
}
