// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/T-Crypt/berth/internal/alerts"
	"github.com/T-Crypt/berth/internal/broker"
	"github.com/T-Crypt/berth/internal/claims"
	"github.com/T-Crypt/berth/internal/config"
	"github.com/T-Crypt/berth/internal/engines"
	"github.com/T-Crypt/berth/internal/gpu"
	"github.com/T-Crypt/berth/internal/history"
	"github.com/T-Crypt/berth/internal/host"
	"github.com/T-Crypt/berth/internal/pair"
	"github.com/T-Crypt/berth/internal/procs"
	"github.com/T-Crypt/berth/internal/proto"
	"github.com/T-Crypt/berth/internal/runs"
	"github.com/T-Crypt/berth/internal/server"
	"github.com/T-Crypt/berth/internal/suite"
)

func runDaemon(c *Context) int {
	fs := c.FlagSet()
	sock := fs.String("socket", defaultSocket(), "Unix socket to listen on ($BERTH_SOCKET)")
	index := fs.Int("gpu", 0, "GPU index")
	backend := fs.String("gpu-backend", "auto", "GPU probe: auto, nvidia or amd (AMD --gpu is the DRM cardN number)")
	poll := fs.Duration("poll", 2*time.Second, "how often the card is read")
	reserve := fs.Uint64("reserve-mib", 0, "memory never handed to sized leases")
	headroomMib := fs.Uint64("headroom-mib", 2048, "RAM kept for the machine: cold-start/park headroom and APU admission floor (0 retains the APU default)")
	wait := fs.Duration("wait", 120*time.Second, "default time a request waits before it is refused; negative = no deadline")
	grace := fs.Duration("release-grace", 30*time.Second, "how long a released lease's processes may linger before they count as squatters")
	reattachWin := fs.Duration("reattach-window", 15*time.Second, "after a restart, how long new grants wait for engines that ran under the last daemon to re-attach")
	allow := fs.String("allow", "", "comma-separated process-name substrings that are never squatters (e.g. Xorg,Hyprland)")
	notify := fs.Bool("notify", false, "desktop notification (notify-send) when a squatter appears")
	webhook := fs.String("webhook", "", "POST squatter events here as JSON")
	cfgPath := fs.String("config", config.DefaultPath(), "berth.toml: engines to read, /metrics listener ($BERTH_CONFIG); missing = none")
	countersFile := fs.String("counters", defaultStateFile("counters.json"), "keeps the collisions board across restarts; \"\" = start at zero every time")
	devicesFile := fs.String("devices", defaultStateFile("harbor-devices.json"), "devices paired to act from the harbor (berth pair); \"\" = pairing off")
	usageFile := fs.String("usage", defaultStateFile("usage.json"), "card-hours per engine, the default juggle ranking; \"\" = memory only")
	footprintFile := fs.String("footprint", defaultStateFile("footprint.json"), "VRAM measured per engine across restarts, the estimate behind EST. VRAM; \"\" = memory only")
	if code, done := c.Parse(fs); done {
		return code
	}
	if code, done := c.noArgs(); done {
		return code
	}
	if err := gpu.ValidateSelection(*backend, *index); err != nil {
		return c.fail(Failf("%v", err))
	}

	log := c.Log()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, warns, err := suite.Merged(*cfgPath)
	if err != nil {
		return c.fail(Failf("config invalid: %s", *cfgPath).Detail("%v", err))
	}
	for _, w := range warns {
		log.Warn(w.Key + ": " + w.Message)
	}
	// A suite install reads the node's model roots through an
	// extra_model_paths.yaml Berth writes; rewriting it at startup is what a
	// root change in berth.toml applies on the next start.
	if st, err := suite.Load(suite.SuitePath(*cfgPath)); err != nil {
		log.Error("suite state", "path", suite.SuitePath(*cfgPath), "err", err)
	} else {
		for _, in := range st.Installs {
			if fi, err := os.Stat(in.Path); err != nil || !fi.IsDir() {
				log.Info("suite install has no directory, left as is", "install", in.Name, "path", in.Path)
				continue
			}
			if err := suite.WriteModelPaths(in, suiteModelRoots(cfg)); err != nil {
				log.Error("model paths", "install", in.Name, "err", err)
			}
		}
	}
	log.Debug("config", "path", *cfgPath, "engines", len(cfg.Engine), "discover", *cfg.Engines.Discover,
		"metrics", cfg.Metrics.Listen, "juggle_window", cfg.Priority.JuggleWindow.String())

	probe, err := gpu.Open(gpu.Options{Backend: *backend, Index: *index, Interval: *poll, Log: log})
	if err != nil {
		return c.fail(Systemf("gpu backend: %v", err))
	}
	rep := server.Reporter{Notify: *notify, Webhook: *webhook, Log: log}
	// The engine readers exist before the broker: a juggle asks them whether
	// the holder is busy. They start reading below, once the card is known.
	var watcher *engines.Watcher
	if len(cfg.Engine) > 0 || *cfg.Engines.Discover {
		watcher = &engines.Watcher{Engines: cfg.Engine, Every: cfg.Engines.Every.Duration,
			Discover: *cfg.Engines.Discover, Hosts: cfg.Engines.Hosts, Ports: cfg.Engines.Ports}
	}
	// The broker's squat-free path (ask a squatting ComfyUI to unload through
	// its own /free and hold the waiter) fires only for engines this list
	// names. Without it every whole-card request is granted over the squat
	// and dies on it.
	comfy := comfyEngines(cfg)
	b := broker.New(broker.Config{
		Poll:           *poll,
		Reserve:        *reserve * gpu.MiB,
		APUHeadroom:    *headroomMib * gpu.MiB,
		ReleaseGrace:   *grace,
		DefaultWait:    *wait,
		Allow:          splitList(*allow),
		RunsDir:        runs.Dir(*sock),
		CountersFile:   *countersFile,
		UsageFile:      *usageFile,
		FootprintFile:  *footprintFile,
		Order:          cfg.Priority.Order,
		JuggleWindow:   cfg.Priority.JuggleWindow.Duration,
		Busy:           watcher.Busy,
		Comfy:          comfy,
		ReattachWindow: *reattachWin,
		// The park budget, from [park]: the floor adds the headroom floor
		// the gateway's guard uses, so a parked copy and a cold start
		// share one picture of the machine's room.
		ParkFloor:       parkFloorBytes(cfg, *headroomMib),
		ParkMax:         parkMaxBytes(cfg),
		ParkTTL:         cfg.ParkTTL(),
		ParkResidualMax: cfg.ParkResidualMax(),
		ParkPressure:    cfg.ParkPressure(),
		Version:         c.Version,
		Reporter:        rep,
		Log:             log,
	}, probe, procs.System{})

	// The alert table is the node's own thresholds: the built-in danger
	// keys plus what the operator enabled. One level crossing is one
	// event and one notification through the same channels as squatters.
	ev := alerts.New(alerts.FromConfig(cfg.Alerts), *cfg.Alerts.ClearPolls,
		func(key alerts.Key, lv alerts.Level, raised bool, msg string) {
			if raised {
				b.Note(proto.Event{Kind: proto.EvAlertRaised, Detail: lv.Name() + ": " + msg})
				rep.Alert(lv.Name(), string(key), msg)
			} else {
				b.Note(proto.Event{Kind: proto.EvAlertCleared, Detail: msg})
			}
		})

	// Inventory every card with its real limits before taking any lease, and
	// refuse to schedule a card that does not exist.
	snap, err := probe.Read(ctx)
	if err != nil {
		return c.fail(Systemf("gpu inventory: %v", err))
	}
	for _, d := range snap.Devices {
		log.Info("gpu found", "index", d.Index, "name", d.Name, "pci", d.BusID, "total_mib", d.Total/gpu.MiB, "scheduled", d.Index == *index)
	}
	if len(snap.Unobserved) > 0 {
		log.Warn("some same-user GPU process data is inaccessible; unknown allocations remain in the driver baseline", "pids", snap.Unobserved)
	}
	// Seed the broker before accepting clients, not in a racing goroutine:
	// guarded admission must never see an uninitialised lease-only snapshot.
	b.Tick(ctx)
	if st := b.Status(); st.ProbeErr != "" {
		return c.fail(Systemf("gpu initial observation: %s", st.ProbeErr))
	}

	ln, err := server.Listen(*sock)
	if err != nil {
		return c.fail(Systemf("listen on %s: %v", *sock, err))
	}
	defer os.Remove(*sock)
	log.Info("berthd up", "version", c.Version, "socket", *sock, "gpu", *index)
	if stream, ok := probe.(interface{ Run(context.Context) }); ok {
		go stream.Run(ctx)
	}
	go b.Run(ctx)
	// Claims: games and apps Berth does not run hold the card or have their
	// memory named, from the process table. Validate already compiled them.
	if rules, err := cfg.ClaimRules(); err == nil && len(rules) > 0 {
		cw := &claims.Watcher{Rules: rules, Every: cfg.Claims.Every.Duration, B: b, Log: log}
		go cw.Run(ctx)
		hold, track := 0, 0
		for _, r := range rules {
			if r.Action == claims.ActionHold {
				hold++
			} else {
				track++
			}
		}
		log.Info("claims armed", "hold", hold, "track", track, "builtin", *cfg.Claims.Builtin)
	}
	if watcher != nil {
		go watcher.Run(ctx)
		for _, e := range cfg.Engine {
			log.Info("reading engine", "name", e.Name, "type", e.Type, "url", e.URL)
		}
		if *cfg.Engines.Discover {
			log.Info("discovering engines", "every", engines.DiscoveryEvery, "hosts", cfg.Engines.Hosts, "extra_ports", cfg.Engines.Ports)
		}
	}
	logComfyArmed(log, comfy)
	if cfg.Metrics.Listen != "" {
		go func() {
			if err := server.ServeMetrics(ctx, cfg.Metrics.Listen, b, watcher.Snapshot, log); err != nil {
				log.Error("metrics", "err", err)
			}
		}()
	}
	// The sampler feeds the graphs, caches host stats for status, so a
	// status read never resets the CPU interval, and runs the alert
	// evaluator on the same sample it records: one clock for the levels,
	// the events and the notifications.
	hr := &host.Reader{}
	rec := &history.Recorder{Every: 5 * time.Second}
	var lastHost atomic.Pointer[proto.Host]
	var lastAlerts atomic.Pointer[proto.Alerts]
	lastAlerts.Store(ev.Snapshot())
	go samplerLoop(ctx, rec, hr, b, watcher, ev, &lastHost, &lastAlerts)
	x := server.Extras{Engines: watcher.Snapshot, Host: lastHost.Load, History: rec.Get,
		Insights: func() *proto.Insights { return watcher.Insights(time.Now()) },
		Requests: watcher.Requests, Alerts: lastAlerts.Load}
	if *devicesFile != "" {
		x.Pair = &pair.Store{Path: *devicesFile}
	}
	if err := server.Serve(ctx, ln, b, x, log); err != nil {
		return c.fail(Systemf("serve: %v", err))
	}
	return ExitOK
}

