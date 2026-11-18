#!/usr/bin/env python3
"""Own physical Ethernet ports and an optional virtual Stage 3 endpoint.

Physical mode requires two physical ports. Explicit hybrid mode creates its
own virtual left endpoint and still requires a physical right port.
"""
import argparse
import fcntl
import ipaddress
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import signal
import stat
import subprocess
import sys
import tempfile

ROOT = Path(__file__).resolve().parent.parent
STATE = Path("/run/goodnews-lab-stage3")
NAMESPACE = "gn3-r1"
NM_FILE = Path("/run/NetworkManager/conf.d/90-goodnews-stage3.conf")
OFFLOADS = ("rx-checksumming", "tx-checksumming", "scatter-gather",
            "tcp-segmentation-offload", "generic-segmentation-offload",
            "generic-receive-offload", "large-receive-offload")
AGGREGATION = OFFLOADS[3:]
PORT_SYSCTLS = {"ipv4": ("forwarding", "rp_filter", "send_redirects", "accept_redirects", "proxy_arp",
                         "arp_filter", "arp_ignore", "arp_announce"),
                "ipv6": ("disable_ipv6", "forwarding", "accept_ra", "autoconf", "addr_gen_mode")}


class LabError(RuntimeError):
    pass


def run(*args, namespace=None):
    argv = [str(arg) for arg in args]
    if namespace:
        argv = ["ip", "netns", "exec", namespace, *argv]
    result = subprocess.run(argv, capture_output=True, text=True, timeout=20,
                            env={**os.environ, "LC_ALL": "C"})
    if result.returncode:
        raise LabError(f"{' '.join(argv)}: {result.stderr.strip() or result.stdout.strip()}")
    return result.stdout.strip()


def write_json(path, value):
    fd, temporary = tempfile.mkstemp(prefix=".write-", dir=path.parent)
    try:
        with os.fdopen(fd, "w") as stream:
            json.dump(value, stream, indent=2)
            stream.write("\n")
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, path)
        directory = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def subnets(left, right):
    result = [ipaddress.IPv4Network(value) for value in (left, right)]
    for network in result:
        first = int(network.network_address) >> 24
        if network.prefixlen > 30 or first in (0, 127) or first >= 224:
            raise LabError("lab subnets require at least two usable unicast host addresses")
    if result[0].overlaps(result[1]):
        raise LabError("left and right subnets overlap")
    return result


