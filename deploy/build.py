"""在维护环境构建安装包；不打包后端源码或凭据。"""
import argparse
import hashlib
import json
import os
import re
import shutil
import subprocess
import tarfile
import tempfile
from pathlib import Path

from release import download, publish

ROOT = Path(__file__).resolve().parent.parent


def licenses(go, destination, cloudflared_version):
    destination.mkdir()
    goroot = subprocess.check_output([go, "env", "GOROOT"], text=True).strip()
    shutil.copyfile(Path(goroot) / "LICENSE", destination / "Go.txt")
    for name in ("LICENSE", "SOURCE.json", "NOTICE"):
        shutil.copyfile(ROOT / "backend/internal/carrierconfig" / name, destination / ("AOSP-APN-" + name))
    reference = ROOT / "backend/internal/vocat"
    for name in ("LICENSE", "SOURCE.json", "MODIFICATIONS.md"):
        shutil.copyfile(reference / name, destination / ("VoCat-" + name))
    modules = subprocess.check_output([go, "list", "-m", "-json", "all"], cwd=ROOT / "backend", text=True)
    decoder = json.JSONDecoder()
    while modules.strip():
        module, offset = decoder.raw_decode(modules.lstrip())
        modules = modules.lstrip()[offset:]
        if module.get("Main") or not module.get("Dir"):
            continue
        for name in ("LICENSE", "LICENSE.txt", "COPYING", "NOTICE"):
            path = Path(module["Dir"]) / name
            if path.is_file():
                shutil.copyfile(path, destination / (module["Path"].replace("/", "_") + "_" + name + ".txt"))
    download(f"https://raw.githubusercontent.com/cloudflare/cloudflared/{cloudflared_version}/LICENSE", destination / "cloudflared.txt")


def build(go, output):
    version = (ROOT / "VERSION").read_text().strip()
    if not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+", version):
        raise ValueError("版本格式无效")
    config = json.loads((ROOT / "deploy/runtime.json").read_text())
    result = subprocess.check_output([go, "version"], text=True)
    if "go" + config["go"]["version"] + " " not in result:
        raise ValueError("构建 Go 版本与 runtime.json 不一致")
    output.mkdir(parents=True, exist_ok=True)
    environment = os.environ | {"GOTOOLCHAIN": "local"}
    for command in (["mod", "verify"], ["test", "./..."], ["vet", "./..."]):
        subprocess.run([go] + command, cwd=ROOT / "backend", env=environment, check=True)
    manifest = {"version": version, "assets": {}}
    for arch in ("amd64", "arm64"):
        with tempfile.TemporaryDirectory(prefix="rykvo-build-") as temporary:
            bundle = Path(temporary)
            (bundle / "bin").mkdir()
            (bundle / "deploy").mkdir()
            environment.update(GOOS="linux", GOARCH=arch, CGO_ENABLED="0")
            subprocess.run([go, "build", "-trimpath", "-ldflags=-s -w", "-o", str(bundle / "bin/rykvo-auth"), "."], cwd=ROOT / "backend", env=environment, check=True)
            runtime = config["cloudflared"]
            download(f"https://github.com/cloudflare/cloudflared/releases/download/{runtime['version']}/cloudflared-linux-{arch}", bundle / "bin/cloudflared", runtime[arch])
            for path in ("bin/rykvo-auth", "bin/cloudflared"):
                (bundle / path).chmod(0o755)
            licenses(go, bundle / "licenses", runtime["version"])
            web = publish(ROOT / "frontend", bundle / "web")
            (bundle / "manifest.json").write_text(json.dumps(web, indent=2))
            for path in ("install.sh", "VERSION", "deploy/nginx.conf", "deploy/rykvo-auth.service", "deploy/release.py", "deploy/70-rykvo-voice.rules", "deploy/70-rykvo-voice-pcsc.rules", "deploy/qmi-read.py", "deploy/sip-network.py", "deploy/rykvo-sip-network.socket", "deploy/rykvo-sip-network@.service", "deploy/rykvo-sip-network.service", "deploy/rykvo-qmi.socket", "deploy/rykvo-qmi@.service", "deploy/rykvo-wifi.socket", "deploy/rykvo-wifi@.service"):
                shutil.copyfile(ROOT / path, bundle / path)
            (bundle / "install.sh").chmod(0o755)
            archive = output / f"rykvo-voice-linux-{arch}.tar.gz"
            with tarfile.open(archive, "w:gz") as tar:
                for path in sorted(bundle.rglob("*")):
                    tar.add(path, arcname=path.relative_to(bundle), recursive=False)
            manifest["assets"][arch] = {"sha256": hashlib.sha256(archive.read_bytes()).hexdigest(), "size": archive.stat().st_size}
    (output / "release.json").write_text(json.dumps(manifest, indent=2) + "\n")
    print(output)


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--go", default="go")
    parser.add_argument("--output", type=Path, default=ROOT / "dist")
    args = parser.parse_args()
    build(args.go, args.output.resolve())
