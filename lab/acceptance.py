#!/usr/bin/env python3
"""Privileged end-to-end tests. Uses only Python's standard library and host tools.

All artifacts survive failure. Setup refuses an existing lab; cleanup never adopts
or kills unrelated processes. Run from any directory after `make build`.
"""
import copy
import ipaddress
import json
import os
from pathlib import Path
import shutil
import signal
import struct
import subprocess
import tempfile
import time

ROOT = Path(__file__).resolve().parent.parent
BIN = ROOT / "bin/goodnews"
SCRIPT = ROOT / "lab/stage1.sh"
SOCKET = "/run/goodnews-lab-stage1/r1.sock"


def command(*args, ok=True, stdin=None):
    result = subprocess.run([str(a) for a in args], text=True, capture_output=True, timeout=15, stdin=stdin)
    if ok and result.returncode:
        raise AssertionError(f"{args}: {result.stdout}\n{result.stderr}")
    return result


def host(ns, *args, **kwargs):
    return command("ip", "netns", "exec", ns, *args, **kwargs)


def wait_for(predicate, description):
    deadline = time.monotonic() + 5
    while time.monotonic() < deadline:
        if predicate():
            return
        time.sleep(0.05)
    raise AssertionError(f"timed out waiting for {description}")


def checksum(data):
    data += b"\0" if len(data) % 2 else b""
    value = sum(struct.unpack(f"!{len(data)//2}H", data))
    while value >> 16:
        value = (value & 65535) + (value >> 16)
    return (~value) & 65535


def packets(path):
    data = path.read_bytes()
    assert len(data) >= 24
    assert struct.unpack_from("<I", data, 0)[0] == 0xA1B2C3D4
    assert struct.unpack_from("<I", data, 20)[0] == 101
    offset = 24
    result = []
    while offset < len(data):
        _, _, length, original = struct.unpack_from("<IIII", data, offset)
        assert length == original
        offset += 16
        packet = data[offset:offset+length]
        assert len(packet) == length
        result.append(packet)
        offset += length
    return result


