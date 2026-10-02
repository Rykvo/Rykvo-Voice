"""Prepare kernel USB networking without replacing network or modem management."""
import json
import hashlib
import sys
import tempfile
import uuid
import os
from pathlib import Path
import platform
import re
import subprocess

USB_DRIVERS = ("usbnet", "rndis_host", "cdc_ether", "cdc_ncm", "cdc_eem", "ipheth",
               "qmi_wwan", "cdc_mbim", "huawei_cdc_ncm", "r8152", "asix", "ax88179_178a")
TOOLS = ("kmod", "usbutils", "usb-modeswitch", "usb-modeswitch-data", "usbmuxd", "ipheth-utils", "libmbim-utils", "libqmi-utils")
SYS = Path("/sys")
RUN = Path("/run")
ETC = Path("/etc")
AUTO_PREFIX = "90-rykvo-uplink-"
AUTO_MARKER = "# Rykvo Voice automatic module uplink\n"
ENV = {"PATH": "/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL": "C.UTF-8", "DEBIAN_FRONTEND": "noninteractive", "NEEDRESTART_MODE": "l"}


def command(*args, timeout=30):
    try:
        return subprocess.run(args, env=ENV, capture_output=True, text=True, timeout=timeout)
    except (OSError, subprocess.TimeoutExpired) as error:
        return subprocess.CompletedProcess(args, 1, "", str(error))


def read(path):
    try:
        return path.read_text().strip()
    except OSError:
        return ""


def installed(package):
    result = command("dpkg-query", "-W", "-f=${Status}", package)
    return result.returncode == 0 and result.stdout.strip() == "install ok installed"


def available(package):
    return command("apt-cache", "show", "--no-all-versions", package).returncode == 0


def install_packages(packages, report):
    missing = sorted(set(packages) - {p for p in packages if installed(p)})
    if not missing:
        return
    if not report.get("refreshed"):
        report["refreshed"] = True
        if command("apt-get", "-o", "Acquire::Retries=1", "-o", "Acquire::http::Timeout=20", "-o", "Acquire::https::Timeout=20", "update", "-qq", timeout=120).returncode:
            report["issues"].append("软件源刷新失败；使用现有索引")
    candidates = [p for p in missing if available(p)]
    report["unavailable"].extend(p for p in missing if p not in candidates)
    if candidates:
        result = command("apt-get", "-o", "DPkg::Lock::Timeout=30", "-o", "Acquire::Retries=1", "-o", "Acquire::http::Timeout=20", "-o", "Acquire::https::Timeout=20", "install", "-y", "--no-install-recommends", *candidates, timeout=600)
        report["installed"].extend(p for p in candidates if installed(p))
        if result.returncode:
            report["issues"].append("网络组件安装未完成；请检查网络和软件源")


def network_modules():
    modules = set(USB_DRIVERS)
    for bus in ("usb", "pci"):
        for device in (SYS / "bus" / bus / "devices").glob("*"):
            if bus == "pci" and not read(device / "class").startswith("0x0200"):
                continue
            alias = read(device / "modalias")
            if not alias.startswith(bus + ":") or len(alias) > 256:
                continue
            for module in command("modprobe", "--resolve-alias", alias).stdout.split():
                if re.fullmatch(r"[A-Za-z0-9_]+", module):
                    filename = command("modinfo", "-F", "filename", module).stdout
                    if "/drivers/net/usb/" in filename or "/drivers/net/ethernet/" in filename:
                        modules.add(module)
    return sorted(modules)


def firmware_packages(system, modules):
    needed = set()
    for module in modules:
        firmware = command("modinfo", "-F", "firmware", module).stdout.strip()
        if not firmware:
            continue
        if system == "ubuntu":
            needed.add("linux-firmware")
        elif module.startswith(("r81", "rtl")):
            needed.add("firmware-realtek")
        elif module in ("bnx2", "bnx2x", "tg3"):
            needed.add("firmware-" + module if module != "tg3" else "firmware-misc-nonfree")
    return needed


def interfaces():
    result = []
    try:
        addresses = json.loads(command("ip", "-j", "address", "show").stdout)
    except (ValueError, TypeError):
        addresses = []
    for path in sorted((SYS / "class/net").glob("*")):
        if path.name == "lo":
            continue
        driver = (path / "device/driver").resolve().name if (path / "device/driver").exists() else ""
        configured = any(a.get("scope") == "global" for item in addresses if item.get("ifname") == path.name for a in item.get("addr_info", []))
        result.append({"name": path.name, "driver": driver, "state": read(path / "operstate"), "hasAddress": configured})
    return result


def prepare(system, kernel):
    report = {"installed": [], "unavailable": [], "missingDrivers": [], "issues": []}
    if system not in ("ubuntu", "debian") or not re.fullmatch(r"[A-Za-z0-9.+_-]{1,128}", kernel):
        raise ValueError("系统或内核不支持")
    install_packages(TOOLS, report)
    # Match the running kernel exactly; never upgrade or unload the active kernel.
    if system == "ubuntu" and any(command("modinfo", name).returncode for name in USB_DRIVERS):
        install_packages(["linux-modules-extra-" + kernel], report)
    modules = network_modules()
    install_packages(firmware_packages(system, modules), report)
    for module in modules:
        if command("modinfo", module).returncode or command("modprobe", "--use-blacklist", module).returncode or not (SYS / "module" / module).exists():
            report["missingDrivers"].append(module)
    report["interfaces"] = interfaces()
    report.pop("refreshed", None)
    return report



