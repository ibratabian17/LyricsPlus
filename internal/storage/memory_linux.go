//go:build linux

package storage

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// rssBytes measures actual process memory on Linux & Docker containers.
// Priority:
// 1. cgroup v2 (/sys/fs/cgroup/memory.current)
// 2. cgroup v1 (/sys/fs/cgroup/memory/memory.usage_in_bytes)
// 3. /proc/self/statm (pages * pageSize)
// 4. getrusage Maxrss (in KB on Linux)
// 5. In-use memory fallback
func rssBytes() uint64 {
	// 1. cgroup v2 (Docker / Kubernetes default on modern Linux)
	if data, err := os.ReadFile("/sys/fs/cgroup/memory.current"); err == nil {
		if val, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64); err == nil && val > 0 {
			return val
		}
	}

	// 2. cgroup v1 (legacy Docker / Linux)
	if data, err := os.ReadFile("/sys/fs/cgroup/memory/memory.usage_in_bytes"); err == nil {
		if val, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64); err == nil && val > 0 {
			return val
		}
	}

	// 3. /proc/self/statm (Linux physical RSS in pages)
	if data, err := os.ReadFile("/proc/self/statm"); err == nil {
		fields := strings.Fields(string(data))
		if len(fields) >= 2 {
			if pages, err := strconv.ParseUint(fields[1], 10, 64); err == nil && pages > 0 {
				return pages * uint64(os.Getpagesize())
			}
		}
	}

	// 4. getrusage (Maxrss is in KB on Linux)
	var r syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &r); err == nil && r.Maxrss > 0 {
		return uint64(r.Maxrss) * 1024
	}

	// 5. Fallback to active in-use memory
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapInuse + m.StackInuse + m.MSpanInuse + m.MCacheInuse
}