def main():
    if os.geteuid() != 0:
        raise SystemExit("run with sudo after make build")
    for tool in ("ip", "sysctl", "ethtool", "ping", "traceroute", "tcpdump"):
        if not shutil.which(tool):
            raise SystemExit(f"missing host tool: {tool}")
    if not BIN.is_file():
        raise SystemExit("run make build first")
    (ROOT / "artifacts").mkdir(exist_ok=True)
    evidence = Path(tempfile.mkdtemp(prefix="stage1-", dir=ROOT / "artifacts"))
    print(f"Acceptance evidence: {evidence}", flush=True)
    base = json.loads((ROOT / "lab/stage1.json").read_text())
    process = None
    logfile = None
    created = False
    run_index = 0

    def show(resource):
        return json.loads(command(BIN, "show", resource, "--socket", SOCKET, "--json").stdout)

    def start(config):
        nonlocal process, logfile, run_index
        run_index += 1
        config = copy.deepcopy(config)
        directory = evidence / f"run-{run_index}"
        config["observability"]["directory"] = str(directory)
        file = evidence / f"config-{run_index}.json"
        file.write_text(json.dumps(config, indent=2))
        logfile = (evidence / f"router-{run_index}.log").open("w")
        process = subprocess.Popen(["ip", "netns", "exec", "gn-r1", str(BIN), "run", "--config", str(file)], stdout=logfile, stderr=logfile)

        def ready():
            if process.poll() is not None:
                raise AssertionError(f"router exited {process.returncode}; see {logfile.name}")
            return command(BIN, "show", "routes", "--socket", SOCKET, "--json", ok=False).returncode == 0

        wait_for(ready, "management socket")
        # Creating network sockets now also exercises namespace restoration.
        assert show("interfaces")["interfaces"][0]["name"] == "left"
        return next(directory.iterdir())

    def stop():
        nonlocal process, logfile
        if process is None:
            return
        process.send_signal(signal.SIGTERM)
        try:
            code = process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait()
            raise AssertionError("router did not unblock TUN readers during shutdown")
        finally:
            process = None
            logfile.close()
        assert code == 0, f"router exit={code}"

    def craft(*args):
        host("gn-h1", BIN, "craft", "--interface", "gn0", *args)

    def expect_drop(reason, *args):
        before = show("counters")["drop_reasons"].get(reason, 0)
        craft(*args)
        wait_for(lambda: show("counters")["drop_reasons"].get(reason, 0) > before, reason)

    try:
        command("bash", SCRIPT, "up")
        created = True
        run = start(base)
        for ns, gateway, remote in (("gn-h1", "10.0.0.1", "10.0.1.2"), ("gn-h2", "10.0.1.1", "10.0.0.2")):
            host(ns, "ping", "-n", "-c", "2", "-W", "1", gateway)
            host(ns, "ping", "-n", "-c", "2", "-W", "1", remote)
            result = host(ns, "traceroute", "-n", "-q", "1", "-w", "1", "-m", "3", remote)
            assert gateway in result.stdout and remote in result.stdout, result.stdout
            (evidence / f"traceroute-{ns}.txt").write_text(result.stdout)
        expect_drop("invalid_checksum", "--bad-checksum")
        expect_drop("invalid_version", "--version", "6")
        expect_drop("invalid_header_length", "--ihl", "4")
        expect_drop("invalid_total_length", "--total-length", "65000")
        expect_drop("ttl_expired", "--ttl", "1")
        expect_drop("no_route", "--dst", "192.0.2.1")
        expect_drop("unsupported_destination", "--dst", "224.0.0.1")
        expect_drop("no_route", "--dst", "192.0.2.1", "--icmp-type", "3")
        assert show("arp")["enabled"] is False
        stop()

        traces = [json.loads(line) for line in (run / "trace.jsonl").read_text().splitlines()]
        assert any(t.get("icmp_suppressed") == "icmp_error" for t in traces)
        ids = {t["packet_id"] for t in traces}
        assert len(ids) == len(traces)
        assert all(t["parent_id"] in ids for t in traces if "parent_id" in t)
        manifest = json.loads((run / "run.json").read_text())
        assert manifest["version"] == 1 and manifest["router_id"] == "r1"
        tables = {row["version"] for row in map(json.loads, (run / "fib.jsonl").read_text().splitlines())}
        counts = {row["after_packet_id"] for row in map(json.loads, (run / "counters.jsonl").read_text().splitlines())}
        assert 0 in counts, "missing startup counters"
        assert all(t["fib_version"] in tables for t in traces), "missing historical forwarding table"
        assert all(t.get("parent_id", t["packet_id"]) in counts for t in traces), "missing event-linked counters"
        left, right = packets(run / "left.pcap"), packets(run / "right.pcap")
        src, dst = ipaddress.ip_address("10.0.0.2").packed, ipaddress.ip_address("10.0.1.2").packed
        requests = [p for p in left if len(p) > 28 and p[0] == 0x45
                    and struct.unpack_from("!H", p, 2)[0] == len(p)
                    and p[12:16] == src and p[16:20] == dst and p[9] == 1
                    and p[20] == 8 and p[8] == 64 and checksum(p[:20]) == 0]
        assert requests, "missing host request"
        for original in requests:
            matching = [p for p in right if p[12:20] == original[12:20] and p[20:] == original[20:] and p[8] == original[8]-1 and checksum(p[:20]) == 0]
            assert matching, "missing forwarded packet with decremented TTL and valid checksum"
        assert any(checksum(p[:20]) != 0 for p in left if len(p) >= 20 and p[0] == 0x45), "craft checksum was repaired before reaching TUN"
        for path in (run / "left.pcap", run / "right.pcap"):
            # Open as the harness owner before tcpdump drops privileges. The
            # inherited descriptor also avoids pathname restrictions imposed by
            # tcpdump's AppArmor profile on captures under a home directory.
            with path.open("rb") as capture:
                result = command("tcpdump", "-nn", "-r", "-", stdin=capture)
            (evidence / f"{path.stem}-decode.txt").write_text(result.stdout + result.stderr)
        assert host("gn-h1", "ping", "-n", "-c", "1", "-W", "1", "10.0.1.2", ok=False).returncode != 0, "kernel bypass: ping works with router stopped"

        small = copy.deepcopy(base)
        small["interfaces"][1]["mtu"] = 100
        run = start(small)
        expect_drop("fragmentation_not_implemented", "--payload", "200")
        expect_drop("mtu_exceeded", "--payload", "200", "--df")
        stop()
        errors = [p for p in packets(run / "left.pcap") if len(p) >= 28 and p[9] == 1 and p[20:22] == bytes([3, 4])]
        assert errors and struct.unpack_from("!H", errors[-1], 26)[0] == 100

        unprotected = copy.deepcopy(base)
        unprotected["protections"] = {"verify_ipv4_checksum": False, "decrement_ttl": False}
        run = start(unprotected)
        assert show("interfaces")["effective_protections"] == unprotected["protections"]
        before = show("counters")["transmitted"]
        craft("--bad-checksum")
        wait_for(lambda: show("counters")["transmitted"] > before, "unprotected forwarding")
        stop()
        assert any(p[12:16] == src and p[16:20] == dst and p[8] == 64 and checksum(p[:20]) == 0 for p in packets(run / "right.pcap")), "TTL switch had no effect"

        command("bash", SCRIPT, "check")
        command("bash", SCRIPT, "down")
        created = False
        # Prove a second setup/teardown does not inherit stale devices or sockets.
        command("bash", SCRIPT, "up")
        created = True
        command("bash", SCRIPT, "down")
        created = False
        (evidence / "result.json").write_text(json.dumps({"stage": 1, "status": "passed", "postgresql": "separate manual acceptance"}, indent=2))
        print("Stage 1 networking acceptance passed.")
    finally:
        try:
            stop()
        finally:
            if created:
                command("bash", SCRIPT, "down")


if __name__ == "__main__":
    main()
