#!/usr/bin/env python3
"""Capture bounded serial evidence; never mix this reader with the running daemon."""
import argparse
from pathlib import Path
import time
import serial


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--port', required=True)
    parser.add_argument('--baud', type=int, default=115200)
    parser.add_argument('--seconds', type=float, default=15)
    parser.add_argument('--output', required=True, type=Path)
    parser.add_argument('--reset', action='store_true', help='Reset the explicitly selected board using RTS')
    args = parser.parse_args()
    if not 0 < args.seconds <= 300:
        parser.error('capture duration must be 0..300 seconds')
    args.output.parent.mkdir(parents=True, exist_ok=True)
    # Reserve the evidence file before opening or resetting any device.
    output = args.output.open('xb')
    port = serial.Serial(port=None, baudrate=args.baud, timeout=0.2)
    port.dtr = False
    port.rts = False
    port.port = args.port
    count = 0
    try:
        port.open()
        if args.reset:
            port.rts = True
            # usbser.sys may only send the changed control-line state after DTR
            # is also assigned (the same workaround used by Espressif esptool).
            port.dtr = port.dtr
            time.sleep(0.2)
            port.rts = False
            port.dtr = port.dtr
            time.sleep(0.2)
        deadline = time.monotonic() + args.seconds
        with output:
            while time.monotonic() < deadline and count < 2 * 1024 * 1024:
                try:
                    if not port.is_open:
                        port.open()
                    data = port.read(4096)
                except serial.SerialException:
                    port.close()
                    time.sleep(0.3)
                    continue
                if data:
                    output.write(data)
                    count += len(data)
    finally:
        port.close()
        output.close()
    print(f'Captured {count} bytes in {args.output}')


if __name__ == '__main__':
    main()
