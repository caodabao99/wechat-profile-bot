//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

var (
	kernel32                 = syscall.NewLazyDLL("kernel32.dll")
	psapi                    = syscall.NewLazyDLL("psapi.dll")
	procGlobalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")
	procGetCurrentProcess    = kernel32.NewProc("GetCurrentProcess")
	procGetProcessTimes      = kernel32.NewProc("GetProcessTimes")
	procGetDiskFreeSpaceExW  = kernel32.NewProc("GetDiskFreeSpaceExW")
	procGetProcessMemoryInfo = psapi.NewProc("GetProcessMemoryInfo")
)

// memoryStatusEx 对应 Win32 MEMORYSTATUSEX
type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

// filetime 对应 Win32 FILETIME
type filetime struct {
	LowDateTime  uint32
	HighDateTime uint32
}

func (ft filetime) uint64() uint64 {
	return uint64(ft.HighDateTime)<<32 | uint64(ft.LowDateTime)
}

// processMemoryCounters 对应 PROCESS_MEMORY_COUNTERS（64 位布局）
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

// readMemory 返回（系统总内存、可用内存、进程工作集），单位字节
func readMemory() (uint64, uint64, uint64, error) {
	var mem memoryStatusEx
	mem.Length = uint32(unsafe.Sizeof(mem))
	r1, _, err := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&mem)))
	if r1 == 0 {
		return 0, 0, 0, err
	}

	var rss uint64
	var counters processMemoryCounters
	counters.CB = uint32(unsafe.Sizeof(counters))
	hProc, _, _ := procGetCurrentProcess.Call()
	r1, _, err = procGetProcessMemoryInfo.Call(hProc, uintptr(unsafe.Pointer(&counters)), uintptr(counters.CB))
	if r1 != 0 {
		rss = uint64(counters.WorkingSetSize)
	}
	return mem.TotalPhys, mem.AvailPhys, rss, nil
}

// readLoadAvg Windows 不提供 load average，返回错误让上层保持 -1
func readLoadAvg() (float64, float64, float64, error) {
	return 0, 0, 0, syscall.ENOTSUP
}

// processCPUTimeSeconds 返回进程累计 CPU 时间（Kernel + User），单位秒。
// FILETIME 单位为 100 纳秒。
func processCPUTimeSeconds() (float64, error) {
	hProc, _, _ := procGetCurrentProcess.Call()
	var creation, exit, kernel, user filetime
	r1, _, err := procGetProcessTimes.Call(
		hProc,
		uintptr(unsafe.Pointer(&creation)),
		uintptr(unsafe.Pointer(&exit)),
		uintptr(unsafe.Pointer(&kernel)),
		uintptr(unsafe.Pointer(&user)),
	)
	if r1 == 0 {
		return 0, err
	}
	hundredNS := kernel.uint64() + user.uint64()
	return float64(hundredNS) / 1e7, nil
}

// diskUsage 返回路径所在盘的（总容量、可用容量），单位字节
func diskUsage(path string) (uint64, uint64, error) {
	pathPtr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, err
	}
	var freeAvail, total, totalFree uint64
	r1, _, err := procGetDiskFreeSpaceExW.Call(
		uintptr(unsafe.Pointer(pathPtr)),
		uintptr(unsafe.Pointer(&freeAvail)),
		uintptr(unsafe.Pointer(&total)),
		uintptr(unsafe.Pointer(&totalFree)),
	)
	if r1 == 0 {
		return 0, 0, err
	}
	return total, freeAvail, nil
}
