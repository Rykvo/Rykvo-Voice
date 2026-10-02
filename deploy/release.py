"""下载校验、受限解包和静态资源发布。"""
import argparse
import hashlib
import json
import re
import shutil
import sys
import tarfile
import time
import urllib.error
import urllib.request
from pathlib import Path, PurePosixPath
from urllib.parse import urlsplit

UPDATE_EPOCH = "2"
VERSION_PATTERN = r"(0|[1-9]\d{0,3})\.(0|[1-9]\d{0,3})\.(0|[1-9]\d{0,3})"
LIMIT = 256 * 1024 * 1024


class HTTPSRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        if urlsplit(newurl).scheme != "https":
            raise ValueError("下载地址必须使用 HTTPS")
        request = super().redirect_request(req, fp, code, msg, headers, newurl)
        if urlsplit(req.full_url).netloc != urlsplit(newurl).netloc:
            request.remove_header("Authorization")
        return request


def download(url, target, digest=None, token="", accept="application/octet-stream", max_size=LIMIT):
    if urlsplit(url).scheme != "https":
        raise ValueError("下载地址必须使用 HTTPS")
    headers = {"User-Agent": "Rykvo-Voice-Installer", "Accept": accept}
    if token:
        headers["Authorization"] = "Bearer " + token
    opener = urllib.request.build_opener(HTTPSRedirect())
    target = Path(target)
    for attempt in range(3):
        try:
            size = 0
            sha = hashlib.sha256()
            with opener.open(urllib.request.Request(url, headers=headers), timeout=120) as response, target.open("wb") as output:
                while chunk := response.read(1024 * 1024):
                    size += len(chunk)
                    if size > max_size:
                        raise ValueError("下载文件过大")
                    sha.update(chunk)
                    output.write(chunk)
            if digest and sha.hexdigest() != digest:
                raise ValueError("文件 SHA-256 不匹配")
            return
        except (urllib.error.URLError, TimeoutError, OSError):
            if attempt == 2:
                raise RuntimeError("下载失败，请检查网络和更新服务") from None
            time.sleep(2 ** attempt)
        finally:
            if target.exists():
                target.chmod(0o600)


def extract(archive, destination):
    destination = Path(destination)
    destination.mkdir(parents=True, exist_ok=True)
    with tarfile.open(archive, "r:gz") as tar:
        members = tar.getmembers()
        if sum(m.size for m in members) > LIMIT * 4 or len(members) > 100000:
            raise ValueError("归档超出限制")
        for member in members:
            path = PurePosixPath(member.name)
            if path.is_absolute() or ".." in path.parts or "\\" in member.name or not (member.isdir() or member.isfile()):
                raise ValueError("归档包含不安全路径或特殊文件")
            target = destination.joinpath(*path.parts)
            if not target.resolve().is_relative_to(destination.resolve()):
                raise ValueError("归档路径越界")
        for member in members:
            target = destination / member.name
            if member.isdir():
                target.mkdir(parents=True, exist_ok=True)
            else:
                target.parent.mkdir(parents=True, exist_ok=True)
                with tar.extractfile(member) as source, target.open("wb") as output:
                    shutil.copyfileobj(source, output)
                target.chmod(0o755 if member.mode & 0o111 else 0o644)


def copy_text(source, destination):
    """Linux deployment files must not inherit Windows checkout line endings."""
    Path(destination).write_text(Path(source).read_text(encoding="utf-8"), encoding="utf-8", newline="\n")


def package(source, destination):
    source = Path(source)

    def metadata(member):
        executable = member.name in ("install.sh", "bin/rykvo-auth", "bin/cloudflared", "bin/sing-box")
        member.mode = 0o755 if member.isdir() or executable else 0o644
        member.uid = member.gid = 0
        member.uname = member.gname = "root"
        return member

    with tarfile.open(destination, "w:gz") as tar:
        for path in sorted(source.rglob("*")):
            tar.add(path, arcname=path.relative_to(source), recursive=False, filter=metadata)


def publish(source, destination):
    source, destination = Path(source), Path(destination)
    files = {"index.html", "login.html", "THIRD_PARTY_LICENSE.txt"}
    html = "\n".join((source / name).read_text(encoding="utf-8") for name in ("index.html", "login.html"))
    files.update(re.findall(r'(?:src|href)="([^"?]+)', html))
    runtime = "\n".join(p.read_text(encoding="utf-8") for p in source.iterdir() if p.suffix in (".js", ".css", ".html") and not p.name.startswith("."))
    for asset in (source / "assets").iterdir():
        if asset.is_file() and asset.name in runtime:
            files.add("assets/" + asset.name)
    manifest = {}
    for name in sorted(files):
        path = PurePosixPath(name)
        if path.is_absolute() or ".." in path.parts or "\\" in name:
            raise ValueError("静态资源路径无效")
        src = source / name
        if src.is_symlink() or not src.is_file() or not src.resolve().is_relative_to(source.resolve()):
            raise ValueError("静态资源缺失或越界: " + name)
        dst = destination / name
        dst.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(src, dst)
        dst.chmod(0o644)
        manifest[name] = hashlib.sha256(dst.read_bytes()).hexdigest()
    return manifest


def check_version(version, current="", epoch="1"):
    if not isinstance(version, str) or not re.fullmatch(VERSION_PATTERN, version):
        raise ValueError("发布版本无效")
    if epoch not in ("1", UPDATE_EPOCH):
        raise ValueError("更新通道不匹配")
    if current:
        if not re.fullmatch(VERSION_PATTERN, current):
            raise ValueError("当前版本无效")
        if epoch == UPDATE_EPOCH and tuple(map(int, version.split("."))) < tuple(map(int, current.split("."))):
            raise ValueError("更新版本低于当前版本")


