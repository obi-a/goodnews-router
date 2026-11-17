#!/usr/bin/env python3
"""Privileged Stage 4 DV/LS acceptance, with retained packet and algorithm evidence."""
import fcntl
import heapq
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import time

from acceptance import checksum, command
from acceptance_stage2 import ipv4, packets
from stage3 import LabError, write_json
from stage4 import BIN, EDGES, IDS, NAMES, ROOT, STATE, Lab, create_evidence, inode

BAD = ROOT / "bin/badnews"


def oracle(changed=None, stopped=(), partition=(), blocked=()):
    graph = {id: [] for id in IDS if id not in stopped}
    for a, b, cost in EDGES:
        if (a not in graph or b not in graph or a in partition or b in partition
                or (a, b) in blocked or (b, a) in blocked):
            continue
        graph[a].append((b, (changed or {}).get((a, b), cost)))
        graph[b].append((a, (changed or {}).get((b, a), cost)))
    result = {}
    for source in graph:
        best, queue = {}, [(0, "", source)]
        while queue:
            cost, first, current = heapq.heappop(queue)
            if current in best:
                continue
            best[current] = (cost, first)
            for other, weight in graph[current]:
                if other not in best:
                    heapq.heappush(queue, (cost + weight, first or other, other))
        result[source] = best
    return result


def wait_for(predicate, description, timeout=20):
    deadline, consecutive = time.monotonic() + timeout, 0
    while time.monotonic() < deadline:
        consecutive = consecutive + 1 if predicate() else 0
        if consecutive >= 3:
            return
        time.sleep(0.1)
    raise AssertionError(f"timed out waiting for {description}")


def verify_captures(directory):
    echoes, protocols = {}, 0
    for path in (directory / "runs").glob("*/*.pcap"):
        for packet in ipv4(packets(path)):
            assert checksum(packet[:20]) == 0, f"invalid IPv4 checksum in {path}"
            if packet[9] == 253:
                protocols += 1
                assert packet[8] == 1 and packet[20:23] == b"GN\x01"
                assert checksum(packet[20:]) == 0, "invalid control checksum"
            if packet[9] == 1 and len(packet) >= 28 and packet[20] == 8:
                key = packet[12:20] + packet[24:]
                echoes.setdefault(key, set()).add(packet[8])
    assert protocols > 0, "no routing packets captured"
    assert any(len(ttls) >= 3 for ttls in echoes.values()), "no multi-router TTL evidence"
    for run in (directory / "runs").iterdir():
        traces = [json.loads(line) for line in (run / "trace.jsonl").read_text().splitlines()]
        ids = [row["packet_id"] for row in traces]
        assert ids == sorted(set(ids)), "nonmonotonic packet IDs"
        known = set(ids)
        assert all(row["parent_id"] in known for row in traces if "parent_id" in row)
        fib = {row["version"] for row in map(json.loads, (run / "fib.jsonl").read_text().splitlines())}
        assert all(row["fib_version"] in fib for row in traces), "packet references missing FIB"
        events = [json.loads(line) for line in (run / "routing.jsonl").read_text().splitlines()]
        assert events and any(row["event"] == "neighbor_up" for row in events)


