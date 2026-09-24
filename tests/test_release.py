import importlib.util
import io
import json
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
        wrapper = (ROOT / "deploy.ps1").read_text(encoding="utf-8")
        self.assertIn("unset token RYKVO_GITHUB_TOKEN", installer)
        self.assertNotIn("go build", installer)
        self.assertNotIn("git clone", installer)
        self.assertNotIn("StrictHostKeyChecking=no", wrapper)
        self.assertIn("Rykvo/Rykvo-Voice", wrapper)
        self.assertIn("127.0.0.1:8080", (ROOT / "deploy/nginx.conf").read_text(encoding="utf-8"))


@unittest.skipUnless(Path("/bin/bash").exists(), "Linux Bash required")
class InstallerTests(unittest.TestCase):
    def run_shell(self, script, directory):
        result = subprocess.run(["bash", "-c", 'source "$1/install.sh"; ' + script, "test", str(ROOT), directory], capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr + result.stdout)

    def test_removal_scope(self):
        with tempfile.TemporaryDirectory() as directory:
            self.run_shell('BASE="$2/app"; mkdir -p "$BASE/releases/test"; touch "$BASE/releases/test/file"; remove_app_path "$BASE/releases/test"; [[ ! -e "$BASE/releases/test" ]]; if (remove_app_path "$2/elsewhere"); then exit 1; fi', directory)

    def test_rollback_restores_files_and_link(self):
        with tempfile.TemporaryDirectory() as directory:
            self.run_shell('''BASE="$2/app"; BACKUP="$2/backup"; UNIT="$2/unit"; SITE="$2/site"; ENABLED="$2/enabled"; HARDWARE_RULE="$2/hardware-rule"; PCSC_RULE="$2/pcsc-rule";
mkdir -p "$BASE/releases/old" "$BASE/releases/new" "$BACKUP";
printf old-unit > "$BACKUP/unit"; printf old-site > "$BACKUP/nginx";
printf '%s' "$BASE/releases/old" > "$BACKUP/live-link";
printf '%s' "$SITE" > "$BACKUP/enabled-link";
printf new > "$UNIT"; printf new > "$SITE"; ln -s "$BASE/releases/new" "$BASE/live"; ln -s "$SITE" "$ENABLED";
systemctl() { :; }; nginx() { :; }; udevadm() { :; }; WAS_ACTIVE=1; SWITCHING=1; rollback;
[[ $(cat "$UNIT") == old-unit && $(cat "$SITE") == old-site ]];
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

    def test_uninstall_deletes_app_data_backups_and_manager(self):
        with tempfile.TemporaryDirectory() as directory:
            self.run_shell('''BASE="$2/app"; STATE="$2/state"; BACKUPS="$2/backups"; MANAGER="$2/manager";
UNIT="$2/unit"; SITE="$2/site"; ENABLED="$2/enabled"; HARDWARE_RULE="$2/hardware-rule"; PCSC_RULE="$2/pcsc-rule"; WRAPPER="$2/command";
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
