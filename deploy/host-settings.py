"""Private hostname settings. DHCP changes apply on the next host boot."""
import fcntl
import importlib.util
import json
import os
from pathlib import Path
import pwd
import re
import socket
import stat
import struct
import subprocess
import tempfile

ETC = Path("/etc")
RUN = Path("/run")
SYS = Path("/sys")
PROC = Path("/proc")


def valid_name(value):
    return (isinstance(value, str) and value.lower() != "localhost"
            and re.fullmatch(r"[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?", value) is not None
            and re.search(r"[A-Za-z]", value) is not None)


def plain_path(path):
    if any(part.is_symlink() for part in (path, *path.parents)):
        raise ValueError("symlink")
    if path.exists() and not path.is_file():
        raise ValueError("not a file")
    return path


def read(path):
    plain_path(path)
    if not path.exists():
        return None
    if path.stat().st_size > 65536:
        raise ValueError("oversize")
    return path.read_bytes()


def current_name():
    value = (read(ETC / "hostname") or b"").decode().strip()
    return value or socket.gethostname()


def dhcp_targets():
    # Only physical default-route links; never touch SIP/VPN routes or DNS.
    targets = set()
    for line in (PROC / "net/route").read_text().splitlines()[1:]:
        fields = line.split()
        if len(fields) < 8 or fields[1] != "00000000" or not int(fields[3], 16) & 1:
            continue
        interface = fields[0]
        if not re.fullmatch(r"[a-zA-Z0-9_.-]{1,15}", interface):
            continue
        device = SYS / "class/net" / interface
        if not (device / "device").exists():
            continue
        index = (device / "ifindex").read_text().strip()
        if not index.isdecimal():
            raise ValueError("interface")
        state = RUN / "systemd/netif/links" / index
        if not state.exists():
            raise ValueError("unmanaged link")
        data = dict(line.split("=", 1) for line in state.read_text().splitlines() if "=" in line)
        network = Path(data.get("NETWORK_FILE", "")).name
        if not re.fullmatch(r"[a-zA-Z0-9_-][a-zA-Z0-9_.-]*\.network", network):
            raise ValueError("network file")
        targets.add(ETC / "systemd/network" / (network + ".d") / "90-rykvo-hostname.conf")
    if not targets:
        raise ValueError("no managed LAN")
    return sorted(targets)


def hosts_content(data, old, name):
    lines, found = [], False
    replaced = {old.lower(), name.lower()}
    for line in data.decode().splitlines(keepends=True):
        body, marker, comment = line.partition("#")
        fields = body.split()
        if fields and fields[0] == "127.0.1.1":
            aliases = [value for value in fields[1:] if value.lower() not in replaced]
            line = "127.0.1.1\t" + " ".join([name, *aliases])
            if marker:
                line += " #" + comment.rstrip("\r\n")
            line += "\n"
            found = True
        lines.append(line)
    value = "".join(lines)
    if not found:
        value = value.rstrip("\n") + "\n127.0.1.1\t" + name + "\n"
    return value.encode()


def write(path, data):
    plain_path(path)
    if data is None:
        path.unlink(missing_ok=True)
        return
    mode = stat.S_IMODE(path.stat().st_mode) if path.exists() else 0o644
    path.parent.mkdir(parents=True, exist_ok=True)
    if path == ETC / "hosts":
        # This one file is bind-mounted writable by systemd.
        with path.open("r+b") as stream:
            stream.write(data)
            stream.truncate()
            stream.flush()
            os.fsync(stream.fileno())
        return
    temporary = None
    try:
        with tempfile.NamedTemporaryFile(dir=path.parent, delete=False) as stream:
            temporary = Path(stream.name)
            os.fchmod(stream.fileno(), mode)
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
        temporary.replace(path)
    finally:
        if temporary:
            temporary.unlink(missing_ok=True)


def set_system_name(name):
    subprocess.run(
        ["/usr/bin/hostnamectl", "--static", "--transient", "set-hostname", "--", name],
        env={"PATH": "/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL": "C"},
        stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
        timeout=2, check=True,
    )


def status():
    editable = True
    try:
        dhcp_targets()
    except (OSError, ValueError):
        editable = False
    return {"data": {"hostname": current_name(), "editable": editable}}


def update(request):
    old = current_name()
    if request["expected"] != old:
        return {"error": "HOSTNAME_CONFLICT"}
    try:
        targets = dhcp_targets()
    except (OSError, ValueError):
        return {"error": "HOST_NETWORK_UNSUPPORTED"}
    name = request["hostname"]
    if name == old:
        return status()
    planned = {path: ("[DHCPv4]\nSendHostname=yes\nHostname=" + name +
                      "\n[DHCPv6]\nSendHostname=yes\nHostname=" + name + "\n").encode()
               for path in targets}
    cloud = ETC / "cloud/cloud.cfg.d"
    if cloud.is_dir():
        planned[cloud / "99-rykvo-hostname.cfg"] = b"preserve_hostname: true\nmanage_etc_hosts: false\n"
    hosts = ETC / "hosts"
    planned[hosts] = hosts_content(read(hosts) or b"", old, name)
    originals = {path: read(path) for path in planned}
    touched = []
    changing_system = False
    try:
        for path, data in planned.items():
            touched.append(path)
            write(path, data)
        changing_system = True
        set_system_name(name)
        if current_name() != name or socket.gethostname() != name:
            raise ValueError("readback")
    except (OSError, ValueError, subprocess.SubprocessError):
        restored = True
        if changing_system:
            try:
                set_system_name(old)
            except (OSError, subprocess.SubprocessError):
                restored = False
        for path in reversed(touched):
            try:
                write(path, originals[path])
            except (OSError, ValueError):
                restored = False
        return {"error": "HOSTNAME_SAVE_FAILED" if restored else "HOSTNAME_ROLLBACK_FAILED"}
    return {"data": {"hostname": name, "editable": True}}


def query(request):
    if not isinstance(request, dict):
        return {"error": "INVALID_REQUEST"}
    if request == {"action": "prepare-networks"}:
        with (RUN / "rykvo-hostname/network-lock").open("a") as lock:
            try:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError:
                return {"error": "NETWORK_AUTO_BUSY"}
            spec = importlib.util.spec_from_file_location("network_auto", Path(__file__).with_name("network-drivers.py"))
            module = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(module)
            return {"data": module.prepare_uplinks()}
    if request == {"action": "get"}:
        return status()
    if (set(request) != {"action", "hostname", "expected"} or request["action"] != "set"
            or not valid_name(request["hostname"]) or not isinstance(request["expected"], str)
            or not 1 <= len(request["expected"]) <= 253):
        return {"error": "INVALID_REQUEST"}
    with (RUN / "rykvo-hostname/lock").open("a") as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            return {"error": "HOSTNAME_BUSY"}
        return update(request)


def serve(connection):
    connection.settimeout(8)
    _, uid, _ = struct.unpack("3i", connection.getsockopt(socket.SOL_SOCKET, socket.SO_PEERCRED, 12))
    if uid not in (0, pwd.getpwnam("rykvo_voice").pw_uid):
        return
    with connection.makefile("rb") as stream:
        line = stream.readline(1025)
    try:
        result = query(json.loads(line)) if len(line) <= 1024 and line.endswith(b"\n") else {"error": "INVALID_REQUEST"}
    except (OSError, ValueError, KeyError, subprocess.SubprocessError):
        result = {"error": "HOSTNAME_UNAVAILABLE"}
    connection.sendall(json.dumps(result).encode() + b"\n")


if __name__ == "__main__":
    with socket.socket(fileno=0) as connection:
        serve(connection)
