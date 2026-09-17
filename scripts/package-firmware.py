"""Package one already-built transport, excluding per-device pairing material."""
import argparse
import hashlib
from pathlib import Path
import zipfile

ROOT = Path(__file__).resolve().parent.parent

def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--transport',choices=['uart','usb-validation'],required=True)
    args=p.parse_args()
    build=ROOT/'firmware/build'
    config=(build/'sdkconfig').read_text()
    if ('CONFIG_U2_USB_VALIDATION=y' in config) != (args.transport=='usb-validation'):
        raise SystemExit('Transport mismatch; refusing to label the wrong firmware')
    files=['uart2llm.bin','bootloader/bootloader.bin','partition_table/partition-table.bin','ota_data_initial.bin','flasher_args.json']
    for file in files:
        if not (build/file).is_file():raise SystemExit('Missing firmware build input: '+file)
    out=ROOT/'dist/release';out.mkdir(parents=True,exist_ok=True)
    manifest=[]
    with zipfile.ZipFile(out/('uart2llm-firmware-'+args.transport+'.zip'),'x',zipfile.ZIP_DEFLATED) as z:
        for file in files:
            data=(build/file).read_bytes();z.writestr(file,data)
            manifest.append(hashlib.sha256(data).hexdigest()+'  '+file)
        z.writestr('SHA256SUMS','\n'.join(manifest)+'\n')
        z.write(ROOT/'docs/RELEASE.md','README.md')
        z.write(ROOT/'LICENSE','LICENSE')
        for notice in (ROOT/'packaging/licenses').iterdir():
            if notice.is_file():z.write(notice,'licenses/'+notice.name)
    print('Packaged firmware:',args.transport)

if __name__=='__main__':main()
