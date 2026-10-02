"""Run only with unshare -n: no host route or resolver mutation."""
import json
import os
import tempfile
from pathlib import Path
import sys
sys.path.insert(0, str(Path(__file__).resolve().parent.parent / 'deploy'))
import network_runtime as r
if __name__ != '__main__':
    import unittest
    raise unittest.SkipTest('Run explicitly in an isolated Linux network namespace')

assert os.geteuid()==0 and os.readlink('/proc/self/ns/net')!=os.readlink('/proc/1/ns/net')
real=r.run
def run(*args,**kwargs):
    if args[0]=='resolvectl':return ''
    return real(*args,**kwargs)
r.run=run
r.stable_id=lambda name: {'net-a':'a'*32,'net-b':'b'*32}[name]
with tempfile.TemporaryDirectory() as tmp:
    r.BASE=Path(tmp)/'state';r.RUN=Path(tmp)/'run'
    real('ip','link','set','lo','up')
    nets={}
    for slot,name,ip,gateway in [(1,'net-a','192.0.2.2','192.0.2.1'),(2,'net-b','198.51.100.2','198.51.100.1')]:
        real('ip','link','add',name,'type','dummy');real('ip','addr','add',ip+'/24','dev',name);real('ip','link','set',name,'up')
        real('ip','route','add','default','via',gateway,'dev',name,'table','main' if slot==1 else '1390000002')
        ident=('a' if slot==1 else 'b')*32
        nets[ident]={'id':ident,'name':name,'slot':slot,'dns':[gateway],'gateways':[gateway], 'addresses':[{'family':'inet','local':ip,'prefixlen':24,'scope':'global'}]}
    state={'revision':1,'system':'a'*32,'primary':'a'*32,'networks':nets,'enabled':[]}
    r.atomic(r.BASE/'routing.json',state)
    original=real('ip','-j','route','show','table','main')
    c=r.Controller()
    # Dummy links omit LOWER_UP in ip output; use kernel UP for this isolated fixture.
    live=c.live
    def fixture(s):
        values=live(s)
        for n in values.values():
            n['connected']='UP' in json.loads(real('ip','-j','link','show','dev',n['name']))[0]['flags']
            n['online']='UP' in json.loads(real('ip','-j','link','show','dev',n['name']))[0]['flags'] and bool(n['gateways'])
        return values
    c.live=fixture
    c.apply(state,True)
    assert json.loads(real('ip','-j','route','get','203.0.113.9'))[0]['dev']=='net-a'
    c.change('b'*32,'primary',True,1)
    assert json.loads(real('ip','-j','route','get','203.0.113.9'))[0]['dev']=='net-b'
    assert json.loads(real('ip','-j','route','get','192.0.2.9','from','192.0.2.2'))[0]['dev']=='net-a'
    real('ip','link','set','net-b','down')
    c.apply(c.state(),True)
    assert not c.view()['fallback']
    c.absent_at -= r.FALLBACK_DELAY + 1
    c.next_probe = 0
    r.reachable=lambda network,mark: network['name']=='net-a'
    c.apply(c.state(),True)
    assert c.view()['fallback']
    assert json.loads((r.RUN/'egress.json').read_text())['primary']=='b'*32
    assert json.loads(real('ip','-j','route','get','203.0.113.9'))[0]['dev']=='net-a'
    real('ip','link','set','net-b','up')
    # A DHCP client renews the lease/default after reconnect; dummy links have none.
    real('ip','route','replace','default','via','198.51.100.1','dev','net-b','table','1390000002')
    c.apply(c.state(),True)
    assert json.loads(real('ip','-j','route','get','203.0.113.9'))[0]['dev']=='net-b'
    c.cleanup()
    assert real('ip','-j','route','show','table','main')==original
    assert len(json.loads(real('ip','-j','-4','rule','show')))==3
    assert real('nft','list','tables')==''
    print('PASS: primary A/B, source reply, disconnect fallback, reconnect, exact cleanup; system main table unchanged')
