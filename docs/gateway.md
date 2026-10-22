# Gateway (preview)

- **One port for every engine.** `berth gateway` proxies OpenAI-style requests to the engine
  that serves the requested model, streaming.
- **Cold starts under a lease.** An engine with `start` and `serves` in `berth.toml` starts on
  the first request that names one of its models, through the `berth run` path: lease, drain
  of the holder, measured release, then the start. One start per engine, however many
  requests arrive while it loads.
- **Last switch wins.** Give gateway-started engines the priority of the engine they replace
  (`priority = "foreground"` next to a foreground llama-swap) and each side drains the other on
  demand. A lower priority waits behind the holder instead.
- **Clear failures.** Unknown model: 404. A start the broker refuses or an engine that exits
  before it answers: 503 with the reason, the engine's own failure line, a `Retry-After` and an
  error `code` a client can branch on: `outranked` (lost a juggle), `drained_while_starting`,
  `no_berth`, `not_ready`, `start_failed`, `engine_oom` (the engine could not get the GPU
  memory it asked for), `no_headroom` (a cold start would leave the machine short of RAM,
`--headroom-mib`) or `no_start`. An engine that crashes on start isn't started again on
  every retry: for 15 s, doubling to 5 min, the gateway answers at once with the same cause,
  and `Retry-After` says when the next start is allowed. A start that answers clears it. A
  model listed under `serves` by two engines fails `berth config check`.
- **API key.** Bearer token on every route; `berth key` prints it (created on first use, mode
  600). Loopback only; browser origins and non-loopback Host names are refused.

The node key authenticates the client to the gateway and stays there. A proxied request carries the
engine's own key when its entry has one (`api_key_env` or `api_key_file`), and no `Authorization`
when it has none. A credential that will not resolve (an unset variable, an unreadable file) sends
no `Authorization` and logs one WARN line that names the engine and the error, never the key.

## Switch engines by naming a model

Two engines that each want the whole card can't run together, and most harnesses can't start an engine
before they call it. `berth gateway` starts it for them. Give an engine a `start` command and the model names it
`serves` in `berth.toml`:

```toml
[[engine]]
name     = "strata"
type     = "llamacpp"
url      = "http://127.0.0.1:8080"
start    = ["/path/to/strata/.venv/bin/python", "serve/server.py", "--engine", "strata",
            "--config", "strata-262k.json", "--port", "8080"]
serves   = ["router-strata", "qwen3.8-flash-next-iq2_xs"]
priority = "foreground"   # equal to the engine it replaces, so the last model you ask for wins
prefetch = ["/models/strata"]   # read into the page cache while the holder drains
# kv_snapshots    = "/home/you/.cache/berth/kv/strata"  # the --slot-save-path the saves and restores share
# kv_snapshots_mib = 16384
```

Run `berth gateway` (loopback `127.0.0.1:9799`; a user unit is in `contrib/berth-gateway.service`) and send
your requests there with the node's API key as the bearer token. The gateway creates the key on first start
(`~/.config/berth/api.key`, mode 600) and `berth key` prints it, so a client that runs commands for its key
never has the secret written into its config:

```json
{ "baseUrl": "http://127.0.0.1:9799/v1", "apiKey": "!berth key" }
```

For each request the gateway:

1. reads the model name from the body;
2. hands the request straight to an engine that is up and lists that model (llama-swap and Ollama load
   their own models under their own leases);
3. otherwise starts the engine that `serves` it: as its own `berth run` process, under the same lease
   path (Berth drains the holder: llama-swap through its unload URL once its slots are idle, then
   SIGTERM, then SIGKILL), and the gateway waits until the new engine answers `/v1/models`;
4. proxies the request and streams tokens as the engine sends them.

Requests that arrive during a start wait on that one start. A model no engine serves gets a 404, and a
start the broker refuses comes back as a 503 with the broker's reason. A start waits for the card no longer
than `--ready` (10 minutes); if the holder has not left by then, the request fails and the start is
dropped, so it cannot take the card hours later. Asking llama-swap for one of its
models later drains the gateway's engine the same way, so either side can take the card back.

