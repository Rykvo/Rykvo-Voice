import importlib.util
import io
import json
import re
import subprocess
import tarfile
import tempfile
import unittest
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
spec = importlib.util.spec_from_file_location("release", ROOT / "deploy/release.py")
release = importlib.util.module_from_spec(spec)
spec.loader.exec_module(release)


class ReleaseTests(unittest.TestCase):
    def test_official_line_uses_its_own_pinned_signing_key(self):
        current = (ROOT / 'deploy/update-signing.pub').read_text().strip()
        baseline = (ROOT / 'tests/fixtures/updater-1.1.0/update-signing.pub').read_text().strip()
        previous = (ROOT / 'tests/fixtures/updater-1.1.1/update-signing.pub').read_text().strip()
        self.assertEqual(current, baseline)
        self.assertNotEqual(current, previous)

    def test_release_and_page_versions_follow_version_file(self):
        version = (ROOT / 'VERSION').read_text().strip()
        self.assertRegex(version, r'^\d+\.\d+\.\d+$')
        notes = (ROOT / 'deploy/RELEASE_NOTES.md').read_text(encoding='utf-8')
        self.assertEqual(notes.splitlines()[0], '# ' + version)
        for name in ('index.html', 'login.html'):
            html = (ROOT / 'frontend' / name).read_text(encoding='utf-8')
            versions = re.findall(r'(?:src|href)="[^"?]+\?v=([^"&]+)', html)
            self.assertTrue(versions, name)
            self.assertEqual(set(versions), {version}, name)

    def test_bundle_has_explicit_linux_permissions(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source = root / "bundle"
            (source / "bin").mkdir(parents=True)
            for name in ("bin/rykvo-auth", "bin/cloudflared", "bin/sing-box", "install.sh", "VERSION"):
                (source / name).write_bytes(b"fixture\n")
            release.package(source, root / "bundle.tar.gz")
            with tarfile.open(root / "bundle.tar.gz") as tar:
                for member in tar.getmembers():
                    self.assertEqual(member.mode, 0o644 if member.name == "VERSION" else 0o755)
                    self.assertEqual((member.uid, member.gid), (0, 0))

    def test_deployment_files_use_linux_line_endings(self):
        with tempfile.TemporaryDirectory() as directory:
            source, target = Path(directory) / "source", Path(directory) / "target"
            source.write_bytes("主机\r\n1.7.0\r\n".encode())
            release.copy_text(source, target)
            self.assertEqual(target.read_bytes(), "主机\n1.7.0\n".encode())

    def test_web_only_contains_runtime(self):
        with tempfile.TemporaryDirectory() as directory:
            manifest = release.publish(ROOT / "frontend", directory)
            self.assertIn("index.html", manifest)
            self.assertIn("login.html", manifest)
            self.assertIn("assets/rykvo-voice.png", manifest)
            self.assertNotIn("README.md", manifest)
            self.assertFalse(any("tests" in path or path.endswith(".go") for path in manifest))
            self.assertEqual(len(manifest), len(list(Path(directory).rglob("*.*"))))

    def archive(self, root, name, kind=tarfile.REGTYPE):
        path = Path(root) / "test.tar.gz"
        with tarfile.open(path, "w:gz") as tar:
            member = tarfile.TarInfo(name)
            member.type = kind
            member.linkname = "/etc/passwd" if kind == tarfile.SYMTYPE else ""
            if kind == tarfile.REGTYPE:
                member.size = 2
                tar.addfile(member, io.BytesIO(b"ok"))
            else:
                tar.addfile(member)
        return path

    def test_rejects_unsafe_archives(self):
        for name, kind in [("../escape", tarfile.REGTYPE), ("/absolute", tarfile.REGTYPE), ("a\\b", tarfile.REGTYPE), ("link", tarfile.SYMTYPE), ("device", tarfile.CHRTYPE)]:
            with self.subTest(name=name), tempfile.TemporaryDirectory() as directory:
                with self.assertRaises(ValueError):
                    release.extract(self.archive(directory, name, kind), Path(directory) / "out")

    def test_regular_archive(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "out"
            release.extract(self.archive(directory, "web/index.html"), output)
            self.assertEqual((output / "web/index.html").read_text(encoding="utf-8"), "ok")

    def test_redirect_strips_credentials(self):
        redirect = release.HTTPSRedirect()
        request = urllib.request.Request("https://api.github.com/example", headers={"Authorization": "Bearer test-only"})
        result = redirect.redirect_request(request, None, 302, "", {}, "https://codeload.github.com/example")
        self.assertFalse(result.has_header("Authorization"))
        with self.assertRaises(ValueError):
            redirect.redirect_request(request, None, 302, "", {}, "http://example.com")

    def test_plain_http_download_rejected(self):
        with self.assertRaises(ValueError):
            release.download("http://example.com", "unused")

    def test_versions_and_checksums(self):
        config = json.loads((ROOT / "deploy/runtime.json").read_text(encoding="utf-8"))
        for runtime in config.values():
            self.assertRegex(runtime["version"], r"^\d+\.\d+\.\d+$")
            for arch in ("amd64", "arm64"):
                self.assertRegex(runtime[arch], r"^[0-9a-f]{64}$")

    def test_compiled_deployment_and_secret_handling(self):
        installer = (ROOT / "install.sh").read_text(encoding="utf-8")
        self.assertNotIn("RYKVO_GITHUB_TOKEN", installer)
        self.assertIn('verify "$SOURCE" "$current" "$epoch"', installer)
        self.assertNotIn("go build", installer)
        self.assertNotIn("git clone", installer)
        self.assertIn("127.0.0.1:8080", (ROOT / "deploy/nginx.conf").read_text(encoding="utf-8"))

    def test_retention_runs_only_after_health_and_committed_switch(self):
        installer = (ROOT / "install.sh").read_text(encoding="utf-8")
        deploy = installer.split("deploy() {", 1)[1].split("confirm_uninstall()", 1)[0]
        self.assertLess(deploy.index("health || die"), deploy.index("install_manager"))
        self.assertLess(deploy.index("SWITCHING=0"), deploy.index('release.py" prune'))
        self.assertIn('prune || printf', deploy)
        build = (ROOT / "deploy/build.py").read_text(encoding="utf-8")
        self.assertIn('"deploy/network-drivers.py"', build)


class LocalUpdateVersionTests(unittest.TestCase):
    def test_channel_migration_and_version_order(self):
        release.check_version("1.1.0", "1.7.29", "1")
        release.check_version("1.1.2", "1.1.0", "2")
        release.check_version("1.1.2", "1.1.2", "2")
        for version, current, epoch in [("1.1.0", "1.1.2", "2"), ("01.1.0", "", "1"), ("1.1.0", "1.1.0", "3"), (None, "", "1")]:
            with self.subTest(version=version, epoch=epoch), self.assertRaises(ValueError):
                release.check_version(version, current, epoch)

    def test_installer_rejects_local_downgrade_and_wrong_epoch(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "VERSION").write_text("1.1.0")
            (root / "UPDATE_EPOCH").write_text("2")
            with self.assertRaises(ValueError):
                release.verify(root, "1.1.2", "2")
            (root / "UPDATE_EPOCH").write_text("1")
            with self.assertRaises(ValueError):
                release.verify(root)


@unittest.skipUnless(Path("/bin/bash").exists(), "Linux Bash required")
class InstallerTests(unittest.TestCase):
    def run_shell(self, script, directory):
        result = subprocess.run(["bash", "-c", 'source "$1/install.sh"; UNIT="$2/default-unit"; UPDATE_UNITS="$2/update-units"; SIP_SOCKET="$2/sip-socket"; SIP_UNIT="$2/sip-unit"; SIP_BOOT="$2/sip-boot"; UPLINK_TMP="$2/uplink-tmp"; NETWORK_UNIT="$2/network-unit"; ' + script, "test", str(ROOT), directory], capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr + result.stdout)

    def test_removal_scope(self):
        with tempfile.TemporaryDirectory() as directory:
            self.run_shell('BASE="$2/app"; mkdir -p "$BASE/releases/test"; touch "$BASE/releases/test/file"; remove_app_path "$BASE/releases/test"; [[ ! -e "$BASE/releases/test" ]]; if (remove_app_path "$2/elsewhere"); then exit 1; fi', directory)

    def test_rollback_restores_files_and_link(self):
        with tempfile.TemporaryDirectory() as directory:
            self.run_shell('''BASE="$2/app"; BACKUP="$2/backup"; UNIT="$2/unit"; SITE="$2/site"; ENABLED="$2/enabled"; HARDWARE_RULE="$2/hardware-rule"; PCSC_RULE="$2/pcsc-rule"; HOST_SOCKET="$2/host-socket"; HOST_UNIT="$2/host-unit"; QMI_SOCKET="$2/qmi-socket"; QMI_UNIT="$2/qmi-unit"; WIFI_SOCKET="$2/wifi-socket"; WIFI_UNIT="$2/wifi-unit";
MANAGER="$2/manager"; WRAPPER="$2/rykvo"; MANAGER_CHANGED=1;
mkdir -p "$BASE/releases/old" "$BASE/releases/new" "$BACKUP/manager/deploy" "$MANAGER/deploy" "$UPDATE_UNITS";
printf old-helper > "$BACKUP/manager/deploy/local-update.py"; printf new-helper > "$MANAGER/deploy/local-update.py";
printf old-wrapper > "$BACKUP/wrapper"; printf new-wrapper > "$WRAPPER";
printf old-socket > "$BACKUP/rykvo-update.socket"; printf new-socket > "$UPDATE_UNITS/rykvo-update.socket";
printf old-wifi > "$BACKUP/wifi-unit"; printf new-wifi > "$WIFI_UNIT";
printf old-unit > "$BACKUP/unit"; printf old-site > "$BACKUP/nginx";
printf '%s' "$BASE/releases/old" > "$BACKUP/live-link";
printf '%s' "$SITE" > "$BACKUP/enabled-link";
printf new > "$UNIT"; printf new > "$SITE"; ln -s "$BASE/releases/new" "$BASE/live"; ln -s "$SITE" "$ENABLED";
systemctl() { :; }; nginx() { :; }; udevadm() { :; }; WAS_ACTIVE=1; SWITCHING=1; rollback;
[[ $(cat "$MANAGER/deploy/local-update.py") == old-helper && $(cat "$WRAPPER") == old-wrapper && $(cat "$UPDATE_UNITS/rykvo-update.socket") == old-socket ]];
[[ $(cat "$UNIT") == old-unit && $(cat "$SITE") == old-site && $(cat "$WIFI_UNIT") == old-wifi ]];
[[ $(readlink "$BASE/live") == "$BASE/releases/old" && $SWITCHING == 0 ]];''', directory)

    def test_health_checks_path_and_private_assets(self):
        with tempfile.TemporaryDirectory() as directory:
            self.run_shell('''TEMP="$2"; systemctl() { return 0; }; sleep() { :; };
curl() {
  if [[ "$*" == *health.html* ]]; then printf 'id="login-form"' > "$TEMP/health.html";
  elif [[ "$*" == *app.js* ]]; then printf 401;
  elif [[ "$*" == */gly ]]; then printf 200;
  else printf 404; fi
}; health;''', directory)

    def test_failed_candidate_removed_only_after_safe_rollback(self):
        with tempfile.TemporaryDirectory() as directory:
            self.run_shell('''BASE="$2/app"; CANDIDATE="$BASE/releases/failed"; mkdir -p "$BASE/releases/old" "$CANDIDATE";
touch "$CANDIDATE/.managed"; ln -s "$BASE/releases/old" "$BASE/live";
if (trap cleanup EXIT; exit 1); then exit 1; fi;
[[ ! -e "$CANDIDATE" && -d "$BASE/releases/old" ]];
mkdir -p "$CANDIDATE"; touch "$CANDIDATE/.managed"; ln -sfn "$CANDIDATE" "$BASE/live";
if (trap cleanup EXIT; exit 1); then exit 1; fi;
[[ -d "$CANDIDATE" ]];''', directory)

    def test_uninstall_deletes_app_data_backups_and_manager(self):
        with tempfile.TemporaryDirectory() as directory:
            self.run_shell('''BASE="$2/app"; STATE="$2/state"; BACKUPS="$2/backups"; MANAGER="$2/manager";
UNIT="$2/unit"; SITE="$2/site"; ENABLED="$2/enabled"; HARDWARE_RULE="$2/hardware-rule"; PCSC_RULE="$2/pcsc-rule"; HOST_SOCKET="$2/host-socket"; HOST_UNIT="$2/host-unit"; QMI_SOCKET="$2/qmi-socket"; QMI_UNIT="$2/qmi-unit"; WIFI_SOCKET="$2/wifi-socket"; WIFI_UNIT="$2/wifi-unit"; WRAPPER="$2/command";
mkdir -p "$BASE"; touch "$UNIT"; calls="$2/calls";
preflight() { :; }; db_sql() { printf 1; }; app_sql() { printf f; };
confirm_uninstall() { printf 'confirmed\\n' >> "$calls"; };
snapshot() { printf 'snapshot\\n' >> "$calls"; SWITCHING=1; };
systemctl() { printf 'systemctl %s\\n' "$*" >> "$calls"; };
nginx() { :; }; getent() { return 1; };
runuser() { printf 'runuser %s\\n' "$*" >> "$calls"; };
remove_app_path() { printf 'remove %s\\n' "$1" >> "$calls"; };
rm() { printf 'rm %s\\n' "$*" >> "$calls"; };
uninstall;
grep -Fq 'dropdb --if-exists --force rykvo_voice' "$calls";
grep -Fq 'dropuser --if-exists rykvo_voice' "$calls";
for path in "$STATE" "$BACKUPS" "$BASE" "$MANAGER"; do grep -Fq "remove $path" "$calls"; done;
[[ $(head -n 1 "$calls") == confirmed && $SWITCHING == 0 ]];''', directory)

    def test_connected_tunnel_blocks_deletion(self):
        with tempfile.TemporaryDirectory() as directory:
            self.run_shell('''BASE="$2/app"; UNIT="$2/unit"; mkdir -p "$BASE";
preflight() { :; }; db_sql() { printf 1; };
app_sql() { if [[ "$*" == *to_regclass* ]]; then printf t; else printf panel.example.com; fi; };
confirm_uninstall() { touch "$2/confirmed"; };
if (uninstall); then exit 1; fi;
[[ -d "$BASE" && ! -f "$2/confirmed" ]];''', directory)


if __name__ == "__main__":
    unittest.main()
