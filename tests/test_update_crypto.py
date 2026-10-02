import importlib.util
import os
from pathlib import Path
import tempfile
import unittest

from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey

spec = importlib.util.spec_from_file_location('crypto', Path(__file__).resolve().parents[1] / 'deploy/update_crypto.py')
c = importlib.util.module_from_spec(spec)
spec.loader.exec_module(c)


class CryptoTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.source, self.encrypted, self.output = [self.root / n for n in ('source', 'package.rvu', 'verified.zip')]
        self.key, self.sign = os.urandom(32), os.urandom(32)
        self.public = Ed25519PrivateKey.from_private_bytes(self.sign).public_key()
        self.source.write_bytes(os.urandom(1024 * 1024 + 37))
        c.encrypt(self.source, self.encrypted, self.key, self.sign)

    def decrypt(self):
        c.decrypt(self.encrypted, self.output, self.key, self.public)

    def test_roundtrip_and_random_nonce(self):
        first = self.encrypted.read_bytes()
        self.decrypt()
        self.assertEqual(self.source.read_bytes(), self.output.read_bytes())
        c.encrypt(self.source, self.encrypted, self.key, self.sign)
        self.assertNotEqual(first[8:20], self.encrypted.read_bytes()[8:20])

    def test_tampering_never_exposes_plaintext(self):
        original = self.encrypted.read_bytes()
        for position in (0, 8, 20, 28, len(original)-97, len(original)-96, len(original)-1):
            data = bytearray(original); data[position] ^= 1
            self.encrypted.write_bytes(data)
            with self.subTest(position=position), self.assertRaises(ValueError): self.decrypt()
            self.assertFalse(self.output.exists())
            self.assertFalse(self.output.with_suffix('.decrypting').exists())

    def test_wrong_key_and_untrusted_signature(self):
        with self.assertRaisesRegex(ValueError, 'UPDATE_DECRYPT_FAILED'):
            c.decrypt(self.encrypted, self.output, os.urandom(32), self.public)
        with self.assertRaisesRegex(ValueError, 'UPDATE_SIGNATURE_INVALID'):
            c.decrypt(self.encrypted, self.output, self.key, Ed25519PrivateKey.generate().public_key())
        self.assertFalse(self.output.exists())
        self.assertFalse(self.output.with_suffix('.decrypting').exists())

    def test_length_bounds_and_plaintext_rejection(self):
        data = self.encrypted.read_bytes()
        for bad in (b'MZ'+b'0'*200, b'PK'+b'0'*200, data[:-1], data+b'x', data[:100]):
            self.encrypted.write_bytes(bad)
            with self.assertRaises(ValueError): self.decrypt()
        with self.assertRaises(ValueError): c.inspect(data[:28], data[-96:], True, self.public)

    def test_rollback_preserves_provisioned_key(self):
        script = (Path(__file__).resolve().parents[1] / "install.sh").read_text()
        rollback = script.split("rollback() {", 1)[1].split("\n}", 1)[0]
        self.assertNotIn("remove_app_path /etc/rykvo-voice", rollback)

    def test_key_file_is_regular_and_restricted(self):
        key = self.root / 'key'
        key.write_text(self.key.hex())
        key.chmod(0o644)
        if os.name == 'posix':
            with self.assertRaises(ValueError): c.read_key(key)
            link = self.root / 'link'; link.symlink_to(key)
            with self.assertRaises(ValueError): c.read_key(link)
        key.write_text('invalid')
        with self.assertRaises(ValueError): c.read_key(key)


if __name__ == '__main__': unittest.main()
