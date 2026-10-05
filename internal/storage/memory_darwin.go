//go:build darwin

package storage

import (
	"runtime"
	"syscall"
)

func rssBytes() uint64 {
	var r syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &r); err == nil && r.Maxrss > 0 {

		return uint64(r.Maxrss)
	}

	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapInuse + m.StackInuse + m.MSpanInuse + m.MCacheInuse
}
