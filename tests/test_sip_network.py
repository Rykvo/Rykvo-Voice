import base64
import copy
import importlib.util
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parent.parent
loader = importlib.util.spec_from_file_location('sip_network', ROOT / 'deploy/sip-network.py')
sip = importlib.util.module_from_spec(loader)
loader.loader.exec_module(sip)


def fixture():
    key = base64.b64encode(bytes(range(32))).decode()
    return {'version': 1, 'wireguard': {'interface': 'sip1', 'config': f'''[Interface]
PrivateKey = {key}
Address = 10.77.0.2/32
MTU = 1380
Table = off
PostUp = echo THIS_MUST_NOT_EXECUTE
[Peer]
PublicKey = {key}
PresharedKey = {key}
AllowedIPs = 0.0.0.0/0
Endpoint = sip.example.com:51820
PersistentKeepalive = 25
'''}, 'sip': {'server': 'sip.example.com', 'bindAddress': '10.77.0.2', 'publicAddress': '8.8.8.8',
             'portRange': {'start': 20000, 'end': 29999}, 'protocols': ['tcp', 'udp']},
            'routing': {'mode': 'source', 'preserveDefaultRoute': True, 'preserveDns': True}}


def spec():
    return sip.validate(fixture(), 'https://sip.example.com/api/connect', ['8.8.8.8'])


