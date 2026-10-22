# Copyright 2026 Trevin Tindall (T-Crypt)
# SPDX-License-Identifier: Apache-2.0

"""Opt-in Phoenix APU Vulkan embedding/handoff smoke using an existing GGUF.

No downloads or installed-service changes. Requires a local Vulkan llama-server
and embedding model. Uses a private Berth daemon, loopback port and a bounded
follow-up GTT allocation. GPU pools stay separate in the report. This is a
functional smoke test, not an embedding-quality or performance benchmark.
"""

import argparse
import json
import math
import os
from pathlib import Path
import shutil
import socket
import subprocess
import sys
import time
import urllib.error
import urllib.request


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--allow-allocation", action="store_true")
    parser.add_argument("--berth", required=True)
    parser.add_argument("--engine", required=True)
    parser.add_argument("--model", required=True)
    parser.add_argument("--gpu", type=int, default=1)
    parser.add_argument("--render-node", default="/dev/dri/renderD129")
    parser.add_argument("--output", required=True)
    parser.add_argument("--host-rss-limit-mib", type=int, default=4096, help="watchdog budget, 512..4096 MiB; preflight preserves an additional 2 GiB host floor")
    args = parser.parse_args()
    if not args.allow_allocation:
        parser.error("requires --allow-allocation; runs a model on the APU (2 GiB GPU / 4 GiB RSS limits)")
    if not 512 <= args.host_rss_limit_mib <= 4096:
        parser.error("host RSS budget must stay within 512..4096 MiB")
    rss_limit = args.host_rss_limit_mib << 20
    binary = str(Path(shutil.which(args.berth) or args.berth).resolve(strict=True))
    engine = str(Path(args.engine).resolve(strict=True))
    model = Path(args.model).resolve(strict=True)
    if model.stat().st_size > (1 << 30):
        parser.error("use an embedding GGUF of at most 1 GiB for this bounded smoke")
    card = Path(f"/sys/class/drm/card{args.gpu}/device")
    render = Path("/sys/class/drm") / Path(args.render_node).name / "device"
    if card.resolve(strict=True) != render.resolve(strict=True):
        parser.error("DRM card and render node refer to different GPUs")
    if (card / "ip_discovery/die/0/GC/0/revision").read_text().strip() != "1":
        parser.error("this check is for the validated Phoenix APU, not a discrete GPU")
    available = next(int(l.split()[1]) * 1024 for l in Path("/proc/meminfo").read_text().splitlines() if l.startswith("MemAvailable:"))
    free = int((card / "mem_info_vram_total").read_text()) - int((card / "mem_info_vram_used").read_text())
    if available < (2 << 30) + rss_limit or free < (2 << 30):
        parser.error(f"need {2048 + args.host_rss_limit_mib} MiB available host RAM and 2 GiB free reserved UMA before testing")
    pci = card.resolve().name
    vendor = (card / "vendor").read_text().strip().removeprefix("0x")
    device = (card / "device").read_text().strip().removeprefix("0x")
    directory = Path(args.output)
    directory.mkdir(parents=True, exist_ok=False)
    config = directory / "berth.toml"
    config.write_text("[engines]\ndiscover = false\n[claims]\nbuiltin = false\n[priority]\njuggle_window = '-1s'\n")
    sock = str(directory / "berth.sock")
    with socket.socket() as reservation:
        reservation.bind(("127.0.0.1", 0))
        port = reservation.getsockname()[1]
    base = f"http://127.0.0.1:{port}"
    env = os.environ.copy()
    for key in list(env):
        if key.startswith("LLAMA_ARG_") or key in ["GGML_VK_PREFER_HOST_MEMORY", "GGML_VK_ALLOW_SYSMEM_FALLBACK"]:
            del env[key]
    env["MESA_VK_DEVICE_SELECT"] = vendor + ":" + device + "!"
    env["GGML_VK_VISIBLE_DEVICES"] = "0"
    processes, logs = [], []
    report = {"result": "failed", "pci": pci, "port": port, "host_rss_limit": rss_limit, "peak_observed_rss": 0, "peak_observed_gpu": 0, "observed_gpu_pci": []}

    def spawn(name, argv, child_env=None):
        log = (directory / (name + ".log")).open("w")
        logs.append(log)
        p = subprocess.Popen(argv, env=child_env, stdout=log, stderr=subprocess.STDOUT, start_new_session=True)
        processes.append(p)
        return p

    def status():
        r = subprocess.run([binary, "status", "--socket", sock, "--format", "json"], capture_output=True, text=True, timeout=5, check=True)
        return json.loads(r.stdout)

    def guard():
        if not Path(sock).exists():
            return
        st = status()
        report["last_observation"] = st
        for lease in st["leases"]:
            if lease["engine"] != "apu-model":
                continue
            report["peak_observed_gpu"] = max(report["peak_observed_gpu"], lease["used"] + lease.get("gtt_used", 0))
            if lease["used"] + lease.get("gtt_used", 0) > (2 << 30):
                raise RuntimeError("model exceeded the 2 GiB GPU allocation ceiling")
            pids = set(lease.get("gpu_pids", [])) | {lease.get("pid", 0)}
            for pid in pids - {0}:
                try:
                    rss = int(Path(f"/proc/{pid}/statm").read_text().split()[1]) * os.sysconf("SC_PAGE_SIZE")
                    for line in Path(f"/proc/{pid}/status").read_text().splitlines():
                        if line.startswith("VmHWM:"):
                            rss = max(rss, int(line.split()[1]) * 1024)
                    report["peak_observed_rss"] = max(report["peak_observed_rss"], rss)
                    if rss > rss_limit:
                        raise RuntimeError("model exceeded its resident host RAM watchdog budget")
                    for fd in Path(f"/proc/{pid}/fdinfo").iterdir():
                        text = fd.read_text()
                        if "drm-driver:\tamdgpu" in text:
                            actual = next(l.split(":", 1)[1].strip() for l in text.splitlines() if l.startswith("drm-pdev:"))
                            if actual != pci:
                                raise RuntimeError("model opened an unexpected GPU: " + actual)
                            if actual not in report["observed_gpu_pci"]:
                                report["observed_gpu_pci"].append(actual)
                except FileNotFoundError:
                    pass

    def wait_for(predicate, description):
        deadline = time.monotonic() + 60
        while time.monotonic() < deadline:
            guard()
            result = predicate()
            if result:
                return result
            time.sleep(0.1)
        raise RuntimeError("timed out waiting for " + description)

    def healthy():
        try:
            with urllib.request.urlopen(base + "/health", timeout=2) as r:
                return r.status == 200
        except (urllib.error.URLError, TimeoutError):
            return False

    def owns(name, field, minimum):
        st = status()
        return st if any(l["engine"] == name and l.get(field, 0) >= minimum for l in st["leases"]) else None

    try:
        daemon = spawn("daemon", [binary, "daemon", "--socket", sock, "--gpu-backend", "amd", "--gpu", str(args.gpu),
            "--poll", "250ms", "--config", str(config), "--counters", "", "--usage", "", "--footprint", ""])
        wait_for(lambda: Path(sock).exists() or daemon.poll() is not None, "isolated daemon")
        report["initial"] = status()
        spawn("model", [binary, "run", "--socket", sock, "--config", str(config), "--as", "apu-model",
            "--priority", "batch", "--fail-closed", "--grace", "2s", "--wait", "30s", "--drain-slots", base,
            "--", sys.executable, str(Path(__file__).with_name("device-only.py")), args.render_node, "--",
            engine, "--model", str(model), "--alias", "berth-apu-smoke", "--embedding", "--pooling", "mean",
            "--embd-normalize", "2", "--ctx-size", "512", "--parallel", "1", "--batch-size", "512", "--ubatch-size", "512", "--threads", "2", "--threads-batch", "2",
            "--n-gpu-layers", "all", "--device", "Vulkan0", "--load-mode", "none", "--no-host", "--flash-attn", "on",
            "--host", "127.0.0.1", "--port", str(port), "--slots"], env)
        wait_for(healthy, "Vulkan embedding server")
        report["loaded"] = wait_for(lambda: owns("apu-model", "used", model.stat().st_size // 2), "measured model offload")
        outputs = []
        for text in ["Berth APU lease smoke test.", "Reserved UMA and shared system RAM are separate pools."]:
            request = urllib.request.Request(base + "/v1/embeddings", data=json.dumps({"model": "berth-apu-smoke", "input": text}).encode(), headers={"Content-Type": "application/json"})
            with urllib.request.urlopen(request, timeout=30) as response:
                vector = json.load(response)["data"][0]["embedding"]
            if len(vector) != 768 or not all(math.isfinite(v) for v in vector):
                raise RuntimeError("embedding was not a finite 768-dimensional vector")
            norm = math.sqrt(sum(v * v for v in vector))
            if abs(norm - 1) > 0.01:
                raise RuntimeError("embedding normalization smoke failed")
            outputs.append({"dimensions": len(vector), "norm": norm})
            guard()
        report["embeddings"] = outputs
        # Exercise the engine's own /slots idle boundary before normal drain.
        with urllib.request.urlopen(base + "/slots", timeout=5) as r:
            report["slots_after_requests"] = json.load(r)
        followup = spawn("next", [binary, "run", "--socket", sock, "--config", str(config), "--as", "apu-next",
            "--priority", "foreground", "--fail-closed", "--grace", "2s", "--wait", "30s", "--", sys.executable,
            str(Path(__file__).with_name("check.py")), "--allow-allocation", "--buffer", "--pool", "gtt",
            "--render-node", args.render_node, "--ready", str(directory / "next.json")])
        report["next"] = wait_for(lambda: owns("apu-next", "gtt_used", 64 << 20), "model drain and GTT handoff")
        followup.terminate()
        followup.wait(timeout=10)

        def released():
            st = status()
            return st if not st["leases"] else None

        report["final"] = wait_for(released, "final two-pool release")
        r = subprocess.run([binary, "events", "--socket", sock, "--format", "json"], capture_output=True, text=True, check=True, timeout=5)
        report["events"] = [json.loads(line) for line in r.stdout.splitlines() if line.strip()]
        freed = next(e["seq"] for e in report["events"] if e["kind"] == "lease.freed" and e.get("engine") == "apu-model")
        granted = next(e["seq"] for e in report["events"] if e["kind"] == "lease.granted" and e.get("engine") == "apu-next")
        if freed >= granted:
            raise RuntimeError("GTT child admitted before model release")
        report["result"] = "passed"
        print("PASS: real 780M Vulkan embeddings, GPU attribution, idle-boundary drain and GTT handoff")
    finally:
        for proc in reversed(processes):
            if proc.poll() is None:
                proc.terminate()
            try:
                proc.wait(timeout=10)
            except subprocess.TimeoutExpired:
                proc.kill()
                proc.wait(timeout=5)
        for log in logs:
            log.close()
        (directory / "report.json").write_text(json.dumps(report, indent=2) + "\n")
        print("Evidence:", directory)


if __name__ == "__main__":
    main()
