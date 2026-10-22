// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package engines

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/config"
)

// fake serves a fixed body per path; any other path is a 404.
func fake(t *testing.T, routes map[string]string) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := routes[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if body == "204" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if body == "401" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	return s
}

// What each engine serves, as its own documentation and Speculum's
// adapters describe it, and what the card must show from it.
var engineCases = []struct {
	typ        string
	routes     map[string]string
	models     []string
	window     int
	processing float64
	waiting    float64
	output     float64
	reported   bool
}{
	{typ: "vllm", routes: map[string]string{
		"/v1/models": `{"data":[{"id":"Qwen/Qwen3-8B","max_model_len":32768}]}`,
		"/metrics":   "vllm:prompt_tokens_total 400\nvllm:generation_tokens_total 900\nvllm:num_requests_running 2\nvllm:num_requests_waiting 3\n",
		"/version":   `{"version":"0.11.0"}`,
	}, models: []string{"Qwen/Qwen3-8B"}, window: 32768, processing: 2, waiting: 3, output: 900, reported: true},
	// vLLM with metrics disabled is still up.
	{typ: "vllm", routes: map[string]string{
		"/v1/models": `{"data":[{"id":"m","max_model_len":8192}]}`,
	}, models: []string{"m"}, window: 8192},
	{typ: "sglang", routes: map[string]string{
		"/v1/models":      `{"data":[{"id":"glm","context_length":65536}]}`,
		"/metrics":        "sglang:generation_tokens_total 77\nsglang:num_requests_waiting 1\n",
		"/get_model_info": `{"model_path":"glm"}`,
	}, models: []string{"glm"}, window: 65536, waiting: 1, output: 77, reported: true},
	{typ: "localai", routes: map[string]string{
		"/v1/models": `{"data":[{"id":"phi"}]}`,
		"/metrics":   "localai_completion_tokens_total 12\n",
		"/healthz":   "204",
	}, models: []string{"phi"}, output: 12, reported: true},
	{typ: "openai", routes: map[string]string{
		"/v1/models": `{"data":[{"id":"a"},{"id":"b","context_length":4096}]}`,
	}, models: []string{"a", "b"}, window: 4096},
	{typ: "koboldcpp", routes: map[string]string{
		"/api/v1/model":             `{"result":"koboldcpp/Mistral-7B"}`,
		"/api/v1/config/max_length": `{"value":16384}`,
	}, models: []string{"koboldcpp/Mistral-7B"}, window: 16384},
	{typ: "tabbyapi", routes: map[string]string{
		"/tabby/config": `{"context_length":32000}`,
		"/v1/models":    `{"data":[{"id":"exl3-model"}]}`,
	}, models: []string{"exl3-model"}, window: 32000},
	{typ: "lmstudio", routes: map[string]string{
		"/api/v0/models": `{"data":[{"id":"qwen","state":"loaded","max_context_length":131072,"loaded_context_length":8192},{"id":"other","state":"not-loaded","max_context_length":4096}]}`,
	}, models: []string{"qwen"}, window: 8192},
	{typ: "unsloth", routes: map[string]string{
		"/api/health": `{"status":"healthy","service":"Unsloth UI Backend"}`,
		"/v1/models":  `{"data":[{"id":"unsloth/gemma"}]}`,
	}, models: []string{"unsloth/gemma"}},
	{typ: "freetoken", routes: map[string]string{
		"/v1/stats": `{"stats":{"tokens_generated":500,"prompt_tokens":40,"queue":2,"generating":true}}`,
		"/health":   `{"ok":true}`,
	}, processing: 1, waiting: 2, output: 500, reported: true},
	{typ: "comfyui", routes: map[string]string{
		"/queue":        `{"queue_running":[[1,"a"]],"queue_pending":[[2,"b"],[3,"c"]]}`,
		"/system_stats": `{"system":{"comfyui_version":"0.3.60"},"devices":[]}`,
	}, models: []string{"ComfyUI 0.3.60"}, processing: 1, waiting: 2},
}

