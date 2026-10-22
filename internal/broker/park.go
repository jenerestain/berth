// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package broker

// Park in RAM. A draining lease whose engine can offload itself asks the
// broker to park (admission over the machine's readings), parks through its
// adapter, and stays a lease whose residual keeps the card counted. A
// waiter builds beside a small residual, evicts a big one, and a resume
// hands the parked engine the card back at its own priority. Nothing here
// is automatic: park = "hook" on the engine's entry is the only way in, and
// an eviction is a drain the runner stops, exactly as today.

import (
	"cmp"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/T-Crypt/berth/internal/park"
	"github.com/T-Crypt/berth/internal/proto"
)

// Park asks the broker to admit this lease's engine into RAM parking: a
// refusal comes back with its why line, and the drain runs as it always
// has.
func (l *Lease) Park() (bool, string) { return l.b.Park(l.l.id) }

// Parked records that this lease's park call is done, so the broker watches
// for the residual to leave the card.
func (l *Lease) Parked() {
	l.b.mu.Lock()
	l.l.parkedCall = l.b.now()
	l.l.parkedRead = l.b.reads.Load()
	l.b.mu.Unlock()
}

// ParkFailed puts a lease whose park call failed back on today's ladder:
// draining, and the runner stops it as any other.
func (l *Lease) ParkFailed() {
	l.b.mu.Lock()
	defer l.b.mu.Unlock()
	if l.l.state == parking {
		l.l.state = draining
	}
}

// Waits delivers the reasons while a resumed lease waits in the queue.
func (l *Lease) Waits() <-chan string { return l.l.waitCh }

// Resumed delivers the runner's wake signal: one token per grant.
func (l *Lease) Resumed() <-chan struct{} { return l.l.resumeCh }

// Awake records that a resumed lease's engine answers its probe again:
// the event carries the seconds the wake took, and from this point the
// gateway may proxy the engine.
func (l *Lease) Awake() {
	l.b.mu.Lock()
	defer l.b.mu.Unlock()
	if l.l.state != active || l.l.granted.IsZero() {
		return
	}
	l.b.emit(proto.Event{Kind: proto.EvResumed, Lease: l.l.id, Engine: l.l.engine,
		Detail: fmt.Sprintf("resumed in %.1f s", time.Since(l.l.granted).Seconds())})
}

// footprintOf is the VRAM a park would need to hold in RAM: this run's
// measured peak, or the engine's learned footprint when this run has not
// peaked yet. b.mu must be held.
func (b *Broker) footprintOf(l *lease) uint64 {
	if l.peak > 0 {
		return l.peak
	}
	if f := b.footprints[l.key()]; f != nil {
		return f.Last
	}
	return 0
}

// parkedFootprints is the RAM all parked engines are expected to hold: the
// sum of their footprints. b.mu must be held.
func (b *Broker) parkedFootprints() uint64 {
	var n uint64
	for _, l := range b.leases {
		if l.state == parked {
			n += b.footprintOf(l)
		}
	}
	return n
}

// Park admits a draining lease's engine into RAM parking: park.Admit runs
// on the machine's readings, because only the broker sees every parked
// engine. The returned why line is the runner's refusal, and the drain runs
// as it always has.
func (b *Broker) Park(id string) (bool, string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	l, ok := b.leases[id]
	if !ok || l.state != draining {
		return false, fmt.Sprintf("lease %s is not draining", id)
	}
	if b.snap.APU {
		why := "park refused: APU VRAM/GTT offload accounting is not yet supported; using normal drain"
		b.emit(proto.Event{Kind: proto.EvParkRefused, Lease: id, Engine: l.engine, Detail: why})
		return false, why
	}
	foot := b.footprintOf(l)
	if foot == 0 {
		// Nothing was ever measured: there is nothing to hold in RAM,
		// and nothing worth parking.
		why := "park refused: this run never held card memory"
		b.emit(proto.Event{Kind: proto.EvParkRefused, Lease: id, Engine: l.engine, Detail: why})
		return false, why
	}
	ok, why := park.Admit(park.Budget{
		Available: b.cfg.MemAvailable(),
		Floor:     b.cfg.ParkFloor,
		Parked:    b.parkedFootprints(),
		Max:       b.cfg.ParkMax,
	}, foot)
	if !ok {
		b.emit(proto.Event{Kind: proto.EvParkRefused, Lease: id, Engine: l.engine, Bytes: foot, Detail: why})
		return false, why
	}
	l.state = parking
	return true, ""
}

