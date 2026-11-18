#!/usr/bin/env python3
"""Six routing processes and twelve owned namespaces, with virtual cables only."""
import argparse
import fcntl
import ipaddress
import json
import os
from pathlib import Path
import shutil
import signal
import stat
import subprocess
import sys
import tempfile
import time

from stage3 import LabError, run, write_json

ROOT = Path(__file__).resolve().parent.parent
STATE = Path("/run/goodnews-lab-stage4")
BIN = ROOT / "bin/goodnews"
IDS = ("u", "v", "w", "x", "y", "z")
EDGES = (("u", "v", 2), ("u", "x", 1), ("u", "w", 5),
         ("v", "x", 2), ("v", "w", 3), ("x", "y", 1),
         ("x", "w", 3), ("y", "w", 1), ("y", "z", 2), ("w", "z", 5))
NAMES = tuple(f"gn4-{id}" for id in IDS) + tuple(f"gn4-h-{id}" for id in IDS)
OFFLOADS = ("rx-checksumming", "tx-checksumming", "scatter-gather",
            "tcp-segmentation-offload", "generic-segmentation-offload",
            "generic-receive-offload", "large-receive-offload")


def configs(algorithm="dv", directory=STATE, artifacts=ROOT / "artifacts/runs"):
    if algorithm not in ("dv", "ls"):
        raise LabError("algorithm must be dv or ls")
    result = {}
    for i, id in enumerate(IDS):
        result[id] = {"version": 1, "router_id": id,
                      "interfaces": [{"name": "lan", "device": "lan", "driver": "ethernet",
                                      "address": f"10.45.{i}.1/24", "mtu": 1500}],
                      "routes": [], "routing": {"algorithm": algorithm, "links": []},
                      "observability": {"directory": str(artifacts),
                                        "socket": str(directory / f"{id}.sock")}}
    for i, (a, b, cost) in enumerate(EDGES):
        for side, (id, peer) in enumerate(((a, b), (b, a))):
            result[id]["interfaces"].append({"name": f"to-{peer}", "device": f"to-{peer}",
                                             "driver": "ethernet", "mtu": 1500,
                                             "address": f"10.44.0.{i * 4 + side + 1}/30"})
            result[id]["routing"]["links"].append({"interface": f"to-{peer}",
                                                  "peer": f"10.44.0.{i * 4 + 2 - side}",
                                                  "cost": cost})
    return result


def inode(name):
    path = Path("/run/netns") / name
    return path.stat().st_ino if path.exists() else None


def process_identity(pid):
    try:
        fields = Path(f"/proc/{pid}/stat").read_text().rsplit(")", 1)[1].split()
        if fields[0] == "Z":
            return None
        executable = Path(f"/proc/{pid}/exe").stat()
        return {"pid": pid, "start": fields[19],
                "namespace": Path(f"/proc/{pid}/ns/net").stat().st_ino,
                "executable": os.readlink(f"/proc/{pid}/exe").removesuffix(" (deleted)"),
                "executable_inode": executable.st_ino, "executable_device": executable.st_dev}
    except (FileNotFoundError, ProcessLookupError):
        return None


def create_evidence(prefix="stage4-"):
    owner = (int(os.environ.get("SUDO_UID", os.geteuid())),
             int(os.environ.get("SUDO_GID", os.getegid())))
    if any(value < 0 or value >= 2**32 - 1 for value in owner):
        raise LabError("invalid invoking owner")
    base = ROOT / "artifacts"
    if not base.exists():
        base.mkdir(mode=0o700)
        os.chown(base, *owner)
    path = Path(tempfile.mkdtemp(prefix=prefix, dir=base))
    os.chown(path, *owner)
    return path


