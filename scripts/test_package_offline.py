"""Release completeness and rejection checks, without hardware or credentials."""
import contextlib
import hashlib
import importlib.util
import io
from pathlib import Path
import tempfile
import unittest
import zipfile

spec = importlib.util.spec_from_file_location('package_offline', Path(__file__).with_name('package-offline.py'))
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class OfflinePackageTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.assets = Path(self.tmp.name)
        for name in ('uart2llm-agent-windows-x64.exe', 'LICENSES.txt', 'README.md'):
            (self.assets / name).write_bytes(b'test fixture')
        for archive, prefix in module.ARCHIVES.items():
            names = sorted(name[len(prefix) + 1:] for name in module.REQUIRED if name.startswith(prefix + '/'))
            with zipfile.ZipFile(self.assets / archive, 'w') as output:
                for name in names:
                    if name != 'SHA256SUMS':
                        output.writestr(name, b'test fixture')
                if 'SHA256SUMS' in names:
                    output.writestr('SHA256SUMS', ''.join(
                        hashlib.sha256(b'test fixture').hexdigest() + '  ' + name + '\n'
                        for name in names if name != 'SHA256SUMS'))

    def run_package(self):
        with contextlib.redirect_stdout(io.StringIO()):
            return module.package(self.assets)

    def test_complete_bundle_and_hashes(self):
        with zipfile.ZipFile(self.run_package()) as output:
            for line in output.read('uart2llm/SHA256SUMS').decode().splitlines():
                digest, name = line.split('  ', 1)
                self.assertEqual(digest, hashlib.sha256(output.read('uart2llm/' + name)).hexdigest())
            self.assertTrue(all('uart2llm/' + name in output.namelist() for name in module.REQUIRED))

    def test_missing_tools_rejected(self):
        with zipfile.ZipFile(self.assets / 'uart2llm-device-tools-windows-x64.zip', 'w'):
            pass
        with self.assertRaisesRegex(ValueError, 'Missing'):
            self.run_package()

    def test_corrupt_firmware_manifest_rejected(self):
        archive = self.assets / 'uart2llm-firmware-uart.zip'
        with zipfile.ZipFile(archive) as source:
            files = {name: source.read(name) for name in source.namelist()}
        files['uart2llm.bin'] = b'changed'
        with zipfile.ZipFile(archive, 'w') as output:
            for name, data in files.items():
                output.writestr(name, data)
        with self.assertRaisesRegex(ValueError, 'checksum mismatch'):
            self.run_package()

    def test_unsafe_and_private_members_rejected(self):
        archive = self.assets / 'uart2llm-device-tools-windows-x64.zip'
        original = archive.read_bytes()
        for name in ('../escape', '/absolute', 'C:/escape', 'tools\\..\\escape', 'pairing.json'):
            with self.subTest(name=name):
                archive.write_bytes(original)
                with zipfile.ZipFile(archive, 'a') as output:
                    output.writestr(name, b'forbidden fixture')
                with self.assertRaises(ValueError):
                    self.run_package()


if __name__ == '__main__':
    unittest.main()
