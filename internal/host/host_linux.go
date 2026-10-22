// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package host

import (
	"bufio"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
)

// readMeminfo is the kernel's memory view in bytes, keyed by the name
// meminfo uses.
func readMeminfo(root string) map[string]uint64 {
	mem := map[string]uint64{}
	if b, err := os.ReadFile(procRoot(root) + "/meminfo"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			k, v, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			n, _ := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(v), " kB"), 10, 64)
			mem[k] = n * 1024
		}
	}
	return mem
}

// MemAvailable is meminfo's MemAvailable, in bytes: the kernel's
// estimate of the RAM a new allocation can use. It revises as pages are
// reclaimed, so it is never a reservation.
func MemAvailable(root string) uint64 {
	return readMeminfo(root)["MemAvailable"]
}

// MemPressure is the "some avg10" figure of the kernel's memory pressure
// (PSI): the share of the last 10 seconds that tasks spent stalled on
// memory, 0..100. ok is false when the file is absent, as on older
// kernels and off Linux; Windows has no PSI and stays unsupported.
func MemPressure(root string) (float64, bool) {
	f, err := os.Open(procRoot(root) + "/pressure/memory")
	if err != nil {
		return 0, false
	}
	defer f.Close()
	v, err := psiSomeAvg10(f)
	if err != nil {
		return 0, false
	}
	return v, true
}

// psiSomeAvg10 reads the avg10 figure from the "some" line of a PSI
// file.
func psiSomeAvg10(r io.Reader) (float64, error) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "some ") {
			continue
		}
		for _, f := range strings.Fields(line) {
			v, ok := strings.CutPrefix(f, "avg10=")
			if !ok {
				continue
			}
			return strconv.ParseFloat(v, 64)
		}
	}
	return 0, errors.New("no some line")
}

// Read reads the /proc counters: CPU over the interval since the last
// Read, memory from meminfo, and the load averages.
func (r *Reader) Read() Stats {
	var s Stats
	f, err := os.Open(r.root() + "/stat")
	if err != nil {
		return s
	}
	defer f.Close()
	s.Supported = true
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "cpu ") {
			var vals []uint64
			for _, x := range strings.Fields(line)[1:] {
				v, _ := strconv.ParseUint(x, 10, 64)
				vals = append(vals, v)
			}
			var tot uint64
			for _, v := range vals {
				tot += v
			}
			idle := vals[3]
			if len(vals) > 4 {
				idle += vals[4] // iowait
			}
			r.mu.Lock()
			s.CPU = cpuPct(r.idle, r.tot, idle, tot)
			r.idle, r.tot = idle, tot
			r.mu.Unlock()
		} else if strings.HasPrefix(line, "cpu") {
			s.Cores++
		}
	}
	mem := readMeminfo(r.root())
	s.RAMTotal = mem["MemTotal"]
	if a := mem["MemAvailable"]; a <= s.RAMTotal {
		s.RAMUsed = s.RAMTotal - a
	}
	s.SwapTotal = mem["SwapTotal"]
	if f := mem["SwapFree"]; f <= s.SwapTotal {
		s.SwapUsed = s.SwapTotal - f
	}
	if b, err := os.ReadFile(r.root() + "/loadavg"); err == nil {
		f := strings.Fields(string(b))
		var load [3]float64
		for i := 0; i < 3 && i < len(f); i++ {
			load[i], _ = strconv.ParseFloat(f[i], 64)
		}
		s.Load = &load
	}
	return s
}
