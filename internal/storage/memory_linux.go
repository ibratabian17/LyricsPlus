//go:build linux

package storage

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

func rssBytes() uint64 {
	if data, err := os.ReadFile("/sys/fs/cgroup/memory.current"); err == nil {
		if val, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64); err == nil && val > 0 {
			return val
		}
	}

	if data, err := os.ReadFile("/sys/fs/cgroup/memory/memory.usage_in_bytes"); err == nil {
		if val, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64); err == nil && val > 0 {
			return val
		}
	}

	if data, err := os.ReadFile("/proc/self/statm"); err == nil {
		fields := strings.Fields(string(data))
		if len(fields) >= 2 {
			if pages, err := strconv.ParseUint(fields[1], 10, 64); err == nil && pages > 0 {
				return pages * uint64(os.Getpagesize())
			}
		}
	}

	var r syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &r); err == nil && r.Maxrss > 0 {
		return uint64(r.Maxrss) * 1024
	}

	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapInuse + m.StackInuse + m.MSpanInuse + m.MCacheInuse
}
