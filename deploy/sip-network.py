"""Private SIP network helper. Never execute configuration received from the cloud."""
import base64
import configparser
import fcntl
import http.client
import ipaddress
import json
import os
from pathlib import Path
import pwd
import re
import socket
import ssl
import struct
import subprocess
import sys
import time
import urllib.parse
import uuid

ROOT = Path('/var/lib/rykvo-sip-network')
STATE = ROOT / 'state.json'
ENV = {'PATH': '/usr/sbin:/usr/bin:/sbin:/bin', 'LC_ALL': 'C', 'XTABLES_LOCKFILE': str(ROOT / 'xtables.lock')}
MARK = 'rykvo-sip-network'
CHAIN = 'RYKVO_SIP'


class Failure(Exception):
    pass


def require(ok, code='SIP_INVALID_CONFIG'):
    if not ok:
        raise Failure(code)


def run(args, check=True, input=None):
    p = subprocess.run(args, input=input, text=True, stdout=subprocess.PIPE,
                       stderr=subprocess.DEVNULL, timeout=5, env=ENV)
    require(not check or p.returncode == 0, 'SIP_NETWORK_FAILED')
    require(len(p.stdout) < 131072, 'SIP_NETWORK_FAILED')
    return p


def save(state):
    path = ROOT / 'state.next'
    with open(path, 'w', encoding='utf-8', opener=lambda p, f: os.open(p, f, 0o600)) as out:
        json.dump(state, out)
        out.flush()
        os.fsync(out.fileno())
    path.replace(STATE)


def load():
    if STATE.exists():
        return json.loads(STATE.read_text())
    state = {'installationId': str(uuid.uuid4()), 'enabled': False}
    save(state)
    return state


def public_address(value):
    ip = ipaddress.ip_address(value)
    require(ip.version == 4 and ip.is_global and not ip.is_multicast, 'SIP_INVALID_ADDRESS')
    return str(ip)


def endpoint(address):
    require(isinstance(address, str) and len(address) <= 512 and not re.search(r'[\s\\]', address), 'SIP_INVALID_ADDRESS')
    u = urllib.parse.urlsplit(address)
    require(u.scheme == 'https' and u.hostname and not u.username and not u.password
            and u.path == '/api/connect' and not u.query and not u.fragment
            and u.port in (None, 443), 'SIP_INVALID_ADDRESS')
    return u


def redeem(address, code, installation_id):
    u = endpoint(address)
    require(isinstance(code, str) and re.fullmatch(r'[A-Za-z0-9_-]{43}', code), 'SIP_INVALID_CODE')
    # Resolve once; connect to a vetted address while verifying the original TLS name.
    ips = list(dict.fromkeys(public_address(a[4][0]) for a in socket.getaddrinfo(u.hostname, 443, socket.AF_INET, socket.SOCK_STREAM)))
    require(ips, 'SIP_INVALID_ADDRESS')
    conn = http.client.HTTPSConnection(u.hostname, 443, timeout=8)
    try:
        raw = socket.create_connection((ips[0], 443), timeout=8)
        try:
            conn.sock = ssl.create_default_context().wrap_socket(raw, server_hostname=u.hostname)
        except Exception:
            raw.close()
            raise
        conn.request('POST', '/api/connect', json.dumps({'installationId': installation_id}),
                     {'Authorization': 'Bearer ' + code, 'Content-Type': 'application/json', 'Accept': 'application/json'})
        response = conn.getresponse()
        codes = {400: 'SIP_INVALID_CONFIG', 401: 'SIP_INVALID_CODE', 409: 'SIP_CODE_BOUND',
                 410: 'SIP_CODE_REVOKED', 429: 'SIP_RATE_LIMITED'}
        require(response.status == 200, codes.get(response.status, 'SIP_CLOUD_FAILED'))
        data = response.read(131073)
        require(len(data) <= 131072, 'SIP_INVALID_CONFIG')
        return validate(json.loads(data), address, ips)
    finally:
        conn.close()