**A slow engine can name a fallback.** An interactive client should not sit through a 90 s cold start
when an engine that needs no card can answer now. Give an engine a `fallback` (another engine in the
same file, such as a CPU llama-server) and a request that has waited `fallback_after` (10 s by
default) goes there instead, when the fallback answers its probe at that moment. The fallback is
never started, leased or drained by the gateway: one that is not answering means no fallback, and the
request keeps waiting for its own engine. The primary's start is never cancelled either way, so
requests that come later land on it. `fallback_model` names the model the fallback lists the request
under when the two disagree, a non-streaming answer carries `X-Berth-Fallback: <fallback>`, and a
streaming answer, whose headers went out with the keep-alive preamble, carries one
`: berth: fallback <fallback>` comment before its data. The fallback gets the request without the
node's `Authorization` header; when the fallback's entry has its own key (`api_key_env` or `api_key_file`),
that is the one the request carries. One hop, per engine, opt-in: `berth config check`
refuses chains and a fallback that is not an engine in the file.

```toml
[[engine]]
name     = "strata"
type     = "llamacpp"
url      = "http://127.0.0.1:8080"
start    = ["/path/to/strata/.venv/bin/python", "serve/server.py", "--engine", "strata", "--port", "8080"]
serves   = ["qwen3.8-flash-next-iq2_xs"]
priority = "interactive"
fallback       = "strata-cpu"
fallback_after = "10s"
fallback_model = "qwen3.8-cpu"   # what the CPU server lists the model as

[[engine]]
name  = "strata-cpu"
type  = "llamacpp"
url   = "http://127.0.0.1:8010"
start = ["llama-server", "-m", "/models/qwen3.8-cpu.gguf", "--port", "8010"]   # no GPU
serves = ["qwen3.8-cpu"]
```

**A start reads the engine's files into the page cache while the holder drains.** Give an engine a
`prefetch` list of absolute files and directories, and the gateway asks the kernel to read them the
moment it decides to start that engine, before its `berth run` waits for the lease. The kernel reads
on its own schedule and takes the pages back when the machine needs the RAM, so a warm cache commits
no RAM of its own, and a start that finds its files still cached reads them from memory. A model
larger than the RAM the machine can spare gains little: its pages are pushed out as they are read. A directory means
its own files and not its subdirectories, and a file that cannot be asked for is one WARN line in the
gateway's journal while the start goes on. `berth prefetch MODEL` makes the same ask by hand.

**An agent context comes back without the prefill.** Give a llama-server engine `kv_snapshots`,
the same directory its `--slot-save-path` points at. On a drain, once every slot is idle, Berth asks
the engine to save each slot's prompt cache into that directory, records what each save wrote in a
ledger there, skips slots whose cache is empty, and trims the directory to `kv_snapshots_mib`
(16 GiB by default). When the gateway starts that engine again for the same model, it warms the
snapshots into the page cache during the wait, and the engine's empty slots restore their newest
snapshot once it answers, before the first request. A long agent conversation then switches and
switches back without prefilling again.

A warm cache is cached, never reserved: the pages sit in the kernel's page cache until the machine
needs the RAM, and a restore afterwards reads its bytes from the SSD again. Snapshots never cross
models: the restore takes only the files the ledger holds for the model the engine itself reports on
`/v1/models`. An engine that lists no model, or more than one (llama-swap lists every model it can
load), gets no snapshots, so a KV snapshot covers llama-swap's many-model entries only when
llama-swap gains per-model slots. An engine started without `--slot-save-path` says so, the drain
logs one line, and the switch is a plain one. The save step is bounded at 60 s and the restore at
30 s, so a failure on either side leaves the switch exactly as it was before.

A snapshot holds the slot's prompt tokens, so it is the conversation itself. Keep the directory
private to the user the engine runs as (`mkdir -p DIR && chmod 700 DIR`); Berth writes its ledger
there owner-only.

