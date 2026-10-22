// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"encoding/json"
	"net/http"
	"os"

	"github.com/T-Crypt/berth/internal/proto"
)

// The switchboard's read side: one node as GET /v1/berth/board serves it.
// The gateway is the only process that knows a start in flight, so a node's
// board is built here; the harbor reads one per node. Cards is a list and
// every action names its engine, so more cards (v0.5) and more nodes (v0.6)
// add rows, not a new shape.

type boardNode struct {
	Name      string        `json:"name"`
	Reachable bool          `json:"reachable"` // berthd answered; false = cards and holders unknown
	Cards     []boardCard   `json:"cards"`
	Engines   []boardEngine `json:"engines"`
}

type boardCard struct {
	Index int    `json:"index"`
	Name  string `json:"name"`
	// State is free, held, draining (a holder is giving it back),
	// passthrough (in a VM) or probe-failed.
	State   string    `json:"state"`
	Total   uint64    `json:"total"`
	Used    uint64    `json:"used"`
	Holders []guiHold `json:"holders"`
}

type boardEngine struct {
	guiRow
	Stoppable bool         `json:"stoppable"` // holds the card under an active lease that is not pinned
	Models    []boardModel `json:"models"`
}

type boardModel struct {
	ID     string `json:"id"`
	Loaded bool   `json:"loaded,omitempty"` // the engine's reader reports it loaded
}

func (g *Gateway) board(w http.ResponseWriter, r *http.Request) {
	node := g.nodeStatus()
	rows := g.guiRows(r.Context(), node)
	listed := map[string][]string{}
	for i := range g.Engines {
		e := &g.Engines[i]
		if rows[i].State == "up" {
			listed[e.Name] = g.listed(r.Context(), e.URL)
		}
		if len(listed[e.Name]) == 0 {
			listed[e.Name] = e.Serves
		}
	}
	host, _ := os.Hostname()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(boardOf(host, node, rows, listed))
}

// boardOf assembles the board from berthd's status (nil = not answering),
// the engines' rows in config order and each engine's model list.
func boardOf(host string, node *proto.Status, rows []guiRow, listed map[string][]string) boardNode {
	n := boardNode{Name: host, Reachable: node != nil, Cards: []boardCard{}, Engines: make([]boardEngine, 0, len(rows))}
	loaded := map[string]map[string]bool{}
	if node != nil {
		for _, gp := range node.GPUs {
			if !gp.Scheduled {
				continue
			}
			c := boardCard{Index: gp.Index, Name: gp.Name, State: "free", Total: gp.Total, Used: gp.Used, Holders: []guiHold{}}
			for _, l := range node.Leases {
				c.Holders = append(c.Holders, guiHold{Name: holderName(l), State: l.State, Pinned: l.Pinned})
				if c.State == "free" {
					c.State = "held"
				}
				if l.State != "active" {
					c.State = "draining"
				}
			}
			switch {
			case node.Passthrough != nil:
				c.State = "passthrough"
			case node.ProbeErr != "":
				c.State = "probe-failed"
			}
			n.Cards = append(n.Cards, c)
		}
		for _, e := range node.Engines {
			loaded[e.Name] = map[string]bool{}
			for _, m := range e.Models {
				loaded[e.Name][m.Name] = m.State == "loaded" || m.State == "ready"
			}
		}
	}
	for _, row := range rows {
		be := boardEngine{guiRow: row, Models: []boardModel{}}
		if node != nil {
			for _, l := range node.Leases {
				if (l.Engine == row.Name || l.Group == row.Name) && !l.Pinned && l.State == "active" {
					be.Stoppable = true
				}
			}
		}
		for _, id := range listed[row.Name] {
			be.Models = append(be.Models, boardModel{ID: id, Loaded: loaded[row.Name][id]})
		}
		n.Engines = append(n.Engines, be)
	}
	return n
}
