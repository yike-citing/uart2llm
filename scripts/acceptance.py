#!/usr/bin/env python3
"""Bounded, standard-library-only acceptance/soak runner for uart2llm.

Requires the controlled test-upstream sentinel before starting any workload.
Tokens are read only from environment variables and never enter the report.
"""
import argparse
from collections import Counter, deque
import hashlib
import http.client
import ipaddress
import json
import math
import os
from pathlib import Path
import socket
import ssl
import statistics
import sys
import threading
import time
from urllib.parse import urlsplit

MODEL = "uart2llm-acceptance-fixture-v1"
CHUNK = 65536
PATTERN = bytes(range(251)) * 263
JSON_LIMIT = 128 * 1024


class PatternBody:
    def __init__(self, size):
        self.size, self.offset = size, 0

    def read(self, amount=CHUNK):
        count = min(CHUNK, max(0, amount), self.size - self.offset)
        start = self.offset % 251
        data = PATTERN[start:start + count]
        self.offset += count
        return data


class MultipartBody:
    boundary = "uart2llm-acceptance-fixed-boundary-v1"

    def __init__(self, size):
        self.prefix = (f"--{self.boundary}\r\nContent-Disposition: form-data; name=\"purpose\"\r\n\r\nfine-tune\r\n"
                       f"--{self.boundary}\r\nContent-Disposition: form-data; name=\"file\"; filename=\"pattern.bin\"\r\n"
                       "Content-Type: application/octet-stream\r\n\r\n").encode()
        self.suffix = f"\r\n--{self.boundary}--\r\n".encode()
        self.pattern = PatternBody(size)
        self.length = len(self.prefix) + size + len(self.suffix)

    def read(self, amount=CHUNK):
        amount = min(CHUNK, amount)
        if self.prefix:
            data, self.prefix = self.prefix[:amount], self.prefix[amount:]
            return data
        data = self.pattern.read(amount)
        if data:
            return data
        data, self.suffix = self.suffix[:amount], self.suffix[amount:]
        return data


def pattern_hash(size):
    source, digest = PatternBody(size), hashlib.sha256()
    while data := source.read():
        digest.update(data)
    return digest.hexdigest()


def local_url(raw):
    value = urlsplit(raw)
    if value.scheme not in ("http", "https") or value.username or value.password or value.query or value.fragment:
        raise ValueError("use a loopback HTTP(S) base URL without credentials, query or fragment")
    host = value.hostname
    if host != "localhost":
        try:
            if not ipaddress.ip_address(host).is_loopback:
                raise ValueError("acceptance runner connects only to a loopback gateway")
        except (TypeError, ValueError) as exc:
            raise ValueError("acceptance runner connects only to localhost or a loopback IP") from exc
    _ = value.port  # Validate the port before starting worker threads.
    return value


class Endpoint:
    def __init__(self, base, token, timeout, ca_file=None):
        self.url = local_url(base)
        self.token, self.timeout = token, timeout
        self.context = ssl.create_default_context(cafile=ca_file)
        self.lock, self.active = threading.Lock(), set()

    def open(self, method, path, body=None, headers=None):
        host, port = self.url.hostname, self.url.port
        if self.url.scheme == "https":
            conn = http.client.HTTPSConnection(host, port or 443, timeout=self.timeout, context=self.context)
        else:
            conn = http.client.HTTPConnection(host, port or 80, timeout=self.timeout)
        with self.lock:
            self.active.add(conn)
        request_headers = {"Authorization": "Bearer " + self.token, "Connection": "close"}
        request_headers.update(headers or {})
        started = time.monotonic()
        try:
            conn.request(method, self.url.path.rstrip("/") + path, body=body, headers=request_headers)
            response = conn.getresponse()
            return conn, response, started
        except Exception:
            self.close(conn)
            raise

    def close(self, conn):
        conn.close()
        with self.lock:
            self.active.discard(conn)

    def abort(self):
        with self.lock:
            active = list(self.active)
        for conn in active:
            if conn.sock:
                try:
                    conn.sock.shutdown(socket.SHUT_RDWR)
                except OSError:
                    pass
            self.close(conn)

    def json(self, method, path, value=None):
        body = None if value is None else json.dumps(value, ensure_ascii=False).encode("utf-8")
        headers = {} if body is None else {"Content-Type": "application/json", "Content-Length": str(len(body))}
        conn, response, _ = self.open(method, path, body, headers)
        try:
            if response.status != 200:
                raise RuntimeError(f"HTTP {response.status} from {path}")
            data = response.read(JSON_LIMIT + 1)
            if len(data) > JSON_LIMIT:
                raise RuntimeError("JSON response exceeds acceptance harness limit")
            return json.loads(data)
        finally:
            self.close(conn)


