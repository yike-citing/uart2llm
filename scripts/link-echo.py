"""Controlled, bounded-memory TCP echo target for cmd/link-bench.

Run on a LAN host reachable from ESP32; the benchmark PC does not dial this
server directly. No HTTP, credentials, request logging, or disk payload storage.
Example: python scripts/link-echo.py --host 0.0.0.0 --port 9089
"""
import argparse
import socket
import socketserver
import threading


class Server(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = True
    slots = threading.BoundedSemaphore(8)


class Echo(socketserver.BaseRequestHandler):
    def handle(self):
        if not self.server.slots.acquire(blocking=False):
            return
        try:
            self.request.settimeout(180)
            self.request.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)
            while True:
                data = self.request.recv(16384)
                if not data:
                    return
                self.request.sendall(data)
        except (OSError, TimeoutError):
            pass
        finally:
            self.server.slots.release()


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", default=9089, type=int)
    args = parser.parse_args()
    with Server((args.host, args.port), Echo) as server:
        print(f"Echo ready on {args.host}:{server.server_address[1]}", flush=True)
        try:
            server.serve_forever()
        except KeyboardInterrupt:
            pass
