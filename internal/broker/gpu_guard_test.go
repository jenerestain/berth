// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/gpu"
	"github.com/T-Crypt/berth/internal/procs"
	"github.com/T-Crypt/berth/internal/proto"
)

type guardedCard struct {
	snap gpu.Snapshot
	err  error
}

func (c *guardedCard) Read(context.Context) (gpu.Snapshot, error) { return c.snap, c.err }

func guardedSetup(t *testing.T) (*Broker, *guardedCard) {
	t.Helper()
	// No host card in a VM may turn a failed read into passthrough.
	gpu.SysfsRoot = t.TempDir()
	t.Cleanup(func() { gpu.SysfsRoot = "/sys" })
	b, _, _ := setup(t, parkTree(100, 101))
	c := &guardedCard{}
	c.set(0)
	b.probe = c
	b.cfg.MemAvailable = func() uint64 { return 32 * GiB }
	b.cfg.MemPressure = func() float64 { return 0 }
	b.Tick(context.Background())
	return b, c
}

func (c *guardedCard) set(used uint64, ps ...gpu.Proc) {
	c.snap = gpu.Snapshot{Guarded: true, MaxAge: time.Minute, At: time.Now(), Total: 24 * GiB, Used: used, Free: 24*GiB - used, Procs: ps}
}

func TestGuardedReleaseNeedsCardWideConfirmation(t *testing.T) {
	b, c := guardedSetup(t)
	l := must(t, acquire(b, Request{Engine: "amd-first"}))
	l.Attach(100)
	c.set(8*GiB, gpu.Proc{PID: 101, Used: 8 * GiB, Start: 1})
	b.Tick(context.Background())
	l.Release()
	w := acquire(b, Request{Engine: "amd-next"})
	c.set(8 * GiB) // /proc is gone; the driver still holds the buffers
	b.Tick(context.Background())
	pending(t, w)
	if st := b.Status(); len(st.Leases) != 1 {
		t.Fatalf("released before driver reclaimed memory: %+v", st.Leases)
	}
	c.set(0)
	b.Tick(context.Background())
	must(t, w)
}

func TestGuardedUnrelatedReleaseCannotMaskLingeringBuffers(t *testing.T) {
	b, c := guardedSetup(t)
	c.set(16*GiB, gpu.Proc{PID: 9, Used: 16 * GiB})
	b.Tick(context.Background())
	l := must(t, acquire(b, Request{Engine: "first"}))
	l.Attach(100)
	c.set(20*GiB, gpu.Proc{PID: 9, Used: 16 * GiB}, gpu.Proc{PID: 101, Used: 4 * GiB, Start: 1})
	b.Tick(context.Background())
	l.Release()
	c.set(4 * GiB) // neighbour freed 16 GiB, our orphaned buffers still occupy 4 GiB
	b.Tick(context.Background())
	w := acquire(b, Request{Engine: "next"})
	pending(t, w)
	c.set(0)
	b.Tick(context.Background())
	must(t, w)
}

func TestGuardedUnreadablePIDKeepsOwnership(t *testing.T) {
	b, c := guardedSetup(t)
	l := must(t, acquire(b, Request{Engine: "amd-first"}))
	l.Attach(100)
	c.set(8*GiB, gpu.Proc{PID: 101, Used: 8 * GiB, Start: 1})
	b.Tick(context.Background())
	l.Release()
	c.set(8 * GiB)
	c.snap.Unobserved = []int{101}
	b.Tick(context.Background())
	st := b.Status()
	if len(st.Leases) != 1 || st.Leases[0].Used != 8*GiB {
		t.Fatalf("unreadable holder forgotten: %+v", st.Leases)
	}
}

func TestGuardedStaleSuccessCannotFreeLease(t *testing.T) {
	b, c := guardedSetup(t)
	l := must(t, acquire(b, Request{Engine: "first"}))
	l.Attach(100)
	c.set(8*GiB, gpu.Proc{PID: 101, Used: 8 * GiB, Start: 1})
	b.Tick(context.Background())
	l.Release()
	c.set(0)
	c.snap.At = time.Now().Add(-time.Hour)
	b.Tick(context.Background())
	if st := b.Status(); len(st.Leases) != 1 || st.ProbeErr == "" {
		t.Fatalf("stale data freed holder: %+v", st)
	}
	c.set(0)
	b.Tick(context.Background())
	if st := b.Status(); len(st.Leases) != 0 {
		t.Fatalf("fresh recovery did not free holder: %+v", st.Leases)
	}
}

func TestGuardedReattachKeepsDriverBaseline(t *testing.T) {
	b, c := guardedSetup(t)
	baseline := uint64(16 * gpu.MiB)
	c.set(8*GiB+baseline, gpu.Proc{PID: 101, Used: 8 * GiB, Start: 1})
	b.Tick(context.Background())
	l, err := b.Reattach(Request{Engine: "survived-restart"}, 100, 1)
	if err != nil {
		t.Fatal(err)
	}
	b.Tick(context.Background())
	l.Release()
	c.set(baseline)
	b.Tick(context.Background())
	if st := b.Status(); len(st.Leases) != 0 {
		t.Fatalf("reattached lease lost its baseline: %+v", st.Leases)
	}
}