def key(value):
    require(isinstance(value, str) and re.fullmatch(r'[A-Za-z0-9+/]{43}=', value))
    require(len(base64.b64decode(value, validate=True)) == 32)
    return value


def validate(data, address, ips):
    require(isinstance(data, dict) and data.get('version') == 1)
    w, s, r = data['wireguard'], data['sip'], data['routing']
    name = w['interface']
    require(isinstance(name, str) and re.fullmatch(r'sip[1-9][0-9]{0,6}', name))
    require(r == {'mode': 'source', 'preserveDefaultRoute': True, 'preserveDns': True})
    ip = ipaddress.IPv4Address(s['bindAddress'])
    require(ip in ipaddress.IPv4Network('10.77.0.0/24') and 2 <= int(str(ip).split('.')[-1]) <= 254)
    public = public_address(s['publicAddress'])
    require(public in ips)
    start, end = s['portRange']['start'], s['portRange']['end']
    require(type(start) is int and type(end) is int and 1024 <= start < end <= 65535)
    require(not any(start <= p <= end for p in (2019, 8080, 51820, 51821, 51822)))
    require(sorted(s['protocols']) == ['tcp', 'udp'])
    cfg = configparser.ConfigParser(interpolation=None, strict=False)
    require(isinstance(w['config'], str) and len(w['config']) < 100000)
    cfg.read_string(w['config'])
    require(set(cfg.sections()) == {'Interface', 'Peer'})
    local, peer = cfg['Interface'], cfg['Peer']
    require(local['Address'] == str(ip) + '/32' and local.get('Table') == 'off')
    require(peer['AllowedIPs'] == '0.0.0.0/0' and peer.get('PersistentKeepalive') == '25')
    host, port = peer['Endpoint'].rsplit(':', 1)
    require(port == '51820' and host in (endpoint(address).hostname, public))
    require(s['server'] in (endpoint(address).hostname, public))
    require(local.get('MTU') == '1380')
    # PreUp/PostUp/PostDown are deliberately ignored: generate fixed local operations.
    return {'address': address, 'interface': name, 'bindAddress': str(ip), 'server': s['server'],
            'publicAddress': public, 'start': start, 'end': end, 'endpoint': public + ':51820',
            'privateKey': key(local['PrivateKey']), 'publicKey': key(peer['PublicKey']),
            'presharedKey': key(peer['PresharedKey'])}


def link(name):
    p = run(['ip', '-j', 'link', 'show', 'dev', name], check=False)
    return json.loads(p.stdout)[0] if p.returncode == 0 else None


def owned(spec):
    info = link(spec['interface'])
    require(not info or info.get('ifalias') == MARK, 'SIP_INTERFACE_CONFLICT')
    return info


def numbers(spec):
    suffix = int(spec['bindAddress'].split('.')[-1])
    return str(42000 + suffix), str(14000 + suffix)


def firewall(spec, remove=False):
    name = spec['interface']
    jump = ['INPUT', '-i', name, '-j', CHAIN]
    if remove:
        if run(['iptables', '-w', '2', '-C'] + jump, check=False).returncode == 0:
            run(['iptables', '-w', '2', '-D'] + jump)
        run(['iptables', '-w', '2', '-F', CHAIN], check=False)
        run(['iptables', '-w', '2', '-X', CHAIN], check=False)
        return
    require(run(['iptables', '-w', '2', '-S', CHAIN], check=False).returncode != 0, 'SIP_ROUTE_CONFLICT')
    run(['iptables', '-w', '2', '-N', CHAIN])
    for protocol in ('tcp', 'udp'):
        run(['iptables', '-w', '2', '-A', CHAIN, '-d', spec['bindAddress'], '-p', protocol,
             '--dport', f"{spec['start']}:{spec['end']}", '-j', 'ACCEPT'])
    run(['iptables', '-w', '2', '-A', CHAIN, '-j', 'DROP'])
    run(['iptables', '-w', '2', '-I'] + jump)


