import hashlib
import importlib.util
import json
import tempfile
import unittest
import zipfile
from pathlib import Path

spec = importlib.util.spec_from_file_location("update_bundle", Path(__file__).resolve().parents[1] / "deploy/update_bundle.py")
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class UpdateBundleTest(unittest.TestCase):
    def fixture(self, directory):
        assets = {}
        for arch in ("amd64", "arm64"):
            data = ("local-test-" + arch).encode()
            (directory / f"rykvo-voice-linux-{arch}.tar.gz").write_bytes(data)
            assets[arch] = {"size": len(data), "sha256": hashlib.sha256(data).hexdigest()}
        (directory / "release.json").write_text(json.dumps({"version": "1.1.0", "assets": assets}))

    def test_bundle_contains_only_manifest_and_two_archives(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            self.fixture(directory)
            (directory / "not-for-upload.key").write_text("fixture")
            path = module.bundle(directory, "a" * 40)
            self.assertEqual(path.name, "RykvoVoice 1.1.0.zip")
            with zipfile.ZipFile(path) as archive:
                self.assertEqual(set(archive.namelist()), {"release.json", "rykvo-voice-linux-amd64.tar.gz", "rykvo-voice-linux-arm64.tar.gz"})
                self.assertTrue(all(info.compress_type == zipfile.ZIP_STORED for info in archive.infolist()))
                metadata = json.loads(archive.read("release.json"))
                self.assertEqual(metadata["revision"], "a" * 40)
                self.assertEqual(metadata["epoch"], 2)
                self.assertEqual(metadata["product"], "rykvo-voice")
            original = path.read_bytes()
            self.assertEqual(module.bundle(directory, "a" * 40).read_bytes(), original)

    def test_reject_corruption_and_invalid_revision(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            self.fixture(directory)
            with self.assertRaises(ValueError):
                module.bundle(directory, "invalid")
            (directory / "rykvo-voice-linux-amd64.tar.gz").write_bytes(b"changed")
            with self.assertRaises(ValueError):
                module.bundle(directory, "a" * 40)
            self.assertEqual(list(directory.glob("*.zip")), [])

    def test_filename_tracks_manifest_version(self):
        for version in ("1.1.2", "1.1.3", "1.1.4"):
            with self.subTest(version=version), tempfile.TemporaryDirectory() as temporary:
                directory = Path(temporary)
                self.fixture(directory)
                manifest = json.loads((directory / "release.json").read_text())
                manifest["version"] = version
                (directory / "release.json").write_text(json.dumps(manifest))
                path = module.bundle(directory, "a" * 40)
                self.assertEqual(path.name, f"RykvoVoice {version}.zip")
                self.assertEqual(len(list(directory.glob("*.zip"))), 1)
                with zipfile.ZipFile(path) as archive:
                    self.assertEqual(json.loads(archive.read("release.json"))["version"], version)
