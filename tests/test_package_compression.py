import gzip
import io
import shutil
import subprocess
import sys
import tarfile
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parent.parent / 'deploy'))
import build
import release


class PackageCompressionTests(unittest.TestCase):
    def fixture(self, directory):
        raw = io.BytesIO()
        with tarfile.open(fileobj=raw, mode='w') as archive:
            for name, data, mode in [('VERSION', b'1.1.0\n', 0o644),
                                     ('bin/rykvo-auth', bytes(range(256)) * 5120, 0o755),
                                     ('web/index.html', '<title>模块</title>'.encode(), 0o644),
                                     ('licenses/core.txt', b'license\n', 0o644)]:
                member = tarfile.TarInfo(name)
                member.size, member.mode = len(data), mode
                archive.addfile(member, io.BytesIO(data))
        path = Path(directory) / 'runtime.tar.gz'
        path.write_bytes(gzip.compress(raw.getvalue(), compresslevel=1, mtime=0))
        return path, raw.getvalue()

    def compressor(self, transform=lambda raw: gzip.compress(raw, mtime=0)):
        def run(command, *, stdout, check, timeout):
            self.assertEqual(command[:4], ['zopfli', '--gzip', '--i5', '-c'])
            self.assertTrue(check)
            self.assertEqual(timeout, 1800)
            stdout.write(transform(Path(command[4]).read_bytes()))
        return run

    def test_smaller_gzip_preserves_every_tar_byte_and_standard_extraction(self):
        with tempfile.TemporaryDirectory() as directory:
            path, raw = self.fixture(directory)
            before = path.stat().st_size
            with patch.object(build.subprocess, 'run', side_effect=self.compressor()):
                build.compact_archive(path)
            self.assertLess(path.stat().st_size, before)
            self.assertEqual(gzip.decompress(path.read_bytes()), raw)
            with tarfile.open(path, 'r:gz') as archive:
                self.assertEqual(archive.getmember('bin/rykvo-auth').mode, 0o755)
                self.assertEqual(archive.extractfile('bin/rykvo-auth').read(), bytes(range(256)) * 5120)
            self.assertEqual(list(Path(directory).iterdir()), [path])
            release.extract(path, Path(directory) / 'extracted')
            self.assertEqual((Path(directory) / 'extracted/web/index.html').read_text(encoding='utf-8'),
                             '<title>模块</title>')

    def test_changed_truncated_or_extended_content_never_replaces_original(self):
        transforms = [lambda raw: gzip.compress(b'changed'),
                      lambda raw: gzip.compress(raw[:-1]),
                      lambda raw: gzip.compress(raw + b'extra'),
                      lambda raw: gzip.compress(raw)[:-4],
                      lambda raw: b'not gzip']
        for transform in transforms:
            with self.subTest(transform=transform), tempfile.TemporaryDirectory() as directory:
                path, _ = self.fixture(directory)
                original = path.read_bytes()
                with patch.object(build.subprocess, 'run', side_effect=self.compressor(transform)):
                    with self.assertRaises((ValueError, OSError, EOFError)):
                        build.compact_archive(path)
                self.assertEqual(path.read_bytes(), original)
                self.assertEqual(list(Path(directory).iterdir()), [path])

    def test_failed_compressor_preserves_original_and_cleans_temporary_files(self):
        errors = [FileNotFoundError('zopfli'), subprocess.CalledProcessError(1, 'zopfli'),
                  subprocess.TimeoutExpired('zopfli', 1800)]
        for error in errors:
            with self.subTest(error=error), tempfile.TemporaryDirectory() as directory:
                path, _ = self.fixture(directory)
                original = path.read_bytes()
                with patch.object(build.subprocess, 'run', side_effect=error):
                    with self.assertRaises(type(error)):
                        build.compact_archive(path)
                self.assertEqual(path.read_bytes(), original)
                self.assertEqual(list(Path(directory).iterdir()), [path])

    def test_equal_or_larger_valid_encoding_keeps_original(self):
        for level in (0, 1):
            with self.subTest(level=level), tempfile.TemporaryDirectory() as directory:
                path, _ = self.fixture(directory)
                original = path.read_bytes()
                encode = lambda raw: gzip.compress(raw, compresslevel=level, mtime=0)
                with patch.object(build.subprocess, 'run', side_effect=self.compressor(encode)):
                    build.compact_archive(path)
                self.assertEqual(path.read_bytes(), original)
                self.assertEqual(list(Path(directory).iterdir()), [path])

    @unittest.skipUnless(shutil.which('zopfli'), 'Native build-time compressor required')
    def test_native_compressor_and_standard_gzip_decoder(self):
        with tempfile.TemporaryDirectory() as directory:
            path, raw = self.fixture(directory)
            before = path.stat().st_size
            build.compact_archive(path)
            self.assertLess(path.stat().st_size, before)
            self.assertEqual(gzip.decompress(path.read_bytes()), raw)


if __name__ == '__main__':
    unittest.main()
