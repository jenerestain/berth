# Copyright 2026 Trevin Tindall (T-Crypt)
# SPDX-License-Identifier: Apache-2.0

"""Run a validation child with access to exactly one DRM render node.

Linux Landlock ABI >= 5 is required; there is no unisolated fallback. This is a
test helper, not a Berth runtime dependency. No sudo, namespaces, driver changes
or system-wide permissions are used. Normal filesystem permissions still apply.
"""

import ctypes
import os
from pathlib import Path
import platform
import sys


def main():
    if len(sys.argv) < 4 or sys.argv[2] != "--":
        raise SystemExit("usage: device-only.py /dev/dri/renderDN -- COMMAND [ARGS...]")
    if sys.platform != "linux" or platform.machine() not in ["x86_64", "aarch64"]:
        raise SystemExit("validated Landlock syscall numbers require Linux x86_64/aarch64")
    node = Path(sys.argv[1]).resolve(strict=True)
    if node.parent != Path("/dev/dri") or not node.name.startswith("renderD"):
        raise SystemExit("select one DRM render node")
    libc = ctypes.CDLL(None, use_errno=True)

    def call(number, *args):
        result = libc.syscall(number, *args)
        if result < 0:
            error = ctypes.get_errno()
            raise OSError(error, os.strerror(error))
        return result

    abi = call(444, 0, 0, 1)
    if abi < 5:
        raise SystemExit("Landlock ABI >= 5 is required for device-ioctl isolation")
    rights = (1 << 1) | (1 << 2) | (1 << 15)  # WRITE_FILE, READ_FILE, IOCTL_DEV
    ruleset_attr = ctypes.c_uint64(rights)
    ruleset = call(444, ctypes.byref(ruleset_attr), ctypes.sizeof(ruleset_attr), 0)

    class PathRule(ctypes.Structure):
        _pack_ = 1
        _fields_ = [("allowed_access", ctypes.c_uint64), ("parent_fd", ctypes.c_int32)]

    def allow(path):
        fd = os.open(path, os.O_PATH | os.O_CLOEXEC)
        try:
            rule = PathRule(rights, fd)
            call(445, ruleset, 1, ctypes.byref(rule), 0)
        finally:
            os.close(fd)

    try:
        for entry in Path("/").iterdir():
            if entry.name != "dev":
                try:
                    allow(entry)
                except FileNotFoundError:
                    pass
        for name in ["null", "zero", "full", "random", "urandom", "tty", "shm", "pts"]:
            path = Path("/dev") / name
            if path.exists():
                allow(path)
        allow(node)
        if libc.prctl(38, 1, 0, 0, 0) != 0:  # PR_SET_NO_NEW_PRIVS, child only
            error = ctypes.get_errno()
            raise OSError(error, os.strerror(error))
        call(446, ruleset, 0)
    finally:
        os.close(ruleset)
    # Fail closed if an unexpected device is accessible. Landlock checks open
    # permissions before the DRM driver's open callback creates any context.
    for other in Path("/dev/dri").glob("*"):
        if other.is_dir() or other.resolve() == node:
            continue
        try:
            fd = os.open(other, os.O_RDWR | os.O_CLOEXEC)
        except PermissionError:
            continue
        else:
            os.close(fd)
            raise RuntimeError("unexpected GPU node remains accessible: " + str(other))
    os.execvpe(sys.argv[3], sys.argv[3:], os.environ)


if __name__ == "__main__":
    main()
