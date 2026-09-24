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

REPOSITORY = "Rykvo/Rykvo-Voice"
LIMIT = 256 * 1024 * 1024


class HTTPSRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        if urlsplit(newurl).scheme != "https":
            raise ValueError("下载地址必须使用 HTTPS")
        request = super().redirect_request(req, fp, code, msg, headers, newurl)
        if urlsplit(req.full_url).netloc != urlsplit(newurl).netloc:
            request.remove_header("Authorization")
        return request


def download(url, target, digest=None, token="", accept="application/vnd.github+json"):
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
                    if size > LIMIT:
                        raise ValueError("下载文件过大")
                    sha.update(chunk)
                    output.write(chunk)
            if digest and sha.hexdigest() != digest:
                raise ValueError("文件 SHA-256 不匹配")
            return
        except (urllib.error.URLError, TimeoutError, OSError):
            if attempt == 2:
                raise RuntimeError("下载失败，请检查网络或仓库读取权限") from None
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


def fetch(destination, arch):
    destination = Path(destination)
    if arch not in ("amd64", "arm64"):
        raise ValueError("架构无效")
    token = sys.stdin.readline().strip()
    metadata = destination / "github-release.json"
    download(f"https://api.github.com/repos/{REPOSITORY}/releases/latest", metadata, token=token)
    release = json.loads(metadata.read_text())
    version = release["tag_name"].removeprefix("v")
    if not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+", version):
        raise ValueError("发布版本无效")
    assets = {asset["name"]: asset for asset in release["assets"]}
    def asset(name, target, digest=None):
        asset_id = assets[name]["id"]
        if not isinstance(asset_id, int) or asset_id < 1:
            raise ValueError("发布资产无效")
        download(f"https://api.github.com/repos/{REPOSITORY}/releases/assets/{asset_id}", target, digest, token, "application/octet-stream")
    checksums = destination / "release.json"
    asset("release.json", checksums)
    manifest = json.loads(checksums.read_text())
    if manifest["version"] != version:
        raise ValueError("清单版本无效")
    digest = manifest["assets"][arch]["sha256"]
    if not re.fullmatch(r"[0-9a-f]{64}", digest):
        raise ValueError("发布校验和无效")
    archive = destination / f"rykvo-voice-linux-{arch}.tar.gz"
    asset(archive.name, archive, digest)
    extract(archive, destination / "bundle")
    bundle = destination / "bundle"
    if (bundle / "VERSION").read_text().strip() != version:
        raise ValueError("安装包版本不匹配")
    print(bundle)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("action", choices=("fetch", "extract", "web", "download"))
    parser.add_argument("paths", nargs="+")
    args = parser.parse_args()
    if args.action == "fetch":
        fetch(*args.paths)
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
