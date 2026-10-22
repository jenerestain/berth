// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/broker"
	"github.com/T-Crypt/berth/internal/pair"
	"github.com/T-Crypt/berth/internal/procs"
	"github.com/T-Crypt/berth/internal/proto"
	"github.com/T-Crypt/berth/internal/server"
)

func pairDaemon(t *testing.T) (string, *pair.Store) {
	t.Helper()
	dir := t.TempDir()
	sock := filepath.Join(dir, "berth.sock")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	b := broker.New(broker.Config{Poll: 20 * time.Millisecond, Log: log}, emptyCard{}, procs.System{})
	ln, err := server.Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := &pair.Store{Path: filepath.Join(dir, "harbor-devices.json")}
	go server.Serve(ctx, ln, b, server.Extras{Pair: s}, log)
	return sock, s
}

// berth pair mints a code berthd accepts once; --list and --revoke manage
// the device it paired, all over the real socket.
func TestPairCLI(t *testing.T) {
	isolate(t)
	sock, s := pairDaemon(t)

	code, out, errOut := runCLI(t, "pair", "--socket", sock, "--url", "https://box.tail.ts.net/", "--format", "json")
	if code != ExitOK {
		t.Fatalf("pair = %d: %s", code, errOut)
	}
	var got struct {
		Link, Code string
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if got.Link != "https://box.tail.ts.net/pair?code="+got.Code {
		t.Fatalf("link = %q", got.Link)
	}
	tok, err := s.Redeem(got.Code, "iPhone")
	if err != nil {
		t.Fatalf("berthd's code does not redeem: %v", err)
	}

	code, out, _ = runCLI(t, "pair", "--socket", sock, "--list", "--format", "json")
	var devs []proto.PairedDevice
	if code != ExitOK || json.Unmarshal([]byte(out), &devs) != nil || len(devs) != 1 || devs[0].Name != "iPhone" {
		t.Fatalf("list = %d %s", code, out)
	}
	code, out, _ = runCLI(t, "pair", "--socket", sock, "--revoke", devs[0].ID)
	if code != ExitOK || !strings.Contains(out, "unpaired 1 device") || s.Check(tok) {
		t.Fatalf("revoke = %d %q", code, out)
	}

	code, out, _ = runCLI(t, "pair", "--socket", sock, "--url", "https://box.tail.ts.net")
	if code != ExitOK || !strings.Contains(out, "▀") || !strings.Contains(out, "/pair?code=") {
		t.Fatalf("human pair = %d, no QR or link:\n%s", code, out)
	}
	if code, _, _ := runCLI(t, "pair", "--socket", sock, "--url", "ftp://x"); code == ExitOK {
		t.Fatal("a non-http --url was accepted")
	}
}
