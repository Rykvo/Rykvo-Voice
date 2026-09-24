"""Rykvo Voice: fixed read-only QMI queries over a private local socket."""
import json
import os
from pathlib import Path
import pwd
import re
import socket
import stat
import struct
import subprocess

QUERIES = frozenset((
    "--dms-get-ids", "--dms-get-model", "--dms-get-revision",
    "--dms-uim-get-iccid", "--nas-get-serving-system",
    "--nas-get-signal-info", "--dms-get-msisdn",
))


def valid_device(device):
    if not isinstance(device, str) or not re.fullmatch(r"/dev/(cdc-wdm\d+|wwan\d+qmi\d+)", device):
        return False
    path = Path(device)
    if path.is_symlink() or not stat.S_ISCHR(path.stat().st_mode):
        return False
    kind = "usbmisc" if path.name.startswith("cdc-wdm") else "wwan"
    node = Path("/sys/class") / kind / path.name
    major, minor = (int(v) for v in (node / "dev").read_text().strip().split(":"))
    return path.stat().st_rdev == os.makedev(major, minor)


def query(request):
    if not isinstance(request, dict) or set(request) != {"device", "command"}:
        return {"error": "INVALID_REQUEST"}
    command = request["command"]
    if not isinstance(command, str) or command not in QUERIES or not valid_device(request["device"]):
        return {"error": "INVALID_REQUEST"}
    try:
        result = subprocess.run(
            ["/usr/bin/qmicli", "--device=" + request["device"], "--device-open-proxy", command],
            env={"PATH": "/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL": "C"},
            stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
            timeout=3, check=False,
        )
    except subprocess.TimeoutExpired:
        return {"error": "READ_TIMEOUT"}
    if result.returncode or len(result.stdout) > 65536:
        return {"error": "QMI_READ_FAILED"}
    return {"output": result.stdout.decode("utf-8", "replace")}


def serve(connection):
    connection.settimeout(4)
    _, uid, _ = struct.unpack("3i", connection.getsockopt(socket.SOL_SOCKET, socket.SO_PEERCRED, 12))
    if uid not in (0, pwd.getpwnam("rykvo_voice").pw_uid):
        return
    with connection.makefile("rb") as stream:
        line = stream.readline(1025)
    try:
        result = query(json.loads(line)) if len(line) <= 1024 and line.endswith(b"\n") else {"error": "INVALID_REQUEST"}
    except (ValueError, OSError, KeyError):
        result = {"error": "QMI_READ_FAILED"}
    connection.sendall(json.dumps(result).encode() + b"\n")


if __name__ == "__main__":
    with socket.socket(fileno=0) as connection:
        serve(connection)
