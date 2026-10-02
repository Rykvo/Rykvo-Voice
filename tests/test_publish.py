import os
import subprocess
import tempfile
import textwrap
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
WORKFLOW = (ROOT / ".github/workflows/check.yml").read_text(encoding="utf-8")
PUBLISH = textwrap.dedent(WORKFLOW.split("- name: Publish downloadable build artifacts", 1)[1].split("run: |\n", 1)[1])


class PublishWorkflowTests(unittest.TestCase):
    def test_only_web_publish_triggers_formal_build(self):
        self.assertIn("release:\n    types: [published]", WORKFLOW)
        self.assertNotIn("  push:", WORKFLOW)
        self.assertIn("github.event_name == 'release' && startsWith(github.ref, 'refs/tags/release/v')", WORKFLOW)
        self.assertEqual(WORKFLOW.count("if: github.event_name == 'release' || (github.event_name == 'workflow_dispatch' && inputs.build)"), 2)

    def test_publish_does_not_recreate_or_overwrite(self):
        self.assertIn('gh release upload "$TAG"', PUBLISH)
        self.assertNotIn("gh release create", PUBLISH)
        self.assertNotIn("--clobber", PUBLISH)


@unittest.skipUnless(Path("/bin/bash").exists(), "Linux Bash required")
class PublishTests(unittest.TestCase):
    def run_publish(self, upload_status=0):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "VERSION").write_text("1.1.3")
            log = root / "calls"
            env = dict(os.environ, TAG="release/v1.1.3", CALL_LOG=str(log), UPLOAD_STATUS=str(upload_status))
            stub = '''gh() {
              printf '%s\\n' "$*" >> "$CALL_LOG"
              return "$UPLOAD_STATUS"
            }
            '''
            result = subprocess.run(["bash", "-e", "-c", stub + PUBLISH], cwd=root, env=env, capture_output=True, text=True)
            return result, log.read_text().splitlines()

    def test_web_release_receives_assets_without_recreation(self):
        result, calls = self.run_publish()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(len(calls), 1)
        self.assertTrue(calls[0].startswith("release upload release/v1.1.3 "))
        self.assertIn("RykvoVoice.1.1.3.rvu#RykvoVoice 1.1.3.rvu", calls[0])
        self.assertIn("dist/install.rvk", calls[0])

    def test_failed_upload_is_not_hidden_or_replaced(self):
        result, calls = self.run_publish(upload_status=1)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(len(calls), 1)


if __name__ == "__main__":
    unittest.main()
