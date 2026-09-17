#!/usr/bin/env python3
"""Create unique Protocomm Security 2 credentials and the pairing NVS image.

Run the standalone executable or Python with esp-idf-nvs-partition-gen installed.
No firmware contains a shared secret.
The output directory is intentionally outside the distributable firmware files.
"""
import argparse
import csv
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import subprocess
import sys


# RFC 3526 group 15, used by ESP-IDF Security 2 (3072-bit prime, generator 5).
SRP3072_PRIME = (
    "FFFFFFFFFFFFFFFFC90FDAA22168C234C4C6628B80DC1CD129024E088A67CC74020BBEA63B139B22514A08798E3404DD"
    "EF9519B3CD3A431B302B0A6DF25F14374FE1356D6D51C245E485B576625E7EC6F44C42E9A637ED6B0BFF5CB6F406B7ED"
    "EE386BFB5A899FA5AE9F24117C4B1FE649286651ECE45B3DC2007CB8A163BF0598DA48361C55D39A69163FA8FD24CF5F"
    "83655D23DCA3AD961C62F356208552BB9ED529077096966D670C354E4ABC9804F1746C08CA18217C32905E462E36CE3B"
    "E39E772C180E86039B2783A2EC07A28FB5C55DF06F4C52C9DE2BCBF6955817183995497CEA956AE515D2261898FA0510"
    "15728E5A8AAAC42DAD33170D04507A33A85521ABDF1CBA64ECFB850458DBEF0A8AEA71575D060C7DB3970F85A6E1E4C7"
    "ABF5AE8CDB0933D71E8C94E04A25619DCEE3D2261AD2EE6BF12FFA06D98A0864D87602733EC86A64521F2B18177B200C"
    "BBE117577A615D6C770988C0BAD946E208E24FA074E5AB3143DB5BFCE0FD108E4B82D120A93AD2CAFFFFFFFFFFFFFFFF"
)


def srp_parameters(idf_path=None):
    if idf_path is None:
        return int(SRP3072_PRIME, 16), 5
    source = (idf_path / "components/protocomm/src/crypto/srp6a/esp_srp.c").read_text()
    group = re.search(r"static const char N_3072\[\]\s*=\s*\{(.*?)\};", source, re.S)
    if not group:
        raise RuntimeError("Cannot find IDF's supported SRP-3072 group")
    group_bytes = bytes(int(v, 16) for v in re.findall(r"0x([0-9a-fA-F]{2})", group[1]))
    if len(group_bytes) != 384:
        raise RuntimeError("Unexpected SRP group size; refusing incompatible provisioning")
    return int.from_bytes(group_bytes, "big"), 5


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--idf-path", default=None, help="Optional IDF source checkout to cross-check the SRP group")
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--username", default=None)
    args = parser.parse_args()
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=True)
    pairing = output / "pairing.json"
    if any((output / name).exists() for name in ("pairing.json", "pairing.bin", "pairing.csv")):
        parser.error("Output already contains pairing files; choose a fresh directory to avoid replacing device credentials")
    n, g = srp_parameters(Path(args.idf_path) if args.idf_path else None)
    username = args.username or ("uart2llm-" + secrets.token_hex(6))
    if not username or len(username.encode()) > 64:
        parser.error("Username must contain 1-64 UTF-8 bytes")
    password = secrets.token_urlsafe(32)
    salt = secrets.token_bytes(16)
    identity_hash = hashlib.sha512((username + ":" + password).encode()).digest()
    exponent = int.from_bytes(hashlib.sha512(salt + identity_hash).digest(), "big")
    verifier = pow(g, exponent, n).to_bytes(384, "big")
    with (output / "pairing.csv").open("w", newline="", encoding="utf-8") as stream:
        writer = csv.writer(stream)
        writer.writerow(("key", "type", "encoding", "value"))
        writer.writerow(("security", "namespace", "", ""))
        writer.writerow(("salt", "data", "hex2bin", salt.hex()))
        writer.writerow(("verifier", "data", "hex2bin", verifier.hex()))
    from esp_idf_nvs_partition_gen.nvs_partition_gen import main as generate_nvs
    previous_argv = sys.argv
    try:
        sys.argv = ["nvs_partition_gen", "generate", str(output / "pairing.csv"), str(output / "pairing.bin"), "0x6000"]
        try:
            generate_nvs()
        except SystemExit as exc:
            if exc.code not in (None, 0):
                raise
    finally:
        sys.argv = previous_argv
    if not (output / "pairing.bin").is_file() or (output / "pairing.bin").stat().st_size != 0x6000:
        raise RuntimeError("NVS generator did not produce a complete 0x6000-byte pairing image")
    fd = os.open(pairing, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "w", encoding="utf-8") as stream:
        json.dump({"username": username, "password": password}, stream, indent=2)
        stream.write("\n")
    if os.name == "nt":
        # Protect the recovery credential against other ordinary local users.
        subprocess.run(["icacls", str(pairing), "/inheritance:r", "/grant:r", f"{os.environ['USERDOMAIN']}\\{os.environ['USERNAME']}:(R,W)"], check=True, stdout=subprocess.DEVNULL)
    print(f"Created device-specific pairing image: {output / 'pairing.bin'}")
    print(f"Private host pairing credential: {pairing}")
    print("Flash pairing.bin at 0x12000 alongside the firmware. Do not include pairing.json in distributable packages.")


if __name__ == "__main__":
    main()
