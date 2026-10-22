// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package pair

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newStore(t *testing.T) (*Store, *time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	s := &Store{Path: filepath.Join(t.TempDir(), "harbor-devices.json"), Now: func() time.Time { return now }}
	return s, &now
}

// A code pairs one device once; the device's token then checks, and the
// file never holds a code or a token in the clear.
func TestPairOnce(t *testing.T) {
	s, _ := newStore(t)
	code, err := s.Code()
	if err != nil {
		t.Fatal(err)
	}
	if len(code) != codeLen || strings.Trim(code, alphabet) != "" {
		t.Fatalf("code %q is not %d characters of the alphabet", code, codeLen)
	}
	tok, err := s.Redeem(code, "iPhone")
	if err != nil {
		t.Fatal(err)
	}
	if !s.Check(tok) {
		t.Fatal("a fresh device token does not check")
	}
	if s.Check("") || s.Check(tok+"x") {
		t.Fatal("a wrong token checks")
	}
	if _, err := s.Redeem(code, "again"); !errors.Is(err, ErrBadCode) {
		t.Fatalf("second redeem = %v, want ErrBadCode", err)
	}
	b, err := os.ReadFile(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), code) || strings.Contains(string(b), tok) {
		t.Fatalf("the file holds a secret in the clear: %s", b)
	}
	if fi, _ := os.Stat(s.Path); fi.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/' {
		t.Fatalf("file mode = %v, want owner only", fi.Mode().Perm())
	}
	// A restarted daemon reads the same devices back.
	if !(&Store{Path: s.Path, Now: s.Now}).Check(tok) {
		t.Fatal("a token does not survive a restart")
	}
}

func TestCodeExpires(t *testing.T) {
	s, now := newStore(t)
	code, _ := s.Code()
	*now = now.Add(CodeTTL + time.Second)
	if _, err := s.Redeem(code, "late"); !errors.Is(err, ErrBadCode) {
		t.Fatalf("expired redeem = %v, want ErrBadCode", err)
	}
}

// Guessing stops after a few wrong codes a minute, even for a right one.
func TestRedeemLimit(t *testing.T) {
	s, now := newStore(t)
	code, _ := s.Code()
	for i := 0; i < maxFails; i++ {
		if _, err := s.Redeem("WRONGCOD", ""); !errors.Is(err, ErrBadCode) {
			t.Fatalf("wrong code %d = %v", i, err)
		}
	}
	if _, err := s.Redeem(code, ""); !errors.Is(err, ErrTooMany) {
		t.Fatalf("redeem after %d failures = %v, want ErrTooMany", maxFails, err)
	}
	*now = now.Add(time.Minute)
	if _, err := s.Redeem(code, ""); err != nil {
		t.Fatalf("redeem a minute later = %v", err)
	}
}

func TestRevoke(t *testing.T) {
	s, _ := newStore(t)
	var toks []string
	for _, name := range []string{"phone", "laptop"} {
		c, _ := s.Code()
		tok, err := s.Redeem(c, name)
		if err != nil {
			t.Fatal(err)
		}
		toks = append(toks, tok)
	}
	devs := s.List()
	if len(devs) != 2 || devs[0].Name != "phone" || devs[1].Name != "laptop" {
		t.Fatalf("devices = %+v", devs)
	}
	if n, _ := s.Revoke(devs[0].ID); n != 1 || s.Check(toks[0]) || !s.Check(toks[1]) {
		t.Fatalf("revoke one: n=%d", n)
	}
	if n, _ := s.Revoke("nope"); n != 0 {
		t.Fatalf("revoke unknown = %d", n)
	}
	if n, _ := s.Revoke(""); n != 1 || s.Check(toks[1]) || len(s.List()) != 0 {
		t.Fatalf("revoke all: n=%d", n)
	}
}
