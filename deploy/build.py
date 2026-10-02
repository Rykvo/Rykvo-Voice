"""在维护环境构建安装包；不打包后端源码、签名私钥或业务凭据。"""
import argparse
import gzip
import hashlib
import json
import os
import platform
import re
import shutil
import subprocess
import tempfile
from pathlib import Path

from release import copy_text, download, extract, package, publish
from update_bundle import bundle as update_bundle
import update_crypto
import compatibility

ROOT = Path(__file__).resolve().parent.parent


def compact_archive(archive):
    """Change gzip encoding only; verify the complete tar before replacing it."""
    archive = Path(archive)
    before = archive.stat().st_size
    with tempfile.TemporaryDirectory(prefix='rykvo-compress-', dir=archive.parent) as temporary:
        raw = Path(temporary) / 'payload.tar'
        candidate = Path(temporary) / 'payload.tar.gz'
        with gzip.open(archive, 'rb') as source, raw.open('wb') as output:
            shutil.copyfileobj(source, output)
        with candidate.open('wb') as output:
            subprocess.run(['zopfli', '--gzip', '--i5', '-c', str(raw)],
                           stdout=output, check=True, timeout=1800)
        with raw.open('rb') as expected, gzip.open(candidate, 'rb') as actual:
            while True:
                chunk = expected.read(1024 * 1024)
                if actual.read(len(chunk) or 1) != chunk:
                    raise ValueError('压缩前后内容不一致')
                if not chunk:
                    break
        after = candidate.stat().st_size
        if after < before:
            candidate.replace(archive)
        else:
            after = before
    print(f'{archive.name}: {before} -> {after} bytes (identical tar)', flush=True)


def strip_runtime(binary, arch):
    """Remove non-runtime symbols after verifying the pinned upstream download."""
    tools = {'amd64': ('x86_64-linux-gnu-strip', 62), 'arm64': ('aarch64-linux-gnu-strip', 183)}
    tool, machine = tools[arch]
    binary = Path(binary)
    with binary.open('rb') as source:
        header = source.read(20)
    if header[:6] != b'\x7fELF\x02\x01' or int.from_bytes(header[18:20], 'little') != machine:
        raise ValueError('运行文件架构不符')
    before = binary.stat().st_size
    subprocess.run([tool, '--strip-unneeded', str(binary)], check=True)
    after = binary.stat().st_size
    if not 0 < after <= before:
        raise ValueError('运行文件精简结果异常')
    print(f'{binary.name} {arch}: {before} -> {after} bytes', flush=True)


