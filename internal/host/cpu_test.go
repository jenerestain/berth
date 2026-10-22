// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCPUPct(t *testing.T) {
	for _, c := range []struct {
		name                         string
		prevIdle, prevTot, idle, tot uint64
		want                         float64
	}{
		{"first read has no interval", 0, 0, 800, 1000, 0},
		{"half busy", 800, 1000, 900, 1200, 50},
		{"all idle", 800, 1000, 1000, 1200, 0},
		{"all busy", 800, 1000, 800, 1200, 100},
		{"counters went back", 800, 1000, 100, 200, 0},
		{"no time passed", 800, 1000, 800, 1000, 0},
	} {
		if got := cpuPct(c.prevIdle, c.prevTot, c.idle, c.tot); got != c.want {
			t.Errorf("%s: cpuPct = %v, want %v", c.name, got, c.want)
		}
	}
}

// A platform that reads no load averages says so by leaving them out, so a
// page shows n/a instead of three measured zeros.
func TestLoadUnmeasuredIsAbsent(t *testing.T) {
	b, _ := json.Marshal(Stats{Supported: true})
	if strings.Contains(string(b), `"load"`) {
		t.Fatalf("unmeasured load is in the JSON: %s", b)
	}
	b, _ = json.Marshal(Stats{Supported: true, Load: &[3]float64{1.5, 0.75, 0.25}})
	if !strings.Contains(string(b), `"load":[1.5,0.75,0.25]`) {
		t.Fatalf("measured load = %s", b)
	}
}
