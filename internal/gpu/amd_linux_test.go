// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package gpu

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func amdWrite(t *testing.T, path, value string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

func amdFixture(t *testing.T) AMDGPU {
	t.Helper()
	root := t.TempDir()
	a := AMDGPU{SysRoot: filepath.Join(root, "drm"), ProcRoot: filepath.Join(root, "proc")}
	for _, p := range []string{a.SysRoot, a.ProcRoot} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	amdFixtureCard(t, a, 0, "0000:03:00.0", 11, 0, 0)
	return a
}

func amdFixtureCard(t *testing.T, a AMDGPU, index int, bus string, major, minor, rev int) string {
	t.Helper()
	p := filepath.Join(a.SysRoot, fmt.Sprintf("card%d/device", index))
	amdWrite(t, filepath.Join(p, "uevent"), "DRIVER=amdgpu\nPCI_SLOT_NAME="+bus+"\nPCI_ID=1002:7448\n")
	for name, value := range map[string]string{
		"ip_discovery/die/0/GC/0/major": fmt.Sprint(major), "ip_discovery/die/0/GC/0/minor": fmt.Sprint(minor), "ip_discovery/die/0/GC/0/revision": fmt.Sprint(rev),
		"mem_info_vram_total": fmt.Sprint(24 * GiB), "mem_info_vram_used": fmt.Sprint(8 * GiB),
	} {
		amdWrite(t, filepath.Join(p, name), value)
	}
	return p
}

func amdFixtureProc(t *testing.T, a AMDGPU, pid int, name string, start uint64, samples ...string) string {
	t.Helper()
	p := filepath.Join(a.ProcRoot, fmt.Sprint(pid))
	amdWrite(t, filepath.Join(p, "comm"), name+"\n")
	amdWrite(t, filepath.Join(p, "stat"), fmt.Sprintf("%d (%s) S %s %d\n", pid, name, strings.Repeat("0 ", 18), start))
	if err := os.MkdirAll(filepath.Join(p, "fdinfo"), 0o755); err != nil {
		t.Fatal(err)
	}
	for i, s := range samples {
		amdWrite(t, filepath.Join(p, "fdinfo", fmt.Sprint(i)), s)
	}
	return p
}

func drmSample(bus, id, mem string) string {
	return "drm-driver:\tamdgpu\ndrm-pdev:\t" + bus + "\ndrm-client-id:\t" + id + "\n" + mem + "\n"
}

func TestAMDRDNA3And4Snapshots(t *testing.T) {
	for _, ip := range [][3]int{{11, 0, 0}, {11, 0, 2}, {11, 0, 3}, {12, 0, 0}, {12, 0, 1}} {
		t.Run(fmt.Sprint(ip), func(t *testing.T) {
			a := amdFixture(t)
			amdFixtureCard(t, a, 0, "0000:03:00.0", ip[0], ip[1], ip[2])
			amdFixtureProc(t, a, 101, "llama (server)", 123, drmSample("00000000:03:00.0", "7", "drm-memory-vram: 2048 MiB\ndrm-memory-gtt: 9000 MiB\ndrm-memory-cpu: 4000 MiB"))
			s, err := a.Read(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if !s.Guarded || s.MaxAge <= 0 || s.At.IsZero() || s.Total != 24*GiB || s.Used != 8*GiB || s.Free != 16*GiB {
				t.Fatalf("snapshot %+v", s)
			}
			if len(s.Procs) != 1 || s.Procs[0].Used != 2*GiB || s.Procs[0].Start != 123 || s.Procs[0].Name != "llama (server)" {
				t.Fatalf("processes %+v", s.Procs)
			}
			if s.Baseline() != 6*GiB || s.Devices[0].Temp != -1 || s.Devices[0].Fan != -1 {
				t.Fatalf("baseline or unknown telemetry %+v", s)
			}
		})
	}
}

func TestAMDMultiCardAndUnsupportedAPUExclusion(t *testing.T) {
	a := amdFixture(t)
	amdFixtureCard(t, a, 1, "0000:0a:00.0", 11, 5, 0) // unvalidated APU IP remains excluded
	amdFixtureCard(t, a, 10, "0001:04:00.0", 12, 0, 1)
	amdFixtureProc(t, a, 101, "engine", 123,
		drmSample("0000:03:00.0", "9", "drm-memory-vram: 1024 MiB"),
		drmSample("0000:0a:00.0", "9", "drm-memory-vram: 2048 MiB"),
		drmSample("0001:04:00.0", "9", "drm-resident-vram: 3072 MiB"))
	a.Index = 10
	s, err := a.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.Index != 10 || len(s.Devices) != 2 || s.Devices[1].Index != 10 || len(s.Procs) != 1 || s.Procs[0].Used != 3*GiB {
		t.Fatalf("snapshot %+v", s)
	}
	for _, index := range []int{1, 2} {
		a.Index = index
		if _, err := a.Read(context.Background()); !errors.Is(err, ErrUnsafeRead) {
			t.Fatalf("card%d: %v", index, err)
		}
	}
}

func TestAMDClientDeduplicationAndAliases(t *testing.T) {
	a := amdFixture(t)
	sample := drmSample("0000:03:00.0", "7", "drm-resident-vram: 1024 MiB\ndrm-memory-vram: 2048 MiB\ndrm-total-vram: 9999 MiB")
	amdFixtureProc(t, a, 10, "parent", 100, sample, sample)
	amdFixtureProc(t, a, 9, "inherited", 90, sample, drmSample("0000:03:00.0", "8", "drm-memory-vram: 1 MiB"))
	amdFixtureProc(t, a, 12, "empty-context", 120, drmSample("0000:03:00.0", "10", "drm-memory-vram: 0 KiB"))
	s, err := a.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Procs) != 1 || s.Procs[0].PID != 9 || s.Procs[0].Used != 1025*MiB {
		t.Fatalf("dedup %+v", s.Procs)
	}
}

func TestAMDSharedBuffersRemainUnattributed(t *testing.T) {
	a := amdFixture(t)
	for _, pid := range []int{10, 11} {
		amdFixtureProc(t, a, pid, "shared-client", 100, drmSample("0000:03:00.0", fmt.Sprint(pid), "drm-resident-vram: 3 MiB\ndrm-shared-vram: 2 MiB"))
	}
	s, err := a.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Procs) != 2 || s.Procs[0].Used != MiB || s.Procs[1].Used != MiB || s.Baseline() != s.Used-2*MiB {
		t.Fatalf("shared accounting: %+v", s)
	}
}

