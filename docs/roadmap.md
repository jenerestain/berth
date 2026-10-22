# Roadmap

| Phase | What it delivers | Status |
|---|---|---|
| P0 | Safety core: identity, measured release, stuck and contested detection, unprotected runs | **Done** |
| P1 | Node API: event log, collisions board, config, engine readers, `/metrics`, light probe | **Done** |
| P2 | Harbor web UI; native telemetry, request KPIs, ledger, leaderboard, stored history, measured against Speculum parity | **Done** |
| P3 | `berth hold` and claims for games and apps (**done**); drain and hold from the harbor, behind authentication (planned) | Partly done |
| P3.5 | Node gateway: one OpenAI-compatible port; engines start on demand under a lease and drain each other | **Preview** |
| P4 | Restart reconciliation: engines re-attach after a daemon restart, nothing left unprotected | **Done** |
| P5 | Fleet: `berth install --harbor <addr> --join <token>` on every machine; nodes push metrics into the harbor's store (already built for one box in P2) | Planned |
| P5.5 | Harbor router: one URL serves models from any node in the fleet | Planned |
| P6 | ComfyUI job-boundary handoff on Linux (**done**); ComfyUI started on demand through the gateway (**done**); Windows node (**experimental**) | Partly done |
| P7 | Offers: a waiting request can offer for an idle card on another machine; the holder decides | Planned |
| P8 | Multi-GPU scheduling: a lease table per card, "any card that fits" | Planned |
| P9 | Organisations: single sign-on, role-based access, audit log, Postgres, regional federation; a mobile app after | Planned |
| P10 | Opt-in auto-yield: a card's owner can pre-approve offers for their own card | Later |

## Shipped: v0.3, fast swap and proof (2026-10-20)

v0.3.0 went after the cost you feel most, a reload from disk on every switch, and after proof
that Berth holds up when an engine misbehaves. It shipped park in RAM with adapters for ComfyUI,
vLLM and SGLang and an open [park protocol](https://github.com/T-Crypt/berth/blob/main/contrib/PARK-PROTOCOL.md); `berth stop`, `berth resume`
and `berth reclaim`; engines in Docker and Podman; a place and an estimate in every wait reason;
`berth update` and `berth service install`; the page-cache CACHED column and `berth prefetch`;
per-process VRAM on Windows and the test suite on Windows in CI; and the proof kit:
[contrib/chaos](https://github.com/T-Crypt/berth/blob/main/contrib/chaos/README.md), [contrib/bench/swap](https://github.com/T-Crypt/berth/tree/main/contrib/bench/swap), the drain report
form and the [tested hardware](benchmarks.md#tested-hardware) table. v0.3.1 fixed `berth update` answering from
a stale cache. The [CHANGELOG](https://github.com/T-Crypt/berth/blob/main/CHANGELOG.md) has every entry.

## Shipped since: v0.3.3 (2026-10-21)

v0.3.2 carried the first Windows testers' reports: the Epic launcher no longer holds the card from
the tray ([#81](https://github.com/T-Crypt/berth/issues/81)), per-process VRAM counts only the
governed card ([#82](https://github.com/T-Crypt/berth/issues/82)), and `berth service install`
starts its Windows tasks ([#89](https://github.com/T-Crypt/berth/issues/89)), with the minimum turn,
keys on drain requests and fail-closed drain boundaries beside them.

v0.3.3 carries the next round. The squatter total stops at the memory the driver reports in use, so
a Windows compositor's committed reservation no longer blocks admission
([#103](https://github.com/T-Crypt/berth/issues/103)). A lease whose `berth run` process dies after
attach frees itself ([#104](https://github.com/T-Crypt/berth/issues/104)), and the Windows check kit
works again ([#105](https://github.com/T-Crypt/berth/issues/105)). The gateway warms the next
engine's files during a drain (`prefetch`) and serves measured switch costs at `/v1/berth/cost` and
`/v1/berth/plan`.

## Next: v0.3.x

In the next v0.3.x releases. Each item has its building blocks merged or a spec written:

- Measured switch costs on the live daemon.
- Models started on demand for a standalone llama-server.
- Parked resume numbers in [Swap numbers](benchmarks.md#swap-numbers), which take the vLLM and SGLang park
  adapters out of experimental, and a measurement of NVIDIA's `cuda-checkpoint` for unmodified
  CUDA engines on Linux.

Not started, and in no release yet: `berth fit` and scenes, so engines that fit together never swap.

## After v0.3.x

| Release | What it brings |
|---|---|
| v0.4 | Broader AMD Linux hardware validation beyond the experimental RDNA3/RDNA4 probe; the Windows node out of experimental (a packaged installer, a locked-down control pipe) |
| v0.4 | Proxmox in every common layout. Berth detects whether it runs on a Proxmox host, in an LXC container or in a VM and says where it belongs (`berth env`). Engines in LXC containers lease the host's card and show by container, and a system service covers hosts without a user session |
| v0.4 | GPU passthrough to a VM (VFIO), experimental. `berth vfio run` drains the card and proves nothing holds it before your own script switches the driver, and the card shows as passed through until it comes back |
| v0.5 | Multi-GPU scheduling, including one lease across two cards for engines that split a model |
| v0.6 | The fleet: Berth on every machine, one harbor, one URL for every model |
| later | Offers: a waiting request can ask for an idle card on another machine, and its holder decides |

On hold: engine installers beyond the ComfyUI suite, the migration manager, and Berth swapping
models itself. llama-swap already swaps models well. Berth works beside it, and a proposal to
llama-swap for generic load hooks is planned, so Berth needs no wrapper line.
