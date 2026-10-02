import json
import io
import tempfile
import unittest
from pathlib import Path
import sys
sys.path.insert(0, str(Path(__file__).resolve().parent.parent / 'deploy'))
from unittest.mock import patch
import importlib.util
_spec = importlib.util.spec_from_file_location('network_control', Path(__file__).resolve().parent.parent / 'deploy/network-control.py')
service = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(service)

LINK = 'vless://11111111-1111-4111-8111-111111111111@198.51.100.1:443?type=tcp&security=none#Test'


class NetworkControlTests(unittest.TestCase):
    def request(self, body=None, headers=None, query='', authenticated=True, modules=None, writes=None):
        h = object.__new__(service.Handler)
        h.path = '/api/settings/network-control' + query
        h.command = 'POST' if body is not None else 'GET'
        raw = json.dumps(body).encode()
        h.rfile = io.BytesIO(raw)
        h.headers = {'Host': '192.0.2.1', 'Origin': 'http://192.0.2.1', 'X-CSRF-Token': 'csrf',
                     'Content-Length': str(len(raw)), 'Content-Type': 'application/json', **(headers or {})}
        network = {'id': 'a' * 32, 'name': 'eth1', 'state': 'configured'}
        def upstream(path, method='GET', body=None):
            if not authenticated:
                raise service.HTTPError('', 401, '', {}, None)
            if method=='PUT':
                if writes is not None: writes.append(body)
                return None
            return {'csrfToken': 'csrf'} if path == '/session' else {'networks': [network], 'modules': modules or [], 'revision':5}
        h.upstream = upstream
        h.reply = lambda status, data: setattr(h, 'result', (status, data))
        h.dispatch()
        return h.result

    def test_csrf_and_origin_required(self):
        for headers, error in [({'X-CSRF-Token': ''}, 'CSRF_REJECTED'),
                               ({'Origin': 'http://attacker.test'}, 'ORIGIN_REJECTED'),
                               ({'Origin': ''}, 'ORIGIN_REJECTED')]:
            with self.subTest(error=error):
                status, data = self.request({'action': 'save'}, headers)
                self.assertEqual(status, 403)
                self.assertEqual(data['error']['code'], error)

    def test_traffic_actions_rejected(self):
        status, data = self.request({'id': 'a' * 32, 'action': 'enable'})
        self.assertEqual(status, 409)
        self.assertEqual(data['error']['code'], 'TRAFFIC_CONTROL_NOT_READY')

    def test_primary_switch_pins_defaults_once_and_keeps_explicit_bindings(self):
        from unittest.mock import MagicMock
        import threading
        routing=MagicMock()
        routing.lock=threading.RLock()
        routing.inventory.return_value={'revision':3,'primary':'b'*32}
        routing.view.return_value={'primary':'b'*32}
        routing.change.return_value=4
        modules=[{'id':'1','network':'','busy':False},{'id':'2','network':'b'*32,'busy':False},{'id':'3','network':'a'*32,'busy':False}]
        writes=[]
        with patch.object(service,'ROUTING',routing):
            status,_=self.request({'id':'a'*32,'action':'primary','enabled':True,'revision':3},modules=modules,writes=writes)
        self.assertEqual(status,200)
        self.assertEqual(writes,[{'id':'b'*32,'revision':5,'modules':['1','2']}])
        routing.change.assert_called_once_with('a'*32,'primary',True,3)

    def test_busy_default_module_blocks_route_change(self):
        from unittest.mock import MagicMock
        import threading
        routing=MagicMock();routing.lock=threading.RLock()
        routing.inventory.return_value={'revision':3,'primary':'a'*32}
        with patch.object(service,'ROUTING',routing):
            status,data=self.request({'id':'a'*32,'action':'primary','enabled':True,'revision':3},modules=[{'id':'1','network':'','busy':True}])
        self.assertEqual(status,409)
        self.assertEqual(data['error']['code'],'DEVICE_BUSY')
        routing.change.assert_not_called()

    def test_conflict_does_not_replace_node(self):
        with tempfile.TemporaryDirectory() as tmp, patch.object(service, 'BASE', Path(tmp)):
            status, _ = self.request({'id': 'a' * 32, 'action': 'save', 'revision': 1, 'link': LINK})
            self.assertEqual(status, 200)
            status, _ = self.request({'id': 'a' * 32, 'action': 'remove', 'revision': 1})
            self.assertEqual(status, 409)
            self.assertEqual(len(service.read_state()['nodes']), 1)

    def test_parse(self):
        n = service.parse_link(LINK)
        self.assertEqual(n['label'], 'Test')
        self.assertEqual(n['port'], 443)
        self.assertEqual(n['security'], 'none')

    def test_invalid_and_unsupported(self):
        for value in ('http://example.com', LINK.replace(':443', ':99999'), LINK.replace('11111111-', 'bad-', 1),
                      LINK.replace('type=tcp', 'type=ws'), LINK.replace('security=none', 'security=reality'),
                      LINK.replace('type=tcp', 'type=tcp&type=ws'), LINK.replace('security=none', 'security=tls&allowInsecure=1'),
                      LINK.replace('type=tcp', 'type=tcp&unexpected=1')):
            with self.subTest(value=value), self.assertRaises(service.Fault):
                service.parse_link(value)

    def test_redaction(self):
        n = service.parse_link(LINK)
        output = json.dumps(service.public_node(n))
        self.assertNotIn(n['uuid'], output)
        self.assertNotIn(n['server'], output)
        self.assertFalse(service.public_node(n)['enabled'])

    def test_link_round_trip(self):
        for link in (LINK, LINK.replace('198.51.100.1', '[2001:db8::1]')
                     .replace('security=none', 'security=tls&sni=node.example')
                     .replace('#Test', '#%E8%8A%82%E7%82%B9%20%26%20%23')):
            node = service.parse_link(link)
            self.assertEqual(service.parse_link(service.node_link(node)), node)
        self.assertEqual(service.node_link(None), '')

    def test_detail_returns_link_only_to_authenticated_admin(self):
        with tempfile.TemporaryDirectory() as tmp, patch.object(service, 'BASE', Path(tmp)):
            self.request({'id': 'a' * 32, 'action': 'save', 'revision': 1, 'link': LINK})
            status, data = self.request(query='?node=' + 'a' * 32)
            self.assertEqual(status, 200)
            self.assertEqual(data['data']['link'], LINK)
            self.assertEqual(data['data']['revision'], 2)
            status, data = self.request()
            self.assertNotIn('11111111-1111', json.dumps(data))
            status, data = self.request(query='?node=' + 'a' * 32, authenticated=False)
            self.assertEqual(status, 401)
            self.assertNotIn('11111111-1111', json.dumps(data))

    def test_unknown_and_duplicate_detail_network_rejected(self):
        with tempfile.TemporaryDirectory() as tmp, patch.object(service, 'BASE', Path(tmp)):
            for query in ('?node=', '?node=unknown', '?node=' + 'a' * 32 + '&node=' + 'a' * 32):
                status, _ = self.request(query=query)
                self.assertEqual(status, 400)
            status, data = self.request(query='?node=' + 'a' * 32)
            self.assertEqual(status, 200)
            self.assertEqual(data['data']['link'], '')

    def test_no_fake_or_stale_exit(self):
        import time
        result = {'state': 'passed', 'exitIP': '198.51.100.5', 'finishedAt': time.time() - 400}
        self.assertEqual(service.public_node({}, result)['testExitIP'], '')
        result['finishedAt'] = time.time()
        self.assertEqual(service.public_node({}, result)['testExitIP'], '198.51.100.5')
        self.assertFalse(service.public_node({}, result)['enabled'])

    def test_probe_has_no_global_redirect(self):
        c = service.node_config(service.parse_link(LINK), 'eth1', 19999, 123)
        self.assertEqual(c['inbounds'][0]['type'], 'socks')
        self.assertEqual(c['inbounds'][0]['listen'], '127.0.0.1')
        self.assertTrue(c['inbounds'][0]['users'][0]['password'])
        self.assertEqual(c['outbounds'][0]['bind_interface'], 'eth1')
        self.assertEqual(c['outbounds'][0]['routing_mark'], 123)
        self.assertNotIn('auto_route', json.dumps(c))

    def test_atomic_persistence(self):
        with tempfile.TemporaryDirectory() as tmp, patch.object(service, 'BASE', Path(tmp)):
            state = service.read_state()
            state['nodes']['a' * 32] = service.parse_link(LINK)
            state['revision'] += 1
            service.write_state(state)
            self.assertEqual(service.read_state(), state)
            self.assertEqual(len(list(Path(tmp).iterdir())), 1)


if __name__ == '__main__':
    unittest.main()
