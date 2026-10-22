// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package server

// The OpPair* ops: berthd is the one writer of the pairing file, so the
// CLI and the harbor both reach it here. Reaching the socket already means
// being this machine's user. Codes and tokens are never logged.

import (
	"log/slog"

	"github.com/T-Crypt/berth/internal/pair"
	"github.com/T-Crypt/berth/internal/proto"
)

func pairing(conn *proto.Conn, log *slog.Logger, req proto.Msg, s *pair.Store) {
	if s == nil {
		reply(conn, log, proto.Msg{Op: proto.OpRefused, Reason: "device pairing is off on this node"})
		return
	}
	refuse := func(err error) { reply(conn, log, proto.Msg{Op: proto.OpRefused, Reason: err.Error()}) }
	switch req.Op {
	case proto.OpPairCode:
		c, err := s.Code()
		if err != nil {
			refuse(err)
			return
		}
		reply(conn, log, proto.Msg{Op: req.Op, Code: c})
	case proto.OpPairRedeem:
		tok, err := s.Redeem(req.Code, req.DeviceName)
		if err != nil {
			refuse(err)
			return
		}
		log.Info("device paired", "name", req.DeviceName)
		reply(conn, log, proto.Msg{Op: req.Op, Token: tok})
	case proto.OpPairCheck:
		reply(conn, log, proto.Msg{Op: req.Op, Paired: s.Check(req.Token)})
	case proto.OpPairList:
		devs := []proto.PairedDevice{}
		for _, d := range s.List() {
			devs = append(devs, proto.PairedDevice{ID: d.ID, Name: d.Name, Created: d.Created})
		}
		reply(conn, log, proto.Msg{Op: req.Op, Devices: devs})
	case proto.OpPairRevoke:
		n, err := s.Revoke(req.Device)
		if err != nil {
			refuse(err)
			return
		}
		log.Info("devices unpaired", "count", n)
		reply(conn, log, proto.Msg{Op: req.Op, Revoked: n})
	}
}
