// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package cli

// berth pair: let a phone or another browser act from the harbor. berthd
// mints a one-time code; the link carries it to the harbor's pair page,
// whose button trades it for a device cookie. --list and --revoke manage
// what is paired.

import (
	"net/url"
	"strings"
	"time"

	"github.com/T-Crypt/berth/internal/pair"
	"github.com/T-Crypt/berth/internal/proto"
	"github.com/T-Crypt/berth/internal/server"
)

func runPair(c *Context) int {
	fs := c.FlagSet()
	sock := fs.String("socket", defaultSocket(), "berthd socket ($BERTH_SOCKET)")
	base := fs.String("url", "http://127.0.0.1:9798", "the harbor's address as the device reaches it, e.g. https://box.tailnet.ts.net")
	list := fs.Bool("list", false, "list the paired devices")
	revoke := fs.String("revoke", "", "unpair the device with this ID")
	revokeAll := fs.Bool("revoke-all", false, "unpair every device")
	if code, done := c.Parse(fs); done {
		return code
	}
	if code, done := c.noArgs(); done {
		return code
	}
	req := proto.Msg{Op: proto.OpPairCode}
	switch {
	case *list:
		req = proto.Msg{Op: proto.OpPairList}
	case *revoke != "" || *revokeAll:
		req = proto.Msg{Op: proto.OpPairRevoke, Device: *revoke}
	}
	u, err := url.Parse(strings.TrimRight(*base, "/"))
	if req.Op == proto.OpPairCode && (err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "") {
		return c.fail(Failf("--url %q is not an http or https address", *base))
	}
	conn, raw, err := server.Dial(*sock)
	if err != nil {
		return c.fail(Systemf("berthd unreachable on %s: %v", *sock, err))
	}
	defer raw.Close()
	if err := conn.Send(req); err != nil {
		return c.fail(Systemf("berthd on %s: %v", *sock, err))
	}
	m, err := conn.Recv()
	if err != nil {
		return c.fail(Systemf("no reply from berthd: %v", err))
	}
	if m.Op == proto.OpRefused {
		return c.fail(Failf("%s", m.Reason))
	}
	v := pairView{op: req.Op, devices: m.Devices, revoked: m.Revoked}
	if req.Op == proto.OpPairCode {
		v.link = u.String() + "/pair?code=" + m.Code
		v.code = m.Code
	}
	return c.Write(v, nil)
}

type pairView struct {
	op         string
	link, code string
	devices    []proto.PairedDevice
	revoked    int
}

func (v pairView) Data() any {
	switch v.op {
	case proto.OpPairList:
		return v.devices
	case proto.OpPairRevoke:
		return struct {
			Revoked int `json:"revoked"`
		}{v.revoked}
	}
	return struct {
		Link    string `json:"link"`
		Code    string `json:"code"`
		Expires int    `json:"expires_in_s"`
	}{v.link, v.code, int(pair.CodeTTL / time.Second)}
}

func (v pairView) Human(c *Context, ok bool) error {
	switch v.op {
	case proto.OpPairList:
		if len(v.devices) == 0 {
			c.Printf("no paired devices; berth pair pairs one\n")
		}
		for _, d := range v.devices {
			c.Printf("%s  %-20s  paired %s\n", d.ID, d.Name, d.Created.Local().Format("2006-01-02 15:04"))
		}
		return nil
	case proto.OpPairRevoke:
		c.Printf("unpaired %d device%s\n", v.revoked, plural(v.revoked))
		return nil
	}
	q, err := pair.QRTerminal(v.link)
	if err != nil {
		return err
	}
	c.Printf("%s", q)
	c.Printf("%s\n", c.Style.Bold(v.link))
	c.Bullet("scan it, or open the link on the device, then tap Pair this device")
	c.Bullet("the code %s works once, for %s", v.code, pair.CodeTTL)
	c.Bullet("anyone who opens it first can switch and stop engines: send it only to yourself")
	return nil
}