Measured on the reference RTX 4090 with the [swap bench](https://github.com/T-Crypt/berth/blob/main/contrib/bench/README.md) switching between
NInfer (through llama-swap) and Strata (262K context), both on an NVMe SSD, 2026-10-20 (two runs,
five switches each way):

| Switch | Drain of the holder | Request answered after |
|---|---|---|
| NInfer on an empty card | none | 12.2 s |
| Strata to NInfer | Strata released in 2.2 to 4.6 s | 15.4 to 17.5 s |
| NInfer to Strata, NInfer idle | NInfer released in 2.7 s | 74.8 s |
| NInfer to Strata, inside the juggle window | NInfer released in 1.5 to 3.1 s, after about 5 min | 71 to 85 s after the grant (Strata's own load) |

The bench asks for the other model the moment an answer ends, inside the 15 s juggle window, so
most switches to Strata also waited about 5 minutes behind the guard until llama-swap unloaded
NInfer on its own (374 to 388 s end to end). The one switch that came after NInfer sat idle
drained it at once.

At no point did the two engines hold the card at the same time.

**A gateway restart keeps its engines.** Each engine runs as its own `berth run` process, in a systemd
user scope on Linux (`systemd-run --user --scope`, when the user manager is reachable) so it is outside
the gateway service's cgroup, else in a new session. The gateway writes each started run, with its PID,
start time and lease, to a small file under the state directory (`~/.local/state/berth/gateway-engines.json`),
and the run's own output goes to `~/.local/state/berth/engines/<name>.log` (mode 600), never to a pipe the
gateway holds, so a gateway that stops cannot break an engine's writes. While the gateway runs it copies
the log to its own journal as well.
When the gateway comes back it reads that file and adopts every engine whose process is still the same
one, answering or still loading: the next request is proxied, not started again. Records for processes
that left are dropped. Restarting during a cold start (up to 85 s for Strata) costs nothing but the
restart itself.

**Stopping the gateway keeps its engines too.** Each engine runs outside the gateway's cgroup (above),
so stopping the gateway service does not stop the engines: they stay on the card under their own leases
and keep answering. `berth stop ENGINE` is how one is stopped on purpose: the daemon asks the engine's
leases to give the card back through the normal stop ladder, as when another engine asks for it, and a
lease frees only after a card read shows the engine's memory gone. `berth stop ENGINE --wait` follows
the daemon's event log until the leases are freed (exit 0) or stuck (exit 2, pointing at `berth reclaim`
for the stuck ones).

**Long waits and idle clients.** Cold starts take 12-85 s (table above), and minutes when the juggle
guard holds a switch back: longer than many OpenAI clients wait before giving up. Streaming requests get the answer early: the gateway sends its SSE
headers (`Content-Type: text/event-stream`, `Cache-Control: no-cache`) right away and writes an SSE
comment every 10 s while the start runs, `: berth: loading strata, 34 s`, so proxies and clients keep
the connection and a reader sees the wait moving. Once the engine answers, its body streams on; a
start that fails sends one OpenAI-shaped error event with the same reason code the 503 path carries,
then `data: [DONE]`. Non-streaming requests are unchanged; if a client drops a long non-streamed
wait, raise its read timeout or make the request streamed. `GET /v1/berth/wait?model=NAME` blocks
until the engine answers (a failed start is the same 503), so a harness can pre-warm before its
first real request. The gateway serves `/v1/chat/completions`,
`/v1/completions`, `/v1/embeddings` and a merged `/v1/models`, plus the ComfyUI surface: `POST /prompt`
queues a workflow (starting ComfyUI under a lease when it is down, holding the request until its `/queue`
answers), while `/ws`, `/queue`, `/history/`, `/view` and `/upload/` proxy to a running ComfyUI. Every route
needs the key. The gateway listens
on loopback only, refuses a Host that is not a loopback name (DNS rebinding) and refuses any request that
carries a browser `Origin`, so a web page cannot start an engine. ComfyUI already hands off at a job boundary under a lease (`--drain-comfyui`); a drain first unloads its models via `POST /free`, and `--keep-comfyui-up` leaves the server on its port for OpenWebUI instead of stopping it.

## What a switch costs

The gateway runs a start and sees both ends of it: the grant its run printed, which is
when the card showed free, and the engine's first answer. It times that pair for every
start it runs and keeps the last 20 times per engine and tier in a book
(`~/.local/state/berth/swapcost.json`), so it can price the next switch without guessing.
The wait for a free card belongs to the queue, not to the engine, so it is not part of the
figure. A failed start records nothing, and so does an engine the gateway does not start itself
(llama-swap's models load under llama-swap, so the gateway has nothing to time for them).

`GET /v1/berth/cost?engine=NAME` answers one estimate per tier, hot first. On the reference machine,
after five gateway starts of Strata from disk:

```bash
curl -s "http://127.0.0.1:9799/v1/berth/cost?engine=strata" -H "Authorization: Bearer $(berth key)"
```

```json
{"engine":"strata","estimates":[
  {"tier":"hot","took_ms":0,"samples":0,"known":true},
  {"tier":"warm","took_ms":0,"samples":0,"known":false},
  {"tier":"cool","took_ms":0,"samples":0,"known":false},
  {"tier":"cold","took_ms":72400,"samples":5,"known":true}]}
```

Hot, the engine already holding the card, is the free switch. Warm is a parked engine,
its weights in host RAM. Cool and cold split at half of the engine's files still held by
the kernel page cache. An estimate with `known:false` has no samples behind it, and its
`took_ms: 0` is not a prediction: a figure the gateway gives is one it measured.

`POST /v1/berth/plan` prices a chain an orchestrator declares ahead of time, one step per
switch, each naming the engine and the tier to find it in:

```bash
curl -s http://127.0.0.1:9799/v1/berth/plan -H "Authorization: Bearer $(berth key)" \
  -d '{"chain":[{"engine":"strata","tier":"cold"},{"engine":"comfyui","tier":"cold"},{"engine":"strata","tier":"cold"}]}'
```

```json
{"steps":[
  {"engine":"strata","tier":"cold","took_ms":72400,"samples":5,"known":true},
  {"engine":"comfyui","tier":"cold","took_ms":0,"samples":0,"known":false},
  {"engine":"strata","tier":"cold","took_ms":72400,"samples":5,"known":true}],
 "total_ms":144800,
 "unmeasured":1,
 "prefetch":["comfyui","strata",""],
 "keep_parked":["strata"]}
```

`total_ms` sums the known steps only: with `unmeasured` above 0 it is a lower bound, read
as "at least 145 s, 1 step unmeasured", never a promise. `prefetch` is per step, the engine
to warm while that step holds the card, and `keep_parked` lists the engines the chain comes
back to, which are worth parking in RAM instead of stopping and reloading. An unknown tier
or an engine `berth.toml` does not name is a 400 naming the step; a missing or wrong key
and any browser `Origin` are refused, as on every other route.

## Switch or stop an engine directly

A request naming a model switches the card on its own. To switch without sending a prompt, or to give the card back,
name the engine:

```sh
curl -s http://127.0.0.1:9799/v1/berth/switch -H "Authorization: Bearer $(berth key)" -d '{"engine":"strata"}'
curl -s http://127.0.0.1:9799/v1/berth/stop   -H "Authorization: Bearer $(berth key)" -d '{"engine":"strata"}'
```

- **Switch** answers `202` once the start is under way, and starts the engine under a lease the same way a request
  would: the holder drains at its boundary first. It takes any engine with a `start` command; another is a `404`.
- **Stop** answers `202` with the leases berthd asked to give the card back, the same drain as `berth stop`. A
  pinned holder (a game, a render, `berth hold`) is a `409`: Berth never stops it from the network. An engine that is
  not running under a lease is a `409` too, and berthd not answering is a `503`.

`GET /v1/berth/board` is the node in one read, for a board or a script: the card (who holds it, and whether it is
free, held, draining, in a VM or failing its probe), then every engine in `berth.toml` with its state (up, offline,
waiting, starting, failed), whether it can be switched to (`launch`) or stopped (`stoppable`), and its models with a
`loaded` flag from the engine's own reader. `cards` is a list so a node with more cards needs no new shape.

## Open a stopped engine's web UI

A bookmark to ComfyUI or Strata's chat page is a dead link while that engine is stopped. Give the engine a
`gui` URL (where a browser reaches its web UI) next to its `start` command, and run the gateway with
`--gui-listen 127.0.0.1:9796`:

```toml
[[engine]]
name  = "comfyui"
type  = "comfyui"
url   = "http://127.0.0.1:8188"
start = ["comfy", "--workspace", "/path/to/comfy", "launch", "--", "--listen", "127.0.0.1", "--port", "8188"]
gui   = "https://box.tailnet.ts.net:8188/"
```

Point the bookmark (or a home-page tile) at `http://127.0.0.1:9796/gui/comfyui` instead:

- **ComfyUI answers:** the browser goes straight to its UI.
- **ComfyUI is stopped:** the page says so and lists what holds the card (`llama-swap (NInfer-27B)  active`), with a
  **Launch comfyui** button. Opening the page takes nothing; only Launch asks for the card.
- **After Launch:** the page says what the start waits for, in the broker's terms: "Waiting for strata release:
  finishing its current request", "Waiting for llama-swap (NInfer-27B) release: freeing its memory", then
  "loading comfyui". It opens the UI once ComfyUI answers. A failed start shows its reason code and a retry button.
- `/gui/` lists every engine in `berth.toml` with its live state. Any engine with a `start` command gets a Launch
  button there, with or without a `gui` URL, so a phone on the tailnet can switch the card to Strata or a
  llama-server too. An engine with no web UI shows **Running** once it answers.

<p align="center"><img src="https://raw.githubusercontent.com/T-Crypt/berth/main/assets/preview-wake.png" alt="The gateway's wake page for comfyui: comfyui is offline and the card is free, with a Launch comfyui button" width="560"></p>

A start never runs two engines on the card at once. Berth drains the holder only at its boundary: llama-swap once its
slots are idle (`drain_post`), ComfyUI once its queue is empty (`drain_comfyui`), and a server with no unload URL
once its `/slots` show idle (`drain_slots`, below). A pinned holder (a game, a render, `berth hold`) is never stopped;
the page says so and the start waits until it ends.

The wake page needs no API key: it can only start an engine you gave a `start` command. It listens on
loopback, accepts loopback Host names plus the gateway's `--allow-host` names (for `tailscale serve` in
front), and takes a start only as a POST from its own page, so another site cannot make a visitor's
browser drain the card.

## Drain a server at its request boundary

An engine with `/slots` but no unload URL (Strata, a bare llama-server) would otherwise get SIGTERM mid-request when
another engine asks for the card. `drain_slots` (or `berth run --drain-slots URL`) names its base URL: a drain waits
until no slot reports `is_processing`, then stops it.

```toml
[[engine]]
name        = "strata"
type        = "llamacpp"
url         = "http://127.0.0.1:8080"
drain_slots = "http://127.0.0.1:8080"
```

The wait is unbounded by default, so a long generation is never cut off.
`drain_max` (or `berth run --drain-max DURATION`, which wins) sets a ceiling
on it: while the boundary is busy the wrapper prints a line each minute
naming the slots and how long they have been busy, and once the ceiling
passes it runs the normal stop ladder anyway and says so. With `--fail-closed`,
it refuses the drain and keeps the child and lease instead. The same ceiling
applies to a ComfyUI queue that will not empty.

Fail-closed also rejects missing or malformed slot states and unreadable queue states.
Without an explicit `drain_max`, slot checks and continuously unreadable ComfyUI queues
refuse after four times the run's grace interval. A readable busy ComfyUI queue still
waits without a deadline. A later drain can retry after the boundary recovers.
The default fail-open policy and native parking path remain unchanged.

## End a ComfyUI session

`exit_comfyui_idle = true` (or `--exit-comfyui-idle`) releases a run after
its first completed job. Existing history does not count as new work.
`exit_comfyui_idle_after` (or `--exit-comfyui-idle-after`) also releases a
session that receives no work; zero leaves that wait without a deadline.
These options require `drain_comfyui` and cannot combine with `keep_comfyui_up`.

The session detector and daemon drains share one drain ticket. A refusal retains
the original session deadline and work evidence, then retries after recovery.
A readable busy queue is never treated as idle because history is unavailable.
Completed-session release ends the process; daemon-requested parking and resume
keep their native protocol and lease routing. Without these options, behavior
is unchanged.

## ComfyUI nodes that call other engines

Berth hands ComfyUI's card over once its queue is empty. Some nodes do their work outside the
queue. A chat panel or a prompt expander inside ComfyUI can call a model on llama-swap, and that
call can drain ComfyUI's own lease. Without `keep_comfyui_up`, the drain stops the ComfyUI server,
and the request that asked for the card dies with it.

Keep ComfyUI up on a drain and give it a short park ttl:

```toml
[[engine]]
name            = "comfyui"
type            = "comfyui"
url             = "http://127.0.0.1:8188"
drain_comfyui   = "http://127.0.0.1:8188"
keep_comfyui_up = true
park_ttl        = "10m"
```

A drain then unloads ComfyUI's models through `POST /free` and leaves the server running, so the
node's request finishes. The lease stays parked. After `park_ttl` Berth stops the server, which
frees the CUDA context it still holds (504 MiB on the reference card). That stop is a normal drain,
so a job ComfyUI is running at that moment finishes first. `park_ttl` works on any parked engine
and replaces `[park] ttl` (30 minutes by default) for that engine.

## Park in RAM (warm switch)

A drain today ends with the engine's weights on disk, and the next request
reloads them: tens of seconds every switch. Parking keeps the warm copy in
host RAM instead:

| Tier | Where the weights are | Switch back | RAM cost |
|---|---|---|---|
| hot | VRAM | now | none |
| warm (parked) | host RAM, held by the engine | seconds | the footprint, committed while it lasts |
| cool | the kernel page cache | load minus disk | nothing committed |
| cold | disk | full cold start | none |

Config, one line per engine plus the node's budget:

```toml
[[engine]]
name       = "vllm-chat"
type       = "vllm"
url        = "http://127.0.0.1:8000"
park       = "hook"
park_url   = "http://127.0.0.1:8000/sleep?level=1"   # optional: each beats the built-in
resume_url = "http://127.0.0.1:8000/wake_up"
park_ttl   = "1h"                                     # optional: this engine's own ttl

[park]
floor_gib = 4.0        # RAM a park must leave the machine (default 4)
max_gib   = 48         # cap on all parked copies together (default: no cap)
ttl       = "30m"      # a parked copy is stopped after this long
residual_max_gib = 2   # card memory a parked engine may keep
```

- **How it runs.** A drain first waits for the engine's boundary (idle
  slots, or an empty ComfyUI queue), then asks berthd to admit the park:
  admission measures the machine's available RAM, and a park that would
  leave less than the floor is refused quietly, so the drain runs as
  today. On admission the wrapper calls the engine's own offload hook and
  records the park; the lease stays parked, the card's residual counts, and
  a waiter builds beside it. The next request sends the resume, and the
  engine serves again in seconds.
- **Adapters.** ComfyUI needs nothing: its unload keeps the models in RAM
  until memory pressure. vLLM needs `--enable-sleep-mode` and
  `VLLM_SERVER_DEV_MODE=1` in the start command. SGLang needs
  `--enable-memory-saver --enable-weights-cpu-backup`. Any other engine can
  speak the park protocol (`contrib/PARK-PROTOCOL.md`): Berth probes
  `GET /berth/state` once and uses it when the endpoint answers.
- **Eviction.** A parked copy is stopped, least recently parked first,
  when it passes its ttl, when the machine's available RAM falls under the
  floor, or one step at a time under memory pressure. Stopping a parked
  engine is a drain, and its run is Berth's own, so the stop is graceful.
- **Who sits out.** llama.cpp and llama-swap have no offload call upstream
  yet, so they run today's drain; a park hook there waits for an upstream
  endpoint or a patched route.
