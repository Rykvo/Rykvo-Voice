"""生成管理页上传文件；不发布、不携带源码或凭据。"""
import argparse
import hashlib
import json
import re
import zipfile
from pathlib import Path


def bundle(directory, revision):
    directory = Path(directory)
    if not re.fullmatch(r"[a-f0-9]{40}", revision):
        raise ValueError("构建提交无效")
    manifest = json.loads((directory / "release.json").read_text(encoding="utf-8"))
    version = manifest.get("version", "")
    if not re.fullmatch(r"(0|[1-9]\d{0,3})\.(0|[1-9]\d{0,3})\.(0|[1-9]\d{0,3})", version):
        raise ValueError("版本无效")
    if set(manifest.get("assets", {})) != {"amd64", "arm64"}:
        raise ValueError("架构不完整")
    files = {}
    for arch, info in manifest["assets"].items():
        name = f"rykvo-voice-linux-{arch}.tar.gz"
        path = directory / name
        if path.is_symlink() or not path.is_file() or not 0 < path.stat().st_size <= 64 * 1024 * 1024:
            raise ValueError("更新包缺失或过大")
        content = path.read_bytes()
        if len(content) != info.get("size") or hashlib.sha256(content).hexdigest() != info.get("sha256"):
            raise ValueError("更新包校验失败")
        files[name] = path
    metadata = {"schema": 1, "product": "rykvo-voice", "epoch": 2, "version": version,
                "revision": revision, "assets": manifest["assets"]}
    destination = directory / f"RykvoVoice {version}.zip"
    temporary = destination.with_suffix(".tmp")
    try:
        with zipfile.ZipFile(temporary, "w", compression=zipfile.ZIP_STORED, allowZip64=False) as archive:
            info = zipfile.ZipInfo("release.json", (2026, 1, 1, 0, 0, 0))
            archive.writestr(info, json.dumps(metadata, separators=(",", ":")).encode())
            for name, path in sorted(files.items()):
                info = zipfile.ZipInfo(name, (2026, 1, 1, 0, 0, 0))
                archive.writestr(info, path.read_bytes())
        temporary.replace(destination)
    finally:
        temporary.unlink(missing_ok=True)
    return destination


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("directory", type=Path)
    parser.add_argument("--revision", required=True)
    args = parser.parse_args()
    print(bundle(args.directory, args.revision))
