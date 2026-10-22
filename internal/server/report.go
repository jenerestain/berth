// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"time"

	"github.com/T-Crypt/berth/internal/gpu"
)

// Reporter tells a person about squatters and alert-level crossings:
// a desktop notification, a webhook, or both. It only ever reports;
// nothing is killed.
type Reporter struct {
	Notify  bool   // notify-send, when it is installed
	Webhook string // POSTed JSON {event, pid, name, mib, host}
	Log     *slog.Logger
}

func (r Reporter) Squatter(appeared bool, p gpu.Proc) {
	event := "squatter.gone"
	if appeared {
		event = "squatter.appeared"
	}
	go r.send(event, p)
}

func (r Reporter) send(event string, p gpu.Proc) {
	if r.Notify && event == "squatter.appeared" {
		if bin, err := exec.LookPath("notify-send"); err == nil {
			body := fmt.Sprintf("%s (pid %d) holds %d MiB of VRAM without a lease", p.Name, p.PID, p.Used/gpu.MiB)
			if p.GTT > 0 {
				body += fmt.Sprintf(" and %d MiB of shared GTT", p.GTT/gpu.MiB)
			}
			_ = exec.Command(bin, "-a", "berth", "-u", "normal", "GPU squatter", body).Run()
		}
	}
	host, _ := os.Hostname()
	data := map[string]any{
		"event": event, "pid": p.PID, "name": p.Name, "mib": p.Used / gpu.MiB, "host": host,
	}
	if p.GTT > 0 {
		data["gtt_mib"] = p.GTT / gpu.MiB
	}
	payload, _ := json.Marshal(data)
	r.postWebhook(payload)
}

// Alert tells a person about a threshold or danger level crossing: the
// same two channels as squatters, nothing more. Report-only. The work
// runs in a goroutine, as Squatter does: a dead webhook or a hung
// notify-send must not stall the sampler's 5 s cycle.
func (r Reporter) Alert(level, key, message string) {
	go r.sendAlert(level, key, message)
}

func (r Reporter) sendAlert(level, key, message string) {
	if r.Notify {
		if bin, err := exec.LookPath("notify-send"); err == nil {
			urg := "normal"
			if level == "crit" {
				urg = "critical"
			}
			_ = exec.Command(bin, "-a", "berth", "-u", urg, "GPU alert: "+key, message).Run()
		}
	}
	host, _ := os.Hostname()
	payload, _ := json.Marshal(map[string]any{
		"event": "alert." + level, "key": key, "level": level, "message": message, "host": host,
	})
	r.postWebhook(payload)
}

// postWebhook POSTs payload to the configured webhook URL, if any, with
// a 10 s timeout.
func (r Reporter) postWebhook(payload []byte) {
	if r.Webhook == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.Webhook, bytes.NewReader(payload))
	if err != nil {
		r.Log.Warn("webhook", "err", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.Log.Warn("webhook", "err", err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		r.Log.Warn("webhook", "status", resp.Status)
	}
}
