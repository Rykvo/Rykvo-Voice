#!/usr/bin/env python3
"""Authenticated network settings and per-link routing."""
import copy
import hmac
import ipaddress
import json
import os
import re
import socket
import subprocess
import tempfile
import threading
import time
import uuid
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.error import HTTPError
from urllib.parse import parse_qs, quote, unquote, urlencode, urlsplit
from urllib.request import ProxyHandler, Request, build_opener
from network_runtime import Controller, RoutingError, resources

BASE = Path('/var/lib/rykvo-network')
CORE = '/opt/rykvo-voice/live/sing-box'
LOCK = threading.RLock()
PROBE = threading.Lock()
RESULTS = {}
ROUTING = None
OPENER = build_opener(ProxyHandler({}))
MARK, PRIORITY = '0x5a0f0001', '30021'


class Fault(Exception):
    def __init__(self, code, status=400):
        self.code, self.status = code, status


def parse_link(value):
    if not isinstance(value, str) or not 10 < len(value) <= 4096:
        raise Fault('INVALID_VLESS')
    try:
        u = urlsplit(value.strip())
        q = parse_qs(u.query, keep_blank_values=True)
        if u.scheme != 'vless' or u.password is not None or u.path not in ('', '/'):
            raise ValueError()
        user = str(uuid.UUID(unquote(u.username or '')))
        server, port = u.hostname, u.port
        if not server or not port or any(ord(c) < 33 for c in server):
            raise ValueError()
        if len(server) > 253 or not re.fullmatch(r'[a-zA-Z0-9.:_-]+', server):
            raise ValueError()
        if any(len(v) != 1 for v in q.values()):
            raise ValueError()
        if set(q) - {'type', 'security', 'encryption', 'headerType', 'sni', 'allowInsecure', 'flow'}:
            raise Fault('VLESS_OPTIONS_UNSUPPORTED')
        if q.get('type', ['tcp'])[0] not in ('tcp', 'raw') or q.get('headerType', ['none'])[0] != 'none':
            raise Fault('VLESS_OPTIONS_UNSUPPORTED')
        security = q.get('security', ['none'])[0]
        if security not in ('none', 'tls') or q.get('encryption', ['none'])[0] != 'none' or q.get('flow', [''])[0]:
            raise Fault('VLESS_OPTIONS_UNSUPPORTED')
        if q.get('allowInsecure', ['0'])[0] not in ('0', 'false'):
            raise Fault('VLESS_OPTIONS_UNSUPPORTED')
        label = unquote(u.fragment).strip()[:40] or 'VLESS'
        if any(ord(c) < 32 for c in label):
            raise ValueError()
        return dict(server=server, port=port, uuid=user, security=security,
                    sni=q.get('sni', [server])[0], label=label)
    except Fault:
        raise
    except (ValueError, TypeError):
        raise Fault('INVALID_VLESS') from None


def read_state():
    try:
        return json.loads((BASE / 'nodes.json').read_text())
    except FileNotFoundError:
        return {'revision': 1, 'nodes': {}}


def write_state(data):
    BASE.mkdir(mode=0o700, parents=True, exist_ok=True)
    os.chmod(BASE, 0o700)
    fd, path = tempfile.mkstemp(prefix='.nodes-', dir=BASE)
    try:
        with os.fdopen(fd, 'w') as f:
            os.chmod(path, 0o600)
            json.dump(data, f, ensure_ascii=False)
            f.flush()
            os.fsync(f.fileno())
        os.replace(path, BASE / 'nodes.json')
    finally:
        if os.path.exists(path):
            os.unlink(path)


