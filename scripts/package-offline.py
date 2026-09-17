"""Combine verified release artifacts into a self-contained Windows distribution."""
import argparse
import hashlib
from pathlib import Path, PurePosixPath
import zipfile


ARCHIVES = {
    'uart2llm-device-tools-windows-x64.zip': 'device-tools',
    'uart2llm-firmware-uart.zip': 'firmware/uart',
    'uart2llm-firmware-usb-validation.zip': 'firmware/usb-validation',
}
REQUIRED = {
    'device-tools/tools/esptool.exe',
    'device-tools/tools/provision-device.exe',
    'device-tools/source/packaging/sources/esptool-5.4.0.tar.gz',
}
for transport in ('uart', 'usb-validation'):
    REQUIRED.update('firmware/' + transport + '/' + name for name in (
        'uart2llm.bin', 'bootloader/bootloader.bin',
        'partition_table/partition-table.bin', 'ota_data_initial.bin',
        'flasher_args.json', 'SHA256SUMS'))


def package(assets):
    files = {}
    for name in ('uart2llm-agent-windows-x64.exe', 'LICENSES.txt', 'README.md'):
        files[name] = (assets / name).read_bytes()
    for archive, prefix in ARCHIVES.items():
        with zipfile.ZipFile(assets / archive) as source:
            for item in source.infolist():
                path = PurePosixPath(item.filename)
                if path.is_absolute() or '..' in path.parts or '\\' in item.filename or ':' in item.filename:
                    raise ValueError('Unsafe archive member: ' + item.filename)
                if path.name.lower().startswith(('pairing.', '.env', 'credentials')):
                    raise ValueError('Private material in archive: ' + item.filename)
                if item.is_dir():
                    continue
                name = prefix + '/' + path.as_posix()
                if name.casefold() in {key.casefold() for key in files}:
                    raise ValueError('Duplicate archive member: ' + name)
                files[name] = source.read(item)  # Also verifies ZIP CRC.
    missing = REQUIRED - files.keys()
    if missing or any(not data for data in files.values()):
        raise ValueError('Missing or empty release inputs: ' + ', '.join(sorted(missing)))
    for transport in ('uart', 'usb-validation'):
        prefix = 'firmware/' + transport + '/'
        for line in files[prefix + 'SHA256SUMS'].decode().splitlines():
            digest, name = line.split('  ', 1)
            if hashlib.sha256(files[prefix + name]).hexdigest() != digest:
                raise ValueError('Firmware checksum mismatch: ' + prefix + name)
    files['SHA256SUMS'] = ''.join(
        hashlib.sha256(data).hexdigest() + '  ' + name + '\n'
        for name, data in sorted(files.items())).encode()
    output = assets / 'uart2llm-windows-x64-offline.zip'
    with zipfile.ZipFile(output, 'x', zipfile.ZIP_DEFLATED) as bundle:
        for name, data in sorted(files.items()):
            bundle.writestr('uart2llm/' + name, data)
    with zipfile.ZipFile(output) as bundle:
        if bundle.testzip() is not None:
            raise ValueError('Offline bundle CRC verification failed')
    print('Offline bundle verified:', len(files), 'files;', output.stat().st_size, 'bytes')
    return output


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--assets', type=Path, required=True)
    package(parser.parse_args().assets)
