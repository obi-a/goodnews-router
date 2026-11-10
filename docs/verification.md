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

## Fish and Bash lab instructions

The guides support Fish and Bash without switching shells. Capture-directory
prompts are labeled by shell, and configuration examples use shared multiline
`python3 -c` commands instead of Bash here-documents.

Validated 40 command blocks with each shell's syntax checker. Both prompt
versions stored the supplied path correctly, including spaces. All three
configuration examples executed in both shells, produced identical JSON, and
passed `goodnews check`. Trace filtering also worked in both shells with a
capture-directory path containing spaces. Local documentation links resolve.

## Terminal dashboard

Implemented `goodnews dashboard` with live run discovery, disk-indexed saved-run
browsing, historical counters and full forwarding-table versions, ICMP fields,
packet details and topology. A synthetic demo is included under
`internal/dashboard/testdata/demo`.

Race-enabled tests, vet and build passed, including the router-worker/recorder/
Unix-HTTP/dashboard integration test with in-memory links. That test needed to
run outside the sandbox because the sandbox blocks Unix-socket options; it uses
no root privileges or network namespaces. Tests cover histories larger than the
decoded cache, partial/corrupt/truncated logs, missing legacy state, historical
selection during new activity, generated-response correlations, resize layouts,
key/mouse parsing, and preserving history across connection changes.

Pseudo-terminal tests verified that quitting and cancellation restore terminal
settings. A real terminal smoke check also exercised replay, Home, packet
details, and quit. The full privileged namespace ping/dashboard exercise still
needs to be run on the Ubuntu lab host. Existing router processes must be
restarted with the rebuilt binary to produce the new metadata.

After adding the dashboard guide, syntax checking passed for 51 command blocks
in each of Fish and Bash. Local documentation links resolve, and the acceptance
harness parses with its added checks for manifests, FIB history and counter IDs.

## UDP and HTTP lab instructions

Added `docs/traffic.md` with bidirectional interface explanations, the exact
UDP crafting defaults, a UDP echo server/client, HTTP over TCP, and distinctions
between closed ports, HTTP 404, missing routes and timeouts.

Syntax checks passed for 64 command blocks in each of Fish and Bash; local
links resolve and both embedded Python programs parse. A generated UDP packet
confirmed the documented addresses, ports 40000/33434, TTL 64 and 60-byte size.
The UDP server/client exchange, HTTP LICENSE response, HTTP 404 and closed-port
curl failure were exercised on loopback with temporary ports outside the
socket-restricted sandbox. These checks validate the application examples;
they do not replace the pending privileged namespace-router acceptance run.

## Stage 1 fault experiments and badnews status

Documented the available Stage 1 fault exercises and clarified that `badnews`
is a planned Stage 2 tool. Syntax checking passed for 71 blocks in each of Fish
and Bash; local links resolve. Generated packets confirmed the documented IPv4
validation drops, TTL/route ICMP responses, local ICMP rejection and forwarding
of transit ICMP with an invalid transport checksum. These checks used the
forwarding engine directly; outage and namespace-interface observations remain
manual lab exercises.
