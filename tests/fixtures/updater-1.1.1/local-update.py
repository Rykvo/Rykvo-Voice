"""Root-only encrypted updater. HTTP clients never choose paths or commands."""
import fcntl
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import platform
import pwd
import re
import secrets
import shutil
import socket
import struct
import subprocess
import sys
import tarfile
import time
import zipfile

STATE = Path("/var/lib/rykvo-update")
LIVE = Path("/opt/rykvo-voice/live")
MAX_ZIP = 129 * 1024 * 1024
CHUNK_SIZE = 1024 * 1024
UPLOAD_TTL = 24 * 60 * 60
VALIDATOR = "rykvo-update-validate.service"
MAX_ASSET = 64 * 1024 * 1024
UNIT = "rykvo-update.service"
ENV = {"PATH": "/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL": "C.UTF-8", "DEBIAN_FRONTEND": "noninteractive"}
spec = importlib.util.spec_from_file_location("release", Path(__file__).with_name("release.py"))
release = importlib.util.module_from_spec(spec)
spec.loader.exec_module(release)
crypto_spec = importlib.util.spec_from_file_location("update_crypto", Path(__file__).with_name("update_crypto.py"))
crypto = importlib.util.module_from_spec(crypto_spec)
crypto_spec.loader.exec_module(crypto)


def save(data):
    data["updated"] = int(time.time())
    temporary = STATE / "status.tmp"
    temporary.write_text(json.dumps(data))
    temporary.chmod(0o600)
    temporary.replace(STATE / "status.json")


def load():
    try:
        return json.loads((STATE / "status.json").read_text())
    except FileNotFoundError:
        return {"state": "idle"}


def running(unit=UNIT):
    value = subprocess.run(["systemctl", "show", unit, "--property=ActiveState", "--value"], env=ENV, capture_output=True, text=True, timeout=5)
    return value.stdout.strip() in ("activating", "active", "deactivating")


def status(owner=""):
    data = load()
    if data["state"] in ("queued", "running", "validating") and time.time() - data.get("updated", 0) > 15 and not running(VALIDATOR if data["state"] == "validating" else UNIT):
        data.update(state="failed", error="UPDATE_INTERRUPTED")
        save(data)
    if owner and data.get("owner") not in (None, owner):
        return {"error": "UPDATE_BUSY"}
    return {"data": {k: v for k, v in data.items() if k in ("state", "ticket", "version", "error", "uploadId", "offset", "size", "chunkSize")}}


def current():
    return ((LIVE / "VERSION").read_text().strip(), (LIVE / "UPDATE_EPOCH").read_text().strip() if (LIVE / "UPDATE_EPOCH").exists() else "1")


