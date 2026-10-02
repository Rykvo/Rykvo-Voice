"""Native DHCP isolation test; requires fresh mount AND network namespaces."""
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import time

ROOT = Path(__file__).resolve().parent.parent
spec = importlib.util.spec_from_file_location('network_auto', ROOT / 'deploy/network-drivers.py')
auto = importlib.util.module_from_spec(spec); spec.loader.exec_module(auto)


def run(*args):
    return subprocess.check_output(args, text=True, stderr=subprocess.STDOUT).strip()


def main():
    assert os.geteuid() == 0
    for namespace in ('net', 'mnt'):
        assert os.readlink('/proc/self/ns/' + namespace) != os.readlink('/proc/1/ns/' + namespace), 'Isolated namespaces required'
    run('mount', '--make-rprivate', '/')
    # All device state stays in this fixture's mount/network namespaces.
    run('mount', '-t', 'sysfs', '-o', 'ro', 'sysfs', '/sys')
    run('mount', '-t', 'tmpfs', 'tmpfs', '/run')
    Path('/etc/systemd/network').mkdir(exist_ok=True)
    run('mount', '-t', 'tmpfs', 'tmpfs', '/etc/systemd/network')
    Path('/run/dbus').mkdir()
    Path('/run/systemd/network').mkdir(parents=True)
    processes = []
    logs = []
    passed = False
    try:
        def spawn(args, log):
            stream = open('/run/' + log, 'w'); logs.append(stream)
            p = subprocess.Popen(args, stdout=stream, stderr=subprocess.STDOUT, env=os.environ | {"SYSTEMD_LOG_LEVEL":"debug", "SYSTEMD_LOG_TARGET":"console"})
            processes.append(p)
            return p
        bus = spawn(['dbus-daemon', '--system', '--nofork', '--nopidfile'], 'dbus.log')
        for _ in range(30):
            if Path('/run/dbus/system_bus_socket').exists(): break
            time.sleep(.1)
        run('ip', 'link', 'add', 'host0', 'type', 'dummy')
        run('ip', 'address', 'add', '192.168.50.2/24', 'dev', 'host0')
        run('ip', 'link', 'set', 'host0', 'up')
        run('ip', 'route', 'add', 'default', 'via', '192.168.50.1', 'dev', 'host0')
        baseline = json.loads(run('ip', '-j', '-4', 'route', 'show', 'table', 'main'))
        for number in (1, 2):
            peer = spawn(['unshare', '--net', 'sleep', '120'], f'peer{number}.log')
            time.sleep(.2)
            name = f'auto{number}'
            other = f'peer{number}'
            run('ip', 'link', 'add', name, 'type', 'veth', 'peer', 'name', other)
            run('ip', 'link', 'set', other, 'netns', str(peer.pid))
            ns = ['nsenter', '-t', str(peer.pid), '-n']
            run(*ns, 'ip', 'address', 'add', '192.168.7.1/24', 'dev', other)
            run(*ns, 'ip', 'link', 'set', other, 'up')
            mac = run('cat', f'/sys/class/net/{name}/address')
            spawn(ns + ['dnsmasq', '--keep-in-foreground', '--user=root', '--log-facility=-', '--port=0', '--no-resolv', '--no-hosts',
                        '--bind-interfaces', f'--interface={other}', '--dhcp-range=192.168.7.10,192.168.7.30,255.255.255.0,1h',
                        f'--dhcp-host={mac},192.168.7.20', '--dhcp-option=3,192.168.7.1', '--dhcp-option=6,192.168.7.1',
                        f'--dhcp-leasefile=/run/leases{number}', '--pid-file='], f'dhcp{number}.log')
            cfg = auto.networkd_config({'ifname':name, 'address':mac}, f'{number:06x}' + '0' * 10)
            Path(f'/etc/systemd/network/90-test-{number}.network').write_text(cfg)
        # networkd also checks udev initialization on Ubuntu's systemd build.
        # Seed only synthetic devices in the private /run, never host udev state.
        Path('/run/udev/data').mkdir(parents=True)
        for link in json.loads(run('ip', '-j', 'link', 'show')):
            Path('/run/udev/data/n' + str(link['ifindex'])).write_text('I:1\nE:ID_NET_MANAGED_BY=io.systemd.Network\n')
        daemon = spawn(['/usr/lib/systemd/systemd-networkd'], 'networkd.log')
        for _ in range(150):
            assert daemon.poll() is None, 'networkd exited'
            ready = []
            for number in (1, 2):
                value = json.loads(run('ip', '-j', 'address', 'show', 'dev', f'auto{number}'))[0]
                ready.append(any(a.get('local') == '192.168.7.20' for a in value['addr_info']))
            if all(ready): break
            time.sleep(.2)
        assert all(ready), 'DHCP did not acquire both isolated leases'
        assert baseline == json.loads(run('ip', '-j', '-4', 'route', 'show', 'table', 'main')), 'Host routes changed'
        for number in (1, 2):
            routes = json.loads(run('ip', '-j', '-4', 'route', 'show', 'table', str(0x53000000 + number)))
            assert any(r.get('dst') == 'default' and r.get('gateway') == '192.168.7.1' and r.get('dev') == f'auto{number}' for r in routes)
            index = run('cat', f'/sys/class/net/auto{number}/ifindex')
            lease = auto.fields(Path('/run/systemd/netif/leases') / index)
            assert lease['DNS'] == '192.168.7.1' and lease['ROUTER'] == '192.168.7.1'
            link = auto.fields(Path('/run/systemd/netif/links') / index)
            assert not link.get('DNS'), 'DHCP published DNS to host resolver'
        passed = True
        print('PASS: native DHCP, overlapping addresses/gateways, isolated tables, unchanged host routes/DNS')
    finally:
        for p in reversed(processes):
            p.terminate()
            try: p.wait(timeout=3)
            except subprocess.TimeoutExpired: p.kill(); p.wait()
        for stream in logs: stream.close()
        if not passed:
            for path in Path('/run').glob('*.log'):
                print(path.name, path.read_text()[-18000:])
            print(run('ip','-j','address','show'))
            for path in Path('/etc/systemd/network').glob('*.network'): print(path.name, path.read_text())
            for path in Path('/run/systemd/netif/links').glob('*'): print(path.name, path.read_text())


if __name__ == '__main__': main()
