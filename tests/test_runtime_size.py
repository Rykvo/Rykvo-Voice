import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parent.parent / 'deploy'))
import build


class RuntimeSizeTests(unittest.TestCase):
    def test_only_expected_elf_architecture_is_stripped(self):
        with tempfile.TemporaryDirectory() as temp:
            binary = Path(temp) / 'cloudflared'
            for arch, tool, machine in [('amd64', 'x86_64-linux-gnu-strip', 62),
                                        ('arm64', 'aarch64-linux-gnu-strip', 183)]:
                header = bytearray(128)
                header[:6] = b'\x7fELF\x02\x01'
                header[18:20] = machine.to_bytes(2, 'little')
                binary.write_bytes(header)
                with patch.object(build.subprocess, 'run') as run:
                    build.strip_runtime(binary, arch)
                    run.assert_called_once_with([tool, '--strip-unneeded', str(binary)], check=True)
                with patch.object(build.subprocess, 'run') as run:
                    with self.assertRaises(ValueError):
                        build.strip_runtime(binary, 'arm64' if arch == 'amd64' else 'amd64')
                    run.assert_not_called()

    def test_upstream_checksum_verification_precedes_stripping(self):
        source = Path(build.__file__).read_text(encoding='utf-8')
        self.assertLess(source.index('runtime[arch])'), source.index("strip_runtime(bundle / 'bin/cloudflared'"))


if __name__ == '__main__':
    unittest.main()