def validate_zip(archive, destination, installed=None):
    arch = {"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64"}.get(platform.machine())
    if not arch:
        raise ValueError("INVALID_UPDATE_PACKAGE")
    with zipfile.ZipFile(archive) as z:
        names = ["release.json", "rykvo-voice-linux-amd64.tar.gz", "rykvo-voice-linux-arm64.tar.gz"]
        if sorted(z.namelist()) != sorted(names):
            raise ValueError("INVALID_UPDATE_PACKAGE")
        for item in z.infolist():
            limit = 16384 if item.filename == "release.json" else MAX_ASSET
            if item.compress_type != zipfile.ZIP_STORED or item.flag_bits & 1 or not 0 < item.file_size <= limit:
                raise ValueError("INVALID_UPDATE_PACKAGE")
        m = json.loads(z.read("release.json"))
        if not isinstance(m, dict) or m.get("schema") != 1 or m.get("product") != "rykvo-voice" or m.get("epoch") != 2 or not re.fullmatch(r"[a-f0-9]{40}", str(m.get("revision", ""))):
            raise ValueError("INVALID_UPDATE_PACKAGE")
        version, epoch = current() if installed is None else installed
        try:
            release.check_version(m.get("version"), version, epoch)
        except ValueError:
            raise ValueError("UPDATE_DOWNGRADE") from None
        if not isinstance(m.get("assets"), dict) or set(m["assets"]) != {"amd64", "arm64"}:
            raise ValueError("INVALID_UPDATE_PACKAGE")
        for key, info in m["assets"].items():
            name = f"rykvo-voice-linux-{key}.tar.gz"
            if not isinstance(info, dict) or type(info.get("size")) is not int or z.getinfo(name).file_size != info["size"] or not re.fullmatch(r"[a-f0-9]{64}", str(info.get("sha256", ""))):
                raise ValueError("INVALID_UPDATE_PACKAGE")
            digest = hashlib.sha256()
            with z.open(name) as stream:
                while chunk := stream.read(1024 * 1024):
                    digest.update(chunk)
            if digest.hexdigest() != info["sha256"]:
                raise ValueError("INVALID_UPDATE_PACKAGE")
        tar_path = STATE / "archive.tar.gz"
        try:
            with z.open(f"rykvo-voice-linux-{arch}.tar.gz") as stream, tar_path.open("wb") as output:
                shutil.copyfileobj(stream, output, 1024 * 1024)
            with tarfile.open(tar_path, "r:gz") as tar:
                names, size = set(), 0
                for member in tar:
                    size += member.size
                    if member.name in names or len(names) >= 512 or size > 512 * 1024 * 1024:
                        raise ValueError("INVALID_UPDATE_PACKAGE")
                    names.add(member.name)
            release.extract(tar_path, destination)
            if release.verify(destination, version, epoch) != m["version"]:
                raise ValueError("INVALID_UPDATE_PACKAGE")
            for name in ("install.sh", "bin/rykvo-auth", "bin/cloudflared", "deploy/release.py", "deploy/local-update.py", "deploy/update_crypto.py", "deploy/update-signing.pub", "deploy/rykvo-update-validate.service", "deploy/rykvo-update.socket", "deploy/rykvo-update.service", "deploy/rykvo-update@.service", "manifest.json"):
                if not (destination / name).is_file():
                    raise ValueError("INVALID_UPDATE_PACKAGE")
            if not (destination / "web").is_dir() or not (destination / "licenses").is_dir():
                raise ValueError("INVALID_UPDATE_PACKAGE")
        finally:
            tar_path.unlink(missing_ok=True)
    return m["version"]



def validate(archive, destination):
    plain = STATE / "verified.zip"
    try:
        crypto.decrypt(archive, plain)
        return validate_zip(plain, destination)
    finally:
        plain.unlink(missing_ok=True)


def atomic_json(path, data):
    temporary = path.with_suffix(".tmp")
    with temporary.open("w") as stream:
        json.dump(data, stream)
        stream.flush(); os.fsync(stream.fileno())
    temporary.chmod(0o600)
    temporary.replace(path)


def transfer():
    try:
        return json.loads((STATE / "transfer.json").read_text())
    except FileNotFoundError:
        return None


def save_transfer(data):
    data["updated"] = int(time.time())
    atomic_json(STATE / "transfer.json", data)


def clear_upload():
    for name in ("upload.part", "chunk.tmp", "transfer.json"):
        (STATE / name).unlink(missing_ok=True)


def expire_upload():
    meta = transfer()
    if meta and time.time() - meta["updated"] > UPLOAD_TTL and load()["state"] not in ("validating", "queued", "running"):
        clear_upload()
        (STATE / "verified.rvu").unlink(missing_ok=True)
        shutil.rmtree(STATE / "staged", ignore_errors=True)
        save({"state": "idle"})


def uploading(meta):
    save({"state": "uploading", "owner": meta["owner"], "uploadId": meta["id"],
          "offset": meta["offset"], "size": meta["size"], "chunkSize": CHUNK_SIZE})
    return status(meta["owner"])


def start_upload(request):
    try:
        size, header, seal = request["size"], bytes.fromhex(request["header"]), bytes.fromhex(request["seal"])
    except (KeyError, ValueError, TypeError):
        raise ValueError("UPDATE_SIGNATURE_INVALID") from None
    crypto.inspect(header, seal, size)
    crypto.read_key()
    identity = hashlib.sha256(header + seal).hexdigest()
    meta = transfer()
    if meta and meta["owner"] != request["owner"]:
        raise ValueError("UPDATE_BUSY")
    if meta and meta["identity"] == identity and meta["size"] == size:
        if load()["state"] in ("ready", "validating"):
            return status(request["owner"])
        part = STATE / "upload.part"
        if part.is_file() and part.stat().st_size >= meta["offset"]:
            # A crash after fsync but before the offset commit discards only the unacknowledged tail.
            with part.open("r+b") as stream: stream.truncate(meta["offset"])
            save_transfer(meta)
            return uploading(meta)
    if shutil.disk_usage(STATE).free < size * 2 + 600 * 1024 * 1024:
        raise ValueError("UPDATE_STORAGE_LOW")
    clear_upload()
    (STATE / "verified.rvu").unlink(missing_ok=True)
    shutil.rmtree(STATE / "staged", ignore_errors=True)
    meta = {"id": secrets.token_hex(16), "owner": request["owner"], "size": size,
            "identity": identity, "header": header.hex(), "seal": seal.hex(), "offset": 0}
    (STATE / "upload.part").touch(mode=0o600, exist_ok=False)
    save_transfer(meta)
    return uploading(meta)