func TestAMDPCIIdentityValidation(t *testing.T) {
	for _, input := range []string{"0000:03:00.0", "00000000:03:00.0"} {
		if got, err := pciAddress(input); err != nil || got != "0000:03:00.0" {
			t.Fatalf("%q: %q %v", input, got, err)
		}
	}
	for _, input := range []string{"0000:::03:00.0", "03:00.0", "0000:03:20.0", "0000:03:00.8", "0000:03:00", "invalid"} {
		if _, err := pciAddress(input); err == nil {
			t.Fatalf("accepted invalid PCI identity %q", input)
		}
	}
}

func TestAMDRejectsUntrustworthyReadings(t *testing.T) {
	for _, mem := range []string{"drm-total-vram: 3 MiB", "drm-memory-vram: -1 KiB", "drm-memory-vram: 2 GB", "drm-memory-vram: 18446744073709551615 MiB"} {
		t.Run(mem, func(t *testing.T) {
			a := amdFixture(t)
			amdFixtureProc(t, a, 1, "engine", 1, drmSample("0000:03:00.0", "7", mem))
			if _, err := a.Read(context.Background()); !errors.Is(err, ErrUnsafeRead) {
				t.Fatalf("err = %v", err)
			}
		})
	}
	a := amdFixture(t)
	amdWrite(t, filepath.Join(a.SysRoot, "card0/device/mem_info_vram_used"), fmt.Sprint(25*GiB))
	if _, err := a.Read(context.Background()); !errors.Is(err, ErrUnsafeRead) {
		t.Fatalf("overfull reading: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.Read(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
}

func TestAMDMissingOrUnreadableProcessData(t *testing.T) {
	a := amdFixture(t)
	p := amdFixtureProc(t, a, 1, "engine", 1, drmSample("0000:03:00.0", "7", "drm-memory-vram: 1 MiB"))
	// Closed descriptor/process races are ordinary; malformed live data isn't.
	if err := os.Remove(filepath.Join(p, "fdinfo/0")); err != nil {
		t.Fatal(err)
	}
	s, err := a.Read(context.Background())
	if err != nil || len(s.Procs) != 0 {
		t.Fatalf("closed descriptor: %+v %v", s, err)
	}
	amdWrite(t, filepath.Join(p, "stat"), "broken")
	if _, err := a.Read(context.Background()); !errors.Is(err, ErrUnsafeRead) {
		t.Fatalf("bad stat: %v", err)
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permission fixture")
	}
	amdFixtureProc(t, a, 1, "engine", 1, drmSample("0000:03:00.0", "7", "drm-memory-vram: 1 MiB"))
	if err := os.Chmod(filepath.Join(p, "fdinfo/0"), 0); err != nil {
		t.Fatal(err)
	}
	if s, err := a.Read(context.Background()); err != nil || len(s.Procs) != 0 || len(s.Unobserved) != 1 || s.Unobserved[0] != 1 {
		t.Fatalf("permission failure must be explicit, not a complete empty scan: %+v %v", s, err)
	}
}

func TestAMDTelemetry(t *testing.T) {
	a := amdFixture(t)
	p := filepath.Join(a.SysRoot, "card0/device")
	for name, value := range map[string]string{"gpu_busy_percent": "72", "hwmon/hwmon9/name": "amdgpu", "hwmon/hwmon9/temp1_input": "65000", "hwmon/hwmon9/power1_average": "120000000", "hwmon/hwmon9/power1_cap": "295000000", "hwmon/hwmon9/freq1_input": "2500000000", "hwmon/hwmon9/freq2_input": "1250000000", "current_link_speed": "32.0 GT/s PCIe", "current_link_width": "16"} {
		amdWrite(t, filepath.Join(p, name), value)
	}
	s, err := a.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	d := s.Devices[0]
	if d.Util != 72 || d.Temp != 65 || d.Power != 120 || d.PowerLimit != 295 || d.ClockSM != 2500 || d.ClockMem != 1250 || d.PCIeGen != 5 || d.PCIeWidth != 16 || d.Fan != -1 {
		t.Fatalf("telemetry %+v", d)
	}
}

func TestOpenAMDAndMixedVendorSelection(t *testing.T) {
	a := amdFixture(t)
	t.Setenv("PATH", t.TempDir())
	opts := Options{SysRoot: a.SysRoot, ProcRoot: a.ProcRoot, Interval: time.Second}
	p, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.(*AMDGPU); !ok {
		t.Fatalf("probe %T", p)
	}
	bin := filepath.Join(os.Getenv("PATH"), "nvidia-smi")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	// An NVIDIA card beside a supported AMD GPU (a Phoenix laptop with an
	// RTX dGPU) keeps the NVIDIA probe every installed unit has used.
	p, err = Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.(*Stream); !ok {
		t.Fatalf("auto on a mixed host chose %T, want the NVIDIA probe", p)
	}
	opts.Backend = "nvidia"
	p, err = Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.(*Stream); !ok {
		t.Fatalf("probe %T", p)
	}
	opts.Backend = "amd"
	if _, err := Open(opts); err != nil {
		t.Fatal(err)
	}
}

// Opt-in read-only host check; normal tests never depend on installed GPUs.
func TestAMDLiveReadOnly(t *testing.T) {
	if os.Getenv("BERTH_TEST_AMD_LIVE") != "1" {
		t.Skip("set BERTH_TEST_AMD_LIVE=1 to inspect host sysfs/procfs")
	}
	a := AMDGPU{}
	if index := os.Getenv("BERTH_TEST_AMD_CARD"); index != "" {
		if _, err := fmt.Sscan(index, &a.Index); err != nil {
			t.Fatal(err)
		}
	}
	s, err := a.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var card Device
	for _, d := range s.Devices {
		if d.Index == s.Index {
			card = d
		}
	}
	t.Logf("%s PCI=%s APU=%v VRAM total=%d used=%d free=%d GTT capacity=%d used=%d processes=%+v telemetry=%+v", s.Name, card.BusID, s.APU, s.Total, s.Used, s.Free, s.GTTTotal, s.GTTUsed, s.Procs, card)
}
