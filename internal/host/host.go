// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

// Package host reads the machine's memory, CPU and load. Off Unix, where
// the /proc readers do not run, a Windows machine reads its memory from
// GlobalMemoryStatusEx and anything else reports Supported=false rather
// than zeros that look like an idle machine.
package host

import "sync"

type Stats struct {
	Supported bool    `json:"supported"`
	CPU       float64 `json:"cpu_pct"` // all cores, since the previous Read
	Cores     int     `json:"cores"`
	RAMTotal  uint64  `json:"ram_total"`
	RAMUsed   uint64  `json:"ram_used"` // total minus available
	SwapTotal uint64  `json:"swap_total"`
	SwapUsed  uint64  `json:"swap_used"`
	// Load is the 1, 5 and 15 minute load averages; nil where the platform
	// has none (Windows), so the JSON leaves it out instead of saying 0.
	Load *[3]float64 `json:"load,omitempty"`
}

// cpuPct is the share of time not idle between two readings of cumulative
// counters. The first reading, or counters that did not move forward,
// give 0: there is no interval to measure.
func cpuPct(prevIdle, prevTot, idle, tot uint64) float64 {
	if prevTot == 0 || tot <= prevTot || idle < prevIdle {
		return 0
	}
	return 100 * (1 - float64(idle-prevIdle)/float64(tot-prevTot))
}

// Reader keeps the previous CPU counters so each Read returns usage over
// the interval since the last one. ProcRoot points the Linux readers at
// a tree of fixture files; other platforms ignore it.
type Reader struct {
	mu        sync.Mutex
	idle, tot uint64
	ProcRoot  string // default /proc; tests point it elsewhere
}

// procRoot is the /proc tree to read from; empty means the real one.
func procRoot(root string) string {
	if root == "" {
		return "/proc"
	}
	return root
}

func (r *Reader) root() string { return procRoot(r.ProcRoot) }
