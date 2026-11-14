# Verification record

## Stage 2 Ethernet, ARP, and runtime impairments (2026-11-10)

Implemented on `feat/stage2-ethernet-arp`. The original Stage 1 TUN driver,
config, lab script, and acceptance harness remain available. Both configs pass
`goodnews check`, and all existing Stage 1 tests continue to pass.

Passed race-enabled Go tests across all source packages, `go vet`, and builds
of `goodnews` and `badnews` using Go 1.27.3. Added tests cover Ethernet/ARP wire
layout and padding, ARP expiry/retries and bounded queues, gateway resolution,
nonblocking forwarding during resolution, sender validation, retained FIB
versions, Ethernet pcaps, fault controls, bounded delay and shutdown drops, API
validation, and dashboard indexing/counter snapshots for deferred completions.

Ten-second fuzz runs passed: `FuzzEthernetARP` executed 258,692 cases and
`FuzzParseIPv4` executed 256,975 cases. Both Python harnesses parse, the Stage 2
shell script passes `bash -n`, and all ten command blocks in `docs/stage2.md`
pass both Bash and Fish syntax checks. Local links resolve. The full test suite
ran outside the sandbox because its local Unix-socket integration test is
blocked by sandbox socket restrictions.

The privileged Stage 2 acceptance attempt could not start: outside the sandbox,
`sudo -n python3 lab/acceptance_stage2.py` returned `sudo: interactive
authentication is required`. No veth/raw-socket networking result is claimed.
Run `make integration2` on the privileged Ubuntu host; it retains packet
captures, command output, traces and a success-only result under
`artifacts/stage2-*`. The empirical gate remains open until that passes. See
[the Stage 2 guide](stage2.md) for setup and expected observations.

## Artifact ownership (2026-11-10)

New router artifacts use the account identified by `SUDO_UID` and `SUDO_GID`
when the router runs as root through sudo. Ordinary execution and direct root
execution retain process ownership. Newly created parent directories and run
directories stay private (`700`), and captures and logs stay private (`600`).
Existing directories and management-socket permissions are unchanged.

Race-enabled tests, vet, and build passed. Tests cover owner selection, invalid
sudo IDs, ordinary-user execution, new parent directories, all six artifact
files, and preservation of existing parent permissions. The root-only test
also checks access as the invoking account and denial for another account.
That test was skipped here: outside the sandbox, sudo requires interactive
authentication. Full privileged networking acceptance remains pending.

The acceptance harness now checks artifact owners and modes and verifies that
the management socket remains root-owned with mode `600`. Its own evidence
files are also assigned to the invoking account, including on failure.
Python parsing, 79 command blocks in each of Bash and Fish, and 31 local
documentation links passed validation.

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

Stage 2 implementation is recorded above. Stages 3–6 remain future work with
their design gates in the project plan.

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

## CI and Stage 3 implementation (2026-11-13)

Added CI for both binaries, all source packages, race-enabled tests with
coverage, vet, module verification, source/workflow syntax, Python lifecycle
tests, both fuzz targets, and all three namespace acceptance targets. CI also
runs the root-only ownership test and imports actual acceptance JSONL into a
disposable PostgreSQL service. The stable `CI required` aggregate must be
enabled in repository branch protection after the workflow's first hosted run;
this implementation has not changed remote repository settings.

Local build/race/vet/module/source checks and both 10-second fuzz targets passed.
The pinned actionlint 1.7.12 validator accepted the workflow. Python tests cover
setup rollback, recovery across cleanup failures, missing/replaced adapters,
namespace/process ownership, unexpected files, subnet conflicts, offload
dependency restoration, and NetworkManager exclusion ownership.

A disposable, unprivileged PostgreSQL 18 server under `/tmp` verified import
and comparison of the existing dashboard fixture (seven trace records and six
counter snapshots), plus a separate JSON escaping case. The server was stopped
and its temporary database removed. The CI service uses PostgreSQL 17; its
fresh acceptance evidence import remains to be exercised on GitHub.

The physical Stage 3 lifecycle selects unused Ethernet ports, excludes them
from NetworkManager, moves them into an isolated router namespace, and records
settings for restoration. The synthetic acceptance harness uses veth devices
and an injected NetworkManager backend; it does not certify hardware. Linux,
Windows, and macOS endpoint setup and restoration are documented in stage3.md.
The existing forwarding implementation and Stage 1/2 lab interfaces are intact.

Privileged networking acceptance and the root-only ownership test were not run
in this session: host sudo requires interactive authentication, Docker access
is unavailable, and isolated user-namespace creation was denied by the host.
No physical Ethernet adapters were available for the hardware gate. Run
`make integration`, `make integration2`, and `make integration3` on a privileged
host, then complete the physical guide and retain the resulting evidence.
No passing Stage 3 hardware or hosted CI result is claimed here.

## Hosted CI validation (2026-11-14)

[The full CI run for commit 76d5b9b](https://github.com/obi-a/goodnews-router/actions/runs/43634881764)
passed on GitHub-hosted Ubuntu 24.04 runners: both binary builds, race-enabled
tests, vet/source/workflow checks, both fuzz targets, Stage 1 TUN acceptance,
the root-only artifact ownership test, PostgreSQL 17 import of fresh networking
evidence, Stage 2 Ethernet/ARP/impairment acceptance, and Stage 3 synthetic
lifecycle acceptance. The `CI required` aggregate passed. Each networking job
retained its evidence as a workflow artifact.

The first hosted run exposed an ordering error in the existing Stage 2 harness:
it inspected the management socket after clean shutdown had removed it. The
ownership/mode assertion now runs before shutdown; the stopped-router and
cleanup checks remain in place. No forwarding behavior changed.

This closes the automated Stage 1 and Stage 2 privileged namespace gates.
Stage 3 physical adapters, real NetworkManager behavior, and Windows/macOS/Linux
physical endpoint acceptance remain separate pending gates. Branch protection
still needs the `CI required` check enabled by a repository administrator.