class Lab:
    def __init__(self, algorithm="dv", artifacts=None):
        self.algorithm = algorithm
        self.artifacts = Path(artifacts).resolve() if artifacts else None
        self.manifest = None

    def command(self, *args, namespace=None):
        output = run(*args, namespace=namespace)
        if self.artifacts:
            with (self.artifacts / "commands.jsonl").open("a") as stream:
                stream.write(json.dumps({"args": list(map(str, args)), "namespace": namespace,
                                         "stdout": output}) + "\n")
        return output

    def save(self):
        write_json(STATE / "manifest.json", self.manifest)

    def load(self):
        info = STATE.lstat()
        if not stat.S_ISDIR(info.st_mode) or info.st_uid != 0 or stat.S_IMODE(info.st_mode) != 0o700:
            raise LabError("unsafe Stage 4 state directory")
        path = STATE / "manifest.json"
        info = path.lstat()
        if not stat.S_ISREG(info.st_mode) or info.st_uid != 0 or stat.S_IMODE(info.st_mode) != 0o600:
            raise LabError("unsafe Stage 4 manifest")
        self.manifest = json.loads(path.read_text())
        m = self.manifest
        if (m["version"] != 1 or m["project"] != str(ROOT)
                or m["origin_inode"] != Path("/proc/self/ns/net").stat().st_ino
                or set(m["namespaces"]) - set(NAMES)
                or set(m["processes"]) - set(IDS)):
            raise LabError("manifest belongs to another lab or namespace")
        self.algorithm = m["algorithm"]
        self.artifacts = Path(m["artifacts"])
        return m

    def verify_namespaces(self):
        for name, expected in self.manifest["namespaces"].items():
            actual = inode(name)
            if actual is not None and actual != expected:
                raise LabError(f"namespace {name} was replaced; refusing to touch it")

    def offloads(self, namespace, device):
        settings = self.command("ethtool", "-k", device, namespace=namespace)
        for line in settings.splitlines():
            if ":" not in line:
                continue
            feature, description = map(str.strip, line.split(":", 1))
            if feature in OFFLOADS and description.startswith("on"):
                if "[fixed]" in description:
                    if feature in OFFLOADS[3:]:
                        raise LabError(f"{namespace}/{device}: fixed aggregation offload")
                else:
                    self.command("ethtool", "-K", device, feature, "off", namespace=namespace)

    def verify_offloads(self, namespace, device):
        settings = self.command("ethtool", "-k", device, namespace=namespace)
        for line in settings.splitlines():
            if ":" not in line:
                continue
            feature, description = map(str.strip, line.split(":", 1))
            if (feature in OFFLOADS and description.startswith("on")
                    and ("[fixed]" not in description or feature in OFFLOADS[3:])):
                raise LabError(f"unexpected enabled offload {namespace}/{device}/{feature}")

    def pair(self, a, a_device, b, b_device, index):
        self.command("ip", "-n", a, "link", "add", a_device, "type", "veth", "peer", "name", "peer0")
        self.command("ip", "-n", a, "link", "set", "peer0", "netns", b)
        self.command("ip", "-n", b, "link", "set", "peer0", "name", b_device)
        for side, (namespace, device) in enumerate(((a, a_device), (b, b_device))):
            self.command("ip", "-n", namespace, "link", "set", device, "address",
                         f"02:44:00:00:{index:02x}:{side + 1:02x}", "mtu", "1500", "up")
            self.offloads(namespace, device)

    def up(self):
        if STATE.exists() or STATE.is_symlink():
            raise LabError(f"{STATE} exists; inspect it or run lab4-down")
        for tool in ("ip", "sysctl", "ethtool"):
            if not shutil.which(tool):
                raise LabError(f"missing host tool: {tool}")
        for name in NAMES:
            if inode(name) is not None:
                raise LabError(f"namespace {name} already exists; refusing to adopt it")
        for route in json.loads(self.command("ip", "-j", "-4", "route", "show", "table", "all")):
            if route.get("dst", "default") == "default":
                continue
            network = ipaddress.IPv4Network(route["dst"], strict=False)
            if any(network.overlaps(ipaddress.IPv4Network(pool)) for pool in ("10.44.0.0/16", "10.45.0.0/16")):
                raise LabError(f"host route {network} overlaps the Stage 4 address pools")
        if self.artifacts is None:
            self.artifacts = create_evidence("stage4-lab-")
        self.artifacts.mkdir(mode=0o700, parents=True, exist_ok=True)
        STATE.mkdir(mode=0o700)
        self.manifest = {"version": 1, "project": str(ROOT), "algorithm": self.algorithm,
                         "origin_inode": Path("/proc/self/ns/net").stat().st_ino,
                         "artifacts": str(self.artifacts), "namespaces": {}, "processes": {}}
        self.save()
        try:
            for name in NAMES:
                self.command("ip", "netns", "add", name)
                self.manifest["namespaces"][name] = inode(name)
                self.save()
                self.command("ip", "-n", name, "link", "set", "lo", "up")
                self.command("sysctl", "-qw", "net.ipv4.ip_forward=0",
                             "net.ipv4.conf.all.rp_filter=0", "net.ipv4.conf.default.rp_filter=0",
                             "net.ipv4.conf.all.send_redirects=0", "net.ipv4.conf.all.accept_redirects=0",
                             "net.ipv6.conf.all.disable_ipv6=1", "net.ipv6.conf.default.disable_ipv6=1",
                             namespace=name)
            for i, (a, b, _) in enumerate(EDGES):
                self.pair(f"gn4-{a}", f"to-{b}", f"gn4-{b}", f"to-{a}", i)
            for i, id in enumerate(IDS):
                self.pair(f"gn4-{id}", "lan", f"gn4-h-{id}", "eth0", i + len(EDGES))
                self.command("ip", "-n", f"gn4-h-{id}", "address", "add", f"10.45.{i}.2/24", "dev", "eth0")
                self.command("ip", "-n", f"gn4-h-{id}", "route", "add", "default", "via", f"10.45.{i}.1")
            for id, config in configs(self.algorithm, artifacts=self.artifacts / "runs").items():
                write_json(STATE / f"{id}.json", config)
                write_json(self.artifacts / f"{id}.json", config)
            self.check()
        except BaseException:
            self.down()
            raise
        print(f"Stage 4 {self.algorithm} setup ready; evidence: {self.artifacts}", flush=True)

    def check(self):
        self.load()
        self.verify_namespaces()
        expected = configs(self.algorithm)
        if set(self.manifest["namespaces"]) != set(NAMES):
            raise LabError("incomplete namespace manifest")
        for name in NAMES:
            if inode(name) is None:
                raise LabError(f"missing namespace {name}")
            if self.command("sysctl", "-n", "net.ipv4.ip_forward", namespace=name) != "0":
                raise LabError(f"kernel forwarding is enabled in {name}")
        for id in IDS:
            namespace = f"gn4-{id}"
            if json.loads(self.command("ip", "-n", namespace, "-j", "-4", "address", "show", "scope", "global")):
                # ip includes empty address records on some releases.
                records = json.loads(self.command("ip", "-n", namespace, "-j", "address", "show"))
                if any(address["family"] == "inet" and address["local"] != "127.0.0.1"
                       for record in records for address in record.get("addr_info", [])):
                    raise LabError(f"kernel IPv4 address installed in {namespace}")
            for interface in expected[id]["interfaces"]:
                device = interface["device"]
                records = json.loads(self.command("ip", "-n", namespace, "-j", "link", "show", device))
                if len(records) != 1 or "UP" not in records[0]["flags"] or records[0]["mtu"] != 1500:
                    raise LabError(f"invalid lab interface {namespace}/{device}")
                self.verify_offloads(namespace, device)
            hosts = json.loads(self.command("ip", "-n", f"gn4-h-{id}", "-j", "-4", "address", "show", "dev", "eth0"))
            if not any(address["local"] == f"10.45.{IDS.index(id)}.2" and address["prefixlen"] == 24
                       for record in hosts for address in record.get("addr_info", [])):
                raise LabError(f"wrong endpoint address for {id}")
            routes = json.loads(self.command("ip", "-n", f"gn4-h-{id}", "-j", "-4", "route", "show", "default"))
            if len(routes) != 1 or routes[0].get("gateway") != f"10.45.{IDS.index(id)}.1" or routes[0].get("dev") != "eth0":
                raise LabError(f"wrong endpoint gateway for {id}")
            self.verify_offloads(f"gn4-h-{id}", "eth0")
        print("Stage 4 namespaces, interfaces and kernel isolation verified", flush=True)

    def start(self, router=None):
        self.load()
        self.verify_namespaces()
        if not BIN.is_file():
            raise LabError("run make build first")
        for id in (router,) if router else IDS:
            namespace = f"gn4-{id}"
            if inode(namespace) is None:
                raise LabError(f"missing namespace {namespace}")
            if self.command("ip", "netns", "pids", namespace):
                raise LabError(f"processes already use {namespace}")
            socket = STATE / f"{id}.sock"
            if socket.exists():
                if not stat.S_ISSOCK(socket.lstat().st_mode):
                    raise LabError(f"unexpected file at {socket}")
                socket.unlink()
            log = self.artifacts / f"{id}.log"
            with log.open("a") as stream:
                process = subprocess.Popen(["ip", "netns", "exec", namespace, str(BIN), "run", "--config", str(STATE / f"{id}.json")],
                                           stdout=stream, stderr=stream)
            deadline = time.monotonic() + 5
            try:
                while time.monotonic() < deadline:
                    if process.poll() is not None:
                        raise LabError(f"router {id} exited; inspect {log}")
                    identity = process_identity(process.pid)
                    if identity and identity["namespace"] == inode(namespace) and identity["executable"] == str(BIN):
                        self.manifest["processes"][id] = identity
                        self.save()
                        if socket.exists():
                            break
                    time.sleep(0.02)
                else:
                    raise LabError(f"router {id} failed to become ready; inspect {log}")
            except BaseException:
                process.terminate()
                process.wait(timeout=5)
                raise
        print(f"Stage 4 routers started ({self.algorithm}); artifacts: {self.artifacts}", flush=True)

    def stop(self, router=None):
        self.load()
        self.verify_namespaces()
        for id in (router,) if router else IDS:
            expected = self.manifest["processes"].get(id)
            if not expected:
                continue
            current = process_identity(expected["pid"])
            if current is not None:
                if current != expected:
                    raise LabError(f"router {id} PID identity changed; refusing to signal it")
                try:
                    handle = os.pidfd_open(expected["pid"])
                except ProcessLookupError:
                    handle = None
                if handle is not None:
                    try:
                        # Recheck after acquiring the handle. Signals then target
                        # this kernel process, even if its PID gets recycled.
                        if process_identity(expected["pid"]) != expected:
                            raise LabError(f"router {id} PID identity changed; refusing to signal it")
                        try:
                            signal.pidfd_send_signal(handle, signal.SIGTERM)
                        except ProcessLookupError:
                            pass
                        deadline = time.monotonic() + 5
                        while time.monotonic() < deadline and process_identity(expected["pid"]) == expected:
                            time.sleep(0.02)
                        if process_identity(expected["pid"]) == expected:
                            signal.pidfd_send_signal(handle, signal.SIGKILL)
                            raise LabError(f"router {id} did not stop gracefully")
                    finally:
                        os.close(handle)
            del self.manifest["processes"][id]
            self.save()

    def down(self):
        if not STATE.exists() and not STATE.is_symlink():
            return
        self.load()
        self.verify_namespaces()
        self.stop()
        # Verify all resources before deleting any; never kill a user's shell.
        for name in self.manifest["namespaces"]:
            if inode(name) is not None and self.command("ip", "netns", "pids", name):
                raise LabError(f"processes still use {name}; stop them before cleanup")
        allowed = {"manifest.json"} | {f"{id}.{suffix}" for id in IDS for suffix in ("json", "sock", "sock.epoch")}
        if any(path.name not in allowed for path in STATE.iterdir()):
            raise LabError("unexpected state file; refusing cleanup")
        for name in self.manifest["namespaces"]:
            if inode(name) is not None:
                self.command("ip", "netns", "delete", name)
        for path in STATE.iterdir():
            if path.name != "manifest.json":
                path.unlink()
        (STATE / "manifest.json").unlink()
        STATE.rmdir()
        print("Stage 4 cleanup complete; evidence retained", flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("generate", "up", "check", "start", "stop", "down"))
    parser.add_argument("--algorithm", choices=("dv", "ls"), default="dv")
    parser.add_argument("--router", choices=IDS)
    parser.add_argument("--directory", type=Path)
    args = parser.parse_args()
    if args.command == "generate":
        if args.directory is None:
            parser.error("generate requires --directory")
        args.directory.mkdir(mode=0o700, parents=True, exist_ok=True)
        for id, config in configs(args.algorithm).items():
            write_json(args.directory / f"{id}.json", config)
        return
    if os.geteuid() != 0:
        parser.error("run lab lifecycle commands with sudo")
    if args.router and args.command not in ("start", "stop"):
        parser.error("--router applies only to start/stop")
    with open("/run/goodnews-lab-stage4.lock", "a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        lab = Lab(args.algorithm)
        if args.command in ("start", "stop"):
            getattr(lab, args.command)(args.router)
        else:
            getattr(lab, args.command)()


if __name__ == "__main__":
    try:
        main()
    except (LabError, OSError, ValueError, KeyError) as error:
        sys.exit(f"stage4 lab: {error}")