def fetch_proxy(config, arch, destination):
    proxy = config["sing-box"]
    with tempfile.TemporaryDirectory(prefix="rykvo-proxy-") as temporary:
        archive = Path(temporary) / "proxy.tar.gz"
        download(f"https://github.com/SagerNet/sing-box/releases/download/v{proxy['version']}/sing-box-{proxy['version']}-linux-{arch}.tar.gz", archive, proxy[arch])
        extract(archive, Path(temporary) / "unpacked")
        core = Path(temporary) / "unpacked" / f"sing-box-{proxy['version']}-linux-{arch}" / "sing-box"
        shutil.copyfile(core, destination)
        destination.chmod(0o755)


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
    if subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT, text=True).strip():
        raise ValueError("请先提交源码，再从干净提交构建")
    if not shutil.which('zopfli'):
        raise ValueError('构建环境缺少 zopfli')
    revision = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip()
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
    if list(output.iterdir()):
        raise ValueError("输出目录必须为空，避免发布旧文件")
    try:
        encryption_key = bytes.fromhex(os.environ["RYKVO_UPDATE_ENCRYPTION_KEY"])
        signing_key = bytes.fromhex(os.environ["RYKVO_UPDATE_SIGNING_KEY"])
        if len(encryption_key) != 32 or len(signing_key) != 32: raise ValueError()
    except (KeyError, ValueError):
        raise ValueError("发布密钥未配置") from None
    with tempfile.TemporaryDirectory(prefix="rykvo-private-build-") as private_output:
        plaintext = Path(private_output)
        manifest = {"schema": 1, "product": "rykvo-voice", "epoch": 2, "version": version, "revision": revision, "assets": {}}
        for arch in ("amd64", "arm64"):
            with tempfile.TemporaryDirectory(prefix="rykvo-build-") as temporary:
                bundle = Path(temporary)
                (bundle / "bin").mkdir()
                (bundle / "deploy").mkdir()
                environment.update(GOOS="linux", GOARCH=arch, CGO_ENABLED="0")
                subprocess.run([go, "build", "-trimpath", f"-ldflags=-s -w -X main.buildVersion={version}", "-o", str(bundle / "bin/rykvo-auth"), "."], cwd=ROOT / "backend", env=environment, check=True)
                if (arch, platform.machine()) in (('amd64', 'x86_64'), ('arm64', 'aarch64')):
                    reported = subprocess.check_output([str(bundle / 'bin/rykvo-auth'), '-version'], text=True, timeout=15).strip()
                    if reported != version:
                        raise ValueError('程序实际版本与 VERSION 不一致')
                runtime = config["cloudflared"]
                download(f"https://github.com/cloudflare/cloudflared/releases/download/{runtime['version']}/cloudflared-linux-{arch}", bundle / "bin/cloudflared", runtime[arch])
                strip_runtime(bundle / 'bin/cloudflared', arch)
                proxy = config["sing-box"]
                fetch_proxy(config, arch, bundle / "bin/sing-box")
                for path in ("bin/rykvo-auth", "bin/cloudflared", "bin/sing-box"):
                    (bundle / path).chmod(0o755)
                if (arch, platform.machine()) in (('amd64', 'x86_64'), ('arm64', 'aarch64')):
                    subprocess.run([str(bundle / 'bin/cloudflared'), '--version'], check=True, timeout=15)
                licenses(go, bundle / "licenses", runtime["version"])
                (bundle / 'licenses/cloudflared-SOURCE.txt').write_text(
                    f"https://github.com/cloudflare/cloudflared/tree/{runtime['version']}\n"
                    "Pinned official binary; non-runtime debug/symbol sections stripped.\n")
                download(f"https://raw.githubusercontent.com/SagerNet/sing-box/v{proxy['version']}/LICENSE", bundle / "licenses/sing-box.txt")
                (bundle / "licenses/sing-box-SOURCE.txt").write_text(f"https://github.com/SagerNet/sing-box/tree/v{proxy['version']}\nUnmodified official binary.\n")
                web = publish(ROOT / "frontend", bundle / "web")
                (bundle / "manifest.json").write_text(json.dumps(web, indent=2))
                for path in ("install.sh", "VERSION", "UPDATE_EPOCH", "deploy/nginx.conf", "deploy/network-control.py", "deploy/network_runtime.py", "deploy/rykvo-network.service", "deploy/rykvo-auth.service", "deploy/release.py", "deploy/local-update.py", "deploy/update_crypto.py", "deploy/update-signing.pub", "deploy/rykvo-update-validate.service", "deploy/network-drivers.py", "deploy/rykvo-update.socket", "deploy/rykvo-update@.service", "deploy/rykvo-update.service", "deploy/70-rykvo-voice.rules", "deploy/70-rykvo-voice-pcsc.rules", "deploy/qmi-read.py", "deploy/host-settings.py", "deploy/rykvo-host.socket", "deploy/rykvo-host@.service", "deploy/sip-network.py", "deploy/rykvo-sip-network.socket", "deploy/rykvo-sip-network@.service", "deploy/rykvo-sip-network.service", "deploy/rykvo-qmi.socket", "deploy/rykvo-qmi@.service", "deploy/rykvo-wifi.socket", "deploy/rykvo-wifi@.service"):
                    copy_text(ROOT / path, bundle / path)
                (bundle / "install.sh").chmod(0o755)
                archive = plaintext / f"rykvo-voice-linux-{arch}.tar.gz"
                package(bundle, archive)
                compact_archive(archive)
                manifest["assets"][arch] = {"sha256": hashlib.sha256(archive.read_bytes()).hexdigest(), "size": archive.stat().st_size}
        (plaintext / "release.json").write_text(json.dumps(manifest, indent=2) + "\n")
        inner = update_bundle(plaintext, revision)
        encrypted = output / f"RykvoVoice.{version}.rvu"
        update_crypto.encrypt(inner, encrypted, encryption_key, signing_key)
        if compatibility.verify(encrypted, encryption_key) != version:
            raise ValueError("旧版本升级器兼容检查失败")
        with encrypted.open("rb") as stream:
            header = stream.read(update_crypto.HEADER.size)
            stream.seek(-update_crypto.SEAL_SIZE, 2)
            update_crypto.inspect(header, stream.read(), encrypted.stat().st_size)
        outer = {"schema": 2, "product": "rykvo-voice", "version": version, "revision": revision,
                 "package": {"name": encrypted.name, "size": encrypted.stat().st_size, "sha256": hashlib.sha256(encrypted.read_bytes()).hexdigest()}}
        (output / "release.json").write_text(json.dumps(outer, indent=2) + "\n")
        installation = update_crypto.create_install_key(encryption_key, signing_key)
        update_crypto.inspect_install_key(installation)
        (output / "install.rvk").write_bytes(installation)
    print(output)


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--go", default="go")
    parser.add_argument("--output", type=Path, default=ROOT / "dist")
    args = parser.parse_args()
    build(args.go, args.output.resolve())