def fields(path):
    return dict(line.split("=", 1) for line in read(path).splitlines() if "=" in line and not line.startswith("#"))


def uplink_candidate(item):
    name = item.get("ifname", "")
    if not re.fullmatch(r"[A-Za-z0-9_][A-Za-z0-9_.-]{0,14}", name):
        return False
    root = SYS / "class/net" / name
    device = (root / "device").resolve()
    if not (root / "device").exists() or "/virtual/" in str(device):
        return False
    if read(root / "type") != "1" or (root / "wireless").exists() or (root / "master").exists():
        return False
    driver = (root / "device/driver").resolve().name
    if driver in ("qmi_wwan", "cdc_mbim", "mhi_net", "iosm", "t7xx", "huawei_cdc_ncm"):
        return False
    for parent in (device, *device.parents):
        vendor = read(parent / "idVendor")
        if vendor:
            if vendor.lower() in ("05c6", "2c7c", "2ca3", "1199", "1e0e", "1bc7"):
                return False
            break
    # Do not take over manually configured or already-running interfaces.
    if any(a.get("scope") == "global" for a in item.get("addr_info", [])):
        return False
    return re.fullmatch(r"[0-9a-f]{2}(:[0-9a-f]{2}){5}", item.get("address", "")) is not None


def auto_identity(item):
    device = (SYS / "class/net" / item["ifname"] / "device").resolve()
    return hashlib.sha256((str(device) + ":" + item["address"] + ":" + item["ifname"]).encode()).hexdigest()[:16]


def networkd_config(item, identity):
    # No rule selects this table: DHCP never takes the host's default/connected routes.
    table = 0x53000000 | int(identity[:6], 16)
    return AUTO_MARKER + f"""[Match]
Name={item['ifname']}
MACAddress={item['address']}

[Link]
RequiredForOnline=no

[Network]
DHCP=ipv4
IPv6AcceptRA=no
LinkLocalAddressing=no
DNSDefaultRoute=no
LLMNR=no
MulticastDNS=no

[DHCPv4]
RouteTable={table}
UseDNS=no
UseDomains=no
UseNTP=no
UseSIP=no
UseHostname=no
UseTimezone=no
SendHostname=no
"""


def safe_create(path, content):
    if any(p.is_symlink() for p in (path, *path.parents)):
        raise ValueError("unsafe network path")
    if path.exists():
        return read(path) == content.strip()
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, temporary = tempfile.mkstemp(prefix=".rykvo-", dir=path.parent)
    try:
        with os.fdopen(fd, "w") as stream:
            os.chmod(temporary, 0o644)
            stream.write(content)
            stream.flush(); os.fsync(stream.fileno())
        try:
            os.link(temporary, path)
        except FileExistsError:
            return False
    finally:
        Path(temporary).unlink(missing_ok=True)
    return True


def table_available(identity):
    table = 0x53000000 | int(identity[:6], 16)
    for family in ("-4", "-6"):
        for args in (("route", "show", "table", "all"), ("rule", "show")):
            result = command("ip", "-j", family, *args, timeout=3)
            if result.returncode:
                return False
            for entry in json.loads(result.stdout):
                if str(entry.get("table")) == str(table):
                    return False
    return True



def still_candidate(item):
    result = command("ip", "-j", "address", "show", "dev", item["ifname"], timeout=3)
    if result.returncode:
        return False
    values = json.loads(result.stdout)
    return (len(values) == 1 and values[0].get("ifindex") == item["ifindex"]
            and values[0].get("address") == item["address"] and uplink_candidate(values[0]))

def networkd_prepare(item):
    link = RUN / "systemd/netif/links" / str(item["ifindex"])
    info = fields(link)
    if not info or info.get("NETWORK_FILE") or info.get("ADMIN_STATE") != "unmanaged":
        return False
    # ifupdown configuration is separate from networkd; never compete with it.
    if re.search(r"^\s*(?:iface|auto|allow-hotplug)\s+" + re.escape(item["ifname"]) + r"(?:\s|$)",
                 read(ETC / "network/interfaces"), re.M):
        return False
    if any(p.is_file() for p in (ETC / "network/interfaces.d").glob("*")):
        return False
    identity = auto_identity(item)
    path = RUN / "systemd/network" / (AUTO_PREFIX + identity + ".network")
    content = networkd_config(item, identity)
    if path.exists():
        return False
    if not table_available(identity) or not still_candidate(item) or not safe_create(path, content):
        return False
    if command("networkctl", "reload", timeout=3).returncode:
        path.unlink(missing_ok=True)
        raise ValueError("NETWORK_AUTO_RELOAD_FAILED")
    # Reload applies only newly matched links; never restart the network manager.
    return True