def owned_transfer(request):
    meta = transfer()
    if not meta or meta["owner"] != request["owner"] or meta["id"] != request["uploadId"]:
        raise ValueError("UPDATE_CONFLICT")
    return meta


def upload_chunk(request, stream, ready):
    meta = owned_transfer(request)
    offset, size = request["offset"], request["size"]
    if type(offset) is not int or type(size) is not int or offset < 0 or not 0 < size <= CHUNK_SIZE or offset + size > meta["size"] or offset > meta["offset"]:
        raise ValueError("UPDATE_CONFLICT")
    if load()["state"] != "uploading":
        raise ValueError("UPDATE_CONFLICT")
    if shutil.disk_usage(STATE).free < size + 600 * 1024 * 1024:
        raise ValueError("UPDATE_STORAGE_LOW")
    ready()
    temporary, part = STATE / "chunk.tmp", STATE / "upload.part"
    try:
        digest = hashlib.sha256()
        with temporary.open("wb") as out:
            left = size
            while left:
                block = stream.read(min(left, 65536))
                if not block: raise ValueError("UPDATE_CHUNK_INCOMPLETE")
                out.write(block); digest.update(block); left -= len(block)
        if offset < meta["offset"]:
            if offset + size > meta["offset"]: raise ValueError("UPDATE_CONFLICT")
            with part.open("rb") as inp:
                inp.seek(offset)
                if hashlib.sha256(inp.read(size)).digest() != digest.digest(): raise ValueError("UPDATE_CONFLICT")
        else:
            with part.open("r+b") as out, temporary.open("rb") as inp:
                if os.fstat(out.fileno()).st_size < offset: raise ValueError("UPDATE_CONFLICT")
                out.truncate(offset); out.seek(offset)
                shutil.copyfileobj(inp, out, 65536)
                out.flush(); os.fsync(out.fileno())
            meta["offset"] = offset + size
        save_transfer(meta)
        return uploading(meta)
    finally:
        temporary.unlink(missing_ok=True)


def finish_upload(request):
    meta = owned_transfer(request)
    if load()["state"] in ("ready", "validating"):
        return status(request["owner"])
    if meta["offset"] != meta["size"]:
        raise ValueError("UPDATE_CHUNK_INCOMPLETE")
    (STATE / "upload.part").replace(STATE / "verified.rvu")
    save({"state": "validating", "owner": meta["owner"], "uploadId": meta["id"]})
    try:
        subprocess.run(["systemctl", "start", "--no-block", VALIDATOR], env=ENV, check=True, timeout=5)
    except subprocess.SubprocessError:
        (STATE / "verified.rvu").replace(STATE / "upload.part")
        uploading(meta)
        raise ValueError("UPDATE_UNAVAILABLE") from None
    return status(request["owner"])


ERRORS = {"UPDATE_DOWNGRADE", "UPDATE_STORAGE_LOW", "UPDATE_SIGNATURE_INVALID", "UPDATE_KEY_UNAVAILABLE",
          "UPDATE_DECRYPT_FAILED", "UPDATE_BUSY", "UPDATE_CONFLICT", "UPDATE_CHUNK_INCOMPLETE", "UPDATE_UNAVAILABLE"}


