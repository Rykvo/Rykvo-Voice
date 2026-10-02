import importlib.util
from pathlib import Path
import tempfile
import unittest

ROOT = Path(__file__).resolve().parent.parent
spec = importlib.util.spec_from_file_location("release", ROOT / "deploy/release.py")
r = importlib.util.module_from_spec(spec)
spec.loader.exec_module(r)


class RetentionTests(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        self.root = Path(temp.name).resolve()
        self.base, self.backups = self.root / "app", self.root / "backups"
        self.releases = self.base / "releases"
        self.releases.mkdir(parents=True); self.backups.mkdir()
        self.versions, self.snapshots = [], []
        for i in range(1, 6):
            name = f"20260930-00000{i}-abcdef"
            version, snapshot = self.releases / name, self.backups / name
            version.mkdir(); (version / ".managed").write_text("rykvo-release\n")
            (version / "binary").write_text(str(i))
            snapshot.mkdir(); (snapshot / "was-active").write_text("1" if i > 1 else "0")
            (snapshot / "database.dump").write_text("backup")
            if self.versions: (snapshot / "live-link").write_text(str(self.versions[-1]))
            self.versions.append(version); self.snapshots.append(snapshot)
        try:
            (self.base / "live").symlink_to(self.versions[-1], target_is_directory=True)
        except OSError:
            self.skipTest("Symlink support required")
        (self.base / "latest-backup").write_text(str(self.snapshots[-1]))

    def test_bounded_releases_snapshots_and_idempotence(self):
        state = self.root / "business-data"; state.mkdir(); (state / "settings").write_text("keep")
        r.prune(self.base, self.backups); r.prune(self.base, self.backups)
        self.assertEqual(set(self.releases.iterdir()), set(self.versions[-3:]))
        self.assertEqual(set(self.backups.iterdir()), set(self.snapshots[-2:]))
        self.assertEqual((state / "settings").read_text(), "keep")
        self.assertEqual((self.base / "live").resolve(), self.versions[-1])

    def test_latest_backup_is_kept_even_if_clock_went_back(self):
        (self.base / "latest-backup").write_text(str(self.snapshots[1]))
        r.prune(self.base, self.backups)
        self.assertTrue(self.snapshots[1].exists())
        self.assertTrue(self.versions[0].exists())
        self.assertTrue(self.versions[-1].exists())
        self.assertEqual(len(list(self.backups.iterdir())), 2)

    def test_unknown_directories_and_symlink_targets_are_not_deleted(self):
        outside = self.root / "outside"; outside.mkdir(); (outside / "keep").write_text("safe")
        link = self.releases / "20260929-000000-abcdef"; link.symlink_to(outside, target_is_directory=True)
        (self.releases / "manual-copy").mkdir()
        (self.backups / "manual-backup").mkdir()
        r.prune(self.base, self.backups)
        self.assertTrue((outside / "keep").exists())
        self.assertTrue(link.is_symlink())
        self.assertTrue((self.releases / "manual-copy").exists())
        self.assertTrue((self.backups / "manual-backup").exists())

    def test_corrupt_or_escaping_reference_aborts_before_any_deletion(self):
        for value in ("/etc", "../outside", str(self.releases / "missing")):
            (self.snapshots[-1] / "live-link").write_text(value)
            with self.assertRaises(ValueError): r.prune(self.base, self.backups)
            self.assertEqual(len(list(self.releases.iterdir())), 5)
            self.assertEqual(len(list(self.backups.iterdir())), 5)

    def test_relative_live_link_is_supported(self):
        (self.snapshots[-1] / "live-link").write_text("releases/" + self.versions[-2].name)
        r.prune(self.base, self.backups)
        self.assertTrue(self.versions[-2].exists())

    def test_backup_symlink_or_missing_latest_fails_closed(self):
        (self.base / "latest-backup").write_text(str(self.backups / "missing"))
        with self.assertRaises(ValueError): r.prune(self.base, self.backups)
        (self.base / "latest-backup").write_text(str(self.snapshots[-1]))
        link = self.backups / "20260928-000000-abcdef"
        link.symlink_to(self.snapshots[0], target_is_directory=True)
        with self.assertRaises(ValueError): r.prune(self.base, self.backups)
        self.assertTrue(self.snapshots[0].exists())

    def test_symlinked_root_is_rejected(self):
        alias = self.root / "alias"; alias.symlink_to(self.base, target_is_directory=True)
        with self.assertRaises(ValueError): r.prune(alias, self.backups)


if __name__ == "__main__": unittest.main()
