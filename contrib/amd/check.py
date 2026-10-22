# Copyright 2026 Trevin Tindall (T-Crypt)
# SPDX-License-Identifier: Apache-2.0

"""Opt-in, isolated Linux AMD allocation/handoff check; no model or ROCm needed.

Uses two 64 MiB GEM buffers sequentially, on exactly the selected render node.
Never connects to the installed daemon or stops an existing workload. The
operator must explicitly allow allocation. Not part of CI or normal go tests.
"""

import argparse
import fcntl
import json
import os
from pathlib import Path
import signal
import shutil
import struct
import subprocess
import sys
import tempfile
import time


SIZE = 64 << 20


def buffer(node, ready, pool):
    # DRM_IOCTL_AMDGPU_GEM_CREATE: 32-byte in/out union, VRAM domain = 4.
    # A GEM allocation is real driver memory, not a HIP/Vulkan compute context.
    fd = os.open(node, os.O_RDWR | os.O_CLOEXEC)
    try:
        domain = 2 if pool == "gtt" else 4
        req = bytearray(struct.pack("=QQQQ", SIZE, 4096, domain, 0))
        fcntl.ioctl(fd, 0xC0206440, req, True)
        handle = struct.unpack_from("=I", req)[0]
        # Legacy amdgpu fdinfo accounts VM-mapped buffers, not every GEM
        # handle. Map this object into this file's GPU VA, as runtimes do.
        fcntl.ioctl(fd, 0x40286448, struct.pack("=IIIIQQQ", handle, 0, 1, 6, 0x100000000, 0, SIZE))
        stop = False

        def finish(_signum, _frame):
            nonlocal stop
            stop = True

        signal.signal(signal.SIGTERM, finish)
        signal.signal(signal.SIGINT, finish)
        Path(ready).write_text(json.dumps({"pid": os.getpid(), "bytes": SIZE, "pool": pool}))
        deadline = time.monotonic() + 30
        while not stop and time.monotonic() < deadline:
            time.sleep(0.05)
        fcntl.ioctl(fd, 0x40286448, struct.pack("=IIIIQQQ", handle, 0, 2, 0, 0x100000000, 0, SIZE))
        # DRM_IOCTL_GEM_CLOSE. Closing fd in finally also releases all handles.
        fcntl.ioctl(fd, 0x40086409, struct.pack("=II", handle, 0))
    finally:
        os.close(fd)


def wait_for(predicate, description):
    deadline = time.monotonic() + 20
    while time.monotonic() < deadline:
        result = predicate()
        if result:
            return result
        time.sleep(0.1)
    raise RuntimeError("timed out waiting for " + description)