class SIPNetworkTests(unittest.TestCase):
    def test_contract_and_no_remote_code(self):
        value = spec()
        self.assertEqual(value['endpoint'], '8.8.8.8:51820')
        self.assertNotIn('PostUp', json.dumps(value))
        self.assertNotIn('THIS_MUST', json.dumps(value))

    def test_untrusted_addresses_and_config_rejected(self):
        for address in ['http://sip.example.com/api/connect', 'https://u:p@sip.example.com/api/connect',
                        'https://sip.example.com/other', 'https://sip.example.com/api/connect?q=1',
                        'https://sip.example.com:8443/api/connect', 'https://sip.example.com\\@host/api/connect']:
            with self.assertRaises((sip.Failure, ValueError)):
                sip.endpoint(address)
        for ip in ['127.0.0.1', '192.168.8.130', '169.254.169.254', '100.64.0.1', '224.0.0.1', '::1']:
            with self.assertRaises(sip.Failure): sip.public_address(ip)
        for mutate in [lambda x: x['wireguard'].update(interface='../../bad'),
                       lambda x: x['sip'].update(bindAddress='192.168.8.130'),
                       lambda x: x['sip'].update(publicAddress='1.1.1.1'),
                       lambda x: x['sip']['portRange'].update(start=80),
                       lambda x: x['sip']['portRange'].update(start=8080),
                       lambda x: x['routing'].update(preserveDns=False)]:
            value = fixture(); mutate(value)
            with self.assertRaises((sip.Failure, ValueError)):
                sip.validate(value, 'https://sip.example.com/api/connect', ['8.8.8.8'])

    def test_only_fresh_real_handshake_is_connected_and_no_keys_returned(self):
        value = spec()
        state = {'spec': value, 'enabled': True, 'startedAt': 900}
        with patch.object(sip, 'owned', return_value={'ifalias': sip.MARK}), patch.object(sip.time, 'time', return_value=1000):
            for last, expected in [(0, 'failed'), (700, 'failed'), (1001, 'failed'), (999, 'connected')]:
                with patch.object(sip, 'run', return_value=subprocess.CompletedProcess([], 0, value['publicKey'] + '\t' + str(last))):
                    result = sip.view(state)
                    self.assertEqual(result['state'], expected)
                    self.assertNotIn(value['privateKey'], json.dumps(result))
                    self.assertFalse(result['capabilities']['calls'])
        with patch.object(sip, 'owned', return_value=None):
            self.assertEqual(sip.view(state)['state'], 'failed')

    def test_installation_id_persists_and_disconnect_does_not_redeem(self):
        with tempfile.TemporaryDirectory() as directory, patch.object(sip, 'ROOT', Path('/unused')):
            sip.ROOT = Path(directory)
            with patch.object(sip, 'STATE', Path(directory) / 'state.json'), patch.object(sip, 'down') as down, patch.object(sip, 'redeem') as redeem:
                first = sip.load()['installationId']
                self.assertEqual(sip.load()['installationId'], first)
                self.assertEqual(sip.dispatch({'action': 'disconnect'})['state'], 'disconnected')
                redeem.assert_not_called()
                self.assertEqual((Path(directory) / 'state.json').stat().st_mode & 0o777, 0o600)

    def test_same_configuration_does_not_reset_a_live_interface(self):
        value = spec(); state = {'spec': value, 'enabled': True}
        with patch.object(sip, 'owned', return_value={'ifalias': sip.MARK}), patch.object(sip, 'view', return_value={}), patch.object(sip, 'up') as up, patch.object(sip, 'down') as down:
            sip.apply(state, value)
            up.assert_not_called(); down.assert_not_called()

    def test_failed_apply_restores_previous_binding(self):
        old = {'spec': spec(), 'enabled': True}
        new = dict(spec(), interface='sip2', bindAddress='10.77.0.3')
        with patch.object(sip, 'save') as save, patch.object(sip, 'down'), patch.object(sip, 'link', return_value=None), patch.object(sip, 'owned', return_value=None), patch.object(sip, 'up', side_effect=[sip.Failure('SIP_NETWORK_FAILED'), None]) as up:
            with self.assertRaises(sip.Failure): sip.apply(copy.deepcopy(old), new)
            self.assertEqual(up.call_args_list[-1].args[0], old['spec'])
            self.assertEqual(save.call_args.args[0], old)

    def test_routes_are_source_scoped_and_keys_are_not_process_arguments(self):
        commands = []
        def run(args, check=True, input=None):
            commands.append((args, input))
            if args[:3] == ['ip', '-j', '-4']: return subprocess.CompletedProcess(args, 0, '[]')
            return subprocess.CompletedProcess(args, 1 if '-S' in args else 0, '')
        with patch.object(sip, 'link', return_value=None), patch.object(sip, 'run', side_effect=run):
            sip.up(spec())
        self.assertTrue(any(args[:5] == ['ip','-4','route','add','default'] and 'table' in args for args, _ in commands))
        self.assertTrue(any('from' in args and '10.77.0.2/32' in args for args, _ in commands))
        for args, _ in commands:
            self.assertNotIn(spec()['privateKey'], args)
            self.assertNotIn('wg-quick', args)
            self.assertNotIn('resolv.conf', ' '.join(args))
        wire = next(data for args, data in commands if args[0] == 'wg')
        self.assertNotIn('PostUp', wire)

    def test_unknown_commands_and_extra_fields_rejected(self):
        for value in [{'action':'shell'}, {'action':'status','command':'anything'}, None]:
            self.assertEqual(sip.handle(value), {'error':'SIP_INVALID_REQUEST'})

    def test_logout_removes_keys_but_keeps_installation_identity(self):
        with tempfile.TemporaryDirectory() as directory, patch.object(sip, 'ROOT', Path(directory)), patch.object(sip, 'STATE', Path(directory) / 'state.json'), patch.object(sip, 'down') as down:
            state = sip.load()
            installation = state['installationId']
            state.update(spec=spec(), enabled=True, startedAt=1)
            sip.save(state)
            result = sip.dispatch({'action': 'logout'})
            self.assertEqual(result['state'], 'disconnected')
            self.assertFalse(result['configured'])
            self.assertEqual(result['address'], '')
            self.assertEqual(sip.load()['installationId'], installation)
            self.assertNotIn('spec', sip.load())
            self.assertNotIn(spec()['privateKey'], sip.STATE.read_text())
            down.assert_called_once_with(spec())
