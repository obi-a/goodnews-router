#!/usr/bin/env python3
"""Stage 2 exit tests on real AF_PACKET sockets over isolated veth cables."""
import copy
import ipaddress
import json
import os
from pathlib import Path
import re
import shutil
import signal
import stat
import struct
import subprocess
import tempfile
import time

from acceptance import checksum, command, wait_for

ROOT = Path(__file__).resolve().parent.parent
BIN = ROOT / "bin/goodnews"
BAD = ROOT / "bin/badnews"
SCRIPT = ROOT / "lab/stage2.sh"
SOCKET = "/run/goodnews-lab-stage2/r1.sock"


def packets(path):
    data = path.read_bytes()
    assert len(data) >= 24 and struct.unpack_from("<I", data, 0)[0] == 0xA1B2C3D4
    assert struct.unpack_from("<I", data, 20)[0] == 1, "capture must use Ethernet link type"
    result, offset = [], 24
    while offset < len(data):
        _, _, length, original = struct.unpack_from("<IIII", data, offset)
        assert length == original
        offset += 16
        frame = data[offset:offset + length]
        assert len(frame) == length and length >= 14
        result.append(frame)
        offset += length
    return result


def ipv4(frames):
    result = []
    for frame in frames:
        if frame[12:14] == b"\x08\x00" and len(frame) >= 34:
            size = struct.unpack_from("!H", frame, 16)[0]
            if 20 <= size <= len(frame) - 14:
                result.append(frame[14:14 + size])
    return result


