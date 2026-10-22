// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/T-Crypt/berth/internal/gpu"
)

func apuSetup(t *testing.T) (*Broker, *guardedCard) {
	t.Helper()
	b, c := guardedSetup(t)
	b.cfg.MemAvailable = func() uint64 { return 8 * GiB }
	b.cfg.MemPressure = func() float64 { return 0 }
	apuSet(c, 0, 0)
	b.Tick(context.Background())
	return b, c
}

func apuSet(c *guardedCard, vram, gtt uint64, ps ...gpu.Proc) {
	c.set(vram, ps...)
	c.snap.APU = true
	c.snap.GTTTotal = 16 * GiB
	c.snap.GTTUsed = gtt
}

func TestAPUExclusiveOnlyAndSeparateStatusPools(t *testing.T) {
	b, c := apuSetup(t)
	if _, err := b.Acquire(context.Background(), Request{Engine: "sized", Needs: GiB}, nil); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "exclusive") {
		t.Fatalf("sized APU request: %v", err)
	}
	l := must(t, acquire(b, Request{Engine: "gtt-holder"}))
	l.Attach(100)
	apuSet(c, 0, 4*GiB, gpu.Proc{PID: 101, Start: 1, GTT: 4 * GiB})
	b.Tick(context.Background())
	st := b.Status()
	if len(st.Leases) != 1 || st.Leases[0].Used != 0 || st.Leases[0].GTTUsed != 4*GiB || !st.GPUs[0].APU || st.GPUs[0].GTTUsed != 4*GiB {
		t.Fatalf("APU status %+v", st)
	}
}

func TestAPUGTTMustBeReleasedIndependentlyOfVRAM(t *testing.T) {
	b, c := apuSetup(t)
	l := must(t, acquire(b, Request{Engine: "first"}))
	l.Attach(100)
	apuSet(c, 0, 4*GiB, gpu.Proc{PID: 101, Start: 1, GTT: 4 * GiB})
	b.Tick(context.Background())
	l.Release()
	w := acquire(b, Request{Engine: "next"})
	apuSet(c, 0, 4*GiB) // no VRAM, no /proc entry, but GTT still resident
	b.Tick(context.Background())
	pending(t, w)
	apuSet(c, 0, 0)
	b.Tick(context.Background())
	must(t, w)
}

func TestAPUUnreadableGTTProcessKeepsItsLease(t *testing.T) {
	b, c := apuSetup(t)
	l := must(t, acquire(b, Request{Engine: "first"}))
	l.Attach(100)
	apuSet(c, 0, GiB, gpu.Proc{PID: 101, Start: 1, GTT: GiB})
	b.Tick(context.Background())
	l.Release()
	apuSet(c, 0, GiB)
	c.snap.Unobserved = []int{101}
	b.Tick(context.Background())
	if st := b.Status(); len(st.Leases) != 1 || st.Leases[0].GTTUsed != GiB {
		t.Fatalf("forgot unreadable GTT holder: %+v", st.Leases)
	}
}

func TestAPUEmptyPoolCannotOffsetOtherPoolLeak(t *testing.T) {
	b, c := apuSetup(t)
	// A 1 GiB initial shared baseline must not mask a 1 GiB reserved leak.
	apuSet(c, 0, GiB)
	b.Tick(context.Background())
	l := must(t, acquire(b, Request{Engine: "first"}))
	l.Attach(100)
	apuSet(c, GiB, GiB, gpu.Proc{PID: 101, Start: 1, Used: GiB})
	b.Tick(context.Background())
	l.Release()
	apuSet(c, GiB, 0)
	b.Tick(context.Background())
	w := acquire(b, Request{Engine: "next"})
	pending(t, w)
	apuSet(c, 0, 0)
	b.Tick(context.Background())
	must(t, w)
}

func TestAPURAMHeadroomAndPressureBlockOnlyNewAdmissions(t *testing.T) {
	for _, mode := range []string{"unknown-ram", "low-ram", "pressure", "full-gtt"} {
		t.Run(mode, func(t *testing.T) {
			b, c := apuSetup(t)
			switch mode {
			case "unknown-ram":
				b.cfg.MemAvailable = func() uint64 { return 0 }
			case "low-ram":
				b.cfg.MemAvailable = func() uint64 { return GiB }
			case "pressure":
				b.cfg.MemPressure = func() float64 { return 90 }
			case "full-gtt":
				apuSet(c, 0, 16*GiB)
				b.Tick(context.Background())
			}
			w := acquire(b, Request{Engine: "next"})
			pending(t, w)
			b.mu.Lock()
			b.cfg.MemAvailable = func() uint64 { return 8 * GiB }
			b.cfg.MemPressure = func() float64 { return 0 }
			b.mu.Unlock()
			apuSet(c, 0, 0)
			b.Tick(context.Background())
			must(t, w)
		})
	}
	// Low RAM may be caused by the holder: it must still be allowed to drain.
	b, c := apuSetup(t)
	l := must(t, acquire(b, Request{Engine: "first"}))
	l.Attach(100)
	apuSet(c, 0, GiB, gpu.Proc{PID: 101, Start: 1, GTT: GiB})
	b.Tick(context.Background())
	b.cfg.MemAvailable = func() uint64 { return GiB }
	w := acquire(b, Request{Engine: "next", Priority: Foreground})
	pending(t, w)
	drained(t, l)
	if ok, why := l.Park(); ok || !strings.Contains(why, "APU") {
		t.Fatalf("APU park not explicitly refused: %v %q", ok, why)
	}
	l.Release()
	apuSet(c, 0, 0)
	b.Tick(context.Background())
	pending(t, w)
	b.mu.Lock()
	b.cfg.MemAvailable = func() uint64 { return 8 * GiB }
	b.mu.Unlock()
	b.Tick(context.Background())
	must(t, w)
}
