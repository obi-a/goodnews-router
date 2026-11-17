# Continuous integration

`.github/workflows/ci.yml` runs on every push, pull request, and manual dispatch.
It uses disposable Ubuntu 24.04 VMs and reads the Go version/toolchain from
`go.mod`. The Linux router binaries run on Ubuntu; physical endpoints can run
Linux, Windows, or macOS. Windows/macOS binary builds are not applicable.

| Check | Local equivalent |
| --- | --- |
| Build, race tests, vet, source syntax, module verification, Python lifecycle tests | `make check build` |
| IPv4, Ethernet/ARP, and routing-message fuzzing | `make fuzz` |
| Workflow syntax and expressions | `make workflow-lint` (actionlint 1.7.12) |
| Stage 1 TUN acceptance | `make integration` |
| Stage 2 Ethernet/ARP/impairment acceptance | `make integration2` |
| Stage 3 synthetic namespace lifecycle and Linux endpoint regression | `make integration3` |
| Stage 4 six-router DV/LS convergence and failure acceptance | `make integration4` |

CI also runs the root-only artifact ownership test and imports actual networking
acceptance traces/counter snapshots into a disposable PostgreSQL 17 service.
The import compares JSONL row counts, action/drop aggregates, and complete
counter records. No database dependency is added to the router.

For a local database check, use a disposable database and completed evidence:

```sh
python3 -B lab/verify_postgres.py --evidence artifacts/STAGE1-EVIDENCE --database 'postgresql://USER@localhost/DISPOSABLE_DATABASE'
```

Supply authentication through the normal PostgreSQL environment or password
file. The verifier uses temporary tables and rolls the transaction back. The
original [manual SQL exercise](../lab/postgres.sql) remains available.

To run the privileged ownership test without building as root:

```sh
go test -race -c -o bin/owner.test .
sudo ./bin/owner.test -test.run '^TestObserverSudoOwnership$' -test.v
```

Source-package discovery stays inside `.`, `cmd`, `drivers`, and `internal`,
so private artifact directories never become Go packages. `TESTFLAGS` overrides
the default `-race -count=1`; CI adds atomic coverage. `FUZZ_TIME` defaults to
10 seconds for each target. Source checks inspect files without rewriting them.

Networking jobs have independent runners and do not fail-fast across stages.
They require namespace/raw-socket privileges and fail when prerequisites are
missing. Existing acceptance harnesses stop their own router processes and
clean up owned labs in `finally` blocks. A hard VM cancellation discards the
runner, including its namespaces. Timeouts or cleanup failures cannot convert a
failed test into a passing job.

Build/check results, binaries, fuzz diagnostics, and networking logs/configs/
captures/result files are uploaded for 14 days, including on failure. Tar
archives preserve their file modes. Tests check ownership and modes before
archiving; archive collection does not change the original evidence.

After pushing the workflow and completing its first run, configure the repository
ruleset or branch protection for the default branch to require **CI required**.
That stable aggregate fails if any build, fuzz, or networking job fails or is
skipped. Repository settings are not changed by a workflow file; an administrator
must enable that required status check. Official actions are pinned to verified
v7 commit SHAs. Update those pins deliberately when updating action versions.

CI covers the automated behavior of implemented stages. Stage 3 adapter drivers,
NetworkManager integration on the physical router, endpoint firewall policies,
and real Windows/macOS/Linux Ethernet results remain hardware acceptance gates.
Follow [stage3.md](stage3.md) and record evidence in
[verification.md](verification.md) before closing them.