class Measurements:
    def __init__(self):
        self.lock = threading.Lock()
        self.ops = {}
        self.failures = deque(maxlen=32)
        self.telemetry = deque(maxlen=120)
        self.memory = {}
        self.telemetry_count, self.telemetry_failures = 0, 0

    def record(self, operation, seconds, count=0, first_event=None, error=None):
        with self.lock:
            item = self.ops.setdefault(operation, {"attempts": 0, "successes": 0, "failures": 0,
                "bytes": 0, "total_seconds": 0.0, "latency_samples": deque(maxlen=1024),
                "first_event_samples": deque(maxlen=1024), "errors": Counter()})
            item["attempts"] += 1
            item["total_seconds"] += seconds
            if error is not None:
                item["failures"] += 1
                name = type(error).__name__
                item["errors"][name] += 1
                self.failures.append({"operation": operation, "error_type": name, "message": str(error)[:300]})
            else:
                item["successes"] += 1
                item["bytes"] += count
                item["latency_samples"].append(seconds)
                if first_event is not None:
                    item["first_event_samples"].append(first_event)

    def sample(self, elapsed, state):
        # Store only selected numeric metadata, never arbitrary server responses.
        paths = ["device.memory.internal_free", "device.memory.internal_min_free", "device.memory.psram_free",
                 "device.memory.largest_internal_block", "host.memory.rss_bytes", "host.memory.heap_bytes"]
        selected = {}
        with self.lock:
            self.telemetry_count += 1
            for path in paths:
                value = state
                for part in path.split("."):
                    value = value.get(part) if isinstance(value, dict) else None
                if not isinstance(value, (int, float)) or isinstance(value, bool) or not math.isfinite(value):
                    continue
                selected[path] = value
                row = self.memory.setdefault(path, {"samples": 0, "first": value, "last": value, "min": value,
                    "max": value, "sum_t": 0.0, "sum_y": 0.0, "sum_tt": 0.0, "sum_ty": 0.0})
                row["samples"] += 1
                row["last"], row["min"], row["max"] = value, min(row["min"], value), max(row["max"], value)
                row["sum_t"] += elapsed
                row["sum_y"] += value
                row["sum_tt"] += elapsed * elapsed
                row["sum_ty"] += elapsed * value
            self.telemetry.append({"elapsed_seconds": round(elapsed, 3), "connected": state.get("connected"),
                "paired": state.get("paired"), "memory": selected})

    @staticmethod
    def distribution(samples):
        ordered = sorted(samples)
        if not ordered:
            return None
        return {"samples": len(ordered), "min_seconds": ordered[0], "median_seconds": statistics.median(ordered),
                "p95_seconds": ordered[max(0, math.ceil(len(ordered) * .95) - 1)], "max_seconds": ordered[-1]}

    def report(self, elapsed):
        with self.lock:
            operations = {}
            for name, row in self.ops.items():
                operations[name] = {key: value for key, value in row.items() if not key.endswith("_samples")}
                operations[name]["latency_recent_1024"] = self.distribution(row["latency_samples"])
                operations[name]["first_event_recent_1024"] = self.distribution(row["first_event_samples"])
                operations[name]["successful_bytes_per_wall_second"] = row["bytes"] / max(elapsed, .001)
            memory = {}
            for name, row in self.memory.items():
                n = row["samples"]
                denominator = n * row["sum_tt"] - row["sum_t"] ** 2
                slope = ((n * row["sum_ty"] - row["sum_t"] * row["sum_y"]) / denominator * 3600
                         if denominator > 0 else None)
                memory[name] = {k: row[k] for k in ("samples", "first", "last", "min", "max")}
                memory[name]["linear_trend_bytes_per_hour"] = slope
            return {"operations": operations, "recent_failures": list(self.failures), "memory_trends": memory,
                    "telemetry_samples": self.telemetry_count, "telemetry_failures": self.telemetry_failures,
                    "recent_telemetry": list(self.telemetry)}


