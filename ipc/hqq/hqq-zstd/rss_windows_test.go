//go:build windows

package zstd_test

import (
	"fmt"
	"syscall"
	"unsafe"
)

const processQueryLimitedInformation = 0x1000

type processMemoryCounters struct {
	cb                         uint32
	pageFaultCount             uint32
	peakWorkingSetSize         uintptr
	workingSetSize             uintptr
	quotaPeakPagedPoolUsage    uintptr
	quotaPagedPoolUsage        uintptr
	quotaPeakNonPagedPoolUsage uintptr
	quotaNonPagedPoolUsage     uintptr
	pagefileUsage              uintptr
	peakPagefileUsage          uintptr
}

var procK32GetProcessMemoryInfo = syscall.NewLazyDLL("kernel32.dll").NewProc("K32GetProcessMemoryInfo")

// rssOf returns the resident set size (working set) of a process in KiB through the Windows API.
func rssOf(pid int) (int64, error) {
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return 0, fmt.Errorf("OpenProcess(%d): %w", pid, err)
	}
	defer syscall.CloseHandle(h)

	var pmc processMemoryCounters
	pmc.cb = uint32(unsafe.Sizeof(pmc))
	r1, _, e1 := procK32GetProcessMemoryInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&pmc)), uintptr(pmc.cb))
	if r1 == 0 {
		return 0, fmt.Errorf("K32GetProcessMemoryInfo(%d): %w", pid, e1)
	}
	return int64(pmc.workingSetSize >> 10), nil
}
