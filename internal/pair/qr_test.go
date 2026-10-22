// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package pair

import (
	"strings"
	"testing"

	"rsc.io/qr"
)

// Both drawings carry exactly the code's modules.
func TestQR(t *testing.T) {
	const link = "https://box.tailnet.ts.net/pair?code=ABCD2345"
	c, err := qr.Encode(link, qr.M)
	if err != nil {
		t.Fatal(err)
	}
	dark := 0
	for y := 0; y < c.Size; y++ {
		for x := 0; x < c.Size; x++ {
			if c.Black(x, y) {
				dark++
			}
		}
	}

	svg, err := QRSVG(link)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(svg, `width="1" height="1"`); got != dark {
		t.Fatalf("svg squares = %d, want %d dark modules", got, dark)
	}

	term, err := QRTerminal(link)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(term, "\n"), "\n")
	side := c.Size + 2*quiet
	if len(lines) != (side+1)/2 {
		t.Fatalf("terminal lines = %d, want %d", len(lines), (side+1)/2)
	}
	for i, l := range lines {
		if n := strings.Count(l, "▀"); n != side {
			t.Fatalf("line %d has %d cells, want %d", i, n, side)
		}
	}
	// Every dark module shows as a black top (30) or a black bottom (40).
	if got := strings.Count(term, "[30;") + strings.Count(term, ";40m"); got != dark {
		t.Fatalf("terminal dark halves = %d, want %d", got, dark)
	}
}