class NetworkManager:
    def __init__(self, path=NM_FILE):
        self.path = path

    def status(self, device):
        # Require a running NetworkManager rather than guessing ownership when
        # another host network manager might be configuring the selected port.
        managed = run("nmcli", "-g", "GENERAL.NM-MANAGED", "device", "show", device)
        autoconnect = run("nmcli", "-g", "GENERAL.AUTOCONNECT", "device", "show", device)
        connection = run("nmcli", "-g", "GENERAL.CON-UUID", "device", "show", device)
        if managed not in ("yes", "no") or autoconnect not in ("yes", "no"):
            raise LabError(f"cannot determine NetworkManager ownership of {device}")
        return {"managed": managed, "autoconnect": autoconnect, "connection": connection}

    def available(self):
        if self.path.exists() or self.path.is_symlink():
            raise LabError(f"refusing existing NetworkManager exclusion {self.path}")

    @staticmethod
    def contents(ports):
        content = "# Owned by goodnews Stage 3; remove through lab3-down.\n"
        for port in ports:
            content += (f"[device-goodnews-stage3-{port['logical']}]\n"
                        f"match-device=mac:{port['identity']['mac']}\nmanaged=0\n\n")
        return content

    def verify(self, ports):
        info = self.path.lstat()
        if (not stat.S_ISREG(info.st_mode) or info.st_uid != os.geteuid()
                or self.path.read_text() != self.contents(ports)):
            raise LabError("NetworkManager exclusion contents or ownership changed")

    def exclude(self, ports):
        self.path.parent.mkdir(parents=True, exist_ok=True)
        fd = os.open(self.path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
        with os.fdopen(fd, "w") as stream:
            stream.write(self.contents(ports))
        run("nmcli", "general", "reload", "conf")
        for port in ports:
            run("nmcli", "device", "set", port["device"], "autoconnect", "no")
            run("nmcli", "device", "set", port["device"], "managed", "no")
            status = self.status(port["device"])
            if status["managed"] != "no" or status["autoconnect"] != "no":
                raise LabError(f"NetworkManager still manages {port['device']}")

    def restore(self, ports):
        if self.path.is_symlink():
            raise LabError("NetworkManager exclusion was replaced by a symlink")
        if self.path.exists():
            self.verify(ports)
        # Returning a device creates a new NetworkManager device object. Pin it
        # unmanaged before removing the exclusion so old unmanaged ports cannot
        # autoconnect in the interval between reload and state restoration.
        for port in ports:
            run("nmcli", "device", "set", port["device"], "managed", "no")
            run("nmcli", "device", "set", port["device"], "autoconnect", port["network_manager"]["autoconnect"])
        if self.path.exists():
            self.path.unlink()
            run("nmcli", "general", "reload", "conf")
        for port in ports:
            run("nmcli", "device", "set", port["device"], "managed", port["network_manager"]["managed"])
            actual = self.status(port["device"])
            if any(actual[key] != port["network_manager"][key] for key in ("managed", "autoconnect")):
                raise LabError(f"failed to restore NetworkManager state of {port['device']}")


class Linux:
    def links(self, namespace=None):
        return json.loads(run("ip", "-j", "-d", "address", "show", namespace=namespace))

    def port(self, device, namespace=None):
        matches = [link for link in self.links(namespace) if link["ifname"] == device]
        if len(matches) != 1:
            raise LabError(f"missing device {device} in {namespace or 'original namespace'}")
        return matches[0]

    def physical(self, device):
        path = Path("/sys/class/net") / device
        return (path.joinpath("device").exists() and not path.joinpath("wireless").exists()
                and not path.joinpath("phy80211").exists())

    def identity(self, device, namespace=None):
        link = self.port(device, namespace)
        fields = dict(line.split(": ", 1) for line in run("ethtool", "-i", device, namespace=namespace).splitlines()
                      if ": " in line)
        return {"mac": link["address"], "driver": fields.get("driver"), "bus": fields.get("bus-info", "")}

    def routes(self, device=None):
        args = ["ip", "-j", "route", "show", "table", "all"]
        if device:
            args += ["dev", device]
        routes = json.loads(run(*args))
        if device:
            routes += json.loads(run("ip", "-j", "-6", "route", "show", "table", "all", "dev", device))
        return routes

    def features(self, device, namespace=None):
        result = {}
        for line in run("ethtool", "-k", device, namespace=namespace).splitlines():
            match = re.fullmatch(r"\s*([\w-]+): (on|off)(.*)", line)
            if match:
                result[match[1]] = {"on": match[2] == "on", "fixed": "[fixed]" in match[3]}
                requested = re.search(r"\[requested (on|off)\]", match[3])
                if requested:
                    result[match[1]]["requested"] = requested[1] == "on"
        if not result:
            raise LabError(f"cannot read offloads for {device}")
        return result

    def feature(self, device, name, enabled, namespace=None):
        run("ethtool", "-K", device, name, "on" if enabled else "off", namespace=namespace)

    def port_sysctls(self, device):
        return {f"net.{family}.conf.{device}.{name}": run("sysctl", "-n", f"net.{family}.conf.{device}.{name}")
                for family, names in PORT_SYSCTLS.items() for name in names}

    def restore_sysctls(self, settings):
        for key, value in settings.items():
            run("sysctl", "-qw", f"{key}={value}")
        if any(run("sysctl", "-n", key) != value for key, value in settings.items()):
            raise LabError("failed to restore interface sysctls")

    def link(self, device, namespace=None, *, up=None, mtu=None):
        args = ["ip", "link", "set", "dev", device]
        if mtu is not None:
            args += ["mtu", str(mtu)]
        if up is not None:
            args += ["up" if up else "down"]
        run(*args, namespace=namespace)

    def move(self, device, source, target):
        run("ip", "link", "set", "dev", device, "netns", target or str(os.getpid()), namespace=source)

    def namespace_inode(self, name):
        path = Path("/run/netns") / name
        if path.is_symlink():
            raise LabError(f"namespace path is a symlink: {name}")
        return path.stat().st_ino if path.exists() else None

    def namespace_create(self, name):
        run("ip", "netns", "add", name)

    def namespace_delete(self, name):
        run("ip", "netns", "delete", name)

    def pids(self, name):
        return run("ip", "netns", "pids", name).split()

    def isolate(self, name):
        for setting in ("net.ipv4.ip_forward=0", "net.ipv4.conf.all.rp_filter=0",
                        "net.ipv4.conf.default.rp_filter=0", "net.ipv4.conf.all.send_redirects=0",
                        "net.ipv4.conf.all.accept_redirects=0", "net.ipv6.conf.all.disable_ipv6=1",
                        "net.ipv6.conf.default.disable_ipv6=1"):
            run("sysctl", "-qw", setting, namespace=name)
        self.link("lo", name, up=True)

    def forwarding(self, name):
        return run("sysctl", "-n", "net.ipv4.ip_forward", namespace=name)

    def isolate_port(self, device, namespace):
        settings = {"ipv4": {"forwarding": 0, "rp_filter": 0, "send_redirects": 0, "accept_redirects": 0},
                    "ipv6": {"disable_ipv6": 1, "forwarding": 0}}
        for family, values in settings.items():
            for name, value in values.items():
                run("sysctl", "-qw", f"net.{family}.conf.{device}.{name}={value}", namespace=namespace)

    def isolated_port(self, device, namespace):
        return run("sysctl", "-n", f"net.ipv4.conf.{device}.forwarding",
                   f"net.ipv6.conf.{device}.disable_ipv6", namespace=namespace) == "0\n1"

    def veth_create(self, router, device, host, peer, alias):
        # Both ends are created atomically inside owned namespaces. The alias
        # also identifies our cable if setup is interrupted before journaling
        # its MACs/ifindices.
        run("ip", "link", "add", "name", device, "alias", alias, "type", "veth",
            "peer", "name", peer, "netns", host, namespace=router)

    def veth_peer_ifindex(self, device, namespace):
        link = self.port(device, namespace)
        if link.get("link_index") is not None:
            return link["link_index"]
        # Equal interface indices in separate namespaces can omit link_index
        # from ip's JSON. veth(4) exposes the actual peer through ethtool stats;
        # do not assume an absent field means this interface is its own peer.
        output = run("ethtool", "-S", device, namespace=namespace)
        match = re.search(r"^\s*peer_ifindex:\s*(\d+)\s*$", output, re.MULTILINE)
        if match and int(match[1]) > 0:
            return int(match[1])
        raise LabError(f"cannot identify veth peer of {device}")

    def endpoint_configure(self, namespace, device, network):
        run("ip", "address", "add", f"{network[2]}/{network.prefixlen}", "dev", device, namespace=namespace)
        run("ip", "route", "add", "default", "via", str(network[1]), "dev", device, namespace=namespace)

    def endpoint_routes(self, namespace, device):
        return json.loads(run("ip", "-j", "-4", "route", "show", "default", "dev", device, namespace=namespace))


class Lab:
    def __init__(self, backend=None, manager=None, state=STATE, namespace=NAMESPACE, artifacts=None,
                 host_namespace=None):
        self.backend = backend or Linux()
        self.manager = manager or NetworkManager()
        self.state = Path(state)
        self.namespace = namespace
        self.host_namespace = host_namespace or ("gn3-h1" if namespace == NAMESPACE else namespace + "-h1")
        self.artifacts = Path(artifacts or ROOT / "artifacts")

    def save(self, manifest):
        write_json(self.state / "manifest.json", manifest)

    def load(self):
        info = self.state.lstat()
        if (not stat.S_ISDIR(info.st_mode) or info.st_uid != os.geteuid()
                or stat.S_IMODE(info.st_mode) != 0o700):
            raise LabError("unsafe Stage 3 state directory")
        path = self.state / "manifest.json"
        info = path.lstat()
        if not stat.S_ISREG(info.st_mode) or info.st_uid != os.geteuid() or stat.S_IMODE(info.st_mode) != 0o600:
            raise LabError("unsafe Stage 3 manifest")
        manifest = json.loads(path.read_text())
        if (manifest["version"] != 1 or manifest["namespace"] != self.namespace
                or manifest["origin_inode"] != os.stat("/proc/self/ns/net").st_ino):
            raise LabError("manifest belongs to another version or namespace")
        virtual = manifest.get("virtual")
        if (manifest.get("mode", "physical") not in ("physical", "hybrid")
                or bool(virtual) != (manifest.get("mode", "physical") == "hybrid")
                or virtual and (virtual["namespace"] != self.host_namespace
                                or virtual["router_device"] != "left0" or virtual["host_device"] != "gn0")):
            raise LabError("invalid Stage 3 topology manifest")
        return manifest

    def preflight(self, devices, networks, mode="physical"):
        if mode not in ("physical", "hybrid") or len(devices) != 2 or len(networks) != 2:
            raise LabError("select physical or hybrid mode with two subnet slots")
        if mode == "physical" and (not all(devices) or len(set(devices)) != 2):
            raise LabError("select two distinct physical Ethernet devices, or explicitly select hybrid mode")
        if mode == "hybrid" and (devices[0] or not devices[1] or devices[1] == "left0"):
            raise LabError("hybrid mode requires only a right physical device; left is the lab-owned virtual cable")
        if self.state.exists() or self.state.is_symlink():
            raise LabError(f"existing lab state at {self.state}; inspect or run down")
        if self.backend.namespace_inode(self.namespace) is not None:
            raise LabError(f"refusing existing namespace {self.namespace}")
        if mode == "hybrid" and self.backend.namespace_inode(self.host_namespace) is not None:
            raise LabError(f"refusing existing virtual endpoint namespace {self.host_namespace}")
        self.manager.available()
        links = self.backend.links()
        ports = []
        for logical, device, network in zip(("left", "right"), devices, networks):
            if mode == "hybrid" and logical == "left":
                continue
            if not re.fullmatch(r"[a-zA-Z0-9_-]{1,15}", device):
                raise LabError(f"invalid or missing device name: {device!r}")
            link = self.backend.port(device)
            if not self.backend.physical(device) or link.get("link_type") != "ether":
                raise LabError(f"{device} is not a physical wired Ethernet port")
            if 0 < link.get("max_mtu", 0) < 1500:
                raise LabError(f"{device} cannot support MTU 1500")
            if link.get("master") or any(child.get("link_index") == link["ifindex"] for child in links):
                raise LabError(f"{device} is part of a bridge, bond, VLAN, or other attachment")
            nm = self.manager.status(device)
            if link.get("addr_info") or self.backend.routes(device) or nm["connection"] not in ("", "--"):
                raise LabError(f"{device} is configured; select an unused port without addresses, routes, or active connection")
            features = self.backend.features(device)
            for name in AGGREGATION:
                if features.get(name, {}).get("on") and features[name]["fixed"]:
                    raise LabError(f"{device}: required aggregation offload {name} is fixed on")
            ports.append({"logical": logical, "device": device, "subnet": str(network),
                          "identity": self.backend.identity(device), "mtu": link["mtu"],
                          "up": "UP" in link["flags"], "features": features, "network_manager": nm,
                          "sysctls": self.backend.port_sysctls(device),
                          "touched": False, "restored": False})
        if len(ports) == 2 and ports[0]["identity"] == ports[1]["identity"]:
            raise LabError("ports do not have distinct hardware identities")
        for route in self.backend.routes():
            destination = route.get("dst", "default")
            if destination in ("default", "0.0.0.0/0"):
                continue
            if any(ipaddress.IPv4Network(destination, strict=False).overlaps(network) for network in networks):
                raise LabError(f"lab subnet overlaps existing route {destination}; choose alternate subnets")
        virtual = None
        if mode == "hybrid":
            virtual = {"namespace": self.host_namespace, "namespace_inode": None,
                       "router_device": "left0", "host_device": "gn0", "subnet": str(networks[0]),
                       "alias": "goodnews-stage3-" + secrets.token_hex(16),
                       "router_identity": None, "host_identity": None,
                       "router_ifindex": None, "host_ifindex": None}
        return {"version": 1, "namespace": self.namespace, "mode": mode, "virtual": virtual,
                "origin_inode": os.stat("/proc/self/ns/net").st_ino, "namespace_inode": None,
                "manager_touched": False, "ports": ports, "phase": "prepared"}

    def verify_namespace(self, manifest, idle=False):
        inode = self.backend.namespace_inode(self.namespace)
        if inode is not None and inode != manifest["namespace_inode"]:
            raise LabError("router namespace identity changed; refusing to adopt it")
        if idle and inode is not None and self.backend.pids(self.namespace):
            raise LabError("router namespace has running processes; stop them before cleanup")
        return inode

    def verify_port(self, port, namespace):
        if self.backend.identity(port["device"], namespace) != port["identity"]:
            raise LabError(f"{port['device']} hardware identity changed; refusing to touch it")

    def verify_host_namespace(self, manifest, idle=False):
        virtual = manifest.get("virtual")
        if not virtual:
            return None
        inode = self.backend.namespace_inode(virtual["namespace"])
        if inode is not None and inode != virtual["namespace_inode"]:
            raise LabError("virtual endpoint namespace identity changed; refusing to adopt it")
        if idle and inode is not None and self.backend.pids(virtual["namespace"]):
            raise LabError("virtual endpoint namespace has running processes; stop them before cleanup")
        return inode

    def verify_virtual(self, manifest, router_present, host_present):
        virtual = manifest.get("virtual")
        if not virtual:
            return
        router_links = {link["ifname"]: link for link in self.backend.links(self.namespace)} if router_present else {}
        host_links = {link["ifname"]: link for link in self.backend.links(virtual["namespace"])} if host_present else {}
        if host_links.keys() - {"lo", virtual["host_device"]}:
            raise LabError("unexpected devices in virtual endpoint namespace; refusing cleanup")
        a, b = router_links.get(virtual["router_device"]), host_links.get(virtual["host_device"])
        if a is None and b is None:
            return
        if (a is None or b is None or a.get("ifalias") != virtual["alias"]
                or any(link.get("linkinfo", {}).get("info_kind") != "veth" for link in (a, b))):
            details = json.dumps({"expected_alias": virtual["alias"], "router": a, "host": b}, sort_keys=True)
            raise LabError(f"virtual cable identity changed; refusing to touch it: {details}")
        if (self.backend.veth_peer_ifindex(virtual["router_device"], self.namespace) != b["ifindex"]
                or self.backend.veth_peer_ifindex(virtual["host_device"], virtual["namespace"]) != a["ifindex"]):
            raise LabError("virtual cable peer identity changed; refusing to touch it")
        for side, link, namespace, device in (("router", a, self.namespace, virtual["router_device"]),
                                              ("host", b, virtual["namespace"], virtual["host_device"])):
            if virtual[side + "_identity"] is not None:
                if (link["ifindex"] != virtual[side + "_ifindex"]
                        or self.backend.identity(device, namespace) != virtual[side + "_identity"]):
                    raise LabError("virtual cable identity changed; refusing to touch it")

    def disable_offloads(self, device, namespace):
        for name, current in self.backend.features(device, namespace).items():
            if name in AGGREGATION and current["on"] and current["fixed"]:
                raise LabError(f"{device}: required aggregation offload {name} is fixed on")
            if name in OFFLOADS and current["on"] and not current["fixed"]:
                self.backend.feature(device, name, False, namespace)

    def create_virtual(self, manifest):
        virtual = manifest.get("virtual")
        if not virtual:
            return
        host = virtual["namespace"]
        self.backend.namespace_create(host)
        virtual["namespace_inode"] = self.backend.namespace_inode(host)
        self.save(manifest)
        self.backend.isolate(host)
        self.backend.veth_create(self.namespace, virtual["router_device"], host,
                                 virtual["host_device"], virtual["alias"])
        for side, namespace, device in (("router", self.namespace, virtual["router_device"]),
                                         ("host", host, virtual["host_device"])):
            virtual[side + "_identity"] = self.backend.identity(device, namespace)
            virtual[side + "_ifindex"] = self.backend.port(device, namespace)["ifindex"]
        self.save(manifest)
        for namespace, device in ((self.namespace, virtual["router_device"]), (host, virtual["host_device"])):
            self.backend.isolate_port(device, namespace)
            self.disable_offloads(device, namespace)
            self.backend.link(device, namespace, mtu=1500, up=True)
        self.backend.endpoint_configure(host, virtual["host_device"], ipaddress.IPv4Network(virtual["subnet"]))

    def up(self, devices, networks, mode="physical"):
        manifest = self.preflight(devices, networks, mode)
        self.state.mkdir(mode=0o700)
        self.save(manifest)
        try:
            manifest["manager_touched"] = True
            self.save(manifest)
            self.manager.exclude(manifest["ports"])
            # Recheck after exclusion to catch a connection activated during preflight.
            for port in manifest["ports"]:
                if self.backend.port(port["device"]).get("addr_info") or self.backend.routes(port["device"]):
                    raise LabError(f"{port['device']} acquired configuration during setup")
            self.backend.namespace_create(self.namespace)
            manifest["namespace_inode"] = self.backend.namespace_inode(self.namespace)
            self.save(manifest)
            self.backend.isolate(self.namespace)
            for port in manifest["ports"]:
                self.verify_port(port, None)
                port["touched"] = True
                self.save(manifest)
                self.backend.link(port["device"], up=False)
                self.backend.move(port["device"], None, self.namespace)
                self.backend.isolate_port(port["device"], self.namespace)
                self.disable_offloads(port["device"], self.namespace)
                self.backend.link(port["device"], self.namespace, mtu=1500, up=True)
            self.create_virtual(manifest)
            config = {"version": 1, "router_id": "r1", "interfaces": [], "routes": [],
                      "observability": {"directory": str(self.artifacts / "runs"),
                                        "socket": str(self.state / "r1.sock")},
                      "protections": {"verify_ipv4_checksum": True, "decrement_ttl": True}}
            attachments = list(manifest["ports"])
            if manifest.get("virtual"):
                attachments.insert(0, {"logical": "left", "device": manifest["virtual"]["router_device"],
                                       "subnet": manifest["virtual"]["subnet"]})
            for port in attachments:
                network = ipaddress.IPv4Network(port["subnet"])
                config["interfaces"].append({"name": port["logical"], "address": f"{network[1]}/{network.prefixlen}",
                                             "mtu": 1500, "driver": "ethernet", "device": port["device"]})
            write_json(self.state / "router.json", config)
            manifest["phase"] = "ready"
            self.save(manifest)
            self.check()
        except BaseException as error:
            try:
                self.down()
            except BaseException as recovery:
                raise LabError(f"setup failed: {error}; recovery incomplete: {recovery}; retain {self.state} and rerun down") from error
            raise
        return config

    def check(self):
        manifest = self.load()
        if manifest["phase"] != "ready" or self.verify_namespace(manifest) is None:
            raise LabError("lab setup is incomplete")
        if self.backend.forwarding(self.namespace) != "0":
            raise LabError("kernel forwarding enabled in router namespace")
        self.manager.verify(manifest["ports"])
        links = self.backend.links(self.namespace)
        expected = {"lo", *(port["device"] for port in manifest["ports"])}
        if manifest.get("virtual"):
            expected.add(manifest["virtual"]["router_device"])
        if {link["ifname"] for link in links} != expected:
            raise LabError("unexpected devices in router namespace")
        for link in links:
            if link["ifname"] == "lo" and any(not ipaddress.ip_address(address["local"]).is_loopback
                                             for address in link.get("addr_info", [])):
                raise LabError("unexpected kernel address on router loopback")
        for port in manifest["ports"]:
            self.verify_port(port, self.namespace)
            link = self.backend.port(port["device"], self.namespace)
            if not self.backend.isolated_port(port["device"], self.namespace):
                raise LabError(f"{port['device']} kernel forwarding/IPv6 isolation changed")
            if link.get("addr_info") or "UP" not in link["flags"] or link["mtu"] != 1500:
                raise LabError(f"{port['device']} must be up, address-free, with MTU 1500")
            for name, feature in self.backend.features(port["device"], self.namespace).items():
                if name in OFFLOADS and feature["on"] and not feature["fixed"]:
                    raise LabError(f"{port['device']} offload {name} is still enabled")
        virtual = manifest.get("virtual")
        if virtual:
            host = virtual["namespace"]
            if self.verify_host_namespace(manifest) is None:
                raise LabError("virtual endpoint namespace is missing")
            self.verify_virtual(manifest, True, True)
            if self.backend.forwarding(host) != "0":
                raise LabError("kernel forwarding enabled in virtual endpoint namespace")
            for namespace, device in ((self.namespace, virtual["router_device"]), (host, virtual["host_device"])):
                link = self.backend.port(device, namespace)
                if ("UP" not in link["flags"] or link["mtu"] != 1500
                        or not self.backend.isolated_port(device, namespace)):
                    raise LabError("virtual endpoint link settings or isolation changed")
                if namespace == self.namespace and link.get("addr_info"):
                    raise LabError("virtual router port must remain address-free")
                for name, feature in self.backend.features(device, namespace).items():
                    if name in OFFLOADS and feature["on"] and (not feature["fixed"] or name in AGGREGATION):
                        raise LabError(f"virtual cable offload {name} is still enabled")
            network = ipaddress.IPv4Network(virtual["subnet"])
            addresses = self.backend.port(virtual["host_device"], host).get("addr_info", [])
            if (len(addresses) != 1 or addresses[0].get("family") != "inet"
                    or addresses[0].get("local") != str(network[2]) or addresses[0].get("prefixlen") != network.prefixlen):
                raise LabError("virtual endpoint address changed")
            routes = self.backend.endpoint_routes(host, virtual["host_device"])
            if (len(routes) != 1 or routes[0].get("gateway") != str(network[1])
                    or routes[0].get("dev") != virtual["host_device"]):
                raise LabError("virtual endpoint route changed")
        return manifest

    def restore_features(self, port):
        # Restore dependencies first, then verify the complete mutable feature
        # set, including features implicitly changed by ethtool dependencies.
        def differences():
            current = self.backend.features(port["device"])
            return [(name, value.get("requested", value["on"])) for name, value in port["features"].items()
                    if not value["fixed"] and (current.get(name, {}).get("on") != value["on"]
                        or current.get(name, {}).get("requested", current.get(name, {}).get("on"))
                        != value.get("requested", value["on"]))]

        for _ in range(4):
            changes = differences()
            if not changes:
                return
            changes.sort(key=lambda change: (change[0] not in OFFLOADS, OFFLOADS.index(change[0])
                                              if change[0] in OFFLOADS else 0))
            for name, enabled in changes:
                self.backend.feature(port["device"], name, enabled)
        if differences():
            raise LabError(f"cannot restore offloads of {port['device']}")

    def archive(self, manifest):
        uid = int(os.environ.get("SUDO_UID", os.geteuid()))
        gid = int(os.environ.get("SUDO_GID", os.getegid()))
        if not (0 <= uid < 2**32 - 1 and 0 <= gid < 2**32 - 1):
            raise LabError("invalid artifact owner")
        if not self.artifacts.exists():
            self.artifacts.mkdir(mode=0o700)
            os.chown(self.artifacts, uid, gid)
        directory = Path(tempfile.mkdtemp(prefix="stage3-lifecycle-", dir=self.artifacts))
        write_json(directory / "settings.json", manifest)
        write_json(directory / "result.json", {"stage": 3, "mode": manifest.get("mode", "physical"),
                                               "lifecycle": "restored", "physical_acceptance": "not claimed"})
        for path in (*directory.iterdir(), directory):
            os.chown(path, uid, gid)
        return directory

    def down(self):
        if not self.state.exists() and not self.state.is_symlink():
            return None
        manifest = self.load()
        for path in self.state.iterdir():
            if path.name in ("manifest.json", "router.json", "r1.sock"):
                continue
            info = path.lstat()
            if not (path.name.startswith(".write-") and stat.S_ISREG(info.st_mode)
                    and info.st_uid == os.geteuid() and info.st_nlink == 1):
                raise LabError(f"unexpected lab state file {path}; refusing cleanup")
        inode = self.verify_namespace(manifest, idle=True)
        host_inode = self.verify_host_namespace(manifest, idle=True)
        origin = {link["ifname"] for link in self.backend.links()}
        target = {link["ifname"] for link in self.backend.links(self.namespace)} if inode else set()
        expected = {"lo", *(port["device"] for port in manifest["ports"])}
        if manifest.get("virtual"):
            expected.add(manifest["virtual"]["router_device"])
        if target - expected:
            raise LabError("unexpected devices in router namespace; refusing cleanup")
        self.verify_virtual(manifest, inode is not None, host_inode is not None)
        # Validate every port before restoring either, so replacement never
        # causes partial cleanup of an otherwise healthy lab.
        locations = {}
        for port in manifest["ports"]:
            device = port["device"]
            if device in origin and device in target:
                raise LabError(f"{device} name exists in both namespaces")
            if device not in origin and device not in target:
                raise LabError(f"missing adapter {device}; reconnect it and rerun down")
            namespace = self.namespace if device in target else None
            self.verify_port(port, namespace)
            locations[device] = namespace
        manifest["phase"] = "restoring"
        self.save(manifest)
        for port in manifest["ports"]:
            if port["touched"] and not port["restored"]:
                namespace = locations[port["device"]]
                self.backend.link(port["device"], namespace, up=False)
                if namespace:
                    self.backend.move(port["device"], namespace, None)
                self.restore_features(port)
                self.backend.restore_sysctls(port["sysctls"])
                self.backend.link(port["device"], mtu=port["mtu"], up=port["up"])
                actual = self.backend.port(port["device"])
                if actual["mtu"] != port["mtu"] or ("UP" in actual["flags"]) != port["up"]:
                    raise LabError(f"failed to restore link settings of {port['device']}")
                port["restored"] = True
                self.save(manifest)
        if inode:
            self.backend.namespace_delete(self.namespace)
            manifest["namespace_inode"] = None
            self.save(manifest)
        if host_inode:
            self.backend.namespace_delete(manifest["virtual"]["namespace"])
            manifest["virtual"]["namespace_inode"] = None
            self.save(manifest)
        if manifest["manager_touched"]:
            self.manager.restore(manifest["ports"])
            manifest["manager_touched"] = False
            self.save(manifest)
        manifest["phase"] = "restored"
        evidence = self.archive(manifest)
        for name in ("r1.sock", "router.json", "manifest.json"):
            path = self.state / name
            if path.exists() or path.is_symlink():
                path.unlink()
        for path in self.state.glob(".write-*"):
            path.unlink()
        self.state.rmdir()
        return evidence


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    for command in ("preflight", "up"):
        sub = commands.add_parser(command)
        sub.add_argument("--mode", choices=("physical", "hybrid"), default="physical")
        sub.add_argument("--left-device", default="")
        sub.add_argument("--right-device", required=True)
        sub.add_argument("--left-subnet", default="10.0.0.0/24")
        sub.add_argument("--right-subnet", default="10.0.1.0/24")
    commands.add_parser("check")
    commands.add_parser("down")
    args = parser.parse_args()
    if os.geteuid() != 0:
        parser.error("run with sudo; preflight reads privileged device ownership")
    for tool in ("ip", "ethtool", "sysctl", "nmcli"):
        if not shutil.which(tool):
            parser.error(f"missing tool: {tool}")
    if args.command == "preflight":
        print(json.dumps(Lab().preflight([args.left_device, args.right_device],
                                        subnets(args.left_subnet, args.right_subnet), args.mode), indent=2))
        return
    # Lock lives outside the state directory so removing state cannot split
    # concurrent up/down operations across two different lock-file inodes.
    fd = os.open(str(STATE) + ".lock", os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600)
    try:
        info = os.fstat(fd)
        if (not stat.S_ISREG(info.st_mode) or info.st_uid != 0
                or stat.S_IMODE(info.st_mode) != 0o600 or info.st_nlink != 1):
            raise LabError("unsafe Stage 3 lock file")
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        def interrupted(_signum, _frame):
            raise InterruptedError("terminated")

        signal.signal(signal.SIGTERM, interrupted)
        lab = Lab()
        if args.command == "up":
            networks = subnets(args.left_subnet, args.right_subnet)
            devices = [args.left_device, args.right_device]
            lab.up(devices, networks, args.mode)
            print(f"Ready. Run: sudo ip netns exec {NAMESPACE} ./bin/goodnews run --config {STATE}/router.json")
            if args.mode == "hybrid":
                print(f"Virtual left host: {lab.host_namespace} ({networks[0][2]}); physical right endpoint: {networks[1][2]}/{networks[1].prefixlen}, gateway {networks[1][1]}")
        elif args.command == "check":
            lab.check()
            print("Stage 3 settings verified; check carrier and perform physical endpoint acceptance.")
        else:
            evidence = lab.down()
            print(f"Restored; lifecycle evidence: {evidence}" if evidence else "No Stage 3 lab state.")
    finally:
        os.close(fd)


if __name__ == "__main__":
    try:
        main()
    except (LabError, OSError, ValueError, subprocess.TimeoutExpired) as error:
        sys.exit(f"stage3: {error}")
