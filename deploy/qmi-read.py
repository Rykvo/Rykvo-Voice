"""Private helper: fixed QMI operations and a delayed host restart."""
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
    "--uim-get-slot-status", "--uim-get-card-status",
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


def valid_restart(request):
    if set(request) != {"device", "command", "endpoint", "generation"}:
        return False
    endpoint, generation = request["endpoint"], request["generation"]
    if not isinstance(endpoint, str) or not re.fullmatch(r"usb:\d+-\d+(\.\d+)*", endpoint):
        return False
    if not isinstance(generation, str) or not re.fullmatch(r"\d+:\d+", generation):
        return False
    node = Path("/sys/class/usbmisc") / Path(request["device"]).name
    interface = (node / "device").resolve(strict=True)
    usb = interface.parent
    return (
        (interface / "driver").resolve(strict=True).name == "qmi_wwan"
        and "usb:" + usb.name == endpoint
        and (usb / "idVendor").read_text().strip() == "2c7c"
        and (usb / "idProduct").read_text().strip() == "0125"
        and (usb / "product").read_text().strip().upper().startswith("EC20")
        and (usb / "busnum").read_text().strip() + ":" + (usb / "devnum").read_text().strip() == generation
    )


def restart_host():
    try:
        result = subprocess.run(
            ["/usr/bin/systemd-run", "--quiet", "--unit=rykvo-host-restart",
             "--on-active=5s", "--timer-property=AccuracySec=1s",
             "/usr/bin/systemctl", "reboot"],
            env={"PATH": "/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL": "C"},
            stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL, timeout=3, check=False,
        )
        return {"output": "scheduled"} if result.returncode == 0 else {"error": "HOST_RESTART_FAILED"}
    except (OSError, subprocess.TimeoutExpired):
        return {"error": "HOST_RESTART_UNKNOWN"}


def query(request):
    if request == {"command": "host-restart"}:
        return restart_host()
    if not isinstance(request, dict) or not {"device", "command"}.issubset(request):
        return {"error": "INVALID_REQUEST"}
    command = request["command"]
    restart = command == "--dms-set-operating-mode=reset"
    if not isinstance(command, str) or (not restart and (command not in QUERIES or set(request) != {"device", "command"})):
        return {"error": "INVALID_REQUEST"}
    if not valid_device(request["device"]) or (restart and not valid_restart(request)):
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
