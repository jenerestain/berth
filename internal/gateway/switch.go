// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/T-Crypt/berth/internal/config"
	"github.com/T-Crypt/berth/internal/proto"
	"github.com/T-Crypt/berth/internal/server"
)

// The switchboard's actions on the keyed API: switch the card to an engine,
// or stop one. The gui plane's Launch and the harbor's board call the same
// two paths, so every surface switches the card the same way.

// switchTo starts the engine in the background under a lease, the way a
// request for one of its models would; the caller reads the engine's state
// to follow it. A second ask while a start is under way joins it.
func (g *Gateway) switchTo(e *config.Engine) {
	g.mu.Lock()
	if g.wakes == nil {
		g.wakes = map[string]*wake{}
	}
	already := g.wakes[e.Name] != nil && g.wakes[e.Name].starting
	if !already {
		g.wakes[e.Name] = &wake{starting: true}
	}
	g.mu.Unlock()
	if already {
		return
	}
	g.log().Info("switching the card", "engine", e.Name)
	go func() {
		// The start outlives the request that asked for it: a closed tab
		// still leaves the engine coming up, bounded by the gateway's ready.
		ctx, cancel := context.WithTimeout(context.Background(), g.ready())
		defer cancel()
		err := g.ensure(ctx, e)
		if err != nil {
			g.log().Warn("switch failed", "engine", e.Name, "err", err)
		}
		g.mu.Lock()
		g.wakes[e.Name] = &wake{err: err}
		g.mu.Unlock()
	}()
}

// engineReq reads the actions' body, {"engine":"NAME"}.
func engineReq(w http.ResponseWriter, r *http.Request) (string, bool) {
	var req struct {
		Engine string `json:"engine"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil || json.Unmarshal(body, &req) != nil || req.Engine == "" {
		apiError(w, http.StatusBadRequest, `the body must be JSON like {"engine":"NAME"}`)
		return "", false
	}
	return req.Engine, true
}

// apiSwitch is POST /v1/berth/switch: 202 once the start is under way.
func (g *Gateway) apiSwitch(w http.ResponseWriter, r *http.Request) {
	name, ok := engineReq(w, r)
	if !ok {
		return
	}
	e, ok := g.launchable(name)
	if !ok {
		apiError(w, http.StatusNotFound, fmt.Sprintf("no engine %q with a start command in berth.toml", name))
		return
	}
	g.switchTo(e)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(guiStatus{State: "waiting", Message: "Asking berth for the card"})
}

// apiStop is POST /v1/berth/stop: berthd asks the engine's leases to give
// the card back at their request boundary, as `berth stop` does. Unlike the
// terminal, a remote stop never touches a pinned lease (a game, a render,
// berth hold): those end only when their owner ends them.
func (g *Gateway) apiStop(w http.ResponseWriter, r *http.Request) {
	name, ok := engineReq(w, r)
	if !ok {
		return
	}
	if g.engineByName(name) == nil {
		apiError(w, http.StatusNotFound, fmt.Sprintf("no engine %q in berth.toml", name))
		return
	}
	st := g.nodeStatus()
	if st == nil {
		apiError(w, http.StatusServiceUnavailable, "berthd is not answering; nothing was stopped")
		return
	}
	for _, l := range st.Leases {
		if (l.Engine == name || l.Group == name) && l.Pinned {
			apiError(w, http.StatusConflict, name+" is pinned; Berth never stops it")
			return
		}
	}
	conn, raw, err := server.Dial(g.Socket)
	if err != nil {
		apiError(w, http.StatusServiceUnavailable, "berthd is not answering; nothing was stopped")
		return
	}
	defer raw.Close()
	if err := conn.Send(proto.Msg{Op: proto.OpStop, Engine: name}); err != nil {
		apiError(w, http.StatusServiceUnavailable, "berthd: "+err.Error())
		return
	}
	m, err := conn.Recv()
	switch {
	case err != nil:
		apiError(w, http.StatusServiceUnavailable, "no stop reply from berthd: "+err.Error())
	case m.Op == proto.OpRefused:
		apiError(w, http.StatusConflict, m.Reason)
	default:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(struct {
			Engine string   `json:"engine"`
			Leases []string `json:"leases"`
		}{name, m.Leases})
	}
}