def main():
    if os.geteuid() != 0:
        raise SystemExit("run with sudo after make build")
    for tool in ("ip", "sysctl", "ethtool", "ping", "traceroute", "tcpdump"):
        if not shutil.which(tool):
            raise SystemExit(f"missing host tool: {tool}")
    if not BIN.is_file() or not BAD.is_file():
        raise SystemExit("run make build first")
    owner = (int(os.environ.get("SUDO_UID", os.geteuid())), int(os.environ.get("SUDO_GID", os.getegid())))
    if any(value < 0 or value >= 2**32 - 1 for value in owner):
        raise SystemExit("invalid SUDO_UID or SUDO_GID")
    artifacts = ROOT / "artifacts"
    try:
        artifacts.mkdir(mode=0o700)
    except FileExistsError:
        pass
    else:
        os.chown(artifacts, *owner)
    evidence = Path(tempfile.mkdtemp(prefix="stage2-", dir=artifacts))
    os.chown(evidence, *owner)
    print(f"Stage 2 evidence: {evidence}", flush=True)
    process, logfile, created = None, None, False

    def logged(*args, **kwargs):
        result = command(*args, **kwargs)
        with (evidence / "commands.jsonl").open("a") as stream:
            stream.write(json.dumps({"args": [str(a) for a in args], "returncode": result.returncode,
                                     "stdout": result.stdout, "stderr": result.stderr}) + "\n")
        return result

    def host(namespace, *args, **kwargs):
        return logged("ip", "netns", "exec", namespace, *args, **kwargs)

    def show(resource):
        return json.loads(logged(BIN, "show", resource, "--socket", SOCKET, "--json").stdout)

    def bad(*args):
        return logged(BAD, "--socket", SOCKET, *args)

    def ping(ok=True):
        return host("gn2-h1", "ping", "-n", "-c", "1", "-W", "1", "10.0.1.2", ok=ok)

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
            raise AssertionError("AF_PACKET reader failed to unblock on shutdown")
        finally:
            process = None
            logfile.close()
        assert code == 0, f"router exited {code}"

    try:
        logged("bash", SCRIPT, "up")
        created = True
        config = copy.deepcopy(json.loads((ROOT / "lab/stage2.json").read_text()))
        config["observability"]["directory"] = str(evidence / "runs")
        config_file = evidence / "config.json"
        config_file.write_text(json.dumps(config, indent=2))
        logfile = (evidence / "router.log").open("w")
        process = subprocess.Popen(["ip", "netns", "exec", "gn2-r1", str(BIN), "run", "--config", str(config_file)],
                                   stdout=logfile, stderr=logfile)

        def ready():
            assert process.poll() is None, "router exited; inspect router.log"
            return command(BIN, "show", "routes", "--socket", SOCKET, "--json", ok=False).returncode == 0

        wait_for(ready, "Ethernet router startup")
        run = next((evidence / "runs").iterdir())
        for namespace, gateway, remote in (("gn2-h1", "10.0.0.1", "10.0.1.2"), ("gn2-h2", "10.0.1.1", "10.0.0.2")):
            host(namespace, "ping", "-n", "-c", "2", "-W", "1", gateway)
            host(namespace, "ping", "-n", "-c", "2", "-W", "1", remote)
            result = host(namespace, "traceroute", "-n", "-q", "1", "-w", "1", "-m", "3", remote)
            assert gateway in result.stdout and remote in result.stdout, result.stdout
        arp = show("arp")
        assert arp["enabled"] and all(arp["interfaces"][side][0]["state"] == "reachable" for side in ("left", "right"))
        before = (run / "trace.jsonl").read_text().count('"action":"arp_request"')
        ping()
        assert (run / "trace.jsonl").read_text().count('"action":"arp_request"') == before, "cache hit broadcast another request"
        before = show("counters")["drop_reasons"].get("invalid_checksum", 0)
        host("gn2-h1", BIN, "craft", "--interface", "gn0", "--dst-mac", "02:00:00:00:00:01", "--bad-checksum")
        wait_for(lambda: show("counters")["drop_reasons"].get("invalid_checksum", 0) > before, "unmodified crafted corruption")
        for setting in ("loss", "corruption"):
            bad("link", "right", setting, "100")
            assert ping(ok=False).returncode != 0, f"{setting} had no effect"
            if setting == "loss":
                assert show("counters")["drop_reasons"].get("impaired_loss", 0) > 0
            bad("clear")
            ping()
        bad("link", "right", "delay", "150")
        result = ping()
        match = re.search(r"time[=<]([\d.]+)\s*ms", result.stdout)
        assert match and float(match.group(1)) >= 120, result.stdout
        bad("clear")
        bad("link", "right", "down")
        assert ping(ok=False).returncode != 0
        assert show("counters")["drop_reasons"].get("link_down", 0) > 0
        bad("clear")
        ping()
        before = show("counters")["drop_reasons"].get("arp_timeout", 0)
        host("gn2-h1", "ping", "-n", "-c", "1", "-W", "4", "10.0.1.99", ok=False)
        wait_for(lambda: show("counters")["drop_reasons"].get("arp_timeout", 0) > before, "ARP timeout")
        logged("bash", SCRIPT, "check")
        stop()
        traces = [json.loads(line) for line in (run / "trace.jsonl").read_text().splitlines()]
        ids = [t["packet_id"] for t in traces]
        assert ids == sorted(set(ids)), "history IDs are not unique and ordered"
        assert all(t["parent_id"] in ids for t in traces if "parent_id" in t)
        assert any(t["action"] == "queue_arp" for t in traces)
        assert any(t["action"] == "queue_delay" for t in traces)
        tables = {row["version"] for row in map(json.loads, (run / "fib.jsonl").read_text().splitlines())}
        assert all(t["fib_version"] in tables for t in traces)
        counters = {row["after_packet_id"] for row in map(json.loads, (run / "counters.jsonl").read_text().splitlines())}
        assert all(t.get("counter_packet_id", t.get("parent_id", t["packet_id"])) in counters for t in traces)
        left_frames, right_frames = packets(run / "left.pcap"), packets(run / "right.pcap")
        for frames in (left_frames, right_frames):
            arps = [f[14:] for f in frames if f[12:14] == b"\x08\x06"]
            assert any(a[6:8] == b"\x00\x01" for a in arps) and any(a[6:8] == b"\x00\x02" for a in arps)
            assert all(a[:6] == b"\x00\x01\x08\x00\x06\x04" for a in arps)
        left, right = ipv4(left_frames), ipv4(right_frames)
        src, dst = ipaddress.ip_address("10.0.0.2").packed, ipaddress.ip_address("10.0.1.2").packed
        requests = [p for p in left if p[12:16] == src and p[16:20] == dst and p[9] == 1 and len(p) > 28 and p[20] == 8 and checksum(p[:20]) == 0]
        assert requests
        assert any(q[12:20] == p[12:20] and q[20:] == p[20:] and q[8] == p[8]-1 and checksum(q[:20]) == 0 for p in requests for q in right), "no forwarded packet with TTL decrement and valid checksum"
        assert any(checksum(p[:20]) != 0 for p in left), "crafted checksum repaired before reaching router"
        assert any(checksum(p[:20]) != 0 for p in right), "corruption did not reach Ethernet capture"
        for path in (run / "left.pcap", run / "right.pcap"):
            with path.open("rb") as stream:
                logged("tcpdump", "-enn", "-r", "-", stdin=stream)
        for path in (evidence / "runs", run, *run.iterdir()):
            info = path.stat()
            assert (info.st_uid, info.st_gid) == owner
            assert stat.S_IMODE(info.st_mode) == (0o700 if path.is_dir() else 0o600)
        assert Path(SOCKET).stat().st_uid == 0 and stat.S_IMODE(Path(SOCKET).stat().st_mode) == 0o600
        assert ping(ok=False).returncode != 0, "kernel bypass: stopped router still forwards"
        logged("bash", SCRIPT, "down")
        created = False
        logged("bash", SCRIPT, "up")
        created = True
        logged("bash", SCRIPT, "down")
        created = False
        (evidence / "result.json").write_text(json.dumps({"stage": 2, "status": "passed"}, indent=2))
        print("Stage 2 Ethernet/ARP and badnews acceptance passed.")
    finally:
        try:
            stop()
        finally:
            try:
                if created:
                    logged("bash", SCRIPT, "down")
            finally:
                for path in (*evidence.rglob("*"), evidence):
                    if not path.is_symlink():
                        path.chmod(0o700 if path.is_dir() else 0o600)
                        os.chown(path, *owner)


if __name__ == "__main__":
    main()