def stream_work(endpoint):
    body = json.dumps({"model": MODEL, "stream": True, "messages": [{"role": "user", "content": "fixture"}],
                       "stream_options": {"include_usage": True}}).encode()
    conn, response, started = endpoint.open("POST", "/v1/chat/completions", body,
        {"Content-Type": "application/json", "Content-Length": str(len(body))})
    count, first, content, arguments, usage, extension, done = 0, None, "", "", False, False, False
    try:
        if response.status != 200 or "text/event-stream" not in response.getheader("Content-Type", ""):
            raise RuntimeError(f"SSE returned HTTP {response.status} or incorrect content type")
        while True:
            line = response.readline(8193)
            if len(line) > 8192:
                raise RuntimeError("SSE line exceeds bounded parser limit")
            if not line:
                break
            count += len(line)
            if count > 65536:
                raise RuntimeError("unexpectedly large fixture stream")
            if not line.startswith(b"data: "):
                continue
            if first is None:
                first = time.monotonic() - started
            payload = line[6:].strip()
            if payload == b"[DONE]":
                done = True
                continue
            if done:
                raise RuntimeError("fixture emitted data after its completion marker")
            event = json.loads(payload.decode("utf-8"))
            usage |= event.get("usage", {}).get("total_tokens") == 9
            extension |= event.get("fixture_extension", {}).get("preserved") is True
            for choice in event.get("choices", []):
                delta = choice.get("delta", {})
                content += delta.get("content", "")
                for tool in delta.get("tool_calls", []):
                    arguments += tool.get("function", {}).get("arguments", "")
        if not done or content != "你好，串口" or json.loads(arguments) != {"value": "中文"} or not usage or not extension:
            raise RuntimeError("SSE content/tool deltas/usage/extension/DONE verification failed")
        return count, first
    finally:
        endpoint.close(conn)


def upload_work(endpoint, size, expected):
    body = MultipartBody(size)
    conn, response, _ = endpoint.open("POST", "/v1/files", body, {
        "Content-Type": f"multipart/form-data; boundary={body.boundary}", "Content-Length": str(body.length),
        "X-Expected-Sha256": expected})
    try:
        data = response.read(JSON_LIMIT + 1)
        if response.status != 200 or len(data) > JSON_LIMIT:
            raise RuntimeError(f"upload returned HTTP {response.status} or oversized JSON")
        value = json.loads(data)
        if value.get("bytes") != size or value.get("sha256") != expected:
            raise RuntimeError("upload size or SHA-256 mismatch")
        return size, None
    finally:
        endpoint.close(conn)


def download_work(endpoint, size, expected):
    conn, response, _ = endpoint.open("GET", f"/v1/files/fixture-pattern/content?size={size}")
    digest, count = hashlib.sha256(), 0
    try:
        if response.status != 200:
            raise RuntimeError(f"download returned HTTP {response.status}")
        while data := response.read(CHUNK):
            count += len(data)
            if count > size:
                raise RuntimeError("download exceeded declared fixture size")
            digest.update(data)
        if count != size or digest.hexdigest() != expected:
            raise RuntimeError("download size or SHA-256 mismatch")
        return count, None
    finally:
        endpoint.close(conn)


