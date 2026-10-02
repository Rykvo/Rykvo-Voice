"""Validate new packages with the immutable first encrypted updater."""
import importlib.util
from pathlib import Path
import hashlib
import json
import tempfile
from types import SimpleNamespace

ROOT = Path(__file__).resolve().parents[1]
BASELINE_VERSION = "1.1.0"
BASELINE = ROOT / "tests/fixtures/updater-1.1.0"


def baseline_files():
    hashes = json.loads((BASELINE / "SHA256.json").read_text())
    required = {"release.py", "local-update.py", "update_crypto.py", "update-signing.pub"}
    if set(hashes) != required:
        raise ValueError("Invalid updater baseline manifest")
    files = {name: (BASELINE / name).read_bytes() for name in hashes}
    for name, content in files.items():
        if hashlib.sha256(content).hexdigest() != hashes[name]:
            raise ValueError("Updater baseline checksum mismatch: " + name)
    return files


def verify(archive, encryption_key):
    with tempfile.TemporaryDirectory(prefix="rykvo-compat-") as directory:
        root = Path(directory)
        for name, content in baseline_files().items():
            (root / name).write_bytes(content)
        spec = importlib.util.spec_from_file_location("baseline_updater", root / "local-update.py")
        baseline = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(baseline)
        baseline.crypto.read_key = lambda: encryption_key
        baseline.current = lambda: (BASELINE_VERSION, "2")
        versions = []
        for machine in ("x86_64", "aarch64"):
            baseline.platform = SimpleNamespace(machine=lambda: machine)
            baseline.STATE = root / machine
            baseline.STATE.mkdir(mode=0o700)
            versions.append(baseline.validate(Path(archive), baseline.STATE / "bundle"))
        if versions[0] != versions[1]:
            raise ValueError("双架构升级版本不一致")
        return versions[0]