def query(request, stream, ready=lambda: None):
    if not isinstance(request, dict) or not re.fullmatch(r"[a-f0-9]{64}", str(request.get("owner", ""))):
        return {"error": "INVALID_UPDATE_PACKAGE"}
    owner, action = request["owner"], request.get("action")
    if set(request) == {"action", "owner"} and action == "status":
        return status(owner)
    with (STATE / "lock").open("a") as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            return {"error": "UPDATE_BUSY"}
        expire_upload()
        data = load()
        if action == "finish" and set(request) == {"action", "owner", "uploadId"} and data["state"] in ("validating", "ready"):
            owned_transfer(request)
            return status(owner)
        if data["state"] in ("queued", "running", "validating"):
            return {"error": "UPDATE_BUSY"}
        if action == "start" and set(request) == {"action", "owner", "size", "header", "seal"}:
            return start_upload(request)
        if action == "chunk" and set(request) == {"action", "owner", "uploadId", "offset", "size"}:
            return upload_chunk(request, stream, ready)
        if action == "finish" and set(request) == {"action", "owner", "uploadId"}:
            return finish_upload(request)
        if action == "cancel" and set(request) == {"action", "owner", "uploadId"}:
            owned_transfer(request); clear_upload()
            (STATE / "verified.rvu").unlink(missing_ok=True)
            shutil.rmtree(STATE / "staged", ignore_errors=True)
            save({"state": "idle"}); return status(owner)
        if action == "apply" and set(request) == {"action", "owner", "ticket"}:
            if data["state"] != "ready" or data.get("owner") != owner or request["ticket"] != data.get("ticket"):
                return {"error": "UPDATE_CONFLICT"}
            data["state"] = "queued"; save(data)
            try:
                subprocess.run(["systemctl", "start", "--no-block", UNIT], env=ENV, check=True, timeout=5)
            except subprocess.SubprocessError:
                data["state"] = "ready"; save(data)
                return {"error": "UPDATE_UNAVAILABLE"}
            return status(owner)
    return {"error": "INVALID_UPDATE_PACKAGE"}


def verify_staged():
    destination = STATE / "incoming"
    try:
        shutil.rmtree(destination, ignore_errors=True)
        destination.mkdir(mode=0o700)
        version = validate(STATE / "verified.rvu", destination)
        shutil.rmtree(STATE / "staged", ignore_errors=True)
        destination.replace(STATE / "staged")
        return version
    finally:
        shutil.rmtree(destination, ignore_errors=True)


def run(validating=False):
    with (STATE / "lock").open("a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        data = load()
        if data["state"] != ("validating" if validating else "queued"):
            return
        try:
            # Rebuild staged files from the authenticated envelope immediately before execution.
            version = verify_staged()
            if validating:
                data.update(state="ready", version=version, ticket=secrets.token_hex(16))
            else:
                data["state"] = "running"; save(data)
                with (STATE / "install.log").open("wb") as log:
                    result = subprocess.run(["/bin/bash", str(STATE / "staged/install.sh"), "update"], env=ENV, stdin=subprocess.DEVNULL, stdout=log, stderr=subprocess.STDOUT)
                data.update(state="complete" if result.returncode == 0 else "failed")
                if result.returncode != 0: data["error"] = "UPDATE_INSTALL_FAILED"
        except (OSError, ValueError, subprocess.SubprocessError, tarfile.TarError, zipfile.BadZipFile) as error:
            data.update(state="failed", error=str(error) if str(error) in ERRORS else "INVALID_UPDATE_PACKAGE")
        finally:
            save(data)
            if not validating or data["state"] == "failed":
                shutil.rmtree(STATE / "staged", ignore_errors=True)
                (STATE / "verified.rvu").unlink(missing_ok=True)
                clear_upload()


def serve(connection):
    connection.settimeout(65)
    _, uid, _ = struct.unpack("3i", connection.getsockopt(socket.SOL_SOCKET, socket.SO_PEERCRED, 12))
    if uid not in (0, pwd.getpwnam("rykvo_voice").pw_uid): return
    try:
        with connection.makefile("rb") as stream:
            line = stream.readline(1025)
            if len(line) > 1024 or not line.endswith(b"\n"): raise ValueError("INVALID_UPDATE_PACKAGE")
            result = query(json.loads(line), stream, lambda: connection.sendall(b'{"ready":true}\n'))
    except (OSError, ValueError, KeyError, TypeError, tarfile.TarError, zipfile.BadZipFile, subprocess.SubprocessError) as error:
        result = {"error": str(error) if str(error) in ERRORS else "INVALID_UPDATE_PACKAGE"}
    try:
        connection.sendall(json.dumps(result).encode() + b"\n")
    except OSError:
        pass  # Disconnected clients resume using the committed offset.


if __name__ == "__main__":
    os.umask(0o077)
    STATE.mkdir(mode=0o700, parents=True, exist_ok=True)
    if sys.argv[1:] in (["run"], ["validate"]):
        run(validating=sys.argv[1] == "validate")
    elif not sys.argv[1:]:
        with socket.socket(fileno=0) as connection: serve(connection)