def check(args):
    card = Path(f"/sys/class/drm/card{args.gpu}/device")
    render = Path("/sys/class/drm") / Path(args.render_node).name / "device"
    if card.resolve(strict=True) != render.resolve(strict=True):
        raise RuntimeError("render node and selected DRM card refer to different GPUs")
    free = int((card / "mem_info_vram_total").read_text()) - int(
        (card / "mem_info_vram_used").read_text()
    )
    if free < 2 * SIZE:
        raise RuntimeError("less than 128 MiB free; refusing to allocate")
    if args.pool != "vram":
        gtt_free = int((card / "mem_info_gtt_total").read_text()) - int((card / "mem_info_gtt_used").read_text())
        available = next(int(line.split()[1]) * 1024 for line in Path("/proc/meminfo").read_text().splitlines() if line.startswith("MemAvailable:"))
        if gtt_free < 2 * SIZE or available < (2 << 30) + 2 * SIZE:
            raise RuntimeError("insufficient GTT/host RAM headroom; refusing to allocate")
    directory = Path(args.output) if args.output else Path(tempfile.mkdtemp(prefix="berth-amd-"))
    if args.output:
        directory.mkdir(parents=True, exist_ok=False)
    sock = str(directory / "berth.sock")
    config = directory / "berth.toml"
    config.write_text("[engines]\ndiscover = false\n[claims]\nbuiltin = false\n[priority]\njuggle_window = '-1s'\n")
    binary = str(Path(shutil.which(args.berth) or args.berth).resolve(strict=True))
    processes = []
    logs = []

    def spawn(name, argv):
        log = (directory / (name + ".log")).open("w")
        logs.append(log)
        proc = subprocess.Popen(argv, stdout=log, stderr=subprocess.STDOUT, start_new_session=True)
        processes.append(proc)
        return proc

    def status():
        result = subprocess.run(
            [binary, "status", "--socket", sock, "--format", "json"],
            capture_output=True, text=True, timeout=5, check=True,
        )
        return json.loads(result.stdout)

    def holding(name, pool):
        st = status()
        report["last_observation"] = st
        field = "gtt_used" if pool == "gtt" else "used"
        matches = [l for l in st["leases"] if l["engine"] == name and l.get(field, 0) >= SIZE]
        return st if matches else None

    def released():
        st = status()
        return st if not st["leases"] else None

    report = {"result": "failed"}
    try:
        daemon = spawn("daemon", [binary, "daemon", "--socket", sock,
            "--gpu-backend", "amd", "--gpu", str(args.gpu), "--poll", "250ms",
            "--config", str(config), "--counters", "", "--usage", "", "--footprint", ""])
        wait_for(lambda: Path(sock).exists() or daemon.poll() is not None, "isolated daemon")
        if daemon.poll() is not None:
            raise RuntimeError("daemon failed; see " + str(directory / "daemon.log"))
        report["initial"] = status()
        scheduled = next(g for g in report["initial"]["gpus"] if g["scheduled"])
        if args.pool != "vram" and not scheduled.get("apu"):
            raise RuntimeError("GTT checks require an APU; refusing to allocate on another device")
        if scheduled.get("apu"):
            # Simulate an insufficient floor, without creating actual RAM
            # pressure or allocating anything. The child must never start.
            guard_sock = str(directory / "ram-guard.sock")
            guard = spawn("ram-guard", [binary, "daemon", "--socket", guard_sock,
                "--gpu-backend", "amd", "--gpu", str(args.gpu), "--poll", "250ms",
                "--headroom-mib", "1048576", "--config", str(config),
                "--counters", "", "--usage", "", "--footprint", ""])
            wait_for(lambda: Path(guard_sock).exists() or guard.poll() is not None, "RAM-guard daemon")
            marker = directory / "must-not-start"
            blocked = subprocess.run([binary, "run", "--socket", guard_sock, "--config", str(config),
                "--as", "ram-guard", "--wait", "500ms", "--fail-closed", "--", sys.executable,
                "-c", "from pathlib import Path; Path(" + repr(str(marker)) + ").touch()"],
                capture_output=True, text=True, timeout=10)
            if blocked.returncode == 0 or marker.exists() or "APU host RAM headroom" not in blocked.stderr:
                raise RuntimeError("APU RAM guard did not refuse before starting the child: " + blocked.stderr)
            report["ram_guard"] = "passed"
            guard.terminate()
            guard.wait(timeout=10)
        pools = ["vram", "gtt"] if args.pool == "mixed" else [args.pool, args.pool]
        for (name, priority), pool in zip([("amd-check-a", "batch"), ("amd-check-b", "foreground")], pools):
            spawn(name, [binary, "run", "--socket", sock, "--config", str(config),
                "--as", name, "--priority", priority, "--fail-closed", "--grace", "2s",
                "--wait", "15s", "--", sys.executable, str(Path(__file__).resolve()),
                "--allow-allocation", "--buffer", "--render-node", args.render_node,
                "--pool", pool,
                "--ready", str(directory / (name + ".json"))])
            report[name] = wait_for(lambda: holding(name, pool), name + " " + pool + " allocation attribution")
        a_pid = json.loads((directory / "amd-check-a.json").read_text())["pid"]
        b_pid = json.loads((directory / "amd-check-b.json").read_text())["pid"]
        if Path(f"/proc/{a_pid}/fdinfo").exists():
            raise RuntimeError("first buffer process still exists when the second was admitted")
        # Stop only our own wrapper; runner forwards the signal to its child.
        processes[-1].terminate()
        processes[-1].wait(timeout=10)
        report["final"] = wait_for(released, "final driver-confirmed release")
        events = subprocess.run([binary, "events", "--socket", sock, "--format", "json"],
            capture_output=True, text=True, timeout=5, check=True)
        report["events"] = [json.loads(line) for line in events.stdout.splitlines() if line.strip()]
        if not any(e["kind"] == "lease.draining" and e.get("engine") == "amd-check-a" for e in report["events"]):
            raise RuntimeError("first engine was not drained by the higher-priority waiter")
        freed = next(e["seq"] for e in report["events"] if e["kind"] == "lease.freed" and e.get("engine") == "amd-check-a")
        granted = next(e["seq"] for e in report["events"] if e["kind"] == "lease.granted" and e.get("engine") == "amd-check-b")
        if freed >= granted:
            raise RuntimeError("next lease was granted before the previous release was confirmed")
        report["result"] = "passed"
        report["buffer_pids"] = [a_pid, b_pid]
        report["bytes_per_buffer"] = SIZE
        report["pools"] = pools
        print("PASS: real AMD allocation, PID attribution, priority drain, driver-confirmed release and handoff")
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


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--allow-allocation", action="store_true")
    parser.add_argument("--berth", default="berth")
    parser.add_argument("--gpu", type=int, default=0)
    parser.add_argument("--render-node", default="/dev/dri/renderD128")
    parser.add_argument("--output")
    parser.add_argument("--pool", choices=["vram", "gtt", "mixed"], default="vram", help="APU: test reserved VRAM, shared GTT, or a handoff between them")
    parser.add_argument("--buffer", action="store_true", help=argparse.SUPPRESS)
    parser.add_argument("--ready", help=argparse.SUPPRESS)
    args = parser.parse_args()
    if not args.allow_allocation:
        parser.error("requires --allow-allocation; allocates up to 128 MiB on the selected GPU")
    if args.buffer:
        buffer(args.render_node, args.ready, args.pool)
    else:
        check(args)


if __name__ == "__main__":
    main()
