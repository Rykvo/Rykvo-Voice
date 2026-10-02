import importlib.util
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey

ROOT = Path(__file__).resolve().parents[1]


def load(name, file):
    spec = importlib.util.spec_from_file_location(name, ROOT / 'deploy' / file)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


c = load('crypto_install_test', 'update_crypto.py')
r = load('release_install_test', 'release.py')


class InstallKeyTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve()
        self.key, self.sign = os.urandom(32), os.urandom(32)
        self.public = Ed25519PrivateKey.from_private_bytes(self.sign).public_key()
        self.data = c.create_install_key(self.key, self.sign)
        self.path = self.root / 'host' / 'update.key'
        patched = patch.object(c, 'public_key', return_value=self.public)
        patched.start(); self.addCleanup(patched.stop)

    def test_same_key_installs_on_multiple_hosts_without_input(self):
        for host in ('one', 'two'):
            path = self.root / host / 'update.key'
            c.provision_install_key(self.data, path)
            self.assertEqual(bytes.fromhex(path.read_text().strip()), self.key)
            if os.name == 'posix':
                self.assertEqual(path.stat().st_mode & 0o777, 0o600)
                self.assertEqual(path.parent.stat().st_mode & 0o777, 0o700)
            self.assertEqual([p.name for p in path.parent.iterdir()], ['update.key'])

    def test_existing_key_never_overwritten(self):
        c.provision_install_key(self.data, self.path)
        before = self.path.stat().st_mtime_ns
        with patch.object(c, 'read_key', return_value=self.key) as read:
            c.provision_install_key(self.data, self.path)
            read.assert_called_once_with(self.path)
        self.assertEqual(self.path.stat().st_mtime_ns, before)
        other = c.create_install_key(os.urandom(32), self.sign)
        with patch.object(c, 'read_key', return_value=self.key), self.assertRaisesRegex(ValueError, 'INSTALL_KEY_CONFLICT'):
            c.provision_install_key(other, self.path)
        self.assertEqual(bytes.fromhex(self.path.read_text().strip()), self.key)

    def test_invalid_signature_never_writes_key(self):
        for index in (0, 8, 39, 40, 103):
            bad = bytearray(self.data); bad[index] ^= 1
            with self.assertRaisesRegex(ValueError, 'INSTALL_SIGNATURE_INVALID'):
                c.provision_install_key(bytes(bad), self.path)
            self.assertFalse(self.path.parent.exists())
        for bad in (self.data[:-1], self.data + b'x', b'MZ'+b'0'*102):
            with self.assertRaises(ValueError): c.inspect_install_key(bad)
        with self.assertRaises(ValueError):
            c.inspect_install_key(self.data, Ed25519PrivateKey.generate().public_key())

    @unittest.skipUnless(os.name == 'posix', 'Linux paths')
    def test_symlink_and_read_failure_are_not_repaired_by_overwriting(self):
        real = self.root / 'real'; real.mkdir()
        self.path.parent.symlink_to(real, target_is_directory=True)
        with self.assertRaises(ValueError): c.provision_install_key(self.data, self.path)
        self.assertFalse((real / 'update.key').exists())

    def test_bootstrap_downloads_signed_material_once_and_cleans_it(self):
        base = 'https://github.com/Rykvo/Rykvo-Voice/releases/download/release/v1.1.5'
        def download(url, destination, **options):
            self.assertEqual(url, base+'/install.rvk')
            self.assertEqual(options, {'max_size': 104})
            Path(destination).write_bytes(self.data)
        with patch.object(c, 'DECRYPT_KEY', self.path), patch.object(r, 'download', side_effect=download) as fetch:
            r.ensure_install_key(c, base, self.root)
            with patch.object(c, 'read_key', return_value=self.key) as read:
                r.ensure_install_key(c, base, self.root)
                read.assert_called_once_with(self.path)
            self.assertEqual(fetch.call_count, 1)
        self.assertFalse((self.root / 'install.rvk').exists())

    def test_bootstrap_rejects_bad_material_and_preserves_existing_errors(self):
        def download(url, destination, **options): Path(destination).write_bytes(b'bad')
        with patch.object(c, 'DECRYPT_KEY', self.path), patch.object(r, 'download', side_effect=download):
            with self.assertRaises(ValueError): r.ensure_install_key(c, 'https://example.test', self.root)
        self.assertFalse(self.path.exists())
        self.assertFalse((self.root / 'install.rvk').exists())
        self.path.parent.mkdir(); self.path.write_text('damaged')
        with patch.object(c, 'DECRYPT_KEY', self.path), patch.object(r, 'download') as fetch:
            with self.assertRaises(ValueError): r.ensure_install_key(c, 'https://example.test', self.root)
            fetch.assert_not_called()
        self.assertEqual(self.path.read_text(), 'damaged')

    def test_entrypoint_keeps_one_command_without_key_prompt(self):
        script = (ROOT / 'deploy.sh').read_text(encoding='utf-8')
        self.assertNotIn('read -', script)
        self.assertNotIn('/dev/tty', script)
        self.assertIn('bootstrap "$temporary" "$arch"', script)

    @unittest.skipUnless(hasattr(os, 'geteuid') and os.geteuid() == 0, 'Root-only temporary-path integration')
    def test_root_fresh_hosts_provision_then_decrypt_without_input(self):
        plain, encrypted = self.root/'payload', self.root/'test.rvu'
        plain.write_bytes(b'bootstrap integration fixture')
        c.encrypt(plain, encrypted, self.key, self.sign)
        def download(url, destination, **options): Path(destination).write_bytes(self.data)
        for name in ('fresh-one', 'fresh-two'):
            key_path = self.root/name/'update.key'
            with patch.object(c, 'DECRYPT_KEY', key_path), patch.object(r, 'download', side_effect=download) as fetch:
                r.ensure_install_key(c, 'https://fixture.test', self.root)
                self.assertEqual(c.read_key(), self.key)
                self.assertEqual(key_path.stat().st_uid, 0)
                c.decrypt(encrypted, self.root/(name+'.verified'))
                r.ensure_install_key(c, 'https://fixture.test', self.root)
                self.assertEqual(fetch.call_count, 1)
            self.assertEqual((self.root/(name+'.verified')).read_bytes(), plain.read_bytes())


if __name__ == '__main__': unittest.main()
