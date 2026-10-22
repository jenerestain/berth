// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package gpu

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func phoenixFixture(t *testing.T) AMDGPU {
	t.Helper()
	a := amdFixture(t)
	a.Index = 1
	p := amdFixtureCard(t, a, 1, "0000:0a:00.0", 11, 0, 1)
	for name, value := range map[string]uint64{"mem_info_vram_total": 8 * GiB, "mem_info_vram_used": 3 * GiB, "mem_info_gtt_total": 16 * GiB, "mem_info_gtt_used": 2 * GiB} {
		amdWrite(t, filepath.Join(p, name), fmt.Sprint(value))
	}
	return a
}

func TestPhoenix780MIsSelectable(t *testing.T) {
	a := phoenixFixture(t)
	s, err := a.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !s.APU || s.Total != 8*GiB || s.Free != 5*GiB || s.GTTTotal != 16*GiB || s.GTTUsed != 2*GiB {
		t.Fatalf("pools mixed or missing: %+v", s)
	}
}

func TestPhoenixTracksGTTOnlyAndMixedClients(t *testing.T) {
	a := phoenixFixture(t)
	amdFixtureProc(t, a, 1, "hip-style", 1, drmSample("0000:0a:00.0", "1", "drm-memory-vram: 0 KiB\ndrm-memory-gtt: 1024 MiB\ndrm-memory-cpu: 9999 MiB"))
	amdFixtureProc(t, a, 2, "vulkan-style", 2, drmSample("0000:0a:00.0", "2", "drm-resident-vram: 1024 MiB\ndrm-resident-gtt: 512 MiB\ndrm-memory-gtt: 1024 MiB"))
	amdFixtureProc(t, a, 3, "different-gpu", 3, drmSample("0000:03:00.0", "1", "drm-memory-vram: 4096 MiB"))
	s, err := a.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Procs) != 2 || s.Procs[0].Used != 0 || s.Procs[0].GTT != GiB || s.Procs[1].Used != GiB || s.Procs[1].GTT != 512*MiB {
		t.Fatalf("domain accounting %+v", s.Procs)
	}
	if s.Baseline() != 2*GiB || s.GTTBaseline() != 512*MiB {
		t.Fatalf("independent baselines %+v", s)
	}
}

func TestPhoenixDeduplicatesBothPoolsAndDiscountsShared(t *testing.T) {
	a := phoenixFixture(t)
	sample := drmSample("0000:0a:00.0", "5", "drm-memory-vram: 2 MiB\ndrm-memory-gtt: 4 MiB\ndrm-shared-vram: 1 MiB\ndrm-shared-gtt: 2 MiB")
	amdFixtureProc(t, a, 1, "parent", 1, sample, sample)
	amdFixtureProc(t, a, 2, "inherited", 2, sample)
	s, err := a.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Procs) != 1 || s.Procs[0].Used != MiB || s.Procs[0].GTT != 2*MiB {
		t.Fatalf("shared duplicate %+v", s.Procs)
	}
}

func TestPhoenixRejectsMissingAndInvalidGTT(t *testing.T) {
	a := phoenixFixture(t)
	p := filepath.Join(a.SysRoot, "card1/device/mem_info_gtt_used")
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Read(context.Background()); !errors.Is(err, ErrUnsafeRead) {
		t.Fatalf("missing GTT: %v", err)
	}
	amdWrite(t, p, fmt.Sprint(17*GiB))
	if _, err := a.Read(context.Background()); !errors.Is(err, ErrUnsafeRead) {
		t.Fatalf("overfull GTT: %v", err)
	}
	amdWrite(t, p, fmt.Sprint(2*GiB))
	amdFixtureProc(t, a, 1, "incomplete-client", 1, drmSample("0000:0a:00.0", "1", "drm-memory-vram: 1 MiB"))
	if _, err := a.Read(context.Background()); !errors.Is(err, ErrUnsafeRead) {
		t.Fatalf("missing client GTT: %v", err)
	}
}