def down(spec):
    if not spec or not owned(spec):
        return
    table, priority = numbers(spec)
    run(['ip', '-4', 'rule', 'del', 'priority', priority, 'from', spec['bindAddress'] + '/32', 'table', table], check=False)
    firewall(spec, remove=True)
    run(['ip', 'link', 'del', 'dev', spec['interface']])


def up(spec):
    name, ip = spec['interface'], spec['bindAddress']
    table, priority = numbers(spec)
    require(not link(name), 'SIP_INTERFACE_CONFLICT')
    require(not run(['ip', '-4', 'route', 'show', 'table', table], check=False).stdout.strip(), 'SIP_ROUTE_CONFLICT')
    rules = json.loads(run(['ip', '-j', '-4', 'rule', 'show']).stdout)
    require(not any(str(r.get('priority')) == priority or str(r.get('table')) == table for r in rules), 'SIP_ROUTE_CONFLICT')
    addresses = json.loads(run(['ip', '-j', '-4', 'address', 'show']).stdout)
    require(not any(a['local'].startswith('10.77.0.') for x in addresses for a in x.get('addr_info', [])), 'SIP_ROUTE_CONFLICT')
    require(run(['iptables', '-w', '2', '-S', CHAIN], check=False).returncode != 0, 'SIP_ROUTE_CONFLICT')
    run(['ip', 'link', 'add', 'dev', name, 'type', 'wireguard'])
    try:
        run(['ip', 'link', 'set', 'dev', name, 'alias', MARK])
    except Exception:
        # The interface was created above; no existing interface is removed.
        run(['ip', 'link', 'del', 'dev', name], check=False)
        raise
    try:
        config = ('[Interface]\nPrivateKey = ' + spec['privateKey'] + '\n[Peer]\nPublicKey = ' + spec['publicKey']
                  + '\nPresharedKey = ' + spec['presharedKey'] + '\nAllowedIPs = 0.0.0.0/0\nEndpoint = '
                  + spec['endpoint'] + '\nPersistentKeepalive = 25\n')
        run(['wg', 'setconf', name, '/dev/stdin'], input=config)
        run(['ip', 'address', 'add', ip + '/32', 'dev', name])
        run(['ip', 'link', 'set', 'dev', name, 'mtu', '1380'])
        run(['sysctl', '-q', '-w', f'net.ipv4.conf.{name}.rp_filter=2'])
        firewall(spec)
        run(['ip', 'link', 'set', 'dev', name, 'up'])
        run(['ip', '-4', 'route', 'add', 'default', 'dev', name, 'table', table])
        run(['ip', '-4', 'rule', 'add', 'priority', priority, 'from', ip + '/32', 'table', table])
    except Exception:
        down(spec)
        raise


def view(state):
    spec = state.get('spec')
    result = {'state': 'disconnected', 'configured': bool(spec), 'enabled': state['enabled'],
              'address': spec['address'] if spec else '', 'issue': state.get('issue', ''),
              'capabilities': {'network': True, 'calls': False}}
    if state.get('pending'):
        result.update(state='failed', issue='SIP_RECOVERY_REQUIRED')
        return result
    if not spec:
        return result
    result['network'] = {k: spec[k] for k in ('interface', 'bindAddress', 'publicAddress', 'server', 'start', 'end')}
    if state['enabled']:
        result['state'] = 'connecting'
        info = owned(spec)
        if not info:
            result.update(state='failed', issue=state.get('issue') or 'SIP_INTERFACE_MISSING')
            return result
        p = run(['wg', 'show', spec['interface'], 'latest-handshakes'])
        times = [int(row.split()[1]) for row in p.stdout.splitlines() if len(row.split()) == 2 and row.split()[0] == spec['publicKey']]
        last = max(times, default=0)
        result['lastHandshake'] = last
        if 0 < last <= time.time() and time.time() - last <= 180:
            result.update(state='connected', issue='')
        elif time.time() - state.get('startedAt', 0) > 90:
            result.update(state='failed', issue='SIP_HANDSHAKE_TIMEOUT')
    return result