// parkingTick moves a parking lease to parked once a card read at or
// after the park call shows the residual, and gives up on the park after
// the grace: the runner is told to stop the engine the usual way. Called
// with b.mu held, from Tick.
func (b *Broker) parkingTick(key string, l *lease, now time.Time, read uint64) {
	if l.parkedCall.IsZero() || read <= l.parkedRead {
		return
	}
	if l.used() <= b.cfg.ParkResidualMax && b.driverReleased(l) {
		l.state = parked
		l.parkedAt = now
		l.parkedN = b.order()
		foot := b.footprintOf(l)
		b.log.Info("parked", "lease", key, "engine", l.engine, "footprint", sizeHuman(foot), "residual", sizeHuman(l.used()))
		b.emit(proto.Event{Kind: proto.EvParked, Lease: key, Engine: l.engine, Bytes: foot,
			Detail: fmt.Sprintf("residual %s", sizeHuman(l.used()))})
		return
	}
	if now.Sub(l.parkedCall) > b.cfg.ReleaseGrace {
		// The park did not free the card: stop the engine as any drain.
		l.state = draining
		msg := "park did not free the card"
		select {
		case l.drainCh <- msg:
		default:
		}
		b.log.Warn("park did not free the card", "lease", key, "engine", l.engine, "holding", holding(l))
		b.emit(proto.Event{Kind: proto.EvDraining, Lease: key, Engine: l.engine, Detail: msg})
	}
}

// evictParked asks the eviction table which parked engines to stop this
// tick: past their ttl, for the RAM floor, or under memory pressure. Each
// named lease goes draining and its runner hears OpDrain with the why.
// Called with b.mu held, from Tick.
func (b *Broker) evictParked(now time.Time) {
	var entries []park.Parked
	for _, l := range b.leases {
		if l.state == parked {
			entries = append(entries, park.Parked{Name: l.id, Bytes: b.footprintOf(l), Since: l.parkedAt, TTL: l.parkTTL})
		}
	}
	if len(entries) == 0 {
		return
	}
	avail := b.cfg.MemAvailable()
	names := park.Evict(entries, park.Budget{
		Available: avail,
		Floor:     b.cfg.ParkFloor,
		Parked:    b.parkedFootprints(),
		Max:       b.cfg.ParkMax,
	}, b.cfg.MemPressure(), b.cfg.ParkPressure, now, b.cfg.ParkTTL)
	for _, name := range names {
		l, ok := b.leases[name]
		if !ok || l.state != parked {
			continue
		}
		why := "the memory pressure is high"
		switch {
		case now.Sub(l.parkedAt) > cmp.Or(l.parkTTL, b.cfg.ParkTTL):
			why = fmt.Sprintf("parked %s, past the ttl", now.Sub(l.parkedAt).Round(time.Second))
		case avail < b.cfg.ParkFloor:
			why = "available RAM is under the floor"
		}
		l.state = draining
		msg := "evicted: " + why
		select {
		case l.drainCh <- msg:
		default:
		}
		b.log.Warn("evicting a parked engine", "lease", name, "engine", l.engine, "why", why)
		b.emit(proto.Event{Kind: proto.EvParkEvicted, Lease: name, Engine: l.engine, Bytes: b.footprintOf(l), Detail: why})
	}
}

// Resume puts the engine's parked lease back in the queue at its own
// priority, keeping the runner's connection. The runner hears
// OpGranted{Resume: true} when the grant lands.
func (b *Broker) Resume(engine string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, l := range b.leases {
		if l.state != parked || (l.engine != engine && l.key() != engine) {
			continue
		}
		l.state = waiting
		l.resumed = true
		l.arrived = b.now()
		l.arrivedN = b.order()
		// The waits from before the park are stale; the runner hears
		// only what happens from here.
		for {
			select {
			case <-l.waitCh:
				continue
			default:
			}
			break
		}
		b.queue = append(b.queue, l)
		b.schedule(b.now())
		return nil
	}
	return fmt.Errorf("%s is not parked", engine)
}

// besideLine is the note a grant beside parked engines carries.
func besideLine(parked []*lease, residual uint64) string {
	names := make([]string, 0, len(parked))
	for _, p := range parked {
		names = append(names, p.engine)
	}
	sort.Strings(names)
	return fmt.Sprintf("beside parked %s (%d MiB)", strings.Join(names, ", "), residual/(1<<20))
}

// sortParked orders parked leases least recently parked first.
func sortParked(ls []*lease) []*lease {
	out := append([]*lease(nil), ls...)
	sort.Slice(out, func(i, j int) bool { return out[i].parkedN < out[j].parkedN })
	return out
}

// subtract is as - bs, by identity.
func subtract(as, bs []*lease) []*lease {
	drop := map[*lease]bool{}
	for _, b := range bs {
		drop[b] = true
	}
	var out []*lease
	for _, a := range as {
		if !drop[a] {
			out = append(out, a)
		}
	}
	return out
}
