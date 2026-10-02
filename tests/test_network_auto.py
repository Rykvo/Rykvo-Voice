import importlib.util
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parent.parent
spec = importlib.util.spec_from_file_location('network_auto', ROOT / 'deploy/network-drivers.py')
n = importlib.util.module_from_spec(spec)
spec.loader.exec_module(n)


class AutomaticNetworks(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        root = Path(self.tmp.name)
        for key in ('SYS', 'RUN', 'ETC'):
            target = root / key
            target.mkdir()
            p = patch.object(n, key, target); p.start(); self.addCleanup(p.stop)
        self.item = {'ifname': 'enx001122334455', 'ifindex': 26, 'address': '00:11:22:33:44:55', 'addr_info': []}
        self.link = n.SYS / 'class/net' / self.item['ifname']
        (self.link / 'device/driver').mkdir(parents=True)
        (self.link / 'type').write_text('1')
        self.state = n.RUN / 'systemd/netif/links/26'
        self.state.parent.mkdir(parents=True)
        self.state.write_text('ADMIN_STATE=unmanaged\n')
        self.calls = []

    def cmd(self, *args, **kwargs):
        self.calls.append(args)
        value = json.dumps([self.item]) if args[:5] == ('ip','-j','address','show','dev') else '[]' if args[0] == 'ip' else ''
        return subprocess.CompletedProcess(args, 0, value, '')

    def test_only_unconfigured_physical_ethernet(self):
        self.assertTrue(n.uplink_candidate(self.item))
        self.assertFalse(n.uplink_candidate({**self.item, 'ifname': '../etc'}))
        self.assertFalse(n.uplink_candidate({**self.item, 'address': 'invalid'}))
        self.assertFalse(n.uplink_candidate({**self.item, 'addr_info': [{'scope':'global'}]}))
        (self.link / 'wireless').mkdir()
        self.assertFalse(n.uplink_candidate(self.item))
        (self.link / 'wireless').rmdir()
        (self.link / 'master').mkdir()
        self.assertFalse(n.uplink_candidate(self.item))

    def test_modem_vendors_not_acquired(self):
        for vendor in ('05c6', '2c7c', '2ca3', '1199', '1e0e', '1bc7'):
            (self.link / 'device/idVendor').write_text(vendor)
            self.assertFalse(n.uplink_candidate(self.item))

    def test_networkd_dhcp_no_main_routes_or_dns_takeover(self):
        with patch.object(n, 'command', side_effect=self.cmd):
            self.assertTrue(n.networkd_prepare(self.item))
            self.assertFalse(n.networkd_prepare(self.item))
        path, = (n.RUN / 'systemd/network').glob('*.network')
        value = path.read_text()
        self.assertIn('Name=' + self.item['ifname'], value)
        self.assertIn('MACAddress=' + self.item['address'], value)
        self.assertIn('UseDNS=no', value)
        self.assertIn('DNSDefaultRoute=no', value)
        self.assertIn('UseHostname=no', value)
        self.assertIn('RequiredForOnline=no', value)
        table = int(value.split('RouteTable=')[1].splitlines()[0])
        self.assertEqual(table >> 24, 0x53)
        self.assertEqual(sum(c == ('networkctl', 'reload') for c in self.calls), 1)
        self.assertFalse(any('restart' in c or 'reconfigure' in c for c in self.calls))

    def test_existing_admin_network_never_replaced(self):
        for value in ('ADMIN_STATE=configured\n', 'ADMIN_STATE=unmanaged\nNETWORK_FILE=/etc/systemd/network/admin.network\n', ''):
            self.state.write_text(value)
            with patch.object(n, 'command') as call:
                self.assertFalse(n.networkd_prepare(self.item))
            call.assert_not_called()

    def test_ifupdown_preserved(self):
        path = n.ETC / 'network/interfaces'; path.parent.mkdir()
        path.write_text('iface ' + self.item['ifname'] + ' inet static\n')
        with patch.object(n, 'command') as call:
            self.assertFalse(n.networkd_prepare(self.item))
        call.assert_not_called()

    def test_occupied_route_table_rejected(self):
        table = 0x53000000 | int(n.auto_identity(self.item)[:6], 16)
        with patch.object(n, 'command', return_value=subprocess.CompletedProcess([], 0, json.dumps([{'table':table}]), '')):
            self.assertFalse(n.networkd_prepare(self.item))
        self.assertFalse((n.RUN / 'systemd/network').exists())

    def test_networkd_reload_failure_removes_new_file(self):
        def cmd(*args, **kwargs):
            if args[0] == 'networkctl': return subprocess.CompletedProcess(args, 1, '', 'failure')
            return self.cmd(*args, **kwargs)
        with patch.object(n, 'command', side_effect=cmd):
            with self.assertRaisesRegex(ValueError, 'RELOAD_FAILED'):
                n.networkd_prepare(self.item)
        self.assertEqual(list((n.RUN / 'systemd/network').glob('*.network')), [])

    def test_network_manager_preserves_unmanaged_and_existing_profiles(self):
        with patch.object(n, 'command', return_value=subprocess.CompletedProcess([],0,'10 (unmanaged)\n','')) as call:
            self.assertFalse(n.nm_prepare(self.item))
            self.assertEqual(call.call_count, 1)
        results = [subprocess.CompletedProcess([],0,'30 (disconnected)\n',''), subprocess.CompletedProcess([],0,'existing-profile','')]
        with patch.object(n, 'command', side_effect=results):
            self.assertFalse(n.nm_prepare(self.item))

    def test_nm_creates_only_volatile_isolated_profile(self):
        def cmd(*args, **kwargs):
            if 'GENERAL.STATE' in args: return subprocess.CompletedProcess(args,0,'30 (disconnected)\n','')
            return self.cmd(*args, **kwargs)
        with patch.object(n, 'command', side_effect=cmd):
            self.assertTrue(n.nm_prepare(self.item))
        create, = [c for c in self.calls if c[:3] == ('nmcli','connection','add')]
        self.assertEqual(create[create.index('save')+1], 'no')
        self.assertEqual(create[create.index('ipv4.ignore-auto-dns')+1], 'yes')
        self.assertGreater(int(create[create.index('ipv4.route-table')+1]), 255)
        self.assertTrue(list((n.RUN / 'rykvo-hostname').glob('uplink-*.json')))

    def test_nm_recovers_failed_temporary_profile_without_duplicates(self):
        failed = True
        def cmd(*args, **kwargs):
            if 'GENERAL.STATE' in args: return subprocess.CompletedProcess(args,0,'30 (disconnected)\n','')
            if args[:3] == ('nmcli','connection','add') and failed:
                return subprocess.CompletedProcess(args,1,'','temporary failure')
            return self.cmd(*args, **kwargs)
        with patch.object(n, 'command', side_effect=cmd):
            with self.assertRaises(ValueError): n.nm_prepare(self.item)
            failed = False
            self.assertTrue(n.nm_prepare(self.item))
        record, = (n.RUN / 'rykvo-hostname').glob('uplink-*.json')
        profile = json.loads(record.read_text())['uuid']
        with patch.object(n, 'command', side_effect=lambda *a, **k: subprocess.CompletedProcess(a,0,profile,'') if 'UUID' in a else cmd(*a, **k)):
            self.assertFalse(n.nm_prepare(self.item))

    def test_both_managers_never_claim_networkd_link(self):
        def cmd(*args, **kwargs):
            if args[0] == 'ip': return subprocess.CompletedProcess(args,0,json.dumps([self.item]),'')
            return self.cmd(*args, **kwargs)
        with patch.object(n, 'command', side_effect=cmd), patch.object(n, 'nm_prepare', return_value=False), patch.object(n, 'networkd_prepare') as nd:
            self.assertEqual(n.prepare_uplinks(), {'prepared':''})
        nd.assert_not_called()


    def test_link_replaced_between_snapshot_and_apply_is_not_touched(self):
        def command(*args, **kwargs):
            if args[:5] == ('ip','-j','address','show','dev'):
                return subprocess.CompletedProcess(args,0,json.dumps([{**self.item,'ifindex':99}]),'')
            return self.cmd(*args, **kwargs)
        with patch.object(n, 'command', side_effect=command):
            self.assertFalse(n.networkd_prepare(self.item))
        self.assertFalse((n.RUN / 'systemd/network').exists())

    def test_address_acquired_during_probe_is_not_touched(self):
        updated = {**self.item,'addr_info':[{'scope':'global','local':'192.168.8.130'}]}
        with patch.object(n, 'command', return_value=subprocess.CompletedProcess([],0,json.dumps([updated]),'')):
            self.assertFalse(n.still_candidate(self.item))

    def test_install_handles_boot_and_lifecycle(self):
        script = (ROOT / 'install.sh').read_text()
        self.assertIn('d /run/systemd/network 0755 root root -', script)
        self.assertIn('uplink-tmp) path=$UPLINK_TMP', script)
        self.assertIn('uplink-tmp:$UPLINK_TMP', script)
        self.assertIn('"$HOST_UNIT" "$UPLINK_TMP"', script)
        self.assertIn('"$CANDIDATE/network-drivers.py"', script)

    def test_cleanup_only_owned_config(self):
        folder = n.RUN / 'systemd/network'; folder.mkdir(parents=True)
        owned = folder / (n.AUTO_PREFIX + 'fixture.network'); owned.write_text(n.AUTO_MARKER + '[Match]\nName=eth9\n')
        foreign = folder / (n.AUTO_PREFIX + 'admin.network'); foreign.write_text('# Administrator\n')
        with patch.object(n, 'command', side_effect=self.cmd): n.cleanup_uplinks()
        self.assertFalse(owned.exists()); self.assertTrue(foreign.exists())


if __name__ == '__main__': unittest.main()