func TestGuardedProbePreservesVFIOHoldAndRecovery(t *testing.T) {
	skipNonLinux(t)
	root := sysfsCard(t, "0000:03:00.0", "amdgpu")
	b, c := guardedSetup(t)
	c.snap.Devices = []gpu.Device{{Index: 0, BusID: "0000:03:00.0", Total: 24 * GiB}}
	b.Tick(context.Background())
	hold := must(t, acquire(b, vfioRequest()))
	// A hold intentionally suppresses observations. It must not look like a
	// failed or expired AMD probe, nor allow new work while the card is away.
	c.err = fmt.Errorf("%w: probe must not run under a VFIO hold", gpu.ErrUnsafeRead)
	b.Tick(context.Background())
	if st := b.Status(); st.ProbeErr != "" || st.Passthrough == nil || st.Passthrough.State != proto.CardDetaching {
		t.Fatalf("held AMD state: %+v", st)
	}
	bindCard(t, root, "0000:03:00.0", gpu.VFIODriver)
	b.Tick(context.Background())
	w := acquire(b, Request{Engine: "after-vm", Needs: GiB})
	pending(t, w)
	hold.Release()
	bindCard(t, root, "0000:03:00.0", "amdgpu")
	c.err = nil
	c.set(0)
	c.snap.Devices = []gpu.Device{{Index: 0, BusID: "0000:03:00.0", Total: 24 * GiB}}
	b.Tick(context.Background())
	must(t, w)
	if st := b.Status(); st.Passthrough != nil {
		t.Fatalf("returned card still marked passed through: %+v", st.Passthrough)
	}
}

func TestGuardedProbeFailureAndStalenessBlockAdmissions(t *testing.T) {
	for _, mode := range []string{"failed", "stale", "initial-failure"} {
		t.Run(mode, func(t *testing.T) {
			b, c := guardedSetup(t)
			switch mode {
			case "failed":
				c.err = fmt.Errorf("%w: fixture", gpu.ErrUnsafeRead)
				b.Tick(context.Background())
			case "stale":
				b.mu.Lock()
				b.snap.At = time.Now().Add(-time.Hour)
				b.mu.Unlock()
			case "initial-failure":
				b.snap = gpu.Snapshot{}
				c.err = fmt.Errorf("%w: fixture", gpu.ErrUnsafeRead)
				b.Tick(context.Background())
			}
			w := acquire(b, Request{Engine: "must-wait"})
			pending(t, w)
			c.err = nil
			c.set(0)
			b.Tick(context.Background())
			must(t, w)
		})
	}
}

func TestGuardedParkingNeedsCardWideConfirmation(t *testing.T) {
	b, c := guardedSetup(t)
	b.cfg.MemAvailable = func() uint64 { return 32 * GiB }
	l := must(t, acquire(b, Request{Engine: "amd-park", Needs: 8 * GiB}))
	l.Attach(100)
	c.set(8*GiB, gpu.Proc{PID: 101, Used: 8 * GiB, Start: 1})
	b.Tick(context.Background())
	w := acquire(b, Request{Engine: "next", Needs: 20 * GiB, Priority: Foreground})
	pending(t, w)
	drained(t, l)
	if ok, why := l.Park(); !ok {
		t.Fatal(why)
	}
	l.Parked()
	c.set(8 * GiB) // lost process visibility alone cannot confirm the park
	b.Tick(context.Background())
	if st := b.Status(); len(st.Leases) != 1 || st.Leases[0].State != "parking" {
		t.Fatalf("premature park: %+v", st.Leases)
	}
	pending(t, w)
	c.set(512*gpu.MiB, gpu.Proc{PID: 101, Used: 512 * gpu.MiB, Start: 1})
	b.Tick(context.Background())
	must(t, w)
}

func TestGuardedDriverFreeCapsSizedAdmissions(t *testing.T) {
	b, c := guardedSetup(t)
	// Shared/overlapping per-client counters must not manufacture free VRAM.
	c.set(23*GiB, gpu.Proc{PID: 9, Used: 24 * GiB})
	b.Tick(context.Background())
	w := acquire(b, Request{Engine: "sized", Needs: 2 * GiB})
	pending(t, w)
	c.set(0)
	b.Tick(context.Background())
	must(t, w)
}

func TestProbeSampledIdentityDoesNotInheritReusedPID(t *testing.T) {
	b, c := guardedSetup(t)
	b.tree = procs.Fake{Parents: map[int]int{101: 100}, Starts: map[int]uint64{100: 2, 101: 2}}
	l := must(t, acquire(b, Request{Engine: "new-process"}))
	l.Attach(100)
	c.set(GiB, gpu.Proc{PID: 101, Used: GiB, Start: 1}) // observation of the previous PID
	b.Tick(context.Background())
	if st := b.Status(); st.Leases[0].Used != 0 {
		t.Fatalf("reused PID inherited old observation: %+v", st.Leases)
	}
}
