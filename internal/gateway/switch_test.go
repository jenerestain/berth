// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package gateway

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/broker"
	"github.com/T-Crypt/berth/internal/config"
)

// switchGateway is a gateway with one API-only engine the test binary
// serves, the shape of Strata or a bare llama-server.
func switchGateway(t *testing.T) (*httptest.Server, *broker.Broker) {
	t.Helper()
	sock, b := daemon(t)
	addr := freeAddr(t)
	t.Setenv("BERTH_GW_ENGINE", addr+",strata-model,300ms")
	g := &Gateway{Socket: sock, Ready: 10 * time.Second, Key: testKey, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Engines: []config.Engine{
			{Name: "strata", Type: "llamacpp", URL: "http://" + addr, Start: []string{os.Args[0]}, Serves: []string{"strata-model"}},
			{Name: "blender", Type: "custom", URL: "http://127.0.0.1:1"},
		}}
	srv := httptest.NewServer(g.Handler())
	t.Cleanup(srv.Close)
	return srv, b
}

func until(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A switch starts the engine under a lease and answers before it is up; a
// stop gives the card back through berthd's own drain.
func TestSwitchThenStop(t *testing.T) {
	srv, b := switchGateway(t)

	resp := call(t, "POST", srv.URL+"/v1/berth/switch", `{"engine":"strata"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("switch = %d", resp.StatusCode)
	}
	until(t, "strata to hold the card", func() bool {
		st := b.Status()
		return len(st.Leases) == 1 && st.Leases[0].Engine == "strata" && st.Leases[0].State == "active"
	})

	resp = call(t, "POST", srv.URL+"/v1/berth/stop", `{"engine":"strata"}`)
	var got struct {
		Engine string   `json:"engine"`
		Leases []string `json:"leases"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted || got.Engine != "strata" || len(got.Leases) != 1 {
		t.Fatalf("stop = %d %+v", resp.StatusCode, got)
	}
	until(t, "the card to be free", func() bool { return len(b.Status().Leases) == 0 })
}

func TestSwitchStopRefusals(t *testing.T) {
	srv, b := switchGateway(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	l, err := b.Acquire(ctx, broker.Request{Engine: "blender", Priority: broker.Hold, Pinned: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	l.Attach(os.Getpid())

	for _, c := range []struct {
		path, body string
		want       int
	}{
		{"/v1/berth/switch", `{"engine":"nope"}`, http.StatusNotFound},
		{"/v1/berth/switch", `{"engine":"blender"}`, http.StatusNotFound}, // no start command
		{"/v1/berth/switch", `not json`, http.StatusBadRequest},
		{"/v1/berth/stop", `{"engine":"nope"}`, http.StatusNotFound},
		{"/v1/berth/stop", `{"engine":"blender"}`, http.StatusConflict}, // pinned: never stopped from here
		{"/v1/berth/stop", `{"engine":"strata"}`, http.StatusConflict},  // configured, not running
	} {
		resp := call(t, "POST", srv.URL+c.path, c.body)
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("%s %s = %d, want %d", c.path, c.body, resp.StatusCode, c.want)
		}
	}
	if st := b.Status(); len(st.Leases) != 1 || st.Leases[0].State != "active" {
		t.Fatalf("a refused stop touched the pinned lease: %+v", st.Leases)
	}
}
