// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package harbor

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/proto"
	"github.com/T-Crypt/berth/internal/store"
)

func TestBrowserBoundary(t *testing.T) {
	h := &Harbor{Socket: filepath.Join(t.TempDir(), "none.sock"), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "default-src 'self'") {
		t.Fatalf("index: %s csp=%q", resp.Status, resp.Header.Get("Content-Security-Policy"))
	}

	resp, _ = http.Post(srv.URL+"/v1/status", "application/json", strings.NewReader("{}"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST got %s; the harbor must be read-only", resp.Status)
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/status", nil)
	req.Host = "attacker.example"
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign Host got %s", resp.Status)
	}

	resp, _ = http.Get(srv.URL + "/v1/status")
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(b), `"node_unreachable"`) {
		t.Fatalf("node down: %s %s", resp.Status, b)
	}
}

// TestGuardModes proves the two guard postures: a LAN-exposed harbor (the
// operator's explicit opt-in) drops only the Host check, while the default
// loopback harbor keeps it. The method gate, the security headers, and the
// read-only handler set are identical in both modes.
func TestGuardModes(t *testing.T) {
	down := filepath.Join(t.TempDir(), "none.sock")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	lan := httptest.NewServer((&Harbor{Socket: down, Mode: ModeLAN, Log: log}).Handler())
	defer lan.Close()
	loop := httptest.NewServer((&Harbor{Socket: down, Log: log}).Handler())
	defer loop.Close()

	do := func(base, method, path, host string) (int, string, http.Header) {
		req, err := http.NewRequest(method, base+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if host != "" {
			req.Host = host
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		hdr := resp.Header
		return resp.StatusCode, string(b), hdr
	}

	// LAN mode: a foreign Host passes the guard and reaches the (down) node;
	// headers and the method gate are unchanged.
	code, b, _ := do(lan.URL, http.MethodGet, "/v1/status", "phone.lan")
	if code != http.StatusBadGateway || !strings.Contains(b, `"node_unreachable"`) {
		t.Fatalf("LAN foreign Host: %d %s (want 502 from the node, not 403)", code, b)
	}
	code, _, hdr := do(lan.URL, http.MethodGet, "/", "")
	if code != http.StatusOK || !strings.Contains(hdr.Get("Content-Security-Policy"), "default-src 'self'") {
		t.Fatalf("LAN mode /: %d csp=%q", code, hdr.Get("Content-Security-Policy"))
	}
	code, _, _ = do(lan.URL, http.MethodPost, "/v1/status", "")
	if code != http.StatusMethodNotAllowed {
		t.Fatalf("LAN mode POST: %d (must stay read-only)", code)
	}
	code, b, _ = do(lan.URL, http.MethodGet, "/v1/mode", "")
	if code != http.StatusOK || !strings.Contains(b, `"mode":"lan"`) {
		t.Fatalf("LAN mode /v1/mode: %d %s", code, b)
	}

	// Loopback mode (zero-value Mode): the Host check is on, exactly as before.
	code, _, _ = do(loop.URL, http.MethodGet, "/v1/status", "attacker.example")
	if code != http.StatusForbidden {
		t.Fatalf("loopback foreign Host: %d (want 403)", code)
	}
	code, b, _ = do(loop.URL, http.MethodGet, "/v1/mode", "")
	if code != http.StatusOK || !strings.Contains(b, `"mode":"loopback"`) {
		t.Fatalf("loopback /v1/mode: %d %s", code, b)
	}
}

// TestAllowHosts proves a loopback harbor behind a same-machine proxy:
// the listed name passes with or without a port and in any case, every
// other foreign name is still refused, and the mode stays loopback.
func TestAllowHosts(t *testing.T) {
	down := filepath.Join(t.TempDir(), "none.sock")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer((&Harbor{Socket: down, Log: log, AllowHosts: []string{"box.tail1234.ts.net"}}).Handler())
	defer srv.Close()

	for _, tc := range []struct {
		host string
		want int
	}{
		{"box.tail1234.ts.net", http.StatusBadGateway},
		{"box.tail1234.ts.net:9798", http.StatusBadGateway},
		{"BOX.tail1234.ts.net", http.StatusBadGateway},
		{"attacker.example", http.StatusForbidden},
		{"box.tail1234.ts.net.attacker.example", http.StatusForbidden},
	} {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/status", nil)
		req.Host = tc.host
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("Host %q: %d, want %d", tc.host, resp.StatusCode, tc.want)
		}
	}
	resp, _ := http.Get(srv.URL + "/v1/mode")
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), `"mode":"loopback"`) {
		t.Fatalf("allow-host harbor /v1/mode: %s (must stay loopback)", b)
	}
}

