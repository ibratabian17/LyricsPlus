//go:build linux

package storage

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// rssBytes returns the current process resident set size (RSS) in bytes.
// Importantly, it reads /proc/self/statm first rather than /sys/fs/cgroup/memory.current,
// because cgroup memory accounting includes the Linux kernel page cache (such as
// SQLite WAL/database pages cached by the OS), which is not process heap memory
// and cannot be reclaimed by shedding Go caches or calling debug.FreeOSMemory().
func rssBytes() uint64 {
	if data, err := os.ReadFile("/proc/self/statm"); err == nil {
		fields := strings.Fields(string(data))
		if len(fields) >= 2 {
			if pages, err := strconv.ParseUint(fields[1], 10, 64); err == nil && pages > 0 {
				return pages * uint64(os.Getpagesize())
			}
		}
	}

	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	if m.HeapInuse > 0 {
		return m.HeapInuse + m.StackInuse + m.MSpanInuse + m.MCacheInuse
	}

	var r syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &r); err == nil && r.Maxrss > 0 {
		return uint64(r.Maxrss) * 1024
	}

	return 0
}
