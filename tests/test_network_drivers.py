import importlib.util
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parent.parent
spec = importlib.util.spec_from_file_location("network", ROOT / "deploy/network-drivers.py")
n = importlib.util.module_from_spec(spec)
spec.loader.exec_module(n)


class NetworkDriverTests(unittest.TestCase):
    def test_summary_requires_all_components_ready(self):
        report = {"installed": [], "unavailable": [], "missingDrivers": [], "issues": []}
        self.assertTrue(n.complete(report))
        for key in ("unavailable", "missingDrivers", "issues"):
            self.assertFalse(n.complete({**report, key: ["missing"]}))

    def test_existing_packages_never_reinstall_or_refresh(self):
        report = {"installed": [], "unavailable": [], "issues": []}
        with patch.object(n, "installed", return_value=True), patch.object(n, "command") as call:
            n.install_packages(n.TOOLS, report)
        call.assert_not_called()

    def test_missing_packages_offline_and_unavailable_are_explicit(self):
        report = {"installed": [], "unavailable": [], "issues": []}
        with patch.object(n, "installed", return_value=False), patch.object(n, "available", side_effect=lambda p: p == "kmod"), patch.object(n, "command", return_value=subprocess.CompletedProcess([], 1, "", "offline")) as call:
            n.install_packages(["kmod", "usbutils"], report)
            n.install_packages(["usbutils"], report)
        self.assertEqual(len([c for c in call.call_args_list if "update" in c.args]), 1)
        self.assertEqual(report["installed"], [])
        self.assertIn("usbutils", report["unavailable"])
        self.assertEqual(len(report["issues"]), 2)

    def test_ubuntu_installs_only_matching_kernel_not_replacement_kernel(self):
        packages = []
        with patch.object(n, "install_packages", side_effect=lambda p, r: packages.extend(p)), patch.object(n, "command", return_value=subprocess.CompletedProcess([], 1, "", "missing")) as call, patch.object(n, "interfaces", return_value=[]), patch.object(n, "network_modules", return_value=list(n.USB_DRIVERS)):
            report = n.prepare("ubuntu", "6.8.0-85-generic")
        self.assertIn("linux-modules-extra-6.8.0-85-generic", packages)
        self.assertNotIn("linux-generic", packages)
        self.assertEqual(report["missingDrivers"], list(n.USB_DRIVERS))
        self.assertFalse(any(c.args[0] in ("systemctl", "networkctl", "nmcli", "ip") for c in call.call_args_list))
        self.assertFalse(any("-r" in c.args or "--remove" in c.args for c in call.call_args_list))

    def test_debian_and_bad_kernel_never_install_ubuntu_packages(self):
        packages = []
        with patch.object(n, "install_packages", side_effect=lambda p, r: packages.extend(p)), patch.object(n, "command", return_value=subprocess.CompletedProcess([], 0, "", "")), patch.object(n, "interfaces", return_value=[]), patch.object(n, "network_modules", return_value=[]):
            n.prepare("debian", "6.12.0-amd64")
            with self.assertRaises(ValueError): n.prepare("ubuntu", "$(touch /tmp/bad)")
        self.assertFalse(any(p.startswith("linux-modules") for p in packages))
        self.assertNotIn("modemmanager", packages)
        self.assertNotIn("network-manager", packages)

    def test_only_network_modaliases_load_and_no_wireless_login(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            usb = root / "bus/usb/devices/usb-fixture"
            usb.mkdir(parents=True); (usb / "modalias").write_text("usb:vTEST")
            def command(*args, **kwargs):
                text = "cdc_ether usbhid" if args[0] == "modprobe" else "/kernel/drivers/net/usb/cdc_ether.ko" if args[-1] == "cdc_ether" else "/kernel/drivers/hid/usbhid.ko"
                return subprocess.CompletedProcess(args, 0, text, "")
            with patch.object(n, "SYS", root), patch.object(n, "command", side_effect=command):
                self.assertIn("cdc_ether", n.network_modules())
                self.assertNotIn("usbhid", n.network_modules())

    def test_firmware_packages_match_distribution(self):
        with patch.object(n, "command", return_value=subprocess.CompletedProcess([], 0, "rtl_nic/rtl8153a-3.fw", "")):
            self.assertEqual(n.firmware_packages("ubuntu", ["r8152"]), {"linux-firmware"})
            self.assertEqual(n.firmware_packages("debian", ["r8152"]), {"firmware-realtek"})


if __name__ == "__main__": unittest.main()
