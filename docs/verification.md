# Verification record

Stage 1 implementation checks were run on 2026-11-09. These results distinguish
tests of packet decisions from observations on actual Linux interfaces.

## Passed

- `make check build`: race-enabled Go tests, `go vet`, and binary build.
- A ten-second `FuzzParseIPv4` run: 340,002 executions, no failure.
- Configuration validation against `lab/stage1.json`.
- Packet-crafting file output, including malformed headers and bad checksums.
- Bash syntax checking and Python parsing for the lab harness.

The Go tests cover IPv4 bounds and checksums, longest prefix match, immutable
and concurrently replaced tables, TTL handling, local replies, ICMP error
quotes and suppression, MTU behavior, unsupported traffic, protection switches,
send failures, counter snapshots, read-only management handlers, shutdown of
blocked fake links, pcap contents, and JSONL output.

Go 1.27.3 and the temporary GCC/make packages used for verification were
downloaded under `/tmp`; no system-wide toolchain was installed. The Go archive
matched its published SHA-256 digest. The x/sys dependency is pinned in
`go.mod` and authenticated by `go.sum`.

## Pending on the Ubuntu lab host

The privileged harness could not start: sandboxed sudo is blocked by the
no-new-privileges setting, and the attempt outside the sandbox returned
`sudo: interactive authentication is required`. The sandbox also lacks
`/dev/net/tun`. No namespace networking acceptance result is claimed.

After installing the prerequisites in the README, run:

```sh
make build
sudo python3 lab/acceptance.py
```

This must prove the actual TUN namespace arrangement, unmodified AF_PACKET
corruption, bidirectional ping/traceroute, capture checksums and TTL, protection
switches, MTU errors, lack of kernel bypass, and repeatable cleanup. Retain the
result and evidence under `artifacts/stage1-*` before marking the Stage 1
networking requirements complete.

PostgreSQL import/query acceptance is also pending. A ready-to-run example is
in `lab/postgres.sql`; update its trace path and run it against a disposable
database. JSONL encoding itself is covered by automated tests.

Stages 2–6 are not implemented. Their design gates remain in the project plan.

## Capture decoding permission fix

A user-run acceptance test reached offline pcap decoding, where tcpdump could
not open the private capture under the project directory. The harness now opens
the capture itself and passes that descriptor as tcpdump's standard input. This
supports privilege dropping and pathname restrictions without changing file
permissions or keeping tcpdump privileged.

A focused check reproduced pathname access denial using a mode-000 pcap, then
successfully decoded the same capture through its previously opened descriptor.
Python syntax validation also passed. The full privileged acceptance suite still
needs a rerun with this change.

## Checks after running a privileged lab

Go's recursive `./...` pattern attempts to enter the root-owned capture
directories and fails for an unprivileged user. `make check` now selects the
source packages explicitly: `.`, `./cmd/...`, and `./drivers/...`. Capture
permissions stay private, and checks can run after a lab without sudo.