def verify(bundle, current="", epoch="1"):
    bundle = Path(bundle)
    version = (bundle / "VERSION").read_text().strip()
    if (bundle / "UPDATE_EPOCH").read_text().strip() != UPDATE_EPOCH:
        raise ValueError("安装包通道不匹配")
    check_version(version, current, epoch)
    return version


def prune(base="/opt/rykvo-voice", backups="/var/backups/rykvo-voice"):
    """Keep live plus the releases referenced by the latest two snapshots."""
    base, backups = Path(base), Path(backups)
    releases = base / "releases"
    for root in (base, backups, releases):
        if not root.is_absolute() or root.resolve() != root or not root.is_dir() or root == Path(root.anchor):
            raise ValueError("清理目录无效")

    def regular(path):
        return path.is_file() and not path.is_symlink()

    def managed(path):
        return path.parent == releases and not path.is_symlink() and path.is_dir() and regular(path / ".managed") and (path / ".managed").read_text().strip() == "rykvo-release"

    live = (base / "live").resolve()
    if not (base / "live").is_symlink() or not managed(live) or not regular(base / "latest-backup"):
        raise ValueError("缺少当前版本或恢复记录")
    latest = Path((base / "latest-backup").read_text().strip())
    snapshots = {}
    # Validate the entire plan before deleting; unknown directories stay untouched.
    for path in backups.iterdir():
        if not re.fullmatch(r"\d{8}-\d{6}-[a-f0-9]{6}", path.name):
            continue
        if path.is_symlink() or not path.is_dir() or not regular(path / "was-active") or (path / "was-active").read_text().strip() not in ("0", "1"):
            raise ValueError("备份记录不完整，已保留")
        previous = None
        if (path / "was-active").read_text().strip() == "1" and not regular(path / "live-link"):
            raise ValueError("恢复记录不完整，已保留")
        if (path / "live-link").exists() or (path / "live-link").is_symlink():
            if not regular(path / "live-link"):
                raise ValueError("恢复路径无效")
            previous = Path((path / "live-link").read_text().strip())
            if not previous.is_absolute():
                previous = base / previous
            if previous.resolve() != previous or not managed(previous):
                raise ValueError("恢复版本缺失，已保留")
        snapshots[path] = previous
    if latest not in snapshots:
        raise ValueError("当前备份缺失，已保留")
    keep = {latest}
    keep.update(sorted((p for p in snapshots if p != latest), reverse=True)[:1])
    protected = {live} | {snapshots[p] for p in keep if snapshots[p] is not None}
    for path in snapshots.keys() - keep:
        shutil.rmtree(path)
    for path in releases.iterdir():
        if path not in protected and managed(path) and re.fullmatch(r"\d{8}-\d{6}-[a-f0-9]{6}", path.name):
            shutil.rmtree(path)


def ensure_install_key(crypto, base, destination):
    path = crypto.DECRYPT_KEY
    if path.exists() or path.is_symlink():
        crypto.read_key(path)
        return
    installation = Path(destination) / "install.rvk"
    try:
        download(base + "/install.rvk", installation, max_size=crypto.INSTALL_SIZE)
        crypto.provision_install_key(installation.read_bytes(), path)
    finally:
        installation.unlink(missing_ok=True)


def bootstrap(destination, arch):
    destination = Path(destination)
    if arch not in ("amd64", "arm64"):
        raise ValueError("架构无效")
    base = "https://github.com/Rykvo/Rykvo-Voice/releases/latest/download"
    manifest_file = destination / "release.json"
    download(base + "/release.json", manifest_file, max_size=16384)
    manifest = json.loads(manifest_file.read_text())
    if not isinstance(manifest, dict) or manifest.get("schema") != 2 or manifest.get("product") != "rykvo-voice":
        raise ValueError("安装包清单无效")
    check_version(manifest.get("version"))
    info = manifest.get("package", {})
    if info.get("name") != f"RykvoVoice.{manifest['version']}.rvu" or type(info.get("size")) is not int or not 140 < info["size"] <= 130 * 1024 * 1024 or not re.fullmatch(r"[a-f0-9]{64}", str(info.get("sha256", ""))):
        raise ValueError("安装包清单无效")
    # Keep all files on the manifest's release even if latest changes mid-install.
    base = f"https://github.com/Rykvo/Rykvo-Voice/releases/download/release/v{manifest['version']}"
    import importlib.util
    spec = importlib.util.spec_from_file_location("local_updater", Path(__file__).with_name("local-update.py"))
    updater = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(updater)
    ensure_install_key(updater.crypto, base, destination)
    archive = destination / "bundle.rvu"
    download(base + "/" + info["name"], archive, info["sha256"], max_size=info["size"])
    updater.STATE = destination
    updater.current = lambda: ("", UPDATE_EPOCH)
    if updater.validate(archive, destination / "bundle") != manifest["version"]:
        raise ValueError("安装包版本不匹配")
    print(destination / "bundle")



def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("action", choices=("bootstrap", "extract", "web", "download", "verify", "prune"))
    parser.add_argument("paths", nargs="*")
    args = parser.parse_args()
    if args.action == "bootstrap":
        bootstrap(*args.paths)
    elif args.action == "verify":
        verify(*args.paths)
    elif args.action == "prune":
        if args.paths:
            raise ValueError("清理不接受外部路径")
        prune()
    elif args.action == "extract":
        extract(*args.paths)
    elif args.action == "web":
        manifest = publish(*args.paths)
        print(json.dumps(manifest, indent=2))
    else:
        download(*args.paths)


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        print(str(error), file=sys.stderr)
        sys.exit(1)
