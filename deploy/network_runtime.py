"""Per-link routing and carrier-loss fallback; never change the system main table."""
from concurrent.futures import ThreadPoolExecutor
import hashlib
import ipaddress
import json
import os
import re
import socket
import ssl
import subprocess
import tempfile
import threading
import time
from pathlib import Path

BASE = Path('/var/lib/rykvo-network')
RUN = Path('/var/lib/rykvo-network-route')
CORE = '/opt/rykvo-voice/live/sing-box'
HOST_TABLE = 0x5a000001
NFT = 'rykvo_network'
FALLBACK_DELAY = 5
PROBE_INTERVAL = 15


def reachable(network, mark):
    """TLS reachability through this link only; no application data or DNS fallback."""
    targets = [('1.1.1.1', 'cloudflare-dns.com'), ('8.8.8.8', 'dns.google'),
               ('2606:4700:4700::1111', 'cloudflare-dns.com'), ('2001:4860:4860::8888', 'dns.google')]
    deadline = time.monotonic() + 3
    context = ssl.create_default_context()
    for address, name in targets:
        version = ipaddress.ip_address(address).version
        source = next((a['local'] for a in network.get('addresses', [])
                       if ipaddress.ip_address(a['local']).version == version), None)
        if not source or time.monotonic() >= deadline:
            continue
        try:
            family = socket.AF_INET if version == 4 else socket.AF_INET6
            with socket.socket(family, socket.SOCK_STREAM) as connection:
                connection.setsockopt(socket.SOL_SOCKET, socket.SO_BINDTODEVICE, network['name'].encode() + b'\0')
                connection.setsockopt(socket.SOL_SOCKET, socket.SO_MARK, mark)
                connection.bind((source, 0))
                connection.settimeout(min(1, max(.01, deadline - time.monotonic())))
                connection.connect((address, 443))
                connection.settimeout(min(1, max(.01, deadline - time.monotonic())))
                with context.wrap_socket(connection, server_hostname=name):
                    return True
        except OSError:
            continue
    return False


class RoutingError(Exception):
    pass


def run(*args, check=True, data=None):
    p = subprocess.run(args, input=data, text=True, capture_output=True, timeout=10)
    if check and p.returncode:
        raise RoutingError('NETWORK_APPLY_FAILED')
    return p.stdout


def atomic(path, value, mode=0o600):
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, name = tempfile.mkstemp(dir=path.parent, prefix='.next-')
    try:
        with os.fdopen(fd, 'w') as f:
            os.chmod(name, mode)
            json.dump(value, f, ensure_ascii=False)
            f.flush()
            os.fsync(f.fileno())
        os.replace(name, path)
    finally:
        if os.path.exists(name):
            os.unlink(name)


def stable_id(name):
    root = Path('/sys/class/net') / name
    device = (root / 'device').resolve(strict=True)
    def read(p):
        try:
            return p.read_text().strip()
        except OSError:
            return ''
    function = read(device / 'bInterfaceNumber')
    value = str(device) + ':' + read(root / 'address')
    for parent in (device, *device.parents):
        vendor = read(parent / 'idVendor')
        if vendor:
            serial = read(parent / 'serial')
            if serial:
                value = 'usb:' + vendor + ':' + read(parent / 'idProduct') + ':' + serial + ':' + function
            break
    return hashlib.sha256(value.encode()).hexdigest()[:32]


def resources(slot, ident):
    return {'tun': 'rvpn' + ident[:8], 'direct': 0x5a100000 + slot,
            'vpn': 0x5a200000 + slot, 'v4': f'198.18.{slot}.1',
            'dns4': f'198.18.{slot}.2', 'v6': f'fd9a:7279:{slot:x}::1',
            'dns6': f'fd9a:7279:{slot:x}::2'}