func TestEveryEngineReader(t *testing.T) {
	for i, c := range engineCases {
		t.Run(c.typ+"/"+strconv.Itoa(i), func(t *testing.T) {
			s := fake(t, c.routes)
			w := &Watcher{Engines: []config.Engine{{Name: c.typ, Type: c.typ, URL: s.URL}}}
			w.Poll(context.Background())
			got := w.Snapshot()
			if len(got) != 1 || !got[0].Up {
				t.Fatalf("not up: %+v", got)
			}
			e := got[0]
			var names []string
			for _, m := range e.Models {
				names = append(names, m.Name)
			}
			if strings.Join(names, ",") != strings.Join(c.models, ",") {
				t.Fatalf("models = %v, want %v", names, c.models)
			}
			if e.Window != c.window || e.Metrics.Processing != c.processing || e.Metrics.Waiting != c.waiting || e.Metrics.OutputTokens != c.output {
				t.Fatalf("window %d processing %g waiting %g output %g; want %d %g %g %g",
					e.Window, e.Metrics.Processing, e.Metrics.Waiting, e.Metrics.OutputTokens, c.window, c.processing, c.waiting, c.output)
			}
			if e.MetricsReported != c.reported {
				t.Fatalf("metrics_reported = %v, want %v", e.MetricsReported, c.reported)
			}
		})
	}
}

// An engine whose only liveness read fails is down, never up.
func TestEngineReadersDownWhenUnreachable(t *testing.T) {
	for _, typ := range []string{"vllm", "sglang", "localai", "openai", "koboldcpp", "tabbyapi", "lmstudio", "unsloth", "freetoken", "comfyui"} {
		s := fake(t, map[string]string{})
		w := &Watcher{Engines: []config.Engine{{Name: typ, Type: typ, URL: s.URL}}}
		w.Poll(context.Background())
		if got := w.Snapshot(); got[0].Up {
			t.Fatalf("%s up on a server that serves nothing", typ)
		}
	}
}

// The bearer token comes from the named environment variable.
func TestAPIKeyFromEnv(t *testing.T) {
	t.Setenv("BERTH_TEST_KEY", "s3cret")
	var auth string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer s.Close()
	w := &Watcher{Engines: []config.Engine{{Name: "x", Type: "openai", URL: s.URL, APIKeyEnv: "BERTH_TEST_KEY"}}}
	w.Poll(context.Background())
	if auth != "Bearer s3cret" {
		t.Fatalf("Authorization = %q", auth)
	}
}

// Unsloth without a key is up and says a key is needed.
func TestUnslothWithoutKey(t *testing.T) {
	s := fake(t, map[string]string{"/api/health": `{"status":"healthy","service":"Unsloth UI Backend"}`, "/v1/models": "401"})
	w := &Watcher{Engines: []config.Engine{{Name: "u", Type: "unsloth", URL: s.URL}}}
	w.Poll(context.Background())
	e := w.Snapshot()[0]
	if !e.Up || !strings.Contains(e.Error, "API key needed") {
		t.Fatalf("up %v error %q", e.Up, e.Error)
	}
}

// A custom engine reads every path it names; health alone decides up.
func TestCustomEngineAllPaths(t *testing.T) {
	s := fake(t, map[string]string{
		"/ping":   "ok",
		"/stats":  "my_out_total 42\nmy_wait 3\n",
		"/models": `{"data":[{"id":"mine","context_length":2048}]}`,
		"/kv":     `[{"id":0,"n_ctx":2048,"n_prompt_tokens":100,"is_processing":true}]`,
	})
	w := &Watcher{Engines: []config.Engine{{Name: "c", Type: "custom", URL: s.URL, Health: "/ping", Metrics: "/stats",
		Models: "/models", Slots: "/kv", Map: map[string]string{"output_tokens": "my_out_total", "waiting": "my_wait"}}}}
	w.Poll(context.Background())
	e := w.Snapshot()[0]
	if !e.Up || e.Metrics.OutputTokens != 42 || e.Metrics.Waiting != 3 || len(e.Models) != 1 || e.Window != 2048 || len(e.Slots) != 1 {
		t.Fatalf("custom = %+v", e)
	}
	// health failing is down even though the other paths answer
	w2 := &Watcher{Engines: []config.Engine{{Name: "c", Type: "custom", URL: s.URL, Health: "/gone", Models: "/models"}}}
	w2.Poll(context.Background())
	if w2.Snapshot()[0].Up {
		t.Fatal("custom up with its health path failing")
	}
}