// TestListenMode pins the opt-in: a well-formed loopback address stays
// private, any other well-formed host:port is the consent, and a malformed
// address is refused.
func TestListenMode(t *testing.T) {
	for _, tc := range []struct {
		addr, mode string
		wantErr    bool
	}{
		{addr: "127.0.0.1:9798", mode: ModeLoopback},
		{addr: "localhost:9798", mode: ModeLoopback},
		{addr: "[::1]:9798", mode: ModeLoopback},
		{addr: "192.168.1.50:9798", mode: ModeLAN},
		{addr: "0.0.0.0:9798", mode: ModeLAN},
		{addr: "127.0.0.1", wantErr: true}, // no port
		{addr: ":9798", wantErr: true},     // no host
	} {
		got, err := listenMode(tc.addr)
		if tc.wantErr {
			if err == nil {
				t.Errorf("listenMode(%q) = %q, want error", tc.addr, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("listenMode(%q): %v", tc.addr, err)
			continue
		}
		if got != tc.mode {
			t.Errorf("listenMode(%q) = %q, want %q", tc.addr, got, tc.mode)
		}
	}
}

// TestStatusPassthroughReachesThePage: a node that reports the governed card
// in a VM passes the passthrough object and the card's figures through
// /v1/status unshaped, so the page's panels have the state and the last
// reading's age to show.
func TestStatusPassthroughReachesThePage(t *testing.T) {
	n := newFakeNode(t, 0, nil)
	n.st = &proto.Status{
		API: proto.APIVersion,
		GPUs: []proto.GPU{{
			Index: 0, Name: "NVIDIA GeForce RTX 4090", Scheduled: true,
			Total: 24 << 30, Used: 20 << 30, Free: 4 << 30, Baseline: 2 << 30, Age: 300,
		}},
		Passthrough: &proto.Passthrough{State: proto.CardPassthrough, Driver: "vfio-pci"},
	}
	srv := newEventsHarbor(t, n)
	resp, err := http.Get(srv.URL + "/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/v1/status: %s %s", resp.Status, b)
	}
	body := string(b)
	for _, want := range []string{
		`"passthrough":{"state":"passthrough","driver":"vfio-pci"}`,
		`"scheduled":true`,
		`"age_seconds":300`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/v1/status does not carry %s:\n%s", want, body)
		}
	}

	// detaching reads as its own state, a switch under way, not passthrough
	n.st.Passthrough = &proto.Passthrough{State: proto.CardDetaching, Driver: "nvidia"}
	resp, err = http.Get(srv.URL + "/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), `"passthrough":{"state":"detaching","driver":"nvidia"}`) {
		t.Fatalf("detaching: %s", b)
	}
}

// logBuf is a bytes.Buffer safe to poll from the test while Serve logs to it.
type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func (l *logBuf) waitContain(t *testing.T, s string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(l.String(), s) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("log does not contain %q: %s", s, l.String())
}

// TestServeModes: a non-loopback listen address logs the exposure warning and
// records the LAN mode; a loopback address warns nothing; a malformed address
// is refused before anything binds.
func TestServeModes(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "none.sock")

	t.Run("lan warns", func(t *testing.T) {
		buf := &logBuf{}
		h := &Harbor{Socket: sock, Log: slog.New(slog.NewTextHandler(buf, nil))}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- h.Serve(ctx, "0.0.0.0:0") }()
		buf.waitContain(t, "exposed on the LAN")
		cancel()
		if err := <-done; err != nil {
			t.Fatalf("Serve: %v", err)
		}
		if h.Mode != ModeLAN {
			t.Fatalf("Serve set Mode=%q, want %q", h.Mode, ModeLAN)
		}
	})

	t.Run("loopback warns nothing", func(t *testing.T) {
		buf := &logBuf{}
		h := &Harbor{Socket: sock, Log: slog.New(slog.NewTextHandler(buf, nil))}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- h.Serve(ctx, "127.0.0.1:0") }()
		buf.waitContain(t, "harbor up")
		cancel()
		if err := <-done; err != nil {
			t.Fatalf("Serve: %v", err)
		}
		if strings.Contains(buf.String(), "exposed on the LAN") {
			t.Fatalf("loopback serve warned: %q", buf.String())
		}
		if h.Mode != ModeLoopback {
			t.Fatalf("Serve set Mode=%q, want %q", h.Mode, ModeLoopback)
		}
	})

	t.Run("malformed address is refused without binding", func(t *testing.T) {
		h := &Harbor{Socket: sock, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		err := h.Serve(ctx, "127.0.0.1")
		if err == nil || !strings.Contains(err.Error(), "not a host:port address") {
			t.Fatalf("Serve(\"127.0.0.1\") = %v, want the malformed-address error", err)
		}
		if h.Mode != "" {
			t.Fatalf("Serve set Mode=%q for a refused address", h.Mode)
		}
	})
}