def nm_prepare(item):
    name = item["ifname"]
    state = command("nmcli", "-g", "GENERAL.STATE", "device", "show", name, timeout=3)
    if state.returncode or not state.stdout.strip().startswith("30 "):
        return False
    profiles = command("nmcli", "-g", "CONNECTIONS.AVAILABLE-CONNECTIONS", "device", "show", name, timeout=3)
    if profiles.returncode or profiles.stdout.strip():
        return False
    identity = auto_identity(item)
    profile = str(uuid.uuid5(uuid.NAMESPACE_URL, "rykvo-uplink:" + identity))
    table = 0x53000000 | int(identity[:6], 16)
    # Temporary profile, no autoconnect across restart or takeover of user profiles.
    args = ("nmcli", "connection", "add", "save", "no", "type", "ethernet", "ifname", name,
            "con-name", "rykvo-uplink-" + identity, "connection.uuid", profile, "802-3-ethernet.mac-address", item["address"],
            "connection.autoconnect", "yes", "ipv4.method", "auto", "ipv4.route-table", str(table),
            "ipv4.ignore-auto-dns", "yes", "ipv4.dns-priority", "9999", "ipv6.method", "disabled")
    if not table_available(identity) or not still_candidate(item):
        return False
    record = RUN / "rykvo-hostname" / ("uplink-" + identity + ".json")
    if record.exists():
        if record.is_symlink() or json.loads(read(record)) != {"marker": AUTO_MARKER, "uuid": profile}:
            return False
        profiles = command("nmcli", "-g", "UUID", "connection", "show", timeout=3)
        if profiles.returncode or profile in profiles.stdout.splitlines():
            return False
        record.unlink()  # A failed/expired temporary profile may be recreated.
    if not safe_create(record, json.dumps({"marker": AUTO_MARKER, "uuid": profile})):
        return False
    result = command(*args, timeout=3)
    if result.returncode:
        raise ValueError("NETWORK_AUTO_PROFILE_FAILED")
    return True


def cleanup_uplinks():
    changed = False
    for path in (RUN / "systemd/network").glob(AUTO_PREFIX + "*.network"):
        if not path.is_symlink() and read(path).startswith(AUTO_MARKER.strip()):
            path.unlink()
            changed = True
    if changed and command("systemctl", "is-active", "--quiet", "systemd-networkd", timeout=3).returncode == 0 and command("networkctl", "reload", timeout=3).returncode:
        raise ValueError("NETWORK_AUTO_RELOAD_FAILED")
    for path in (RUN / "rykvo-hostname").glob("uplink-*.json"):
        if path.is_symlink():
            continue
        record = json.loads(read(path))
        if record.get("marker") != AUTO_MARKER:
            continue
        profile = str(uuid.UUID(record["uuid"]))
        if command("systemctl", "is-active", "--quiet", "NetworkManager", timeout=3).returncode == 0:
            profiles = command("nmcli", "-g", "UUID", "connection", "show", timeout=3)
            if profiles.returncode:
                raise ValueError("NETWORK_AUTO_CLEANUP_FAILED")
            if profile in profiles.stdout.splitlines():
                if command("nmcli", "connection", "delete", "uuid", profile, timeout=3).returncode:
                    raise ValueError("NETWORK_AUTO_CLEANUP_FAILED")
        path.unlink()


def prepare_uplinks():
    # One new link per pass bounds latency and avoids touching existing carrier sessions.
    result = command("ip", "-j", "address", "show", timeout=3)
    if result.returncode:
        raise ValueError("NETWORK_AUTO_INVENTORY_FAILED")
    items = json.loads(result.stdout)
    nm = command("systemctl", "is-active", "--quiet", "NetworkManager", timeout=3).returncode == 0
    networkd = command("systemctl", "is-active", "--quiet", "systemd-networkd", timeout=3).returncode == 0
    for item in items:
        if not uplink_candidate(item):
            continue
        # Do not guess manager ownership on systems running both.
        if nm:
            if nm_prepare(item):
                return {"prepared": item["ifname"]}
        elif networkd and networkd_prepare(item):
            return {"prepared": item["ifname"]}
    return {"prepared": ""}


def complete(report):
    return not any(report.get(key) for key in ("unavailable", "missingDrivers", "issues"))


if __name__ == "__main__":
    if os.geteuid() != 0:
        raise SystemExit("需要 root")
    if sys.argv[1:] == ["--cleanup-uplinks"]:
        cleanup_uplinks()
        raise SystemExit(0)
    if sys.argv[1:] == ["--uplinks"]:
        print(json.dumps(prepare_uplinks()))
        raise SystemExit(0)
    system = dict(line.split("=", 1) for line in Path("/etc/os-release").read_text().splitlines() if "=" in line)
    result = prepare(system.get("ID", "").strip('"'), platform.release())
    print(json.dumps(result, ensure_ascii=False))
    print("手机需开启 USB 共享；拨号型上网棒需 APN。未修改现有网络，未测试互联网连接。")
    raise SystemExit(0 if complete(result) else 1)