def jobs_work(endpoint):
    created = endpoint.json("POST", "/v1/fine_tuning/jobs", {"training_file": "file-fixture", "model": MODEL})
    if created.get("id") != "ftjob-fixture" or created.get("status") != "succeeded":
        raise RuntimeError("unexpected fixture job")
    events = endpoint.json("GET", "/v1/fine_tuning/jobs/ftjob-fixture/events")
    if not events.get("data") or events["data"][0].get("id") != "event-fixture":
        raise RuntimeError("job events did not survive proxying")
    model = endpoint.json("GET", "/v1/models/" + MODEL)
    if model.get("id") != MODEL:
        raise RuntimeError("model resource mismatch")
    return 0, None


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base-url", default="http://localhost:8765")
    parser.add_argument("--admin-url", default="http://localhost:8766/admin/v1")
    duration = parser.add_mutually_exclusive_group()
    duration.add_argument("--seconds", type=float, default=None, help="short test duration; default 30")
    duration.add_argument("--hours", type=float, help="explicit soak duration, e.g. --hours 24")
    parser.add_argument("--file-bytes", type=int, default=65536, help="deterministic file size, 1..1073741824")
    parser.add_argument("--timeout", type=float, default=600, help="individual socket inactivity timeout, seconds")
    parser.add_argument("--telemetry-seconds", type=float, default=5)
    parser.add_argument("--ca-file", help="optional trust roots for an HTTPS localhost endpoint; never disables verification")
    parser.add_argument("--label", default="unspecified test environment")
    parser.add_argument("--output", type=Path, default=Path("acceptance-report.json"))
    args = parser.parse_args()
    seconds = args.hours * 3600 if args.hours is not None else (args.seconds if args.seconds is not None else 30)
    if not 0 < seconds <= 7 * 24 * 3600 or not 1 <= args.file_bytes <= 1024 ** 3 or not math.isfinite(args.timeout) or args.timeout <= 0 or not math.isfinite(args.telemetry_seconds) or args.telemetry_seconds < 1:
        parser.error("invalid duration, file size, timeout or telemetry interval")
    token = os.environ.get("UART2LLM_API_TOKEN", "")
    if not token:
        parser.error("set UART2LLM_API_TOKEN in the environment")
    endpoint = Endpoint(args.base_url.rstrip("/").removesuffix("/v1"), token, args.timeout, args.ca_file)
    sentinel = endpoint.json("GET", "/v1/models")
    marker = sentinel.get("uart2llm_fixture", {}) if isinstance(sentinel, dict) else {}
    if not isinstance(marker, dict) or marker.get("version") != 1 or marker.get("no_external_calls") is not True or marker.get("pattern") != "offset-mod-251" or not any(
            isinstance(model, dict) and model.get("id") == MODEL and model.get("owned_by") == "uart2llm-controlled-fixture" for model in sentinel.get("data", [])):
        raise RuntimeError("REFUSED: upstream is not the controlled uart2llm fixture; no workload was started")
    expected = pattern_hash(args.file_bytes)
    admin_token = os.environ.get("UART2LLM_ADMIN_TOKEN", "")
    admin = Endpoint(args.admin_url, admin_token, min(args.timeout, 15), args.ca_file) if admin_token else None
    metrics, stop = Measurements(), threading.Event()
    started = time.monotonic()
    started_utc = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
    deadline = started + seconds
    operations = [("sse", lambda: stream_work(endpoint)),
                  ("upload", lambda: upload_work(endpoint, args.file_bytes, expected)),
                  ("download", lambda: download_work(endpoint, args.file_bytes, expected)),
                  ("jobs_models", lambda: jobs_work(endpoint))]

    def worker(name, work):
        while not stop.is_set() and time.monotonic() < deadline:
            before = time.monotonic()
            try:
                count, first = work()
                metrics.record(name, time.monotonic() - before, count, first)
            except Exception as exc:
                metrics.record(name, time.monotonic() - before, error=exc)
                if stop.wait(.25):
                    return
            stop.wait(.05)

    def telemetry():
        while not stop.is_set():
            try:
                metrics.sample(time.monotonic() - started, admin.json("GET", "/state"))
            except Exception:
                with metrics.lock:
                    metrics.telemetry_failures += 1
            if stop.wait(args.telemetry_seconds):
                return

    workers = [threading.Thread(target=worker, args=operation, name=operation[0], daemon=True) for operation in operations]
    sampler = threading.Thread(target=telemetry, name="telemetry", daemon=True) if admin else None
    print(f"Verified controlled fixture. Running four mixed workers for {seconds:g}s; file SHA-256 {expected}.", flush=True)
    interrupted = False
    for thread in workers:
        thread.start()
    if sampler:
        sampler.start()
    try:
        while any(thread.is_alive() for thread in workers):
            for thread in workers:
                thread.join(.2)
    except KeyboardInterrupt:
        interrupted = True
        stop.set()
        endpoint.abort()
        if admin:
            admin.abort()
        for thread in workers:
            thread.join(2)
    finally:
        stop.set()
        if admin:
            admin.abort()
        if sampler:
            sampler.join(2)
    elapsed = time.monotonic() - started
    report = metrics.report(elapsed)
    finished = all(not thread.is_alive() for thread in workers)
    passed = finished and not interrupted and all(row["successes"] > 0 and row["failures"] == 0 for row in report["operations"].values()) and len(report["operations"]) == 4
    if admin:
        passed = passed and report["telemetry_samples"] > 0 and report["telemetry_failures"] == 0
    report.update({"schema_version": 1, "label": args.label, "started_utc": started_utc,
        "requested_seconds": seconds, "elapsed_seconds": elapsed, "interrupted": interrupted,
        "all_workers_finished": finished, "software_checks_passed": passed, "fixture_verified": True,
        "hardware_path_verified": False, "hardware_verification_note": "Operator must verify the actual ESP32/UART/Wi-Fi route and absence of a PC network fallback separately.",
        "api_base_url": args.base_url, "workers": 4, "file_bytes": args.file_bytes,
        "expected_file_sha256": expected, "telemetry_enabled": admin is not None,
        "soak_24h_completed": seconds >= 24 * 3600 and elapsed >= 24 * 3600 and passed,
        "memory_interpretation": "Trends use all samples; a slope alone does not prove or exclude a leak. Missing metrics remain absent.",
        "bounded_storage": {"latencies_per_operation": 1024, "recent_failures": 32, "recent_telemetry": 120}})
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(report, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")
    print(f"Report: {args.output.resolve()} — {'PASS' if passed else 'FAIL/INCOMPLETE'}", flush=True)
    return 0 if passed else 1


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (ValueError, OSError, RuntimeError, http.client.HTTPException, json.JSONDecodeError) as error:
        print(f"Acceptance runner stopped: {error}", file=sys.stderr)
        sys.exit(2)
