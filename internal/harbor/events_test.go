// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package harbor

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/proto"
)

// fakeNode is a unix-socket server that speaks the node's NDJSON protocol
// well enough for the harbor's events endpoint: OpStatus gets a canned
// Status, OpEvents gets one Msg per backlog event (Seq > since) and, with
// Watch, the connection then holds until the test stops the node or the
// client goes away. It never emits new events.
type fakeNode struct {
	sock      string
	lastEvent uint64
	evts      []proto.Event
	st        *proto.Status // canned OpStatus reply when set; otherwise an empty status

	ln      net.Listener
	mu      sync.Mutex
	watchCh chan struct{} // closed by stop; unblocks every held watch
	closed  bool
}

func newFakeNode(t *testing.T, lastEvent uint64, evts []proto.Event) *fakeNode {
	t.Helper()
	f := &fakeNode{lastEvent: lastEvent, evts: evts, watchCh: make(chan struct{})}
	ln, err := net.Listen("unix", filepath.Join(t.TempDir(), "node.sock"))
	if err != nil {
		t.Fatal(err)
	}
	f.ln = ln
	f.sock = ln.Addr().String()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serveConn(c)
		}
	}()
	t.Cleanup(f.stop)
	return f
}

func (f *fakeNode) path() string { return f.sock }

func (f *fakeNode) stop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	f.closed = true
	close(f.watchCh)
	f.ln.Close()
}

func (f *fakeNode) serveConn(c net.Conn) {
	defer c.Close()
	conn := proto.NewConn(c)
	m, err := conn.Recv()
	if err != nil {
		return
	}
	switch m.Op {
	case proto.OpStatus:
		st := f.st
		if st == nil {
			st = &proto.Status{API: proto.APIVersion, LastEvent: f.lastEvent}
		}
		_ = conn.Send(proto.Msg{Op: proto.OpStatus, Status: st})
	case proto.OpEvents:
		for i := range f.evts {
			if f.evts[i].Seq > m.Since {
				if conn.Send(proto.Msg{Op: proto.OpEvent, Event: &f.evts[i]}) != nil {
					return
				}
			}
		}
		if !m.Watch {
			return
		}
		// A read returning means the client hung up; stop means the test is
		// over. The fake node has no live events.
		done := make(chan struct{})
		go func() {
			var b [1]byte
			_, _ = c.Read(b[:])
			close(done)
		}()
		select {
		case <-f.watchCh:
		case <-done:
		}
	default:
		_ = conn.Send(proto.Msg{Op: proto.OpRefused, Reason: "unknown op " + m.Op})
	}
}

// testEvents is the generation's backlog: seq 1..5, gap-free.
func testEvents(t *testing.T) []proto.Event {
	t.Helper()
	now := time.Now()
	evts := make([]proto.Event, 0, 5)
	for i := uint64(1); i <= 5; i++ {
		evts = append(evts, proto.Event{Seq: i, At: now, Kind: proto.EvGranted, Lease: "l", Engine: "agent"})
	}
	return evts
}

func newEventsHarbor(t *testing.T, n *fakeNode) *httptest.Server {
	t.Helper()
	h := &Harbor{Socket: n.path(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	srv := httptest.NewServer(h.Handler())
	t.Cleanup(srv.Close)
	return srv
}

// getEventsSSE opens the events stream and returns what the server has
// flushed until stopMarker arrives (or the connection ends). The request's
// context is cancelled as soon as the marker is seen, so the handler's watch
// loop ends without waiting for the timeout.
func getEventsSSE(t *testing.T, base, lastEventID, stopMarker string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+"/v1/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /v1/events: %s %s", resp.Status, b)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type %q", ct)
	}
	var got strings.Builder
	buf := make([]byte, 8192)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			got.Write(buf[:n])
		}
		if strings.Contains(got.String(), stopMarker) {
			cancel() // the expected frames are in; end the stream now
			break
		}
		if rerr != nil {
			break
		}
	}
	return got.String()
}

// TestEventsResume: a reconnecting browser sends Last-Event-ID and gets the
// backlog after that ID, with no rebase frame (its ID is still inside the
// node's current generation).
func TestEventsResume(t *testing.T) {
	n := newFakeNode(t, 5, testEvents(t))
	srv := newEventsHarbor(t, n)
	body := getEventsSSE(t, srv.URL, "2", "id: 5")
	if !strings.HasPrefix(body, "id: 3") {
		t.Fatalf("first frame after since=2 must be id: 3:\n%s", body)
	}
	if strings.Contains(body, "event: rebase") {
		t.Fatalf("no rebase inside the same generation:\n%s", body)
	}
	if strings.Contains(body, "id: 1") || strings.Contains(body, "id: 2") {
		t.Fatalf("already-seen events must not be replayed:\n%s", body)
	}
}

// TestEventsGenerationChange: the node restarted, its Seq began again at 1,
// and the browser's stale Last-Event-ID sits above the node's LastEvent. The
// harbor rebases: a rebase frame carrying the node's last event, then the
// full ring replay.
func TestEventsGenerationChange(t *testing.T) {
	n := newFakeNode(t, 5, testEvents(t))
	srv := newEventsHarbor(t, n)
	body := getEventsSSE(t, srv.URL, "90", "id: 5")
	if !strings.HasPrefix(body, "event: rebase") {
		t.Fatalf("stale generation must open with a rebase frame:\n%s", body)
	}
	if !strings.Contains(body, `{"last_event":5}`) {
		t.Fatalf("rebase frame must carry the node's last event:\n%s", body)
	}
	if !strings.Contains(body, "id: 1") {
		t.Fatalf("rebase must replay the new ring from the beginning:\n%s", body)
	}
}

// TestEventsFreshClient: a page that never saw an event sends no
// Last-Event-ID; it gets the full stream and no rebase frame.
func TestEventsFreshClient(t *testing.T) {
	n := newFakeNode(t, 5, testEvents(t))
	srv := newEventsHarbor(t, n)
	body := getEventsSSE(t, srv.URL, "", "id: 5")
	if strings.Contains(body, "event: rebase") {
		t.Fatalf("a fresh client must not rebase:\n%s", body)
	}
	if !strings.HasPrefix(body, "id: 1") {
		t.Fatalf("a fresh client gets the full stream:\n%s", body)
	}
}
