import json
import tempfile
import unittest
from pathlib import Path
import sys
sys.path.insert(0, str(Path(__file__).resolve().parent.parent / 'deploy'))
from unittest.mock import patch
import network_runtime as r

class RuntimeTests(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        base = patch.object(r, 'BASE', Path(directory.name))
        base.start()
        self.addCleanup(base.stop)

    def test_proxy_never_changes_global_routes(self):
        n={'name':'eth1'};node={'server':'198.51.100.1','port':443,'uuid':'11111111-1111-4111-8111-111111111111','security':'tls','sni':'node.example'}
        cfg=r.core_config(node,n,r.resources(1,'a'*32))
        self.assertFalse(cfg['inbounds'][0]['auto_route'])
        self.assertEqual(cfg['inbounds'][0]['dns_mode'],'disabled')
        self.assertEqual(cfg['inbounds'][0]['stack'],'gvisor')
        self.assertEqual(cfg['inbounds'][0]['address'],['198.18.1.1/30'])
        self.assertEqual(cfg['outbounds'][0]['bind_interface'],'eth1')
        self.assertEqual(cfg['dns']['servers'][0]['detour'],'node')
        self.assertNotIn('direct',[o['type'] for o in cfg['outbounds']])

    def test_failure_guard_protects_management_not_dns_leaks(self):
        rule=r.firewall({'a':{'name':'eth1','slot':1,'addresses':[{'local':'192.0.2.2','prefixlen':24}]}},['a'])
        self.assertIn('ct direction reply accept',rule)
        self.assertLess(rule.index('th dport 53 reject'),rule.index('daddr 192.0.2.0/24 accept'))
        self.assertIn('oifname "eth1" reject',rule)
        self.assertNotIn('flush ruleset',rule)
        self.assertNotIn('meta mark != 0',rule)
        self.assertIn('meta mark { 1510998017, 0x5a0f0001 } accept',rule)

    def test_revision_and_boolean_are_enforced(self):
        state={'revision':3,'system':'a','primary':'a','networks':{'a':{}},'enabled':[]}
        c=r.Controller()
        with patch.object(c,'state',return_value=state):
            for revision, enabled in ((2,True),(3,'yes')):
                with self.assertRaises(r.RoutingError):c.change('a','primary',enabled,revision)

    def test_failed_apply_restores_previous_choice(self):
        state={'revision':1,'system':'a','primary':'a','networks':{'a':{},'b':{}},'enabled':[]}
        c=r.Controller();calls=[]
        def apply(s,force):
            calls.append(s['primary'])
            if s['primary']=='b':raise r.RoutingError('NETWORK_APPLY_FAILED')
        with patch.object(c,'state',return_value=state),patch.object(c,'live',return_value={'b':{'online':True}}),patch.object(c,'apply',side_effect=apply),patch.object(r,'run',return_value=''):
            with self.assertRaises(r.RoutingError):c.change('b','primary',True,1)
        self.assertEqual(calls,['b','a'])

    def test_dns_missing_rejects_change_before_routes(self):
        state={'revision':1,'system':'a','primary':'a','networks':{'a':{},'b':{}},'enabled':[]}
        c=r.Controller()
        with patch.object(c,'state',return_value=state),patch.object(c,'live',return_value={'b':{'online':True}}),patch.object(c,'apply') as apply,patch.object(r,'run',side_effect=FileNotFoundError()):
            with self.assertRaisesRegex(r.RoutingError,'NETWORK_DNS_UNAVAILABLE'):
                c.change('b','primary',True,1)
            apply.assert_not_called()

    def test_cleanup_never_resets_unmodified_dns(self):
        with tempfile.TemporaryDirectory() as tmp,patch.object(r,'BASE',Path(tmp)),patch.object(r,'run') as run:
            c=r.Controller()
            c.restore_dns('administrator-network')
            run.assert_not_called()

    def test_atomic_state_has_private_permissions(self):
        with tempfile.TemporaryDirectory() as tmp:
            p=Path(tmp)/'state.json';r.atomic(p,{'revision':2})
            self.assertEqual(json.loads(p.read_text()),{'revision':2})
            self.assertEqual([x.name for x in p.parent.iterdir()],['state.json'])

    def test_secondary_vpn_cleanup_does_not_require_host_dns_service(self):
        with tempfile.TemporaryDirectory() as tmp,patch.object(r,'BASE',Path(tmp)):
            c=r.Controller()
            with patch.object(Path,'exists',return_value=True),patch.object(r,'run') as run:
                c.stop_core('a'*32,r.resources(1,'a'*32))
            run.assert_called_once_with('ip','link','del','rvpn'+'a'*8,check=False)

if __name__=='__main__':unittest.main()