// samplerLoop builds one status per tick, records it in the in-memory
// history and runs the alert evaluator on that same sample, so the
// levels, the events and the notifications all come from one clock. The
// first tick fires immediately, then every rec.Every until ctx is done.
func samplerLoop(ctx context.Context, rec *history.Recorder, hr *host.Reader, b *broker.Broker,
	watcher *engines.Watcher, ev *alerts.Evaluator,
	hostOut *atomic.Pointer[proto.Host], alertsOut *atomic.Pointer[proto.Alerts]) {
	t := time.NewTicker(rec.Every)
	defer t.Stop()
	for {
		hs := proto.Host(hr.Read())
		hostOut.Store(&hs)
		st := b.Status()
		st.Engines, st.Host, st.Insights = watcher.Snapshot(), &hs, watcher.Insights(time.Now())
		rec.Add(history.Sample(st))
		ev.Tick(alerts.FromStatus(st))
		alertsOut.Store(ev.Snapshot())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// comfyEngines names the configured ComfyUI engines the broker may ask to
// unload their models when a waiter needs the card they squat.
func comfyEngines(cfg config.Config) []broker.ComfyEngine {
	var out []broker.ComfyEngine
	for _, e := range cfg.Engine {
		if e.Type == "comfyui" {
			out = append(out, broker.ComfyEngine{Name: e.Name, URL: e.URL})
		}
	}
	return out
}

// logComfyArmed reports the squat-free arming at startup. The empty case is
// logged too: an engine found only by discovery is never armed (comfyEngines
// reads cfg.Engine), and that has to be visible, because a silently empty list
// turns every ComfyUI squat into a contested grant.
func logComfyArmed(log *slog.Logger, comfy []broker.ComfyEngine) {
	if len(comfy) == 0 {
		log.Info("comfy squat-free not armed: no [[engine]] with type \"comfyui\"")
		return
	}
	for _, e := range comfy {
		log.Info("comfy squat-free armed", "engine", e.Name, "url", e.URL)
	}
}

// splitList splits a comma-separated flag value, dropping the empties.
func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parkFloorBytes is the RAM a park must leave the machine: the [park]
// floor_gib plus the headroom floor, in bytes.
func parkFloorBytes(cfg config.Config, headroomMib uint64) uint64 {
	floor := cfg.ParkFloorGiB() + float64(headroomMib)/1024
	return uint64(floor * float64(gpu.GiB))
}

// parkMaxBytes is the [park] max_gib cap in bytes; 0 = no cap.
func parkMaxBytes(cfg config.Config) uint64 {
	if cfg.Park.MaxGiB <= 0 {
		return 0
	}
	return uint64(cfg.Park.MaxGiB * float64(gpu.GiB))
}