def public_node(node, result=None, routing=None):
    result = result or {}
    fresh = result.get('finishedAt', 0) > time.time() - 300
    return {
        'configured': bool(node), 'enabled': bool((routing or {}).get('enabled')),
        'label': node.get('label', '') if node else '',
        'security': node.get('security', '') if node else '',
        'state': (routing or {}).get('state', 'not-enabled' if node else 'not-configured'),
        'error': (routing or {}).get('error',''),
        'testState': result.get('state', 'untested'),
        'testExitIP': result.get('exitIP', '') if fresh else '',
        'testLatencyMS': result.get('latencyMS') if fresh else None,
        'testedAt': result.get('finishedAt'),
        'testError': result.get('error', ''),
        'testID': result.get('id', ''),
    }


def node_link(node):
    if not node:
        return ''
    host = '[' + node['server'] + ']' if ':' in node['server'] else node['server']
    query = {'type': 'tcp', 'security': node['security']}
    if node['sni'] != node['server']:
        query['sni'] = node['sni']
    return f"vless://{node['uuid']}@{host}:{node['port']}?{urlencode(query)}#{quote(node['label'], safe='')}"


def command(*args, timeout=8):
    return subprocess.run(args, text=True, stdout=subprocess.PIPE,
                          stderr=subprocess.PIPE, timeout=timeout, check=True).stdout


def node_config(node, interface, port, mark=0):
    outbound = {'type': 'vless', 'tag': 'node', 'server': node['server'],
                'server_port': node['port'], 'uuid': node['uuid'],
                'bind_interface': interface, 'connect_timeout': '6s'}
    if mark:
        outbound['routing_mark'] = mark
    if node['security'] == 'tls':
        outbound['tls'] = {'enabled': True, 'server_name': node['sni']}
    return {'log': {'disabled': True},
            'inbounds': [{'type': 'socks', 'listen': '127.0.0.1', 'listen_port': port,
                          'users': [{'username': 'probe', 'password': uuid.uuid4().hex}]}],
            'outbounds': [outbound], 'route': {'final': 'node'}}


