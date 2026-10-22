// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

// Package pair is the harbor's device pairing: a short one-time code,
// minted on this machine, trades for a long-lived device token that lets a
// browser act on the node. berthd is the only process that writes the
// file, so the CLI and the harbor reach it through berthd's socket and no
// file lock is needed. The file holds only hashes.
package pair

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"time"

	"github.com/T-Crypt/berth/internal/fsx"
)

const (
	// CodeTTL is how long a code may wait to be used.
	CodeTTL = 15 * time.Minute
	codeLen = 8
	// alphabet has no 0/O, 1/I/L or U, so a code read off a screen types
	// right on a phone.
	alphabet = "23456789ABCDEFGHJKMNPQRSTVWXYZ"
	maxCodes = 5  // live codes kept; a new one ends the oldest
	maxFails = 10 // wrong codes a minute before every redeem is refused
)

var (
	ErrBadCode = errors.New("that pairing code is wrong, used or expired")
	ErrTooMany = errors.New("too many wrong pairing codes; try again in a minute")
)

// Device is one paired browser, as the CLI lists it.
type Device struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Created time.Time `json:"created"`
}

type code struct {
	Hash    string    `json:"hash"`
	Expires time.Time `json:"expires"`
}

type device struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Hash    string    `json:"hash"`
	Created time.Time `json:"created"`
}

type file struct {
	Devices []device `json:"devices"`
	Codes   []code   `json:"codes"`
}

// Store is the pairing file. Now is the clock; nil means time.Now.
type Store struct {
	Path string
	Now  func() time.Time

	mu    sync.Mutex
	fails []time.Time
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func hash(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:])
}

func same(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

func (s *Store) load() (file, error) {
	var f file
	b, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	return f, json.Unmarshal(b, &f)
}

// save writes f with the expired codes dropped.
func (s *Store) save(f file) error {
	live := f.Codes[:0]
	for _, c := range f.Codes {
		if s.now().Before(c.Expires) {
			live = append(live, c)
		}
	}
	f.Codes = live
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return fsx.WriteFile(s.Path, b, 0o600)
}

// Code mints a one-time pairing code that lasts CodeTTL.
func (s *Store) Code() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.load()
	if err != nil {
		return "", err
	}
	buf := make([]byte, 0, codeLen)
	one := make([]byte, 1)
	for len(buf) < codeLen {
		if _, err := rand.Read(one); err != nil {
			return "", err
		}
		// Rejection keeps every character equally likely: 240 is the
		// largest multiple of the alphabet's 30 under 256.
		if int(one[0]) < 256/len(alphabet)*len(alphabet) {
			buf = append(buf, alphabet[int(one[0])%len(alphabet)])
		}
	}
	c := string(buf)
	f.Codes = append(f.Codes, code{Hash: hash(c), Expires: s.now().Add(CodeTTL)})
	if len(f.Codes) > maxCodes {
		f.Codes = f.Codes[len(f.Codes)-maxCodes:]
	}
	return c, s.save(f)
}

// Redeem trades a live code for a new device's token. The code is spent.
func (s *Store) Redeem(c, name string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	recent := s.fails[:0]
	for _, t := range s.fails {
		if now.Sub(t) < time.Minute {
			recent = append(recent, t)
		}
	}
	s.fails = recent
	if len(s.fails) >= maxFails {
		return "", ErrTooMany
	}
	f, err := s.load()
	if err != nil {
		return "", err
	}
	h := hash(c)
	at := -1
	for i, x := range f.Codes {
		if same(x.Hash, h) && now.Before(x.Expires) {
			at = i
		}
	}
	if at < 0 {
		s.fails = append(s.fails, now)
		return "", ErrBadCode
	}
	f.Codes = append(f.Codes[:at], f.Codes[at+1:]...)
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	tok := base64.RawURLEncoding.EncodeToString(raw)
	if name == "" {
		name = "device"
	}
	f.Devices = append(f.Devices, device{ID: hash(tok)[:8], Name: name, Hash: hash(tok), Created: now})
	return tok, s.save(f)
}

// Check reports whether tok belongs to a paired device.
func (s *Store) Check(tok string) bool {
	if tok == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.load()
	if err != nil {
		return false
	}
	h := hash(tok)
	ok := false
	for _, d := range f.Devices {
		if same(d.Hash, h) {
			ok = true
		}
	}
	return ok
}

// List is the paired devices, oldest first.
func (s *Store) List() []Device {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, _ := s.load()
	out := make([]Device, 0, len(f.Devices))
	for _, d := range f.Devices {
		out = append(out, Device{ID: d.ID, Name: d.Name, Created: d.Created})
	}
	return out
}

// Revoke unpairs the device with the ID, or every device when id is "".
// It returns how many it unpaired.
func (s *Store) Revoke(id string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.load()
	if err != nil {
		return 0, err
	}
	kept := f.Devices[:0]
	for _, d := range f.Devices {
		if id != "" && d.ID != id {
			kept = append(kept, d)
		}
	}
	n := len(f.Devices) - len(kept)
	f.Devices = kept
	if n == 0 {
		return 0, nil
	}
	return n, s.save(f)
}
