// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package harbor

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/broker"
	"github.com/T-Crypt/berth/internal/gpu"
	"github.com/T-Crypt/berth/internal/pair"
	"github.com/T-Crypt/berth/internal/procs"
	"github.com/T-Crypt/berth/internal/server"
)

type idleCard struct{}

func (idleCard) Read(context.Context) (gpu.Snapshot, error) {
	return gpu.Snapshot{Total: 24 << 30, Free: 24 << 30, At: time.Now()}, nil
}

// pairHarbor is a harbor in front of a real berthd that owns a pairing file.
func pairHarbor(t *testing.T) (*httptest.Server, *pair.Store) {
	t.Helper()
	dir := t.TempDir()
	sock := filepath.Join(dir, "berth.sock")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	b := broker.New(broker.Config{Poll: 20 * time.Millisecond, Log: log}, idleCard{}, procs.System{})
	ln, err := server.Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := &pair.Store{Path: filepath.Join(dir, "harbor-devices.json")}
	go server.Serve(ctx, ln, b, server.Extras{Pair: s}, log)
	srv := httptest.NewServer((&Harbor{Socket: sock, Log: log}).Handler())
	t.Cleanup(srv.Close)
	return srv, s
}

var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func postPair(t *testing.T, base, code string, hdr map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, base+"/pair", strings.NewReader(url.Values{"code": {code}, "name": {"iPhone"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func paired(t *testing.T, base string, c *http.Cookie) bool {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, base+"/v1/me", nil)
	if c != nil {
		req.AddCookie(c)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var me struct{ Paired bool }
	_ = json.NewDecoder(resp.Body).Decode(&me)
	return me.Paired
}

// The link's GET only shows the button, so a chat app's link preview does
// not spend the code; the button's POST pairs the browser once.
func TestPairFlow(t *testing.T) {
	srv, s := pairHarbor(t)
	code, err := s.Code()
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(srv.URL + "/pair?code=" + code)
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(page), "Pair this device") ||
		!strings.Contains(resp.Header.Get("Content-Security-Policy"), "form-action 'self'") {
		t.Fatalf("pair page = %s csp=%q", resp.Status, resp.Header.Get("Content-Security-Policy"))
	}

	if r := postPair(t, srv.URL, code, map[string]string{"Sec-Fetch-Site": "cross-site"}); r.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site pair = %s", r.Status)
	}
	r := postPair(t, srv.URL, strings.ToLower(code), map[string]string{"Sec-Fetch-Site": "same-origin", "X-Forwarded-Proto": "https"})
	if r.StatusCode != http.StatusSeeOther {
		t.Fatalf("pair = %s; the GET or the cross-site POST spent the code", r.Status)
	}
	var c *http.Cookie
	for _, x := range r.Cookies() {
		if x.Name == cookieName {
			c = x
		}
	}
	if c == nil || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.MaxAge != cookieAge {
		t.Fatalf("device cookie = %+v", c)
	}
	if len(s.List()) != 1 || s.List()[0].Name != "iPhone" {
		t.Fatalf("devices = %+v", s.List())
	}

	if !paired(t, srv.URL, c) || paired(t, srv.URL, nil) || paired(t, srv.URL, &http.Cookie{Name: cookieName, Value: "forged"}) {
		t.Fatal("/v1/me does not tell the paired browser from the others")
	}
	if r := postPair(t, srv.URL, code, nil); r.StatusCode != http.StatusForbidden {
		t.Fatalf("a spent code paired again: %s", r.Status)
	}
	if _, err := s.Revoke(""); err != nil || paired(t, srv.URL, c) {
		t.Fatal("a revoked device is still paired")
	}

	resp, _ = http.Post(srv.URL+"/v1/status", "application/json", strings.NewReader("{}"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST outside /pair = %s, want 405", resp.Status)
	}
}