def core_config(node, network, owned):
    # Resolve before applying VPN DNS, then bind the node transport to its physical link.
    server = node['server']
    try:
        ipaddress.ip_address(server)
    except ValueError:
        server = socket.getaddrinfo(server, node['port'], socket.AF_INET, socket.SOCK_STREAM)[0][4][0]
    out = {'type': 'vless', 'tag': 'node', 'server': server, 'server_port': node['port'],
           'uuid': node['uuid'], 'bind_interface': network['name'],
           'routing_mark': owned['direct'], 'connect_timeout': '8s'}
    if node['security'] == 'tls':
        out['tls'] = {'enabled': True, 'server_name': node['sni']}
    return {'log': {'disabled': True},
            'dns': {'servers': [{'type': 'https', 'tag': 'remote', 'server': '1.1.1.1',
                                'tls': {'enabled': True, 'server_name': 'cloudflare-dns.com'},
                                'detour': 'node'}], 'final': 'remote'},
            'inbounds': [{'type': 'tun', 'tag': 'vpn', 'interface_name': owned['tun'],
                          # Manage IPv6 ourselves: the core otherwise adds priority-0 rules even without auto_route.
                          'address': [owned['v4'] + '/30'], 'stack': 'gvisor',
                          'mtu': 1400, 'auto_route': False, 'dns_mode': 'disabled'}],
            'outbounds': [out],
            'route': {'rules': [{'port': 53, 'action': 'hijack-dns'}], 'final': 'node'}}


def firewall(networks, enabled):
    lines = [f'table inet {NFT} {{', 'chain ingress { type filter hook prerouting priority mangle; policy accept;']
    for ident,n in networks.items():
        if n.get('slot'):
            mark=resources(n['slot'],ident)['direct']
            lines.append(f'iifname {json.dumps(n["name"])} ct direction original ct mark set {mark}')
    lines += ['}', 'chain reply { type route hook output priority mangle; policy accept;',
              'ct direction reply meta mark 0 ct mark != 0 meta mark set ct mark', '}',
              'chain output { type filter hook output priority 0; policy accept;']
    for ident in enabled:
        n = networks[ident]
        iface = json.dumps(n['name'])
        # Only return traffic and this link's proxy/probe transport bypass the VPN.
        prefix = f'oifname {iface} '
        transport=resources(n['slot'],ident)['direct']
        lines += [prefix + 'ct direction reply accept',
                  prefix + f'meta mark {{ {transport}, 0x5a0f0001 }} accept',
                  prefix + 'meta l4proto { tcp, udp } th dport 53 reject']
        for a in n.get('addresses', []):
            ip = ipaddress.ip_address(a['local'])
            subnet = ipaddress.ip_network(f"{ip}/{a['prefixlen']}", strict=False)
            lines.append(prefix + ('ip' if ip.version == 4 else 'ip6') + f' daddr {subnet} accept')
        lines += [prefix + 'ip6 daddr fe80::/10 accept', prefix + 'reject']
    lines += ['}', '}']
    return '\n'.join(lines) + '\n'


