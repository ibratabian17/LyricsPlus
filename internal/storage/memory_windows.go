//go:build windows

package storage

import (
	"runtime"
	"syscall"
	"unsafe"
)

type processMemoryCounters struct {
	CB                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

var (
	modPsapi                 = syscall.NewLazyDLL("psapi.dll")
	procGetProcessMemoryInfo = modPsapi.NewProc("GetProcessMemoryInfo")
)

func rssBytes() uint64 {
	handle, err := syscall.GetCurrentProcess()
	if err == nil && procGetProcessMemoryInfo.Find() == nil {
		var pmc processMemoryCounters
		pmc.CB = uint32(unsafe.Sizeof(pmc))
		r1, _, _ := procGetProcessMemoryInfo.Call(
			uintptr(handle),
			uintptr(unsafe.Pointer(&pmc)),
			uintptr(pmc.CB),
		)
		if r1 != 0 && pmc.WorkingSetSize > 0 {
			return uint64(pmc.WorkingSetSize)
		}
	}

	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapInuse + m.StackInuse + m.MSpanInuse + m.MCacheInuse
}
