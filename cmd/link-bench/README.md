# Controlled serial TCP benchmark

This diagnostic measures the actual Windows → COM → ESP32 → Wi-Fi → LAN TCP
path without an LLM provider. It does not use the PC network stack for the
outbound test connection. It exclusively opens the selected COM port, so stop
the uart2llm daemon first and restart it afterwards. The tool itself never stops
or starts the daemon, changes firmware, or modifies Wi-Fi configuration.

## Run

On a LAN host reachable from the ESP32, run the bounded echo server:

```powershell
python scripts/link-echo.py --host 0.0.0.0 --port 9089
```

Use the host's LAN address, not `localhost`, as the benchmark target. A temporary
firewall rule may be required for this chosen inbound test port. Restrict it to
the test network and remove it afterwards. The echo server is a diagnostic raw
TCP endpoint; do not expose it to an untrusted network.

On the Windows computer with the ESP32 attached:

```powershell
.\dist\link-bench.exe --port COM4 --target 192.168.1.10:9089 --bytes 262144 --parallel 4 --report docs/test-reports/link-bench.json
```

The tool reads the existing per-device pairing credentials from Windows
Credential Manager via the same `device.Manager` used by the product. It does
not print credentials or copy them to the report. It uses the fixed recovery
line setting 115200/8N1 without flow control; for the current native USB firmware
this setting does not constrain USB wire speed.

Available options: `--port`, `--target`, `--bytes` (per connection),
`--parallel` (1–4), `--report`, and `--timeout` (default 30m). The overall timeout
is diagnostic-only; each blocked data operation also has a 120-second idle
timeout. Ctrl+C cancels the run and attempts to write a partial failure report.

## Results

All connections are established before the timed transfer starts. Each writes
deterministic pseudorandom bytes while concurrently reading the echo. Each
direction uses a 16 KiB application buffer per connection; memory use does not
scale with requested byte count. SHA-256 values and lengths must match exactly.
No payload is written to disk.

The aggregate interval includes stream cleanup but excludes connection setup.
`echo_payload_bytes_per_second` counts the bytes received once;
`duplex_bytes_per_second` counts sent plus received bytes. These are actual
payload rates, not baud-rate estimates. Report per-stream durations for skew.

During transfer the tool requests device telemetry once per second and records
the sample count, failures and maximum response latency. Very short runs can
legitimately produce zero telemetry samples; choose a longer load to measure
management responsiveness. Telemetry response bodies are discarded.

After the main transfer, the cancellation check opens a connection with no data
available, closes it while a read is blocked, and verifies that the reader exits
and the close completes within five seconds. A fresh 4096-byte echo must then
match in the same link session. This is a cancellation/recovery check, not a
claim to have tested physical unplugging or cancellation of one busy stream
while three other streams are carrying data.

`before` and `after` contain host connection counters (session, frame counts,
retries, invalid frames and active slots). Target and COM port are intentionally
included in the diagnostic report; passwords and API keys are not included.
Exit code 0 means all integrity, telemetry and cancellation/recovery checks
passed; nonzero means inspect the JSON report.

For before/after comparisons use identical hardware, target, byte count,
parallelism and echo-server conditions. Preserve both host executable hashes
and firmware hashes outside the report. A diagnostic built after a host fix
cannot serve as the pre-fix baseline.

## Build and software checks

```sh
go test -race ./cmd/link-bench
GOOS=windows GOARCH=amd64 go build -trimpath -o dist/link-bench.exe ./cmd/link-bench
```

Unit tests exercise fragmented echo delivery, actual byte corruption, and
cancellation while both transfer directions are blocked. They use `net.Pipe`
and make no claim about hardware throughput. The Python server can be syntax
checked with `python -m py_compile scripts/link-echo.py`.