def probe_node(network, node, test_id):
    proc, added_rule = None, False
    result = {'id': test_id, 'state': 'failed', 'error': 'VPN_TEST_FAILED'}
    try:
        interface = network['name']
        if not re.fullmatch(r'[a-zA-Z0-9_.:-]{1,15}', interface):
            raise Fault('NETWORK_UNAVAILABLE')
        routes = json.loads(command('ip', '-j', '-4', 'route', 'show', 'table', 'all'))
        defaults = [r for r in routes if r.get('dev') == interface and r.get('dst') == 'default'
                    and r.get('type', 'unicast') == 'unicast' and 'linkdown' not in r.get('flags', [])]
        if not defaults:
            raise Fault('NETWORK_UNAVAILABLE')
        chosen = min(defaults, key=lambda r: (r.get('table', 'main') != 'main', r.get('metric', 0)))
        table = str(chosen.get('table', 'main'))
        if ROUTING:
            saved = ROUTING.state()['networks'].get(network['id'])
            if saved:
                table = str(resources(saved['slot'],network['id'])['direct'])
        mark = 0
        if table not in ('main', '254'):
            if not table.isdecimal():
                raise Fault('NETWORK_UNAVAILABLE')
            rules = json.loads(command('ip', '-j', '-4', 'rule', 'show'))
            if any(str(r.get('priority')) == PRIORITY or r.get('fwmark') == MARK for r in rules):
                raise Fault('PROBE_ROUTE_BUSY', 409)
            (BASE / 'probe-rule.json').write_text(json.dumps({'table': table}))
            command('ip', '-4', 'rule', 'add', 'pref', PRIORITY, 'fwmark', MARK + '/0xffffffff', 'lookup', table)
            added_rule = True
            mark = int(MARK, 16)
        with socket.socket() as listener:
            listener.bind(('127.0.0.1', 0))
            port = listener.getsockname()[1]
        with tempfile.TemporaryDirectory(prefix='probe-', dir=BASE) as temp:
            config = node_config(node, interface, port, mark)
            config_path = Path(temp) / 'config.json'
            config_path.write_text(json.dumps(config))
            os.chmod(config_path, 0o600)
            command(CORE, 'check', '-c', str(config_path))
            proc = subprocess.Popen([CORE, 'run', '-c', str(config_path)],
                                    stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            for _ in range(40):
                if proc.poll() is not None:
                    raise Fault('VPN_CORE_FAILED')
                try:
                    with socket.create_connection(('127.0.0.1', port), timeout=.15):
                        break
                except OSError:
                    time.sleep(.05)
            user = config['inbounds'][0]['users'][0]
            # Public IP checks contain no application data or VLESS credentials.
            curl_config = Path(temp) / 'curl.conf'
            curl_config.write_text('proxy = "socks5h://127.0.0.1:' + str(port) + '"\nproxy-user = "probe:' + user['password'] + '"\n')
            os.chmod(curl_config, 0o600)
            start = time.monotonic()
            raw = command('curl', '--config', str(curl_config), '--silent', '--show-error', '--fail',
                          '--connect-timeout', '6', '--max-time', '12', '--max-filesize', '512',
                          'https://api.ipify.org?format=json', timeout=15)
            address = str(ipaddress.ip_address(json.loads(raw)['ip']))
            if not ipaddress.ip_address(address).is_global:
                raise Fault('VPN_EXIT_INVALID')
            result = {'id': test_id, 'state': 'passed', 'exitIP': address,
                      'latencyMS': round((time.monotonic() - start) * 1000)}
    except Fault as e:
        result['error'] = e.code
    except (OSError, subprocess.SubprocessError, ValueError, KeyError):
        result['error'] = 'VPN_TEST_FAILED'
    finally:
        if proc is not None:
            proc.terminate()
            try:
                proc.wait(timeout=3)
            except subprocess.TimeoutExpired:
                proc.kill()
                proc.wait()
        cleanup_ok = True
        if added_rule:
            try:
                command('ip', '-4', 'rule', 'del', 'pref', PRIORITY, 'fwmark', MARK + '/0xffffffff', 'lookup', table)
                (BASE / 'probe-rule.json').unlink(missing_ok=True)
            except Exception:
                cleanup_ok = False
                result = {'id': test_id, 'state': 'failed', 'error': 'PROBE_CLEANUP_FAILED'}
        result['finishedAt'] = time.time()
        with LOCK:
            RESULTS[network['id']] = result
        if cleanup_ok:
            PROBE.release()


class Handler(BaseHTTPRequestHandler):
    protocol_version = 'HTTP/1.0'

    def setup(self):
        super().setup()
        self.connection.settimeout(10)

    def log_message(self, *args):
        pass

    def reply(self, status, payload):
        body = json.dumps(payload, ensure_ascii=False).encode()
        self.send_response(status)
        self.send_header('Content-Type', 'application/json; charset=utf-8')
        self.send_header('Content-Length', str(len(body)))
        self.send_header('Cache-Control', 'no-store')
        self.send_header('X-Content-Type-Options', 'nosniff')
        self.end_headers()
        self.wfile.write(body)

    def upstream(self, path, method='GET', body=None):
        prefix = '/gly' if self.path.startswith('/gly/') else ''
        req = Request('http://127.0.0.1:8080' + prefix + '/api' + path,
                      headers={'Cookie': self.headers.get('Cookie', ''), 'Accept': 'application/json',
                               'Host': self.headers.get('Host', ''),
                               'X-Forwarded-Proto': self.headers.get('X-Forwarded-Proto', 'http'),
                               'Origin': self.headers.get('Origin',''),
                               'X-CSRF-Token': self.headers.get('X-CSRF-Token',''),
                               'Content-Type':'application/json'},
                      method=method, data=json.dumps(body).encode() if body is not None else None)
        with OPENER.open(req, timeout=6) as response:
            if response.status==204:
                return None
            return json.loads(response.read(2 * 1024 * 1024))['data']

    def handle_request(self):
        path = self.path.split('?', 1)[0]
        if path not in ('/api/settings/network-control', '/gly/api/settings/network-control'):
            raise Fault('NOT_FOUND', 404)
        session = self.upstream('/session')
        networks = self.upstream('/settings/networks')
        if self.command == 'GET':
            with LOCK:
                state = read_state()
                routing = ROUTING.inventory(networks) if ROUTING else None
                view = ROUTING.view() if ROUTING else {}
                query = parse_qs(urlsplit(self.path).query, keep_blank_values=True)
                if 'node' in query:
                    ids = query['node']
                    if len(ids) != 1 or not any(n['id'] == ids[0] for n in networks['networks']):
                        raise Fault('INVALID_NETWORK')
                    node = state['nodes'].get(ids[0])
                    self.reply(200, {'data': {'link': node_link(node),
                                            'label': node['label'] if node else '',
                                            'revision': state['revision']}})
                    return
                for network in networks['networks']:
                    node = state['nodes'].get(network['id'])
                    network['vpn'] = public_node(node, RESULTS.get(network['id']),view.get('vpn',{}).get(network['id']))
                    if routing:
                        network['primary'] = network['id']==view.get('primary',routing['primary'])
                        network['systemDefault'] = network['id']==routing['system']
                        network['routingAvailable'] = bool(routing['system'])
                        network['fallback'] = network['primary'] and view.get('fallback',False)
                networks.update(trafficControlReady=bool(routing and routing['system']), vpnRevision=state['revision'],
                                routingRevision=view.get('revision',1), routingError=view.get('error',''))
            self.reply(200, {'data': networks})
            return
        if self.command != 'POST':
            raise Fault('METHOD_NOT_ALLOWED', 405)
        origin = urlsplit(self.headers.get('Origin', ''))
        host = self.headers.get('Host', '')
        if origin.scheme not in ('http', 'https') or origin.netloc != host or origin.path or origin.query or origin.fragment:
            raise Fault('ORIGIN_REJECTED', 403)
        token = self.headers.get('X-CSRF-Token', '')
        if not token or not hmac.compare_digest(token, session['csrfToken']):
            raise Fault('CSRF_REJECTED', 403)
        length = int(self.headers.get('Content-Length', 0))
        if not 0 < length <= 8192 or self.headers.get('Content-Type', '').split(';')[0] != 'application/json':
            raise Fault('INVALID_REQUEST')
        body = json.loads(self.rfile.read(length))
        if not isinstance(body, dict):
            raise Fault('INVALID_REQUEST')
        network = next((n for n in networks['networks'] if n['id'] == body.get('id')), None)
        if not network:
            raise Fault('INVALID_NETWORK')
        action = body.get('action')
        if action in ('primary','vpn') and ROUTING:
            with ROUTING.lock:
                routing=ROUTING.inventory(networks)
                if type(body.get('enabled')) is not bool or type(body.get('revision')) is not int:
                    raise Fault('INVALID_REQUEST')
                if body['revision']!=routing['revision']:
                    raise Fault('NETWORK_CONFLICT',409)
                current=ROUTING.view().get('primary',routing['primary'])
                affected=[m for m in networks['modules'] if (not m['network'] or action=='vpn' and m['network']==network['id'])]
                if any(m['busy'] for m in affected):
                    raise Fault('DEVICE_BUSY',409)
                if action=='vpn' and body['enabled'] and network['id'] not in read_state()['nodes']:
                    raise Fault('INVALID_VLESS')
                # Pin legacy/default modules before changing the host route.
                if any(not m['network'] for m in networks['modules']):
                    ids=[m['id'] for m in networks['modules'] if not m['network'] or m['network']==current]
                    self.upstream('/settings/networks','PUT',{'id':current,'revision':networks['revision'],'modules':ids})
                revision=ROUTING.change(network['id'],action,body['enabled'],body['revision'])
                self.reply(200,{'data':{'revision':revision}})
                return
        if action not in ('save', 'test', 'remove'):
            raise Fault('TRAFFIC_CONTROL_NOT_READY', 409)
        with LOCK:
            state = read_state()
            if body.get('revision') != state['revision']:
                raise Fault('NETWORK_CONFLICT', 409)
            if RESULTS.get(network['id'], {}).get('state') == 'testing':
                raise Fault('VPN_TEST_BUSY', 409)
            current = state['nodes'].get(network['id'])
            if action in ('save','remove') and ROUTING and network['id'] in ROUTING.state()['enabled']:
                raise Fault('VPN_DISABLE_FIRST',409)
            node = parse_link(body['link']) if body.get('link') else copy.deepcopy(current)
            if action != 'remove' and not node:
                raise Fault('INVALID_VLESS')
            if node and 'label' in body:
                label = body['label']
                if not isinstance(label, str) or len(label.strip()) > 40 or any(ord(c) < 32 for c in label):
                    raise Fault('INVALID_REQUEST')
                node['label'] = label.strip() or node['label']
            if action == 'test':
                if network['state'] != 'configured':
                    raise Fault('NETWORK_UNAVAILABLE', 409)
                if not PROBE.acquire(blocking=False):
                    raise Fault('VPN_TEST_BUSY', 409)
                test_id = uuid.uuid4().hex
                RESULTS[network['id']] = {'id': test_id, 'state': 'testing'}
                threading.Thread(target=probe_node, args=(network, node, test_id), daemon=True).start()
                self.reply(202, {'data': {'testID': test_id}})
                return
            if action == 'save':
                state['nodes'][network['id']] = node
            else:
                state['nodes'].pop(network['id'], None)
            RESULTS.pop(network['id'], None)
            state['revision'] += 1
            write_state(state)
            self.reply(200, {'data': {'revision': state['revision']}})

    def dispatch(self):
        try:
            self.handle_request()
        except Fault as e:
            self.reply(e.status, {'error': {'code': e.code}})
        except RoutingError as e:
            self.reply(409, {'error': {'code': str(e)}})
        except HTTPError as e:
            code='UNAUTHENTICATED' if e.code==401 else 'UPSTREAM_UNAVAILABLE'
            if e.code==409:
                try:
                    candidate=json.loads(e.read(4096)).get('error',{}).get('code','')
                    if candidate in ('DEVICE_BUSY','NETWORK_CONFLICT','NETWORK_ASSIGNED','NETWORK_UNAVAILABLE'):
                        code=candidate
                except (ValueError,AttributeError): pass
            self.reply(e.code if e.code in (401,403,409) else 503, {'error': {'code':code}})
        except (ValueError, TypeError):
            self.reply(400, {'error': {'code': 'INVALID_REQUEST'}})
        except Exception:
            self.reply(503, {'error': {'code': 'NETWORK_CONTROL_UNAVAILABLE'}})

    do_GET = dispatch
    do_POST = dispatch


if __name__ == '__main__':
    os.umask(0o077)
    BASE.mkdir(mode=0o700, parents=True, exist_ok=True)
    os.chmod(BASE, 0o700)
    if '--cleanup' in sys.argv:
        owned = BASE / 'probe-rule.json'
        if owned.exists():
            table = json.loads(owned.read_text())['table']
            if str(table).isdecimal():
                subprocess.run(['ip', '-4', 'rule', 'del', 'pref', PRIORITY, 'fwmark', MARK + '/0xffffffff', 'lookup', str(table)],
                               stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=8)
                owned.unlink()
        raise SystemExit(0)
    ROUTING=Controller()
    LOCK=ROUTING.lock
    # Restore the saved guard/routes before allowing the application to start.
    ROUTING.apply(ROUTING.state(),True,initialize=True)
    server=ThreadingHTTPServer(('127.0.0.1',38186),Handler)
    threading.Thread(target=ROUTING.watch,daemon=True).start()
    notify=os.environ.get('NOTIFY_SOCKET')
    if notify:
        with socket.socket(socket.AF_UNIX,socket.SOCK_DGRAM) as ready:
            ready.connect('\0'+notify[1:] if notify.startswith('@') else notify)
            ready.sendall(b'READY=1')
    server.serve_forever()
