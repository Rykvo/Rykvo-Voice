import os
from pathlib import Path
import socket
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[1]


@unittest.skipUnless(Path('/bin/bash').exists(), 'Linux Bash required')
class InstallerDrainTests(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory(prefix='rykvo-drain-test-')
        self.addCleanup(temp.cleanup)
        self.root = Path(temp.name)
        self.log = self.root / 'calls'
        self.control = self.root / 'control.sock'
        self.sock = socket.socket(socket.AF_UNIX)
        self.sock.bind(str(self.control))
        self.addCleanup(self.sock.close)
        # Execute the actual installer functions with only the socket path relocated.
        self.script = self.root / 'install.sh'
        self.script.write_text((ROOT / 'install.sh').read_text(encoding='utf-8').replace(
            '/run/rykvo-voice/control.sock', str(self.control)), encoding='utf-8')
        for name in ('live', 'candidate'):
            directory = self.root / 'app' / name
            directory.mkdir(parents=True)
            binary = directory / 'rykvo-auth'
            binary.write_text('#!/bin/bash\n'
                              f'printf "{name}:%s\\n" "$*" >> "$TEST_LOG"\n'
                              'if [[ "$*" == "-maintenance drain" ]]; then exit "${TEST_DRAIN_STATUS:-0}"; fi\n',
                              encoding='utf-8')
            binary.chmod(0o755)

    def run_shell(self, body, code=0, drain_status=0):
        prefix = '''source "$1"; BASE="$2/app"; WAS_ACTIVE=1;
STATE="$2/state"; BACKUPS="$2/backups"; MANAGER="$2/manager"; UNIT="$2/unit";
SITE="$2/site"; ENABLED="$2/enabled"; WRAPPER="$2/wrapper";
HARDWARE_RULE="$2/hardware"; PCSC_RULE="$2/pcsc";
HOST_SOCKET="$2/host-socket"; HOST_UNIT="$2/host-unit";
QMI_SOCKET="$2/qmi-socket"; QMI_UNIT="$2/qmi-unit";
WIFI_SOCKET="$2/wifi-socket"; WIFI_UNIT="$2/wifi-unit";
SIP_SOCKET="$2/sip-socket"; SIP_UNIT="$2/sip-unit"; SIP_BOOT="$2/sip-boot";
NETWORK_UNIT="$2/network-unit"; UPLINK_TMP="$2/uplink-tmp";
TEMP=; UPDATE_UNITS="$2/update-units";
'''
        result = subprocess.run(['bash', '-c', prefix + body, 'test', str(self.script), str(self.root)],
                                env=dict(os.environ, TEST_LOG=str(self.log), TEST_DRAIN_STATUS=str(drain_status)),
                                capture_output=True, text=True, timeout=15)
        self.assertEqual(result.returncode, code, result.stderr + result.stdout)
        self.assertNotIn('unbound variable', result.stderr)
        return self.log.read_text().splitlines() if self.log.exists() else []

    def test_uninstall_without_candidate_uses_live_client_once(self):
        calls = self.run_shell('unset CANDIDATE; drain_work; drain_work; [[ "$DRAINED" == 1 ]];')
        self.assertEqual(calls, ['live:-maintenance drain'])

    def test_empty_candidate_uses_live_client(self):
        self.assertEqual(self.run_shell('CANDIDATE=; drain_work;'), ['live:-maintenance drain'])

    def test_upgrade_uses_staged_client(self):
        calls = self.run_shell('CANDIDATE="$BASE/candidate"; drain_work;')
        self.assertEqual(calls, ['candidate:-maintenance drain'])

    def test_failed_drain_resumes_same_client_without_deleting(self):
        calls = self.run_shell('trap cleanup EXIT; drain_work; printf destructive >> "$TEST_LOG";',
                               code=1, drain_status=1)
        self.assertEqual(calls, ['live:-maintenance drain', 'live:-maintenance resume'])

    def test_later_failure_resumes_bound_client_not_new_candidate(self):
        calls = self.run_shell('trap cleanup EXIT; drain_work; CANDIDATE="$BASE/candidate"; exit 23;', code=23)
        self.assertEqual(calls, ['live:-maintenance drain', 'live:-maintenance resume'])

    def test_missing_client_aborts_before_drain_or_cleanup(self):
        (self.root / 'app/live/rykvo-auth').unlink()
        calls = self.run_shell('trap cleanup EXIT; drain_work; printf destructive >> "$TEST_LOG";', code=1)
        self.assertEqual(calls, [])

    def test_inactive_service_does_not_require_client(self):
        calls = self.run_shell('WAS_ACTIVE=0; unset CANDIDATE; drain_work; [[ "$DRAINED" == 0 ]];')
        self.assertEqual(calls, [])

    def test_legacy_busy_check_blocks_without_maintenance_socket(self):
        self.sock.close()
        self.control.unlink()
        calls = self.run_shell('app_sql() { printf 1; }; drain_work; printf destructive >> "$TEST_LOG";', code=1)
        self.assertEqual(calls, [])

    def uninstall_stubs(self):
        return '''
preflight() { :; }; db_sql() { printf 1; }; app_sql() { printf f; };
confirm_uninstall() { printf 'confirmed\\n' >> "$TEST_LOG"; };
snapshot() { drain_work; printf 'snapshot\\n' >> "$TEST_LOG"; SWITCHING=1; };
python3() { printf 'sip-stop\\n' >> "$TEST_LOG"; };
systemctl() { printf 'systemctl %s\\n' "$*" >> "$TEST_LOG"; };
udevadm() { :; }; nginx() { :; }; getent() { return 1; };
runuser() { printf 'runuser %s\\n' "$*" >> "$TEST_LOG"; };
remove_app_path() { printf 'remove %s\\n' "$1" >> "$TEST_LOG"; };
rm() { printf 'rm %s\\n' "$*" >> "$TEST_LOG"; };
'''

    def test_complete_uninstall_keeps_confirmation_drain_and_full_deletion(self):
        (self.root / 'app/live/sip-network.py').write_text('fixture', encoding='utf-8')
        calls = self.run_shell(self.uninstall_stubs() + 'uninstall;')
        self.assertEqual(calls[:4], ['confirmed', 'live:-maintenance drain', 'sip-stop', 'snapshot'])
        self.assertEqual(calls.count('live:-maintenance drain'), 1)
        self.assertIn('runuser -u postgres -- dropdb --if-exists --force rykvo_voice', calls)
        self.assertIn('runuser -u postgres -- dropuser --if-exists rykvo_voice', calls)
        for name in ('app', 'state', 'backups', 'manager'):
            self.assertIn('remove ' + str(self.root / name), calls)
        self.assertIn('remove /var/cache/rykvo-voice', calls)
        self.assertIn('remove /var/lib/rykvo-update', calls)

    def test_failed_drain_does_not_stop_sip_or_delete_data(self):
        (self.root / 'app/live/sip-network.py').write_text('fixture', encoding='utf-8')
        calls = self.run_shell(self.uninstall_stubs() + 'trap cleanup EXIT; uninstall;', code=1, drain_status=1)
        self.assertEqual(calls, ['confirmed', 'live:-maintenance drain', 'live:-maintenance resume'])

    def test_cancelled_uninstall_has_no_maintenance_or_destructive_actions(self):
        calls = self.run_shell(self.uninstall_stubs() + 'confirm_uninstall() { die "已取消"; }; uninstall;', code=1)
        self.assertEqual(calls, [])

    def test_fresh_stage_contains_network_runtime_without_old_files(self):
        source = self.root / 'source'
        for name in ('web', 'licenses', 'bin', 'deploy'):
            (source / name).mkdir(parents=True)
        for name in ('VERSION', 'UPDATE_EPOCH', 'manifest.json'):
            (source / name).write_text('fixture', encoding='utf-8')
        for name in ('rykvo-auth', 'cloudflared', 'sing-box'):
            (source / 'bin' / name).write_text('#!/bin/sh\nexit 0\n', encoding='utf-8')
        for name in ('network-control.py', 'network_runtime.py', 'host-settings.py',
                     'network-drivers.py', 'qmi-read.py', 'sip-network.py'):
            (source / 'deploy' / name).write_text('# fixture\n', encoding='utf-8')
        self.run_shell('SOURCE="$2/source"; stage_release; printf "%s" "$CANDIDATE" > "$2/staged";')
        staged = Path((self.root / 'staged').read_text())
        self.assertTrue((staged / 'network-control.py').is_file())
        self.assertTrue((staged / 'network_runtime.py').is_file())
        self.assertEqual((staged / 'sing-box').stat().st_mode & 0o777, 0o755)

    def rollback_network(self, previous):
        backup = self.root / 'previous'
        backup.mkdir()
        candidate = self.root / 'app/candidate'
        (candidate / 'network_runtime.py').write_text('# fixture', encoding='utf-8')
        (self.root / 'network-unit').write_text('new', encoding='utf-8')
        if previous:
            (backup / 'network-unit').write_text('old', encoding='utf-8')
            (backup / 'network-active').touch()
            (backup / 'network-enabled').touch()
        return self.run_shell('''
BACKUP="$2/previous"; CANDIDATE="$BASE/candidate";
systemctl() { printf 'systemctl %s\\n' "$*" >> "$TEST_LOG"; };
python3() { printf 'runtime %s\\n' "$*" >> "$TEST_LOG"; };
nginx() { :; }; udevadm() { :; };
rollback;
''')

    def test_rollback_to_111_cleans_new_routes_before_restoring_app(self):
        calls = self.rollback_network(False)
        cleanup = 'runtime -I ' + str(self.root / 'app/candidate/network_runtime.py') + ' cleanup'
        self.assertIn(cleanup, calls)
        self.assertLess(calls.index('systemctl stop rykvo-network.service'), calls.index(cleanup))
        self.assertLess(calls.index(cleanup), calls.index('systemctl start rykvo-auth'))
        self.assertFalse((self.root / 'network-unit').exists())

    def test_rollback_preserves_previous_network_service_and_policy(self):
        calls = self.rollback_network(True)
        self.assertFalse(any(c.startswith('runtime ') for c in calls))
        self.assertEqual((self.root / 'network-unit').read_text(), 'old')
        self.assertIn('systemctl enable rykvo-network.service', calls)
        self.assertLess(calls.index('systemctl start rykvo-network.service'),
                        calls.index('systemctl start rykvo-auth'))

    def test_uninstall_cleans_routes_before_deleting_network_state(self):
        (self.root / 'app/live/network_runtime.py').write_text('# fixture', encoding='utf-8')
        calls = self.run_shell(self.uninstall_stubs() + '''
python3() { printf 'runtime %s\\n' "$*" >> "$TEST_LOG"; };
uninstall;
''')
        cleanup = 'runtime -I ' + str(self.root / 'app/live/network_runtime.py') + ' cleanup'
        self.assertLess(calls.index('systemctl stop rykvo-network.service'), calls.index(cleanup))
        for name in ('rykvo-network', 'rykvo-network-route'):
            self.assertLess(calls.index(cleanup), calls.index('remove /var/lib/' + name))


if __name__ == '__main__':
    unittest.main()
