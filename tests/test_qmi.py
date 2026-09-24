import importlib.util
import subprocess
import unittest
from pathlib import Path
from unittest.mock import patch

ROOT = Path(__file__).resolve().parent.parent
spec = importlib.util.spec_from_file_location("qmi_read", ROOT / "deploy/qmi-read.py")
qmi = importlib.util.module_from_spec(spec)
spec.loader.exec_module(qmi)


class QMIReadTests(unittest.TestCase):
    def test_rejects_writes_and_extra_fields_before_execution(self):
        for request in (
            {"device": "/dev/cdc-wdm2", "command": "--dms-set-operating-mode=offline"},
            {"device": "/dev/cdc-wdm2", "command": "--nas-network-scan"},
            {"device": "/dev/cdc-wdm2", "command": "--dms-get-ids", "args": []},
            {"device": "/dev/cdc-wdm2", "command": []}, None,
        ):
            with patch.object(qmi.subprocess, "run") as run:
                self.assertEqual(qmi.query(request)["error"], "INVALID_REQUEST")
                run.assert_not_called()

    def test_rejects_paths_and_non_devices(self):
        for path in ("/dev/../etc/passwd", "/dev/null", "/dev/cdc-wdm0;id", "/tmp/cdc-wdm0", "--help", None):
            self.assertFalse(qmi.valid_device(path))

    def test_fixed_command_timeout_environment_and_redacted_errors(self):
        request = {"device": "/dev/cdc-wdm2", "command": "--dms-get-ids"}
        with patch.object(qmi, "valid_device", return_value=True), patch.object(qmi.subprocess, "run") as run:
            run.return_value = subprocess.CompletedProcess([], 0, b"test output")
            self.assertEqual(qmi.query(request), {"output": "test output"})
            args, kwargs = run.call_args
            self.assertEqual(args[0], ["/usr/bin/qmicli", "--device=/dev/cdc-wdm2", "--device-open-proxy", "--dms-get-ids"])
            self.assertEqual(kwargs["timeout"], 3)
            self.assertNotIn("shell", kwargs)
            self.assertEqual(kwargs["stderr"], subprocess.DEVNULL)
            run.return_value = subprocess.CompletedProcess([], 1, b"private card data")
            self.assertEqual(qmi.query(request), {"error": "QMI_READ_FAILED"})
            run.return_value = subprocess.CompletedProcess([], 0, b"x" * 65537)
            self.assertEqual(qmi.query(request), {"error": "QMI_READ_FAILED"})
            run.side_effect = subprocess.TimeoutExpired("qmicli", 3)
            self.assertEqual(qmi.query(request), {"error": "READ_TIMEOUT"})

    def test_socket_is_private_and_worker_has_no_capabilities(self):
        socket = (ROOT / "deploy/rykvo-qmi.socket").read_text()
        service = (ROOT / "deploy/rykvo-qmi@.service").read_text()
        self.assertIn("SocketMode=0600", socket)
        self.assertIn("SocketUser=rykvo_voice", socket)
        self.assertIn("MaxConnections=8", socket)
        self.assertIn("CapabilityBoundingSet=\n", service)
        self.assertIn("RestrictAddressFamilies=AF_UNIX", service)
        self.assertIn("RuntimeMaxSec=5", service)
        self.assertIn("User=rykvo_voice", (ROOT / "deploy/rykvo-auth.service").read_text())

    def test_helper_is_bundled_backed_up_and_removed_with_application(self):
        installer = (ROOT / "install.sh").read_text()
        builder = (ROOT / "deploy/build.py").read_text()
        for path in ("deploy/qmi-read.py", "deploy/rykvo-qmi.socket", "deploy/rykvo-qmi@.service"):
            self.assertIn(path, builder)
            self.assertIn(path, installer)
        self.assertIn('"qmi-socket:$QMI_SOCKET" "qmi-unit:$QMI_UNIT"', installer)
        self.assertIn('"$PCSC_RULE" "$QMI_SOCKET" "$QMI_UNIT"', installer.split("\nuninstall() {")[1])
