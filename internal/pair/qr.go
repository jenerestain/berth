// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package pair

import (
	"fmt"
	"strings"

	"rsc.io/qr"
)

// quiet is the blank border, in modules, a scanner needs around a code.
const quiet = 2

// QRTerminal draws text as a QR code for a terminal: one character per
// module column and two module rows per line (the upper half block, with
// the top module as foreground and the bottom as background). The colors
// are explicit black and white, so it scans on a dark or a light theme.
func QRTerminal(text string) (string, error) {
	c, err := qr.Encode(text, qr.M)
	if err != nil {
		return "", err
	}
	dark := func(x, y int) bool {
		return x >= 0 && y >= 0 && x < c.Size && y < c.Size && c.Black(x, y)
	}
	var b strings.Builder
	for y := -quiet; y < c.Size+quiet; y += 2 {
		for x := -quiet; x < c.Size+quiet; x++ {
			fg, bg := 97, 107
			if dark(x, y) {
				fg = 30
			}
			if dark(x, y+1) {
				bg = 40
			}
			fmt.Fprintf(&b, "\x1b[%d;%dm▀", fg, bg)
		}
		b.WriteString("\x1b[0m\n")
	}
	return b.String(), nil
}

// QRSVG draws text as a QR code in SVG, one square per dark module on a
// white ground with a 4-module quiet zone.
func QRSVG(text string) (string, error) {
	c, err := qr.Encode(text, qr.M)
	if err != nil {
		return "", err
	}
	n := c.Size + 8
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges"><rect width="%d" height="%d" fill="#fff"/>`, n, n, n, n)
	for y := 0; y < c.Size; y++ {
		for x := 0; x < c.Size; x++ {
			if c.Black(x, y) {
				fmt.Fprintf(&b, `<rect x="%d" y="%d" width="1" height="1"/>`, x+4, y+4)
			}
		}
	}
	b.WriteString(`</svg>`)
	return b.String(), nil
}
