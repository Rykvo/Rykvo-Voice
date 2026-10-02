"""Run in unshare -mn with a private sysfs mount; never changes the host network."""
import json
import os
import socketserver
import tempfile
import threading
from pathlib import Path
import sys
sys.path.insert(0, str(Path(__file__).resolve().parent.parent / 'deploy'))
import network_runtime as r

if __name__ != '__main__':
    import unittest
    raise unittest.SkipTest('Run explicitly in an isolated Linux network namespace')

assert os.geteuid()==0 and os.readlink('/proc/self/ns/net')!=os.readlink('/proc/1/ns/net')
assert os.readlink('/proc/self/ns/mnt')!=os.readlink('/proc/1/ns/mnt')
r.CORE = os.environ['RYKVO_TEST_NETWORK_CORE']
assert Path(r.CORE).is_file()
r.run('ip','link','set','lo','up')
# Reproduce an active module lease, which made the old core choose priority 0.
for family in ('-4','-6'):
    r.run('ip',family,'rule','add','pref','1','fwmark','1375731713','lookup','1375731713')
original={f:r.run('ip','-j',f,'rule','show') for f in ('-4','-6')}

class Proxy(socketserver.BaseRequestHandler):
    def handle(self):
        self.request.settimeout(5)
        stream=self.request.makefile('rb')
        header=stream.read(18)
        assert len(header)==18 and header[0]==0
        stream.read(header[17])
        command=stream.read(4)
        assert command[0]==1 and command[3] in (1,3)
        stream.read(4 if command[3]==1 else 16)
        while stream.readline() not in (b'\r\n',b''):
            pass
        self.request.sendall(b'\x00\x00HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nOK')

server=socketserver.ThreadingTCPServer(('127.0.0.1',0),Proxy)
threading.Thread(target=server.serve_forever,daemon=True).start()
with tempfile.TemporaryDirectory() as tmp:
    r.BASE=Path(tmp)
    c=r.Controller()
    node={'server':'127.0.0.1','port':server.server_address[1],'uuid':'11111111-1111-4111-8111-111111111111','security':'none'}
    network={'name':'lo','addresses':[],'gateways':[]}
    for count in range(3):
        for slot,ident in ((1,'a'*32),(2,'b'*32)):
            owned=r.resources(slot,ident)
            try:
                c.start_core(ident,network,node,owned)
                addrs=json.loads(r.run('ip','-j','addr','show','dev',owned['tun']))[0]['addr_info']
                assert any(a['local']==owned['v6'] for a in addrs)
                for family in original:
                    assert r.run('ip','-j',family,'rule','show')==original[family], 'core altered policy rules'
                for family,dest,source,url in (
                    ('-4','192.0.2.9/32',owned['v4'],'http://192.0.2.9/'),
                    ('-6','2001:db8::9/128',owned['v6'],'http://[2001:db8::9]/')):
                    r.run('ip',family,'route','add',dest,'dev',owned['tun'],'src',source)
                    assert r.run('curl','--noproxy','*','--silent','--fail','--max-time','4','--interface',owned['tun'],url)=='OK'
            finally:
                c.stop_core(ident,owned)
            for family in original:
                assert r.run('ip','-j',family,'rule','show')==original[family], 'core cleanup altered policy rules'
    assert json.loads(r.run('ip','-j','-6','route','get','::1'))[0]['dev']=='lo'
    print('PASS: six VPN starts/stops with active module leases; IPv4/IPv6 TCP via VLESS; rules unchanged; IPv6 loopback preserved')
server.shutdown()
server.server_close()