// Without busy-seconds counters, tok/s is tokens over wall time.
func TestWallTimeRates(t *testing.T) {
	n := 1000
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			_, _ = w.Write([]byte("vllm:generation_tokens_total " + strconv.Itoa(n) + "\n"))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"m"}]}`))
	}))
	defer s.Close()
	w := &Watcher{Engines: []config.Engine{{Name: "v", Type: "vllm", URL: s.URL}}}
	w.Poll(context.Background())
	time.Sleep(200 * time.Millisecond)
	n = 1100
	w.Poll(context.Background())
	if tps := w.Snapshot()[0].DecodeTPS; tps < 300 || tps > 600 {
		t.Fatalf("decode tok/s = %g, want ~500 (100 tokens / 0.2 s)", tps)
	}
}

// Each fingerprint recognises its own engine and not its neighbours on a
// shared port; a configured port is never probed, and an extra port is.
func TestDiscovery(t *testing.T) {
	recognises := map[string]map[string]string{}
	for _, c := range engineCases {
		if _, ok := recognises[c.typ]; !ok {
			recognises[c.typ] = c.routes
		}
	}
	recognises["llamacpp"] = map[string]string{"/props": `{"default_generation_settings":{}}`}
	recognises["ollama"] = map[string]string{"/api/version": `{"version":"0.12"}`}
	recognises["llama-swap"] = map[string]string{"/running": `{"running":[]}`}
	recognises["strata"] = map[string]string{"/health": `{"service":"strata"}`}
	c := &http.Client{Timeout: time.Second}
	for typ, routes := range recognises {
		s := fake(t, routes)
		var got string
		for _, try := range probeOrder {
			if fingerprint(context.Background(), c, s.URL, try) {
				got = try
				break
			}
		}
		if got != typ {
			t.Errorf("%s server fingerprinted as %q", typ, got)
		}
	}
	// KoboldCpp's llama.cpp-shaped /props is not llama.cpp.
	kc := fake(t, map[string]string{"/props": `{"default_generation_settings":{},"total_slots":1}`})
	if fingerprint(context.Background(), c, kc.URL, "llamacpp") {
		t.Error("KoboldCpp /props fingerprinted as llama.cpp")
	}

	s := fake(t, recognises["comfyui"])
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(s.URL, "http://"))
	p, _ := strconv.Atoi(port)
	// Other engines on this machine may be found too; only the fake matters.
	mine := func(w *Watcher) []config.Engine {
		var out []config.Engine
		for _, e := range w.found {
			if e.URL == s.URL {
				out = append(out, e)
			}
		}
		return out
	}
	w := &Watcher{Discover: true, Ports: []int{p}}
	w.discover(context.Background())
	if got := mine(w); len(got) != 1 || got[0].Type != "comfyui" || !strings.HasPrefix(got[0].Name, "ComfyUI") {
		t.Fatalf("discovered %+v", got)
	}
	w.Poll(context.Background())
	for _, e := range w.Snapshot() {
		if e.URL == s.URL && !e.Up {
			t.Fatalf("discovered ComfyUI not up: %+v", e)
		}
	}
	w.discover(context.Background())
	if len(mine(w)) != 1 {
		t.Fatalf("rediscovered a claimed port: %+v", mine(w))
	}
	w2 := &Watcher{Discover: true, Ports: []int{p}, Engines: []config.Engine{{Name: "mine", Type: "comfyui", URL: s.URL}}}
	w2.discover(context.Background())
	if len(mine(w2)) != 0 {
		t.Fatalf("probed a configured engine's port: %+v", mine(w2))
	}
}