def exercise(algorithm, evidence):
    lab = Lab(algorithm, evidence)
    evidence.mkdir(mode=0o700)
    ping_process, ping_stream = None, None

    def show(id, resource):
        result = command(BIN, "show", resource, "--socket", STATE / f"{id}.sock", "--json", ok=False)
        return json.loads(result.stdout) if result.returncode == 0 else None

    def snapshot(label):
        write_json(evidence / f"{label}.json", {id: {name: show(id, name) for name in ("routes", "neighbors", "counters", "lsdb")}
                                                  for id in IDS if id in lab.manifest["processes"]})

    def converged(expected):
        for source, destinations in expected.items():
            response = show(source, "routes")
            if response is None:
                return False
            routes = {route["prefix"]: route for route in response["routes"]}
            for i, destination in enumerate(IDS):
                prefix = f"10.45.{i}.0/24"
                route, want = routes.get(prefix), destinations.get(destination)
                if destination == source:
                    if route is None or route["origin"] != "connected":
                        return False
                elif want is None:
                    if route is not None:
                        return False
                elif (route is None or route["origin"] != algorithm or route.get("metric") != want[0]
                      or route["interface"] != f"to-{want[1]}"):
                    return False
        return True

    def measure(label, action, expected, timeout=20):
        start = time.monotonic()
        action()
        wait_for(lambda: converged(expected), label, timeout)
        elapsed = time.monotonic() - start
        with (evidence / "convergence.jsonl").open("a") as stream:
            stream.write(json.dumps({"scenario": label, "seconds": elapsed}) + "\n")
        snapshot(label)

    def bad(id, *args):
        result = command(BAD, "--socket", STATE / f"{id}.sock", *args)
        with (evidence / "experiments.jsonl").open("a") as stream:
            stream.write(json.dumps({"router": id, "args": args, "stdout": result.stdout}) + "\n")

    def ping(source="u", destination="z", ok=True):
        result = command("ip", "netns", "exec", f"gn4-h-{source}", "ping", "-n", "-c", "2", "-W", "1",
                         f"10.45.{IDS.index(destination)}.2", ok=ok)
        with (evidence / "ping.log").open("a") as stream:
            stream.write(f"{source}->{destination}\n{result.stdout}{result.stderr}\n")
        return result

    try:
        lab.up()
        lab.start()
        measure("initial", lambda: None, oracle())
        for destination in IDS[1:]:
            ping("u", destination)
            ping(destination, "u")
        result = command("ip", "netns", "exec", "gn4-h-u", "traceroute", "-I", "-n", "-q", "1", "-w", "1", "-m", "8", "10.45.5.2")
        (evidence / "traceroute.log").write_text(result.stdout + result.stderr)
        assert "10.45.0.1" in result.stdout and "10.45.5.2" in result.stdout
        measure("cost-change", lambda: bad("u", "link", "to-x", "cost", "60"), oracle({("u", "x"): 60}))
        measure("cost-restored", lambda: bad("u", "clear"), oracle())
        measure("link-down", lambda: bad("u", "link", "to-x", "down"), oracle(blocked=(("u", "x"),)))
        ping()
        measure("link-restored", lambda: bad("u", "clear"), oracle())
        ping_stream = (evidence / "continuous-ping.log").open("w")
        ping_process = subprocess.Popen(["ip", "netns", "exec", "gn4-h-u", "ping", "-n", "-D", "-i", "0.1", "10.45.5.2"], stdout=ping_stream, stderr=ping_stream)
        time.sleep(0.4)
        measure("router-stopped", lambda: lab.stop("x"), oracle(stopped=("x",)), timeout=25)
        ping()
        measure("router-restarted", lambda: lab.start("x"), oracle())
        ping()
        time.sleep(0.4)
        ping_process.send_signal(signal.SIGINT)
        ping_process.wait(timeout=5)
        ping_stream.close()
        ping_process, ping_stream = None, None
        replies = [int(value) for value in re.findall(r"icmp_seq=(\d+).*ttl=", (evidence / "continuous-ping.log").read_text())]
        assert len(replies) >= 3 and any(b > a + 1 for a, b in zip(replies, replies[1:])), "failure had no recorded ping gap"
        if algorithm == "ls":
            # Sequence numbers may refresh in flight; compare current topology.
            records = [show(id, "lsdb")["records"] for id in IDS]
            assert all(len(db) == 6 for db in records)
            topology = lambda db: {m["origin"]: (m.get("links", []), m.get("prefixes", [])) for m in db}
            assert all(topology(db) == topology(records[0]) for db in records)
        measure("partition", lambda: (bad("z", "link", "to-y", "down"), bad("z", "link", "to-w", "down")), oracle(partition=("z",)), timeout=30)
        assert ping(ok=False).returncode != 0, "partition still reachable"
        measure("partition-restored", lambda: bad("z", "clear"), oracle())
        lab.check()
        lab.stop()
        assert ping(ok=False).returncode != 0, "kernel bypass with all routers stopped"
        verify_captures(evidence)
        lab.down()
        assert all(inode(name) is None for name in NAMES) and not STATE.exists()
        # Verify a second lifecycle, not merely an idempotent cleanup.
        lab = Lab(algorithm, evidence)
        lab.up()
        lab.check()
        lab.down()
        lab.down()
        write_json(evidence / "result.json", {"passed": True, "algorithm": algorithm})
    finally:
        if ping_process is not None:
            ping_process.terminate()
            ping_process.wait(timeout=5)
        if ping_stream is not None:
            ping_stream.close()
        if STATE.exists():
            lab.down()


def main():
    if os.geteuid() != 0:
        raise SystemExit("run with sudo after make build")
    if STATE.exists():
        raise SystemExit("existing Stage 4 lab; stop it with make lab4-down first")
    if not BIN.is_file() or not BAD.is_file():
        raise SystemExit("run make build first")
    evidence = create_evidence()
    print(f"Stage 4 acceptance evidence: {evidence}", flush=True)
    try:
        with open("/run/goodnews-lab-stage4.lock", "a") as lock:
            fcntl.flock(lock, fcntl.LOCK_EX)
            for algorithm in ("dv", "ls"):
                print(f"Testing {algorithm}", flush=True)
                exercise(algorithm, evidence / algorithm)
        write_json(evidence / "result.json", {"passed": True, "algorithms": ["dv", "ls"]})
    finally:
        owner = (int(os.environ.get("SUDO_UID", os.geteuid())), int(os.environ.get("SUDO_GID", os.getegid())))
        for path in evidence.rglob("*"):
            if not path.is_symlink():
                os.chown(path, *owner)
        os.chown(evidence, *owner)


if __name__ == "__main__":
    main()