// TestRangeEndpoints serves the stored ranges: 6 h and up read the store
// without the node, 1 h stays a live passthrough, and storage reports the
// file and its rows.
func TestRangeEndpoints(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "harbor.db"), 720*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now()
	m0 := now.Truncate(time.Minute).Add(-30 * time.Minute)
	samples := make([]proto.Sample, 0, 6)
	for i := 0; i < 5; i++ {
		s := proto.Sample{T: m0.Add(time.Duration(i)*time.Minute + 10*time.Second),
			GPUs:    []proto.GPUSample{{Index: 0, Used: 1 << 30, Util: 50, Temp: 60, Power: 200}},
			Holders: map[string]uint64{"agent": 1 << 30}, CPU: 30, RAMUsed: 4 << 30,
			Decode: map[string]float64{"agent": 42}, RPM: 2, P95MS: 150}
		samples = append(samples, s)
	}
	samples = append(samples, proto.Sample{T: m0.Add(5*time.Minute + 40*time.Second)}) // opens the next minute
	if err := db.IngestSamples(samples); err != nil {
		t.Fatal(err)
	}
	if _, err := db.IngestRequests([]proto.Request{{ID: "swap/1", Engine: "swap", Model: "A",
		T: now.Add(-time.Hour), Status: 200, Prompt: 100, Cache: 0, Output: 10, DecodeTPS: 50, DurationMS: 100}}); err != nil {
		t.Fatal(err)
	}

	h := &Harbor{Socket: filepath.Join(t.TempDir(), "none.sock"), Store: db,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()
	get := func(path string) (int, string) {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, string(b)
	}

	code, body := get("/v1/history?range=24h")
	if code != 200 || !strings.Contains(body, `"every_seconds":60`) || !strings.Contains(body, `"agent"`) {
		t.Fatalf("24 h: %d %s", code, body)
	}
	code, body = get("/v1/history?range=7d")
	if code != 200 || !strings.Contains(body, `"every_seconds":3600`) {
		t.Fatalf("7 d: %d %s", code, body) // the 1 m rows are not 1 h rows yet; the table is read, empty is fine
	}
	code, body = get("/v1/history?range=1h")
	if code != http.StatusBadGateway || !strings.Contains(body, `"node_unreachable"`) {
		t.Fatalf("1 h must stay the live passthrough: %d %s", code, body)
	}
	code, body = get("/v1/history?range=nope")
	if code != http.StatusBadRequest {
		t.Fatalf("bad range: %d %s", code, body)
	}
	code, body = get("/v1/insights?range=24h")
	if code != 200 || !strings.Contains(body, `"ledger"`) || !strings.Contains(body, `"leaderboard"`) {
		t.Fatalf("insights: %d %s", code, body)
	}
	code, body = get("/v1/insights?range=1h")
	if code != http.StatusBadRequest {
		t.Fatalf("insights range: %d %s", code, body)
	}
	code, body = get("/v1/storage")
	// the store opens with a 720 h retention; on the wire that is whole
	// seconds, never nanoseconds
	if code != 200 || !strings.Contains(body, `"samples_1m":5`) || !strings.Contains(body, `"requests":1`) ||
		!strings.Contains(body, `"retention_seconds":2592000`) || !strings.Contains(body, `"hour_retention_seconds":34560000`) {
		t.Fatalf("storage: %d %s", code, body)
	}
}

// TestSpansEndpoint serves the model timeline over the store: closed spans
// carry their end, an open one comes back as null, and with no store the
// harbor says so instead of reaching for the node.
func TestSpansEndpoint(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "harbor.db"), 720*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().Truncate(time.Second)
	if err := db.IngestSpans([]proto.Sample{
		{T: now.Add(-2 * time.Hour), Models: map[string][]string{"llama-swap": {"A-27B"}, "ollama": {"qwen3:8b"}}},
		{T: now.Add(-1 * time.Hour), Models: map[string][]string{"llama-swap": {"A-27B"}, "ollama": {"qwen3:8b"}}},
		{T: now.Add(-30 * time.Minute), Models: map[string][]string{"llama-swap": {"B-8B"}, "ollama": {}}},
	}); err != nil {
		t.Fatal(err)
	}
	h := &Harbor{Socket: filepath.Join(t.TempDir(), "none.sock"), Store: db,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()
	get := func(path string) (int, string) {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, string(b)
	}
	code, body := get("/v1/spans?range=1h")
	if code != 200 || !strings.Contains(body, `"llama-swap"`) || !strings.Contains(body, `"qwen3:8b"`) ||
		!strings.Contains(body, `"end":null`) || !strings.Contains(body, `"from"`) {
		t.Fatalf("spans: %d %s", code, body)
	}
	// B-8B is the open span at the newest sample; the 30 d view still finds
	// the hour-old A-27B span the 1 h view has pruned out of its range
	code, body = get("/v1/spans?range=30d")
	if code != 200 || !strings.Contains(body, `"B-8B"`) || !strings.Contains(body, `"A-27B"`) {
		t.Fatalf("30 d spans: %d %s", code, body)
	}
	code, body = get("/v1/spans")
	if code != http.StatusBadRequest {
		t.Fatalf("missing range: %d %s", code, body)
	}
	code, body = get("/v1/spans?range=nope")
	if code != http.StatusBadRequest {
		t.Fatalf("bad range: %d %s", code, body)
	}
	// no store: the timeline has no data source and says so
	h2 := &Harbor{Socket: filepath.Join(t.TempDir(), "none.sock"),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	srv2 := httptest.NewServer(h2.Handler())
	defer srv2.Close()
	resp, err := http.Get(srv2.URL + "/v1/spans?range=1h")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(b), `"no_store"`) {
		t.Fatalf("no store: %d %s", resp.StatusCode, b)
	}
}
