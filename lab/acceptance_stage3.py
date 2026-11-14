#!/usr/bin/env python3
"""Privileged Stage 3 lifecycle regression using owned veth fixtures.

This tests real namespace moves and AF_PACKET forwarding, not physical NICs.
NetworkManager and the physical-device predicate are injected only here.
"""
import argparse
import ipaddress
import json
import os
from pathlib import Path
import signal
import subprocess
import tempfile

from acceptance import checksum, command, wait_for
from acceptance_stage2 import ipv4, packets
from stage3 import Lab, LabError, Linux, ROOT, run, subnets

NAMES = ("gn3-test-origin", "gn3-test-h1", "gn3-test-h2", "gn3-test-r1")


class FixtureLinux(Linux):
    def physical(self, device):
        return device in ("left0", "right0") and self.port(device).get("linkinfo", {}).get("info_kind") == "veth"


class FixtureManager:
    """No host NetworkManager changes: all devices start inside owned fixtures."""
    def available(self):
        pass

    def status(self, device):
        return {"managed": "no", "autoconnect": "no", "connection": ""}

    def exclude(self, ports):
        pass

    def verify(self, ports):
        pass

    def restore(self, ports):
        pass


def fixture(evidence):
    backend = FixtureLinux()
    backend.isolate(NAMES[0])
    for index, name in enumerate(("left0", "right0")):
        host = NAMES[index + 1]
        run("ip", "link", "add", name, "type", "veth", "peer", "name", "peer0")
        run("ip", "link", "set", "peer0", "netns", host)
        backend.isolate(host)
        run("ip", "address", "add", f"10.0.{index}.2/24", "dev", "peer0", namespace=host)
        backend.link("peer0", host, up=True)
        run("ip", "route", "add", "default", "via", f"10.0.{index}.1", namespace=host)
        # Fixture host packets must reach AF_PACKET with software checksums.
        for feature, value in backend.features("peer0", host).items():
            if feature in ("tx-checksumming", "scatter-gather", "tcp-segmentation-offload",
                           "generic-segmentation-offload", "generic-receive-offload") and value["on"] and not value["fixed"]:
                backend.feature("peer0", feature, False, host)
    before = {name: {"link": backend.port(name), "features": backend.features(name),
                     "sysctls": backend.port_sysctls(name)} for name in ("left0", "right0")}
    lab = Lab(backend, FixtureManager(), evidence / "state", NAMES[3], evidence / "router")
    router = None
    logfile = None
    try:
        lab.up(["left0", "right0"], subnets("10.0.0.0/24", "10.0.1.0/24"))
        config = lab.state / "router.json"
        command(ROOT / "bin/goodnews", "check", "--config", config)
        logfile = (evidence / "router.log").open("w")
        router = subprocess.Popen(["ip", "netns", "exec", NAMES[3], str(ROOT / "bin/goodnews"),
                                   "run", "--config", str(config)], stdout=logfile, stderr=logfile)
        socket = lab.state / "r1.sock"

        def ready():
            if router.poll() is not None:
                raise AssertionError("router exited; inspect router.log")
            return command(ROOT / "bin/goodnews", "show", "routes", "--socket", socket, "--json", ok=False).returncode == 0

        wait_for(ready, "Stage 3 fixture router startup")
        for index, host in enumerate(NAMES[1:3]):
            remote = f"10.0.{1-index}.2"
            command("ip", "netns", "exec", host, "ping", "-n", "-c", "2", "-W", "1", remote)
            trace = command("ip", "netns", "exec", host, "traceroute", "-I", "-n", "-q", "1", "-w", "1", "-m", "3", remote)
            if f"10.0.{index}.1" not in trace.stdout or remote not in trace.stdout:
                raise AssertionError(trace.stdout)
        lab.check()
        router.send_signal(signal.SIGTERM)
        if router.wait(timeout=5) != 0:
            raise AssertionError("router did not shut down cleanly")
        router = None
        if command("ip", "netns", "exec", NAMES[1], "ping", "-c", "1", "-W", "1", "10.0.1.2", ok=False).returncode == 0:
            raise AssertionError("stopped router still forwards")
        directory = next((evidence / "router/runs").iterdir())
        left, right = packets(directory / "left.pcap"), packets(directory / "right.pcap")
        if not all(any(frame[12:14] == b"\x08\x06" for frame in frames) for frames in (left, right)):
            raise AssertionError("missing ARP captures")
        source = ipaddress.IPv4Address("10.0.0.2").packed
        target = ipaddress.IPv4Address("10.0.1.2").packed
        requests = [packet for packet in ipv4(left) if packet[12:20] == source + target and packet[9] == 1 and packet[20] == 8]
        if not any(outgoing[12:20] == incoming[12:20] and outgoing[20:] == incoming[20:]
                   and outgoing[8] == incoming[8] - 1 and checksum(outgoing[:20]) == 0
                   for incoming in requests for outgoing in ipv4(right)):
            raise AssertionError("no forwarded packet with valid checksum and TTL decrement")
        lab.down()
        if lab.down() is not None:
            raise AssertionError("cleanup is not idempotent")
        for name in ("left0", "right0"):
            link = backend.port(name)
            for field in ("address", "mtu", "flags", "addr_info"):
                if link[field] != before[name]["link"][field]:
                    raise AssertionError(f"{name} did not restore {field}")
            if backend.features(name) != before[name]["features"] or backend.port_sysctls(name) != before[name]["sysctls"]:
                raise AssertionError(f"{name} settings did not restore")
        # A second real setup catches leftovers that an idempotent down misses.
        lab.up(["left0", "right0"], subnets("10.0.0.0/24", "10.0.1.0/24"))
        lab.down()
        (evidence / "result.json").write_text(json.dumps({"stage": 3, "lifecycle": "passed", "physical_acceptance": "not claimed"}, indent=2))
    finally:
        try:
            if router is not None:
                router.send_signal(signal.SIGTERM)
                try:
                    router.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    router.kill()
                    router.wait(timeout=5)
            if logfile is not None:
                logfile.close()
        finally:
            lab.down()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--fixture", type=Path, help=argparse.SUPPRESS)
    args = parser.parse_args()
    if os.geteuid() != 0:
        parser.error("run with sudo after make build")
    if args.fixture:
        fixture(args.fixture)
        return
    backend = Linux()
    for name in NAMES:
        if backend.namespace_inode(name) is not None:
            raise LabError(f"refusing existing fixture namespace {name}")
    owner = (int(os.environ.get("SUDO_UID", os.geteuid())), int(os.environ.get("SUDO_GID", os.getegid())))
    if any(not 0 <= value < 2**32 - 1 for value in owner):
        raise LabError("invalid artifact owner")
    artifacts = ROOT / "artifacts"
    if not artifacts.exists():
        artifacts.mkdir(mode=0o700)
        os.chown(artifacts, *owner)
    evidence = Path(tempfile.mkdtemp(prefix="stage3-test-", dir=artifacts))
    print(f"Stage 3 synthetic evidence: {evidence}", flush=True)
    owned = []
    try:
        for name in NAMES[:3]:
            backend.namespace_create(name)
            owned.append((name, backend.namespace_inode(name)))
        # The child owns/deletes its router namespace; the parent tracks only
        # its three fixtures and never touches host network configuration.
        with (evidence / "fixture.log").open("w") as output:
            process = subprocess.run(["ip", "netns", "exec", NAMES[0], "python3", "-B", str(Path(__file__).resolve()),
                                      "--fixture", str(evidence)], stdout=output, stderr=output, timeout=180)
        if process.returncode:
            raise LabError(f"fixture failed; inspect {evidence / 'fixture.log'}")
        print("Stage 3 namespace lifecycle and Linux endpoint regression passed; physical acceptance remains separate.")
    finally:
        try:
            for name, inode in reversed(owned):
                if backend.namespace_inode(name) != inode or backend.pids(name):
                    raise LabError(f"fixture {name} changed identity or is occupied; retaining it")
                backend.namespace_delete(name)
        finally:
            for path in (*evidence.rglob("*"), evidence):
                if not path.is_symlink():
                    path.chmod(0o700 if path.is_dir() else 0o600)
                    os.chown(path, *owner)


if __name__ == "__main__":
    main()
