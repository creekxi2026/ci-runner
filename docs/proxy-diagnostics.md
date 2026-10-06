# Public egress tunnel diagnostics

The helper emits one compact JSON `egress_close` record to stderr per completed
request/tunnel, enabled by default. It does not log each data chunk. Use the
helper container's existing job/container labels to associate its Docker logs
with a checkout; no job environment or credentials are read by the logger.
An active tunnel's close record is available only when that tunnel ends.

Before deleting a finished job's proxy, the controller copies up to the last
200 close records into its own Docker logs as `retained_egress_close`, with the
controller-owned `job` name and Docker's original UTC timestamp as `closed_at`.
This survives deletion of the job containers and
workspace. Retrieve it with `docker logs <controller>` filtered by that job.
It is bounded operational retention, subject to the controller's Docker log
rotation/deletion, not a permanent archive. Still-active tunnels may have no
close record when cleanup begins.

The read has a five-second deadline and a 512 KiB stream limit. Docker framing
and JSON are validated; only typed, allowlisted diagnostic fields are emitted.
Unknown fields, arbitrary log lines, exception text, URLs and headers are
discarded. Collection failure is summarized without raw errors and does not
block resource cleanup. Retried cleanup does not re-emit the same snapshot;
after a controller restart a duplicate snapshot is possible and keeps the same
job identity.

Fields:

- `hostname`: ASCII DNS-label syntax, labels at most 63 characters and total at
  most 253; otherwise null. Raw URLs/authorities are never logged. IPv6 literals
  are omitted here; a validated public numeric destination appears in `peer_ip`.
- `peer_ip`: validated, pinned public destination IP of the last dial attempt,
  including when reached through a trusted upstream CONNECT proxy; not the
  upstream gateway's IP. Null when no validated destination was dialed.
- `port`: destination port, not the helper listener's port.
- `phase`: `request`, `policy`, `resolve`, `dial`, `upstream_handshake`,
  `response`, or `relay` at completion/failure.
- `error_type`: exception class name only, never exception text. Null for clean
  EOF; `TimeoutError` for an idle select timeout.
- `close_reason`: `eof`, `idle_timeout`, or `error`.
- `close_direction`: active relay source (`client`/`server`) when a directional
  operation fails; `both` for select errors/timeouts or completed bidirectional
  EOF. `eof_directions` records which read halves reached EOF and in what order.
- `duration_seconds`: monotonic elapsed time from handler entry to completion.
- `last_io_seconds`: monotonic offset from handler entry to the last fully
  forwarded chunk, or relay entry if no chunk was forwarded (zero before relay).
  Subtract from duration to determine time since last relay progress.
- `bytes_client_to_server`, `bytes_server_to_client`: payload bytes in completed
  `sendall` operations; partial writes preceding a send failure are not counted.

No paths, queries, headers, credentials, payload content, exception messages,
resolver/upstream URLs, or container environment are logged.

## Protocol and bounds

After CONNECT success (or the start of a plain HTTP response), errors only close
the relay; they never append an HTTP error to the tunnel/response. Read EOF
propagates a write-half-close to the other socket, while the remaining read half
continues draining. A quiet relay has a finite 300-second idle wait and finite
300-second socket I/O waits, instead of the old 90-second select cutoff. The
job's independent 3600-second hard lifetime remains the overall helper bound.
The trusted upstream handshake retains its total 15-second deadline.

Before response bytes are started: policy/request rejection is 403, DNS or
transport failure is 502, and timeout is 504. Empty/invalid DNS lookup fails
closed as 502. All A and AAAA answers remain subject to public-address policy;
private AAAA answers or failed AAAA lookup are not ignored. Resolver redirects
remain forbidden. Listener binding, firewall, container capabilities/read-only
settings, resolver TLS validation, and deployment configuration are unchanged.

## Verification and causal limits

Run locally without external networking:

```sh
python3 -m unittest discover -s tests -p test_tunnel.py -v
python3 -m unittest discover -s tests -v
```

The socketpair regressions prove no HTTP after CONNECT errors, bidirectional
half-close draining with real bytes, and diagnostic redaction. The idle tests
use a controlled monotonic clock/select seam to exercise 100-second delayed
progress and 300-second idle expiry without waiting those wall-clock durations.
These prove protocol behavior, not the cause of any historical TLS incident.

Baseline published revision `742fbe8` completed [real CI run
`37269947775`](https://github.com/creekxi2026/ci-runner/actions/runs/37269947775)
with four successful jobs. That result does not establish the cause of earlier
intermittent checkout/TLS failures. Use the close records from deployed images
to correlate failures with their actual destination IP, connection phase and
last relay activity; do not infer a historical root cause from unit tests alone.
