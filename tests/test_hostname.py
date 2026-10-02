import importlib.util
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parent.parent
spec = importlib.util.spec_from_file_location("host_settings", ROOT / "deploy/host-settings.py")
host = importlib.util.module_from_spec(spec)
spec.loader.exec_module(host)


class HostnameTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        for key in ("ETC", "RUN", "SYS", "PROC"):
            path = self.root / key.lower()
            patcher = patch.object(host, key, path)
            patcher.start()
            self.addCleanup(patcher.stop)
        files = {
            "etc/hostname": "rykvo\n",
            "etc/hosts": "127.0.0.1 localhost\n127.0.1.1 rykvo alias # keep\n::1 localhost ip6-localhost\n192.0.2.1 other\n",
            "proc/net/route": "Iface Destination Gateway Flags RefCnt Use Metric Mask\nenp2s0 00000000 0108A8C0 0003 0 0 100 00000000\n",
            "sys/class/net/enp2s0/ifindex": "2",
            "run/systemd/netif/links/2": "NETWORK_FILE=/run/systemd/network/10-netplan-enp2s0.network\n",
        }
        for name, content in files.items():
            path = self.root / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(content)
        for directory in ("sys/class/net/enp2s0/device", "etc/systemd/network", "etc/cloud/cloud.cfg.d", "run/rykvo-hostname"):
            (self.root / directory).mkdir(parents=True, exist_ok=True)
        patcher = patch.object(host.socket, "gethostname", side_effect=lambda: (host.ETC / "hostname").read_text().strip())
        patcher.start()
        self.addCleanup(patcher.stop)
        self.request = {"action": "set", "hostname": "rykvo-02", "expected": "rykvo"}

    def set_name(self, name):
        (host.ETC / "hostname").write_text(name + "\n")

    def test_read_and_noop_never_change_system_or_files(self):
        with patch.object(host, "set_system_name") as setter, patch.object(host, "write") as writer:
            self.assertEqual(host.query({"action": "get"})["data"], {"hostname": "rykvo", "editable": True})
            self.assertEqual(host.query(self.request | {"hostname": "rykvo"})["data"]["hostname"], "rykvo")
            setter.assert_not_called()
            writer.assert_not_called()

    def test_validation_and_exact_contract(self):
        for value in ("", "-a", "a-", "a.b", "abc\n", "a;b", "主机", "İ01", "123", "localhost", "LOCALHOST", "LocalHost", "a" * 64, None, [], "$(reboot)"):
            self.assertFalse(host.valid_name(value), value)
            self.assertEqual(host.query(self.request | {"hostname": value})["error"], "INVALID_REQUEST")
        for value in ("a", "A", "a-1", "1-host", "A01", "a01", "Host-01", "a" * 63, "A" * 63):
            self.assertTrue(host.valid_name(value))
        for request in (None, [], {"action": "get", "extra": True}, self.request | {"path": "/etc/passwd"}):
            self.assertEqual(host.query(request)["error"], "INVALID_REQUEST")

    def test_save_persists_dhcp_and_cloud_without_restarting_network(self):
        before = (host.ETC / "hosts").read_text()
        with patch.object(host, "set_system_name", side_effect=self.set_name) as setter:
            result = host.query(self.request)
        self.assertEqual(result["data"]["hostname"], "rykvo-02")
        setter.assert_called_once_with("rykvo-02")
        hosts = (host.ETC / "hosts").read_text()
        self.assertEqual(hosts, before.replace("127.0.1.1 rykvo", "127.0.1.1\trykvo-02"))
        self.assertEqual(host.dhcp_targets()[0].read_text(), "[DHCPv4]\nSendHostname=yes\nHostname=rykvo-02\n[DHCPv6]\nSendHostname=yes\nHostname=rykvo-02\n")
        self.assertIn("preserve_hostname: true", (host.ETC / "cloud/cloud.cfg.d/99-rykvo-hostname.cfg").read_text())

    def test_conflict_and_unsupported_are_read_only(self):
        with patch.object(host, "write") as writer:
            self.assertEqual(host.query(self.request | {"expected": "older"})["error"], "HOSTNAME_CONFLICT")
            (host.RUN / "systemd/netif/links/2").unlink()
            self.assertFalse(host.query({"action": "get"})["data"]["editable"])
            self.assertEqual(host.query(self.request)["error"], "HOST_NETWORK_UNSUPPORTED")
            writer.assert_not_called()

    def test_mixed_case_and_case_only_renames_preserve_spelling(self):
        with patch.object(host, "set_system_name", side_effect=self.set_name) as setter:
            old = "rykvo"
            for name in ("A01", "a01", "Host-01", "HOST-01"):
                result = host.query(self.request | {"hostname": name, "expected": old})
                self.assertEqual(result["data"]["hostname"], name)
                self.assertEqual(host.current_name(), name)
                self.assertEqual(host.query({"action": "get"})["data"]["hostname"], name)
                setter.assert_called_with(name)
                self.assertIn("127.0.1.1\t" + name + " alias # keep", (host.ETC / "hosts").read_text())
                self.assertEqual(host.dhcp_targets()[0].read_text().count("Hostname=" + name + "\n"), 2)
                self.assertEqual(host.query(self.request | {"hostname": "new", "expected": old})["error"], "HOSTNAME_CONFLICT")
                old = name
        self.assertEqual(host.hosts_content(b"127.0.1.1 a01 A01 alias ALIAS\n", "a01", "A01"), b"127.0.1.1\tA01 alias ALIAS\n")

    def test_case_only_failed_save_restores_original_spelling(self):
        self.set_name("A01")
        original = (host.ETC / "hosts").read_bytes()
        def fail(name):
            self.set_name(name)
            if name == "a01":
                raise subprocess.TimeoutExpired("hostnamectl", 2)
        with patch.object(host, "set_system_name", side_effect=fail):
            result = host.query(self.request | {"hostname": "a01", "expected": "A01"})
        self.assertEqual(result["error"], "HOSTNAME_SAVE_FAILED")
        self.assertEqual(host.current_name(), "A01")
        self.assertEqual((host.ETC / "hosts").read_bytes(), original)

    def test_rollback_restores_files_and_hostname_after_partial_set(self):
        original = (host.ETC / "hosts").read_bytes()
        target = host.dhcp_targets()[0]
        def fail(name):
            self.set_name(name)
            if name == "rykvo-02":
                raise subprocess.TimeoutExpired("hostnamectl", 2)
        with patch.object(host, "set_system_name", side_effect=fail):
            self.assertEqual(host.query(self.request)["error"], "HOSTNAME_SAVE_FAILED")
        self.assertEqual(host.current_name(), "rykvo")
        self.assertEqual((host.ETC / "hosts").read_bytes(), original)
        self.assertFalse(target.exists())
        self.assertFalse((host.ETC / "cloud/cloud.cfg.d/99-rykvo-hostname.cfg").exists())

    def test_failure_to_restore_is_not_reported_as_success(self):
        with patch.object(host, "set_system_name", side_effect=OSError("denied")):
            self.assertEqual(host.query(self.request)["error"], "HOSTNAME_ROLLBACK_FAILED")

    def test_rejects_symlink_and_preserves_arbitrary_hosts(self):
        target = host.dhcp_targets()[0]
        target.parent.mkdir()
        target.symlink_to(host.ETC / "hostname")
        with patch.object(host, "set_system_name") as setter:
            with self.assertRaises(ValueError):
                host.query(self.request)
            setter.assert_not_called()
        self.assertEqual(host.current_name(), "rykvo")
        self.assertEqual(host.hosts_content(b"127.0.0.1 localhost\n", "old", "new"), b"127.0.0.1 localhost\n127.0.1.1\tnew\n")

    def test_fixed_command_and_bounded_socket_security(self):
        with patch.object(host.subprocess, "run") as run:
            host.set_system_name("Host-01")
            args, kwargs = run.call_args
            self.assertEqual(args[0], ["/usr/bin/hostnamectl", "--static", "--transient", "set-hostname", "--", "Host-01"])
            self.assertEqual(kwargs["timeout"], 2)
            self.assertNotIn("shell", kwargs)
        service = (ROOT / "deploy/rykvo-host@.service").read_text()
        self.assertIn("CapabilityBoundingSet=\n", service)
        self.assertIn("RestrictAddressFamilies=AF_UNIX", service)
        self.assertNotIn("CAP_SYS_ADMIN", service)
        self.assertIn("SocketMode=0600", (ROOT / "deploy/rykvo-host.socket").read_text())
        for file in ("deploy/host-settings.py", "deploy/rykvo-host.socket", "deploy/rykvo-host@.service"):
            self.assertIn(file, (ROOT / "deploy/build.py").read_text())
            self.assertIn(file, (ROOT / "install.sh").read_text())

    def test_concurrent_saves_do_not_overlap(self):
        with (host.RUN / "rykvo-hostname/lock").open("a") as lock:
            host.fcntl.flock(lock, host.fcntl.LOCK_EX | host.fcntl.LOCK_NB)
            with patch.object(host, "update") as update:
                self.assertEqual(host.query(self.request)["error"], "HOSTNAME_BUSY")
                update.assert_not_called()
