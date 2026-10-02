//go:build !linux && !darwin && !windows

package storage

import "runtime"

// rssBytes returns active in-use memory on other platforms.
func rssBytes() uint64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapInuse + m.StackInuse + m.MSpanInuse + m.MCacheInuse
}
