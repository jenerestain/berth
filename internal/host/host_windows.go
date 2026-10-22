// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package host

import (
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// GlobalMemoryStatusEx and GetSystemTimes are not in golang.org/x/sys,
// so they are loaded by name.
var (
	kernel32                 = windows.NewLazySystemDLL("kernel32.dll")
	procGlobalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")
	procGetSystemTimes       = kernel32.NewProc("GetSystemTimes")
)

// memoryStatusEx is MEMORYSTATUSEX (sysinfoapi.h), what
// GlobalMemoryStatusEx fills in.
type memoryStatusEx struct {
	Length     uint32
	MemoryLoad uint32
	TotalPhys  uint64
	AvailPhys  uint64
	// TotalPageFile is the system's commit limit: physical memory plus
	// the page file. AvailPageFile is what this process could still
	// commit, which is never more than the system-wide remainder.
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

// globalMemoryStatus asks kernel32 once. ok is false when the call
// fails, which a caller reports as unsupported rather than idle.
func globalMemoryStatus() (memoryStatusEx, bool) {
	var m memoryStatusEx
	m.Length = uint32(unsafe.Sizeof(m))
	r, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m)))
	if r == 0 {
		return memoryStatusEx{}, false
	}
	return m, true
}

// MemAvailable is GlobalMemoryStatusEx's available physical memory in
// bytes: the standby, free and zero pages the kernel can hand out
// without writing anything to disk first. It revises as the system
// reclaims pages, so it is never a reservation. The root is there to
// match the /proc reading off Linux and is ignored here.
func MemAvailable(root string) uint64 {
	m, ok := globalMemoryStatus()
	if !ok {
		return 0
	}
	return m.AvailPhys
}

// MemPressure stays unsupported on Windows: the kernel's memory pressure
// stall figures (PSI) are a Linux thing, and no Windows interface reads
// an equivalent.
func MemPressure(root string) (float64, bool) {
	return 0, false
}

// systemTimes is GetSystemTimes as cumulative idle and total time, in
// 100 ns ticks. Kernel time already includes idle time, so the total is
// kernel plus user.
func systemTimes() (idle, tot uint64, ok bool) {
	var i, k, u windows.Filetime
	r, _, _ := procGetSystemTimes.Call(uintptr(unsafe.Pointer(&i)), uintptr(unsafe.Pointer(&k)), uintptr(unsafe.Pointer(&u)))
	if r == 0 {
		return 0, 0, false
	}
	ticks := func(f windows.Filetime) uint64 { return uint64(f.HighDateTime)<<32 | uint64(f.LowDateTime) }
	return ticks(i), ticks(k) + ticks(u), true
}

// Read reports RAM and the page file from GlobalMemoryStatusEx, CPU from
// GetSystemTimes over the interval since the last Read, and the core count
// from the runtime. Windows has no load averages, so Load stays nil.
func (r *Reader) Read() Stats {
	m, ok := globalMemoryStatus()
	if !ok {
		return Stats{}
	}
	s := Stats{Supported: true, Cores: runtime.NumCPU()}
	if idle, tot, ok := systemTimes(); ok {
		r.mu.Lock()
		s.CPU = cpuPct(r.idle, r.tot, idle, tot)
		r.idle, r.tot = idle, tot
		r.mu.Unlock()
	}
	s.RAMTotal = m.TotalPhys
	if m.AvailPhys <= m.TotalPhys {
		s.RAMUsed = m.TotalPhys - m.AvailPhys
	}
	s.SwapTotal = m.TotalPageFile
	if m.AvailPageFile <= m.TotalPageFile {
		s.SwapUsed = m.TotalPageFile - m.AvailPageFile
	}
	return s
}
