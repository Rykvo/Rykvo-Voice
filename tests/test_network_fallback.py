import copy
import json
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch
import sys

sys.path.insert(0, str(Path(__file__).resolve().parent.parent / 'deploy'))
import network_runtime as r


class FallbackTests(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        base = patch.object(r, 'BASE', Path(temp.name))
        base.start()
        self.addCleanup(base.stop)
        self.c = r.Controller()
        self.state = {'primary': 'a', 'system': 'b', 'enabled': [], 'networks': {}}
        self.current = {key: {'id': key, 'name': 'net-' + key, 'slot': i + 1,
                             'connected': True, 'online': True} for i, key in enumerate('abc')}
        self.policy = {'networks': {}}

    def choose(self, now, success=('b', 'c')):
        with patch.object(r.time, 'monotonic', return_value=now), \
             patch.object(self.c, 'live', return_value=self.current), \
             patch.object(r, 'run', return_value=''), \
             patch.object(r, 'reachable', side_effect=lambda n, mark: n['id'] in success) as probe:
            selected = self.c.select_primary(self.state, self.current, self.policy)
        return selected, probe.call_count

    def test_internet_or_dhcp_loss_does_not_switch(self):
        self.current['a']['online'] = False
        for now in (0, 10, 100):
            self.assertEqual(self.choose(now), ('a', 0))

    def test_unplug_debounce_and_module_network_candidate(self):
        self.current['a'].update(connected=False, online=False)
        self.current['c']['modules'] = ['module-16']
        before = copy.deepcopy(self.state)
        self.assertEqual(self.choose(0), ('a', 0))
        self.assertEqual(self.choose(4), ('a', 0))
        selected, count = self.choose(5, ('c',))
        self.assertEqual((selected, count), ('c', 2))
        self.assertEqual(self.state, before)
        self.c.status['primary'] = selected
        self.current['c']['online'] = False
        self.assertEqual(self.choose(30), ('c', 0))

    def test_preferred_reinserted_even_without_internet(self):
        self.current['a'].update(connected=False, online=False)
        self.choose(0)
        self.c.status['primary'] = self.choose(6)[0]
        self.current['a']['connected'] = True
        self.assertEqual(self.choose(7), ('a', 0))

    def test_unreachable_networks_are_not_promoted_or_continuously_probed(self):
        self.current['a'].update(connected=False, online=False)
        self.choose(0)
        self.assertEqual(self.choose(6, ())[0], 'a')
        self.assertEqual(self.choose(7), ('a', 0))
        self.assertEqual(self.choose(21)[0], 'b')

    def test_failed_vpn_never_probes_direct(self):
        self.current['a'].update(connected=False, online=False)
        self.state['enabled'] = ['b']
        self.policy['networks']['b'] = {'ready': False}
        self.choose(0)
        self.assertEqual(self.choose(6), ('c', 1))

    def test_enabled_vpn_uses_tunnel_and_owned_mark(self):
        self.current['a'].update(connected=False, online=False)
        self.current['c']['online'] = False
        self.state['enabled'] = ['b']
        self.policy['networks']['b'] = {'ready': True, 'link': {'id': 'b', 'name': 'rvpnfixture'}}
        self.choose(0)
        with patch.object(r.time, 'monotonic', return_value=6), patch.object(self.c, 'live', return_value=self.current), \
             patch.object(r, 'run', return_value=''), patch.object(r, 'reachable', return_value=True) as probe:
            self.assertEqual(self.c.select_primary(self.state, self.current, self.policy), 'b')
        probe.assert_called_once_with(self.policy['networks']['b']['link'], r.resources(2, 'b')['vpn'])

    def test_new_preference_gets_its_own_debounce(self):
        self.current['a'].update(connected=False, online=False)
        self.choose(0)
        self.state['primary'] = 'b'
        self.current['b'].update(connected=False, online=False)
        self.assertEqual(self.choose(100), ('b', 0))

    def test_candidate_removed_during_probe_is_not_selected(self):
        self.current['a'].update(connected=False, online=False)
        self.choose(0)
        fresh = copy.deepcopy(self.current)
        fresh['b'].update(connected=False, online=False)
        fresh['c'].update(connected=False, online=False)
        with patch.object(r.time, 'monotonic', return_value=6), patch.object(self.c, 'live', return_value=fresh), \
             patch.object(r, 'reachable', return_value=True):
            self.assertEqual(self.c.select_primary(self.state, self.current, self.policy), 'a')

    def test_live_distinguishes_admin_down_from_unplugged(self):
        for flags, connected in [(['UP', 'LOWER_UP'], True), (['UP', 'NO-CARRIER'], False), ([], True)]:
            with self.subTest(flags=flags):
                state = {'networks': {'a': {'name': 'net-a'}}}
                def run(*args, **kwargs):
                    return json.dumps([{'ifname': 'net-a', 'flags': flags}]) if 'address' in args else '[]'
                with patch.object(r, 'run', side_effect=run), patch.object(r, 'stable_id', return_value='a'):
                    self.assertEqual(self.c.live(state)['a']['connected'], connected)

    def test_many_candidates_are_bounded_and_rotate(self):
        self.current.update({str(i): {'id': str(i), 'name': 'net-' + str(i), 'slot': i + 4,
                                     'connected': True, 'online': True} for i in range(20)})
        self.current['a'].update(connected=False, online=False)
        self.choose(0)
        seen = set()
        def probe(n, mark): seen.add(n['id']); return False
        with patch.object(self.c, 'live', return_value=self.current), patch.object(r, 'reachable', side_effect=probe):
            for now in range(5, 96, 15):
                with patch.object(r.time, 'monotonic', return_value=now):
                    before = len(seen)
                    self.c.select_primary(self.state, self.current, self.policy)
                    self.assertLessEqual(len(seen) - before, 4)
        self.assertEqual(len(seen), len(self.current) - 1)


class BootPolicyTests(unittest.TestCase):
    def test_voice_waits_for_guard_not_global_network_online(self):
        root = Path(__file__).resolve().parent.parent
        network = (root/'deploy/rykvo-network.service').read_text()
        auth = (root/'deploy/rykvo-auth.service').read_text()
        sip = (root/'deploy/rykvo-sip-network.service').read_text()
        self.assertNotIn('network-online.target', network + sip)
        self.assertIn('Type=notify', network)
        self.assertIn('rykvo-network.service', auth.split('After=', 1)[1].splitlines()[0])
        helper = (root/'deploy/network-control.py').read_text()
        self.assertLess(helper.index('initialize=True'), helper.index("ready.sendall(b'READY=1')"))
        self.assertIn('Restart=on-failure', sip)


if __name__ == '__main__':
    unittest.main()