def apply(state, spec):
    old = dict(state)
    if state.get('spec') == spec and state['enabled'] and owned(spec):
        return view(state)
    previous = state.get('spec')
    save({**old, 'pending': spec})
    try:
        if previous:
            down(previous)
        up(spec)
        state.update(spec=spec, enabled=True, issue='', startedAt=int(time.time()))
        save(state)
    except Exception:
        try:
            info = link(spec['interface'])
            if info and info.get('ifalias') == MARK:
                down(spec)
            if old['enabled'] and previous and not owned(previous):
                up(previous)
            save(old)
        except Exception:
            raise Failure('SIP_ROLLBACK_FAILED') from None
        raise
    return view(state)


def dispatch(request):
    require(isinstance(request, dict), 'SIP_INVALID_REQUEST')
    action = request.get('action')
    require(action in ('status', 'connect', 'reconnect', 'disconnect', 'logout', 'resume'), 'SIP_INVALID_REQUEST')
    require(set(request) == ({'action', 'address', 'accessCode'} if action == 'connect' else {'action'}), 'SIP_INVALID_REQUEST')
    ROOT.mkdir(mode=0o700, parents=True, exist_ok=True)
    os.chmod(ROOT, 0o700)
    with open(ROOT / 'lock', 'a') as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise Failure('SIP_BUSY') from None
        state = load()
        if state.get('pending') and action != 'status':
            pending = state.pop('pending')
            info = link(pending['interface'])
            if info and info.get('ifalias') == MARK:
                down(pending)
            if action not in ('logout', 'disconnect') and state['enabled'] and state.get('spec') and not owned(state['spec']):
                up(state['spec'])
            save(state)
        if action == 'connect':
            spec = redeem(request['address'], request['accessCode'], state['installationId'])
            return apply(state, spec)
        if action in ('resume', 'reconnect'):
            if action == 'resume' and not state['enabled']:
                return view(state)
            require(state.get('spec'), 'SIP_NOT_CONFIGURED')
            return apply(state, state['spec'])
        if action in ('disconnect', 'logout'):
            down(state.get('spec'))
            state.update(enabled=False, issue='')
            if action == 'logout':
                state.pop('spec', None)
                state.pop('startedAt', None)
            save(state)
        return view(state)


def handle(request):
    try:
        return {'data': dispatch(request)}
    except Failure as e:
        return {'error': str(e)}
    except ssl.SSLError:
        return {'error': 'SIP_TLS_FAILED'}
    except (ValueError, KeyError, TypeError, configparser.Error):
        return {'error': 'SIP_INVALID_CONFIG'}
    except (OSError, subprocess.SubprocessError, http.client.HTTPException):
        return {'error': 'SIP_NETWORK_FAILED'}


def serve(conn):
    conn.settimeout(60)
    _, uid, _ = struct.unpack('3i', conn.getsockopt(socket.SOL_SOCKET, socket.SO_PEERCRED, 12))
    if uid not in (0, pwd.getpwnam('rykvo_voice').pw_uid):
        return
    with conn.makefile('rb') as stream:
        line = stream.readline(4097)
    try:
        result = handle(json.loads(line)) if len(line) <= 4096 and line.endswith(b'\n') else {'error': 'SIP_INVALID_REQUEST'}
    except ValueError:
        result = {'error': 'SIP_INVALID_REQUEST'}
    conn.sendall(json.dumps(result).encode() + b'\n')


if __name__ == '__main__':
    if sys.argv[1:] == ['--resume']:
        result = handle({'action': 'resume'})
        sys.exit(1 if 'error' in result else 0)
    elif sys.argv[1:] == ['--stop']:
        result = handle({'action': 'disconnect'})
        sys.exit(1 if 'error' in result else 0)
    else:
        with socket.socket(fileno=0) as connection:
            serve(connection)