class Controller:
    def __init__(self):
        self.lock = threading.RLock()
        self.processes = {}
        self.signatures = {}
        self.applied = None
        self.status = {}
        self.error = ''
        self.stop = threading.Event()
        self.selection = ''
        self.absent_at = None
        self.next_probe = 0
        self.probe_offset = 0
        self.dns_interface = (BASE / 'dns-active').read_text().strip() if (BASE / 'dns-active').exists() else ''

    def claim(self, state):
        journal = BASE / 'ownership.json'
        journal_state = json.loads(journal.read_text()) if journal.exists() else {}
        previous = journal_state.get('networks', {})
        tables = {HOST_TABLE} | {resources(n['slot'],k)[kind] for k,n in state['networks'].items() for kind in ('direct','vpn')}
        old_tables = ({HOST_TABLE} | {resources(n['slot'],k)[kind] for k,n in previous.items() for kind in (('direct','vpn') if journal_state.get('probeTables') else ('direct',))}) if previous else set()
        for family in ('-4','-6'):
            for rule in json.loads(run('ip','-j',family,'rule','show')):
                table = str(rule.get('table',''))
                if rule['priority']==31000 and table!=str(HOST_TABLE):
                    raise RoutingError('NETWORK_ROUTE_CONFLICT')
                if table.isdigit() and int(table) in tables-old_tables:
                    raise RoutingError('NETWORK_ROUTE_CONFLICT')
            for table in tables-old_tables:
                if run('ip',family,'route','show','table',str(table),check=False).strip():
                    raise RoutingError('NETWORK_ROUTE_CONFLICT')
        if not previous and run('nft','list','table','inet',NFT,check=False):
            raise RoutingError('NETWORK_ROUTE_CONFLICT')
        links=json.loads(run('ip','-j','address','show'))
        our_names={resources(n['slot'],k)['tun'] for k,n in previous.items()}
        for ident,n in state['networks'].items():
            r=resources(n['slot'],ident)
            for link in links:
                if link['ifname'] in our_names: continue
                if link['ifname']==r['tun']: raise RoutingError('NETWORK_ROUTE_CONFLICT')
                for a in link.get('addr_info',[]):
                    prefix=ipaddress.ip_network(f"{a['local']}/{a['prefixlen']}",strict=False)
                    if any(ipaddress.ip_address(r[v]) in prefix for v in ('v4','v6')):
                        raise RoutingError('NETWORK_ROUTE_CONFLICT')
        atomic(journal,{'networks':state['networks'],'probeTables':True})

    def state(self):
        try:
            return json.loads((BASE / 'routing.json').read_text())
        except FileNotFoundError:
            return {'revision': 1, 'system': '', 'primary': '', 'networks': {}, 'enabled': []}

    def inventory(self, source):
        with self.lock:
            state = self.state()
            before = json.dumps(state, sort_keys=True)
            for n in source['networks']:
                if not re.fullmatch('[a-f0-9]{32}', n['id']) or not re.fullmatch('[a-zA-Z0-9_.:-]{1,15}', n['name']):
                    continue
                if n['state'] != 'configured':
                    continue
                entry = state['networks'].get(n['id'])
                if not entry:
                    entry = {'slot': len(state['networks']) + 1}
                    if entry['slot'] > 200:
                        continue
                state['networks'][n['id']] = {**n, 'slot': entry['slot']}
                if not state['system'] and n.get('hostDefault'):
                    state['system'] = state['primary'] = n['id']
            if state['system'] and json.dumps(state, sort_keys=True) != before:
                atomic(BASE / 'routing.json', state)
            return state

    def live(self, state):
        links = {n['ifname']: n for n in json.loads(run('ip', '-j', 'address', 'show'))}
        routes = {family: json.loads(run('ip', '-j', family, 'route', 'show', 'table', 'all')) for family in ('-4', '-6')}
        current = {}
        for ident, old in state['networks'].items():
            n = {**old, 'online': False, 'connected': False}
            link = links.get(n['name'], {})
            try:
                identity_ok = stable_id(n['name']) == ident
            except (OSError, ValueError):
                identity_ok = False
            if identity_ok:
                flags = link.get('flags', [])
                n['connected'] = 'LOWER_UP' in flags or 'UP' not in flags
                n['addresses'] = [{k:a[k] for k in ('family','local','prefixlen','scope')} for a in link.get('addr_info', []) if a.get('scope') == 'global' and not a.get('tentative')]
                defaults = [r for rs in routes.values() for r in rs if r.get('dev') == n['name'] and
                            r.get('dst') == 'default' and int(r.get('table', 254) if str(r.get('table',254)).isdigit() else 254) < 0x5a000000
                            and r.get('gateway') and 'linkdown' not in r.get('flags', [])]
                n['gateways'] = list(dict.fromkeys(r['gateway'] for r in defaults))
                n['online'] = 'LOWER_UP' in flags and bool(n['addresses']) and bool(n['gateways'])
            current[ident] = n
        return current

    def rule(self, family, priority, selectors, table):
        existing = json.loads(run('ip', '-j', family, 'rule', 'show'))
        # Our priorities are reserved on first activation and only exact owned rules are reused.
        if any(str(r.get('table')) == str(table) and r['priority'] == priority and
               (not selectors or (selectors[0] == 'fwmark' and int(str(r.get('fwmark','0')),0)==int(selectors[1])) or
                (selectors[0]=='from' and r.get('src')==selectors[1].split('/')[0])) for r in existing):
            return
        run('ip', family, 'rule', 'add', 'pref', str(priority), *selectors, 'lookup', str(table))

    def table(self, family, table, network):
        run('ip', family, 'route', 'replace', 'unreachable', 'default', 'metric', '32767', 'table', str(table))
        version = 4 if family == '-4' else 6
        addresses = [a for a in network.get('addresses', []) if ipaddress.ip_address(a['local']).version == version] if network.get('online') else []
        desired=set()
        for a in addresses:
            subnet = str(ipaddress.ip_network(f"{a['local']}/{a['prefixlen']}",strict=False))
            desired.add(subnet)
            run('ip', family, 'route', 'replace', subnet, 'dev', network['name'], 'src', a['local'], 'table', str(table))
        gateway = next((g for g in network.get('gateways',[]) if ipaddress.ip_address(g).version == version), None)
        if gateway and addresses:
            desired.add('default')
            run('ip', family, 'route', 'replace', 'default', 'via', gateway, 'dev', network['name'], 'onlink',
                'src', addresses[0]['local'], 'metric', '10', 'table', str(table))
        for route in json.loads(run('ip','-j',family,'route','show','table',str(table))):
            dst=route.get('dst','default')
            if route.get('type')=='unreachable' or dst in desired or table==HOST_TABLE and dst!='default': continue
            args=['ip',family,'route','del',dst,'table',str(table)]
            if route.get('metric'):args+=['metric',str(route['metric'])]
            run(*args,check=False)

    def start_core(self, ident, n, node, owned):
        sig = hashlib.sha256(json.dumps([n['name'],n.get('addresses'),n.get('gateways'),node],sort_keys=True).encode()).hexdigest()[:16]
        proc = self.processes.get(ident)
        if proc and proc.poll() is None and self.signatures.get(ident)==sig:
            return sig
        if proc and proc.poll() is None:
            proc.terminate()
            try: proc.wait(3)
            except subprocess.TimeoutExpired: proc.kill();proc.wait()
        path = BASE / ('vpn-' + ident + '.json')
        atomic(path, core_config(node,n,owned))
        run(CORE, 'check', '-c', str(path))
        if not Path('/sys/class/net',owned['tun']).exists():
            run('ip','tuntap','add','dev',owned['tun'],'mode','tun')
        run('ip','link','set','dev',owned['tun'],'up')
        run('ip','-6','address','replace',owned['v6']+'/126','dev',owned['tun'],'nodad')
        env={k:v for k,v in os.environ.items() if k!='NOTIFY_SOCKET'}
        proc = subprocess.Popen([CORE,'run','-c',str(path)],env=env,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
        self.processes[ident] = proc
        self.signatures[ident] = sig
        deadline = time.monotonic()+3
        while time.monotonic()<deadline:
            if proc.poll() is not None:
                raise RoutingError('VPN_CORE_FAILED')
            addresses=json.loads(run('ip','-j','addr','show','dev',owned['tun']))[0].get('addr_info',[])
            if any(a['local']==owned['v4'] for a in addresses):
                return sig
            time.sleep(.1)
        raise RoutingError('VPN_CORE_FAILED')

    def stop_core(self, ident, owned):
        proc = self.processes.pop(ident,None)
        if proc and proc.poll() is None:
            proc.terminate()
            try: proc.wait(3)
            except subprocess.TimeoutExpired: proc.kill();proc.wait()
        self.signatures.pop(ident,None)
        if Path('/sys/class/net',owned['tun']).exists():
            if self.dns_interface == owned['tun']:
                run('resolvectl','revert',owned['tun'],check=False)
            run('ip','link','del',owned['tun'],check=False)
        (BASE/('vpn-'+ident+'.json')).unlink(missing_ok=True)

    def set_dns(self, interface, servers):
        journal=BASE/'dns-restore.json'
        saved=json.loads(journal.read_text()) if journal.exists() else {}
        if interface not in saved and not interface.startswith('rvpn'):
            saved[interface]={kind:run('resolvectl',kind,interface).split(':',1)[-1].split() for kind in ('dns','domain')}
            atomic(journal,saved)
        previous=getattr(self,'dns_interface','')
        if previous and previous!=interface:
            self.restore_dns(previous)
        run('resolvectl','dns',interface,*servers)
        run('resolvectl','domain',interface,'~.')
        self.dns_interface=interface
        (BASE/'dns-active').write_text(interface)
        run('resolvectl','flush-caches',check=False)

    def restore_dns(self, interface):
        journal=BASE/'dns-restore.json'
        saved=json.loads(journal.read_text()) if journal.exists() else {}
        if interface not in saved and not interface.startswith('rvpn'):
            return
        if Path('/sys/class/net',interface).exists():
            if interface in saved:
                for kind,values in saved[interface].items():
                    run('resolvectl',kind,interface,*(values or ['']))
                saved.pop(interface)
                atomic(journal,saved)
            else:
                run('resolvectl','revert',interface,check=False)

    def select_primary(self, state, current, policy):
        preferred = state['primary']
        now = time.monotonic()
        if self.selection != preferred:
            self.selection, self.absent_at, self.next_probe, self.probe_offset = preferred, None, 0, 0
        # Loss of Internet, DHCP or VPN is not physical disconnection.
        if current[preferred]['connected']:
            self.absent_at, self.next_probe = None, 0
            return preferred
        if self.absent_at is None:
            self.absent_at, self.next_probe = now, now + FALLBACK_DELAY
        active = self.status.get('primary', preferred)
        if active != preferred and current.get(active, {}).get('connected'):
            return active
        if now < self.next_probe:
            return preferred
        self.next_probe = now + PROBE_INTERVAL
        candidates = sorted((key for key,n in current.items() if key != preferred and n['online']),
                            key=lambda key: (key != state['system'], key))
        if not candidates:
            return preferred
        start = self.probe_offset % len(candidates)
        batch = (candidates[start:] + candidates[:start])[:4]
        self.probe_offset = (start + len(batch)) % len(candidates)

        def probe(key):
            network = current[key]
            owned = resources(network['slot'], key)
            if key in state['enabled']:
                vpn = policy['networks'].get(key, {})
                if not vpn.get('ready'):
                    return False
                return reachable(vpn['link'], owned['vpn'])
            return reachable(network, owned['direct'])

        with ThreadPoolExecutor(max_workers=4) as workers:
            results = list(workers.map(probe, batch))
        fresh = self.live(state)
        if fresh[preferred]['connected']:
            self.absent_at = None
            return preferred
        for key, ready in zip(batch, results):
            if ready and fresh.get(key, {}).get('online'):
                try:
                    run('resolvectl', 'status')
                except (RoutingError, OSError, subprocess.SubprocessError):
                    return preferred
                return key
        return preferred

    def apply(self, state, force=False, initialize=False):
        if not state['system']:
            return
        current=self.live(state)
        enabled=state['enabled']
        nodes=json.loads((BASE/'nodes.json').read_text())['nodes'] if (BASE/'nodes.json').exists() else {}
        signature=json.dumps([state,current,nodes],sort_keys=True)
        dead=any(p.poll() is not None for p in self.processes.values())
        active = self.status.get('primary', state['primary'])
        selection_due = (not current[state['primary']]['connected'] and
                         (active == state['primary'] or not current.get(active, {}).get('connected')) and
                         time.monotonic() >= self.next_probe)
        if not force and self.applied==signature and not dead and not selection_due:
            return
        RUN.mkdir(mode=0o755,parents=True,exist_ok=True)
        os.chmod(RUN,0o755)
        self.claim(state)
        owned={k:resources(n['slot'],k) for k,n in current.items()}
        for ident,n in current.items():
            r=owned[ident]
            for family in ('-4','-6'):
                self.table(family,r['direct'],n)
                self.rule(family,100,['fwmark',str(r['direct'])],r['direct'])
        # Install the guard before starting/changing any proxy or host route.
        script=firewall(current,enabled)
        if run('nft','list','table','inet',NFT,check=False):
            script=f'delete table inet {NFT}\n'+script
        run('nft','-c','-f','-',data=script)
        run('nft','-f','-',data=script)
        policy={'primary':state['primary'],'networks':{}}
        status={}
        for ident,n in current.items():
            r=owned[ident]
            if ident not in enabled:
                self.stop_core(ident,r)
                continue
            ready=False;error=''
            try:
                if ident not in nodes: raise RoutingError('INVALID_VLESS')
                if initialize or not n['online']: raise RoutingError('NETWORK_UNAVAILABLE')
                epoch=self.start_core(ident,n,nodes[ident],r)
                ready=n['online']
            except (RoutingError,OSError,ValueError,subprocess.SubprocessError) as exc:
                epoch=self.signatures.get(ident,'blocked')
                error=str(exc) if isinstance(exc,RoutingError) else 'VPN_CORE_FAILED'
            link={'id':ident,'name':r['tun'],'state':'configured','addresses':[
                  {'family':'inet','local':r['v4'],'prefixlen':30,'scope':'global'},
                  {'family':'inet6','local':r['v6'],'prefixlen':126,'scope':'global'}],
                  'gateways':[r['dns4'],r['dns6']],'dns':[r['dns4']]}
            policy['networks'][ident]={'enabled':True,'ready':ready,'epoch':epoch,'link':link}
            status[ident]={'enabled':True,'state':'active' if ready else 'blocked','error':error}
            for family in ('-4','-6'):
                self.table(family,r['vpn'],{**link,'online':ready})
                self.rule(family,100,['fwmark',str(r['vpn'])],r['vpn'])
        primary = self.select_primary(state, current, policy)
        selected = current[primary]
        policy['hostPrimary'] = primary
        atomic(RUN/'egress.json',policy,0o644)
        # Host policy contains connected networks so management replies stay local.
        for family in ('-4','-6'):
            if primary in enabled:
                r=owned[primary]
                route_network={'name':r['tun'],'online':Path('/sys/class/net',r['tun']).exists(),
                               'addresses':policy['networks'][primary]['link']['addresses'],
                               'gateways':policy['networks'][primary]['link']['gateways']}
            else:
                route_network=selected
            self.table(family,HOST_TABLE,route_network)
            for n in sorted(current.values(),key=lambda n:n['id']==primary):
                if not n['online']: continue
                for a in n.get('addresses',[]):
                    if (a['family']=='inet')==(family=='-4'):
                        subnet=str(ipaddress.ip_network(f"{a['local']}/{a['prefixlen']}",strict=False))
                        run('ip',family,'route','replace',subnet,'dev',n['name'],'src',a['local'],'table',str(HOST_TABLE))
            # Preserve explicit system routes (including dedicated service networks).
            connected={str(ipaddress.ip_network(f"{a['local']}/{a['prefixlen']}",strict=False))
                       for n in current.values() for a in n.get('addresses',[])}
            for route in json.loads(run('ip','-j',family,'route','show','table','main')):
                dst=route.get('dst','default')
                if dst!='default' and dst not in connected and not route.get('dev','').startswith('rvpn'):
                    run('ip',family,'route','replace','throw',dst,'table',str(HOST_TABLE))
            self.rule(family,31000,[],HOST_TABLE)
        if primary in enabled and policy['networks'][primary]['ready']:
            self.set_dns(owned[primary]['tun'],[owned[primary]['dns4']])
        elif primary not in enabled and selected['online'] and primary!=state['system']:
            versions={ipaddress.ip_address(a['local']).version for a in selected.get('addresses',[])}
            servers=[s for s in selected.get('dns',[]) if ipaddress.ip_address(s).version in versions]
            if not servers:
                servers=['1.1.1.1','8.8.8.8'] if 4 in versions else ['2606:4700:4700::1111','2001:4860:4860::8888']
            self.set_dns(selected['name'],servers)
        elif primary==state['system'] and self.dns_interface:
            self.restore_dns(self.dns_interface)
            self.dns_interface=''
            (BASE/'dns-active').unlink(missing_ok=True)
        self.status={'primary':primary,'selected':state['primary'],'fallback':primary!=state['primary'],
                     'system':state['system'],'vpn':status,'online':selected['online']}
        self.error=''
        self.applied=None if initialize else signature

    def change(self, ident, action, enabled, revision):
        with self.lock:
            state=self.state()
            if state['revision']!=revision: raise RoutingError('NETWORK_CONFLICT')
            if ident not in state['networks'] or type(enabled) is not bool: raise RoutingError('INVALID_NETWORK')
            old=json.loads(json.dumps(state))
            if enabled and not self.live(state)[ident]['online']: raise RoutingError('NETWORK_UNAVAILABLE')
            needs_dns = enabled and ((action=='primary' and ident!=state['system']) or (action=='vpn' and ident==state['primary']))
            if needs_dns:
                try: run('resolvectl','status')
                except (RoutingError,OSError): raise RoutingError('NETWORK_DNS_UNAVAILABLE') from None
            if action=='primary': state['primary']=ident if enabled else state['system']
            elif action=='vpn':
                state['enabled']=list(dict.fromkeys(state['enabled']+[ident])) if enabled else [v for v in state['enabled'] if v!=ident]
            else: raise RoutingError('INVALID_REQUEST')
            state['revision']+=1
            try:
                self.apply(state,True)
                if enabled and action=='vpn' and self.status['vpn'][ident]['state']!='active': raise RoutingError('VPN_CORE_FAILED')
                atomic(BASE/'routing.json',state)
            except Exception:
                self.apply(old,True)
                raise
            return state['revision']

    def view(self):
        with self.lock:
            state=self.state()
            return {**self.status,'revision':state['revision'],'error':self.error}

    def watch(self):
        while not self.stop.wait(2):
            with self.lock:
                try:
                    self.apply(self.state())
                except Exception:
                    self.error='NETWORK_APPLY_FAILED'

    def cleanup(self):
        self.stop.set()
        with self.lock:
            journal=BASE/'ownership.json'
            if not journal.exists(): return
            journal_state=json.loads(journal.read_text())
            owned=journal_state['networks']
            kinds=('direct','vpn') if journal_state.get('probeTables') else ('direct',)
            tables={HOST_TABLE}|{resources(n['slot'],k)[kind] for k,n in owned.items() for kind in kinds}
            for family in ('-4','-6'):
                for rule in json.loads(run('ip','-j',family,'rule','show')):
                    table=str(rule.get('table',''))
                    if table.isdigit() and int(table) in tables:
                        run('ip',family,'rule','del','pref',str(rule['priority']),'lookup',table,check=False)
                for table in tables:
                    run('ip',family,'route','flush','table',str(table),check=False)
            for ident,n in owned.items():
                self.restore_dns(n['name'])
                self.stop_core(ident,resources(n['slot'],ident))
            run('nft','delete','table','inet',NFT,check=False)
            (RUN/'egress.json').unlink(missing_ok=True)
            (BASE/'dns-active').unlink(missing_ok=True)
            (BASE/'dns-restore.json').unlink(missing_ok=True)
            journal.unlink(missing_ok=True)


if __name__=='__main__':
    import sys
    if sys.argv[1:]==['cleanup']:
        Controller().cleanup()
