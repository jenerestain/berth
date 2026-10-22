// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package host

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRead(t *testing.T) {
	dir := t.TempDir()
	w := func(n, s string) { os.WriteFile(filepath.Join(dir, n), []byte(s), 0o644) }
	w("stat", "cpu  100 0 100 800 0 0 0 0 0 0\ncpu0 1\ncpu1 1\n")
	w("meminfo", "MemTotal: 1000 kB\nMemAvailable: 250 kB\nSwapTotal: 100 kB\nSwapFree: 40 kB\n")
	w("loadavg", "1.50 0.75 0.25 2/300 99\n")
	r := &Reader{ProcRoot: dir}
	s := r.Read()
	if !s.Supported || s.Cores != 2 || s.RAMUsed != 750*1024 || s.SwapUsed != 60*1024 || s.Load == nil || s.Load[0] != 1.5 || s.CPU != 0 {
		t.Fatalf("first read = %+v", s)
	}
	w("stat", "cpu  150 0 150 900 0 0 0 0 0 0\ncpu0 1\ncpu1 1\n") // +100 busy, +100 idle
	if s = r.Read(); s.CPU != 50 {
		t.Fatalf("cpu = %v, want 50", s.CPU)
	}
	if (&Reader{ProcRoot: filepath.Join(dir, "none")}).Read().Supported {
		t.Fatal("missing /proc must read as unsupported")
	}
	if got := MemAvailable(dir); got != 250*1024 {
		t.Fatalf("MemAvailable = %d, want the 250 kB the fixture holds", got)
	}
	if got := MemAvailable(filepath.Join(dir, "none")); got != 0 {
		t.Fatalf("missing meminfo = %d, want 0", got)
	}
}

func TestMemPressure(t *testing.T) {
	dir := t.TempDir()
	w := func(n, s string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, n), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "pressure"), 0o755); err != nil {
		t.Fatal(err)
	}
	w("pressure/memory", "some avg10=2.50 avg60=1.00 avg300=0.50 total=123456\nfull avg10=9.99 avg60=0.00 avg300=0.00 total=0\n")
	if v, ok := MemPressure(dir); !ok || v != 2.5 {
		t.Fatalf("MemPressure = %v, %v, want 2.5 from the some line", v, ok)
	}
	if _, ok := MemPressure(filepath.Join(dir, "none")); ok {
		t.Fatal("missing PSI file must read as unavailable")
	}
}

func TestPsiSomeAvg10(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    float64
		wantErr bool
	}{
		{"some line first", "some avg10=3.25 avg60=1.00 avg300=0.00 total=1\nfull avg10=9.99 avg60=0.00 avg300=0.00 total=0\n", 3.25, false},
		{"no some line", "full avg10=9.99 avg60=0.00 avg300=0.00 total=0\n", 0, true},
		{"empty", "", 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := psiSomeAvg10(strings.NewReader(tc.in))
			if tc.wantErr != (err != nil) {
				t.Fatalf("psiSomeAvg10 = %v, %v", v, err)
			}
			if !tc.wantErr && v != tc.want {
				t.Fatalf("psiSomeAvg10 = %v, want %v", v, tc.want)
			}
		})
	}
}
