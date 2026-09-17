"""Provisioning security regression tests; run in the provisioning Python env."""
import contextlib
import csv
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("provision", Path(__file__).with_name("provision-device.py"))
provision = importlib.util.module_from_spec(spec)
spec.loader.exec_module(provision)


class ProvisionTests(unittest.TestCase):
    def generate(self, output):
        console = io.StringIO()
        with patch.object(sys, "argv", ["provision-device", "--output", str(output)]), contextlib.redirect_stdout(console):
            provision.main()
        return json.loads((output / "pairing.json").read_text()), console.getvalue()

    def test_unique_credentials_and_verifier_match(self):
        with tempfile.TemporaryDirectory() as directory:
            first, text = self.generate(Path(directory) / "first")
            second, _ = self.generate(Path(directory) / "second")
            self.assertNotEqual(first["password"], second["password"])
            self.assertNotEqual(first["username"], second["username"])
            self.assertNotIn(first["password"], text)
            self.assertEqual((Path(directory) / "first/pairing.bin").stat().st_size, 0x6000)
            with (Path(directory) / "first/pairing.csv").open() as source:
                values = {row["key"]: row["value"] for row in csv.DictReader(source)}
            salt = bytes.fromhex(values["salt"])
            self.assertEqual(len(salt), 16)
            digest = hashlib.sha512((first["username"] + ":" + first["password"]).encode()).digest()
            x = int.from_bytes(hashlib.sha512(salt + digest).digest(), "big")
            n, g = provision.srp_parameters()
            self.assertEqual(pow(g, x, n).to_bytes(384, "big").hex(), values["verifier"])

    def test_existing_credentials_never_overwritten(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory)
            self.generate(output)
            original = (output / "pairing.json").read_bytes()
            with self.assertRaises(SystemExit), contextlib.redirect_stderr(io.StringIO()):
                self.generate(output)
            self.assertEqual(original, (output / "pairing.json").read_bytes())

    def test_nvs_success_system_exit_still_writes_credentials(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory)
            def successful_exit():
                (output / "pairing.bin").write_bytes(bytes(0x6000))
                raise SystemExit(0)
            with patch("esp_idf_nvs_partition_gen.nvs_partition_gen.main", side_effect=successful_exit):
                self.generate(output)
            self.assertTrue((output / "pairing.json").is_file())

    def test_nvs_failure_does_not_report_pairing_success(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory)
            with patch("esp_idf_nvs_partition_gen.nvs_partition_gen.main", side_effect=SystemExit(2)):
                with self.assertRaises(SystemExit):
                    self.generate(output)
            self.assertFalse((output / "pairing.json").exists())


if __name__ == "__main__":
    unittest.main()
