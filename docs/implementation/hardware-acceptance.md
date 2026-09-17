# Hardware acceptance and 24-hour soak

These tools provide repeatable test traffic and measured reports. They do **not** establish that hardware acceptance passed merely because they exist or because their local self-test passed.

## Required arrangement

1. Use the Windows PC under test with its ordinary network interfaces disabled/disconnected. Connect its UART adapter to the ESP32-S3, including common ground. Verify the firmware pin mapping, 16 MiB flash and 8 MiB PSRAM.
2. Place `test-upstream.exe` on a **different, networked LAN computer** reachable from the ESP32 Wi-Fi. That computer must have a stable LAN name/address and permit the chosen HTTPS port through its firewall.
3. Supply a PEM server certificate chain and private key. The certificate SAN must match the upstream hostname/address. Trust the issuing CA in the Windows user's trusted roots before disconnecting that PC from its network. Use an appropriately issued private test CA or an existing trusted certificate; neither tool has a TLS-verification-disable option. Keep private keys out of release packages and source control.
4. Pair the actual ESP32, configure Wi-Fi, set the daemon upstream to `https://LAN-NAME:9443/v1`, and set its upstream credential to the fixture token. The daemon still performs TLS verification on Windows; ESP32 only resolves DNS and forwards TCP.

On the LAN fixture computer, set `UART2LLM_FIXTURE_TOKEN` to a test-only secret in the environment, then run:

```powershell
.\test-upstream.exe --listen :9443 --cert C:\fixture\server-chain.pem --key C:\fixture\server-key.pem
```

Source build: `go build -o test-upstream.exe ./cmd/test-upstream` on Windows, or the equivalent `GOOS=windows GOARCH=amd64` cross-build. The fixture has no outbound HTTP client and never calls OpenAI. Its `/v1/models` sentinel identifies the deterministic fixture. Files are generated/discarded as streams; uploads are hashed without storage. The pattern is byte `offset % 251`, with a maximum test file size of 1 GiB. Jobs are deterministic fixture responses, not real fine-tuning jobs.

## Run the checks on the offline Windows PC

Python 3.10 or later is required for this optional test tool; it uses only the standard library. Set `UART2LLM_API_TOKEN` to the daemon's local API token. Optionally set `UART2LLM_ADMIN_TOKEN` to enable management polling and memory trend collection. Obtain these tokens through the local credential/UI workflow; do not put them in command-line arguments or report files.

```powershell
python .\acceptance.py --seconds 30 --output .\short-check.json --label "ESP32-S3 UART hardware"
python .\acceptance.py --hours 24 --file-bytes 65536 --output .\soak-24h.json --label "ESP32-S3 UART hardware"
```

The runner connects only to loopback endpoints. Before any workload, it requires the fixture-specific model ID, ownership, version, pattern and no-external-calls marker. An ordinary OpenAI upstream fails this check, so the runner does not start chat/upload/job traffic there. The initial check is a `/v1/models` read. Do not modify the sentinel guard to test a production upstream.

Exactly four workers run SSE, multipart upload, binary download, and jobs/model requests concurrently. SSE checks UTF-8 text, split tool arguments, usage, an unknown extension field and `[DONE]`. File operations compare byte counts and SHA-256. Requests already running when the duration expires finish normally; consequently wall time can exceed the requested duration. Ctrl+C cancels active sockets and produces an incomplete report. `--timeout` defaults to 600 seconds per socket inactivity wait and can be increased for very large/slow transfers; it is not an overall soak duration limit.

Use `--base-url` and `--admin-url` for nondefault local ports. For TLS on the local endpoint, `--ca-file` can supply additional trust roots without disabling certificate checks. For large-file acceptance, choose a suitable size explicitly, for example `--seconds 300 --file-bytes 16777216 --timeout 1800`; UART wire time may greatly exceed the requested duration. Files are generated/read in bounded chunks and never fully buffered or saved by the runner.

## Interpret the report

- `software_checks_passed` requires all four worker types to succeed at least once with no failures; when admin polling is enabled, telemetry must also succeed without errors.
- `soak_24h_completed` is only true for a requested and completed run of at least 24 hours whose software checks passed.
- `hardware_path_verified` always remains false. The operator must separately record the board/adapter, wiring, firmware/build IDs, Wi-Fi network, disabled PC network interfaces and observed physical path. A local self-test cannot prove those facts.
- Per-operation success/failure counts, payload bytes, recent latency percentiles, SSE first-event latency and wall-time throughput are measured. Throughput excludes UART/TLS framing overhead and is not a baud-rate performance promise.
- Memory trends use every available telemetry sample through bounded online regression. First/last/min/max values and bytes-per-hour slopes help identify drift; they do not prove the absence of leaks. Missing metrics remain absent. Recent latency samples, telemetry snapshots and error details have explicit bounded limits.

Also perform and record separate physical fault cases: unplug UART during each operation, disable/re-enable Wi-Fi, reboot ESP32, inject line errors, change UART speed then omit confirmation, apply wrong Wi-Fi settings, interrupt an upgrade, and remove power during configuration/OTA commit. Verify that other streams survive single-stream cancellation, management remains responsive, invalid sessions fail explicitly, settings recover, and no API operation is silently replayed. Fault-injection runs are expected to record request failures; distinguish them from the uninterrupted soak report.

## Local harness validation

`go test ./cmd/test-upstream` validates the sentinel, deterministic SSE and upload/download hashes. A local HTTPS fixture plus the Python runner can validate the harness using a temporary trusted test certificate. That route is **test-only**, bypasses ESP32, and must be labeled accordingly. Never include temporary certificates, private keys, test tokens, or generated reports in the redistributable tools folder.
