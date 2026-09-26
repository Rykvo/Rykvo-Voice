"""Real WireGuard lifecycle; only run under sudo unshare --net."""
import importlib.util
import os
from pathlib import Path
import subprocess
import tempfile
import time

assert os.geteuid() == 0
assert os.readlink('/proc/self/ns/net') != os.readlink('/proc/1/ns/net'), 'isolated network namespace required'
root = Path(__file__).resolve().parent.parent
loader = importlib.util.spec_from_file_location('sip', root / 'deploy/sip-network.py')
sip = importlib.util.module_from_spec(loader)
loader.loader.exec_module(sip)


def command(*args, input=None):
    return subprocess.check_output(args, input=input, text=True).strip()


def pair():
    private = command('wg', 'genkey')
    return private, command('wg', 'pubkey', input=private)


with tempfile.TemporaryDirectory() as directory:
    sip.ROOT = Path(directory)
    sip.STATE = sip.ROOT / 'state.json'
    sip.ENV['XTABLES_LOCKFILE'] = str(sip.ROOT / 'xtables.lock')
    command('ip', 'link', 'set', 'lo', 'up')
    private, public = pair()
    cloud_private, cloud_public = pair()
    psk = command('wg', 'genpsk')
    command('ip', 'link', 'add', 'cloud', 'type', 'wireguard')
    command('wg', 'setconf', 'cloud', '/dev/stdin', input=f'[Interface]\nPrivateKey={cloud_private}\nListenPort=51820\n[Peer]\nPublicKey={public}\nPresharedKey={psk}\nAllowedIPs=10.77.0.2/32\n')
    command('ip', 'link', 'set', 'cloud', 'up')
    before = command('ip', '-4', 'route', 'show', 'table', 'main')
    dns = Path('/etc/resolv.conf').read_bytes()
    spec = dict(address='https://sip.example.com/api/connect', interface='sip1', bindAddress='10.77.0.2',
                server='sip.example.com', publicAddress='8.8.8.8', start=20000, end=29999,
                endpoint='127.0.0.1:51820', privateKey=private, publicKey=cloud_public, presharedKey=psk)
    # Loopback endpoint is test-only; production requires a verified public HTTPS endpoint.
    state = sip.load()
    sip.apply(state, spec)
    deadline = time.monotonic() + 10
    while sip.view(state)['state'] != 'connected' and time.monotonic() < deadline:
        time.sleep(.1)
    assert sip.view(state)['state'] == 'connected', 'no real WireGuard handshake'
    assert command('ip', '-4', 'route', 'show', 'table', 'main') == before
    assert Path('/etc/resolv.conf').read_bytes() == dns
    assert 'default dev sip1' in command('ip', '-4', 'route', 'show', 'table', '42002')
    assert 'from 10.77.0.2 lookup 42002' in command('ip', '-4', 'rule', 'show')
    sip.apply(state, spec)
    route = command('ip', '-4', 'route', 'get', '8.8.4.4', 'from', '10.77.0.2')
    assert 'dev sip1' in route and 'table 42002' in route
    for protocol in ('tcp', 'udp'):
        command('iptables', '-C', sip.CHAIN, '-d', '10.77.0.2', '-p', protocol,
                '--dport', '20000:29999', '-j', 'ACCEPT')
    command('iptables', '-C', sip.CHAIN, '-j', 'DROP')
    command('iptables', '-C', 'INPUT', '-i', 'sip1', '-j', sip.CHAIN)
    sip.dispatch({'action': 'disconnect'})
    assert not sip.link('sip1')
    assert '42002' not in command('ip', '-4', 'rule', 'show')
    assert subprocess.run(['iptables','-S',sip.CHAIN], stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL).returncode != 0
    command('ip', 'link', 'del', 'cloud')
    command('ip', 'link', 'add', 'sip1', 'type', 'dummy')
    try:
        sip.down(spec)
        raise AssertionError('foreign interface accepted')
    except sip.Failure as e:
        assert str(e) == 'SIP_INTERFACE_CONFLICT' and sip.link('sip1')
    command('ip', 'link', 'del', 'sip1')
    sip.dispatch({'action':'reconnect'})
    assert sip.link('sip1'), 'saved configuration not restored'
    sip.dispatch({'action':'disconnect'})
    assert command('ip', '-4', 'route', 'show', 'table', 'main') == before
    print('SIP_NETWORK_NETNS_OK: handshake, port filter rules, source-only routing, cleanup, reconnect')
