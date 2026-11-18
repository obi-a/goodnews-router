"""Failure, identity, and restoration checks for one physical + one virtual leg."""
import copy
import ipaddress
import json
from pathlib import Path
import tempfile
import unittest

from stage3 import Lab, LabError, subnets
from test_stage3 import FakeLinux, FakeManager


class HybridLinux(FakeLinux):
    def __init__(self):
        super().__init__()
        self.host_inodes = {}
        self.busy_namespaces = set()
        self.defaults = {}
        self.pairs = []

    def namespace_inode(self, name):
        return self.inode if name == "test-r1" else self.host_inodes.get(name)

    def namespace_create(self, name):
        if name == "test-r1":
            return super().namespace_create(name)
        self.fail("host_namespace")
        self.host_inodes[name] = 43
        self.devices[name] = {"lo": {"ifname": "lo"}}

    def namespace_delete(self, name):
        if name == "test-r1":
            self.fail("delete")
            for router, device, host, peer in self.pairs:
                self.devices[router].pop(device, None)
                self.devices[host].pop(peer, None)
                for endpoint in (device, peer):
                    self.settings.pop(endpoint, None)
                    self.sysctls.pop(endpoint, None)
            self.pairs.clear()
            return super().namespace_delete(name)
        self.fail("host_delete")
        del self.devices[name]
        del self.host_inodes[name]
        self.defaults.pop(name, None)

    def isolate(self, name):
        if name != "test-r1":
            self.fail("host_isolate")
        super().isolate(name)

    def pids(self, name):
        return ["123"] if name in self.busy_namespaces else super().pids(name)

    def veth_create(self, router, device, host, peer, alias):
        self.fail("veth")
        for index, (namespace, name, other_index) in enumerate(((router, device, 51), (host, peer, 50)), 50):
            self.devices[namespace][name] = {"ifname": name, "ifindex": index, "link_index": other_index,
                                             "link_type": "ether", "linkinfo": {"info_kind": "veth"},
                                             "address": f"02:03:00:00:00:{index:02x}", "addr_info": [],
                                             "mtu": 1500, "flags": []}
            self.settings[name] = {"tx-checksumming": {"on": True, "fixed": False},
                                   "generic-receive-offload": {"on": True, "fixed": False}}
            self.sysctls[name] = {f"net.ipv6.conf.{name}.disable_ipv6": "1"}
        self.devices[router][device]["ifalias"] = alias
        self.pairs.append((router, device, host, peer))
        self.fail("veth_after_create")

    def feature(self, device, name, enabled, namespace=None):
        if device in ("left0", "gn0"):
            self.fail("virtual_offload")
        super().feature(device, name, enabled, namespace)

    def link(self, device, namespace=None, **kwargs):
        if device in ("left0", "gn0"):
            self.fail("virtual_link")
        super().link(device, namespace, **kwargs)

    def endpoint_configure(self, namespace, device, network):
        self.devices[namespace][device]["addr_info"] = [{"family": "inet", "local": str(network[2]),
                                                          "prefixlen": network.prefixlen}]
        self.defaults[namespace] = [{"dst": "default", "gateway": str(network[1]), "dev": device}]
        self.fail("endpoint")

    def endpoint_routes(self, namespace, device):
        return copy.deepcopy(self.defaults[namespace])


class HybridTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.path = Path(self.temporary.name)
        self.backend, self.manager = HybridLinux(), FakeManager()
        self.lab = Lab(self.backend, self.manager, self.path / "state", "test-r1", self.path / "evidence")
        self.networks = subnets("10.0.0.0/24", "10.0.1.0/24")
        self.original = copy.deepcopy((self.backend.devices, self.backend.settings, self.backend.sysctls,
                                       self.manager.managed, self.manager.autoconnect))

    def up(self):
        return self.lab.up(["", "eth1"], self.networks, mode="hybrid")

    def assert_restored(self):
        self.assertEqual((self.backend.devices, self.backend.settings, self.backend.sysctls,
                          self.manager.managed, self.manager.autoconnect), self.original)
        self.assertFalse(self.lab.state.exists())
        self.assertFalse(self.backend.host_inodes)
        self.assertFalse(self.manager.present)

    def test_success_and_repeated_lifecycle(self):
        for _ in range(2):
            config = self.up()
            self.assertEqual([p["device"] for p in config["interfaces"]], ["left0", "eth1"])
            self.assertEqual(self.lab.check()["mode"], "hybrid")
            self.assertEqual(self.manager.managed["eth0"], "yes")
            self.assertIn("eth0", self.backend.devices[None])
            self.assertEqual(self.backend.endpoint_routes(self.lab.host_namespace, "gn0")[0]["gateway"], "10.0.0.1")
            archive = self.lab.down()
            self.assertEqual(json.loads((archive / "result.json").read_text())["mode"], "hybrid")
            self.assert_restored()
            self.assertIsNone(self.lab.down())

    def test_explicit_selection_and_physical_validation(self):
        for devices, mode in ((["", "eth1"], "physical"), (["eth0", "eth1"], "hybrid"),
                              (["", ""], "hybrid"), (["", "left0"], "hybrid")):
            with self.subTest(devices=devices, mode=mode):
                with self.assertRaises(LabError):
                    self.lab.preflight(devices, self.networks, mode)
                self.assert_restored()
        self.backend.virtual.add("eth1")
        with self.assertRaisesRegex(LabError, "physical wired"):
            self.up()
        self.assert_restored()

    def test_carrier_does_not_choose_the_topology(self):
        self.backend.devices[None]["eth0"]["carrier"] = 0
        manifest = self.lab.preflight(["eth0", "eth1"], self.networks)
        self.assertEqual(manifest["mode"], "physical")
        self.assertIsNone(manifest["virtual"])

    def test_existing_virtual_namespace_is_not_adopted(self):
        self.backend.host_inodes[self.lab.host_namespace] = 99
        with self.assertRaisesRegex(LabError, "existing virtual endpoint"):
            self.up()
        self.assertFalse(self.lab.state.exists())
        self.assertFalse(self.manager.present)

    def test_partial_virtual_setup_rolls_back(self):
        for operation in ("host_namespace", "host_isolate", "veth", "veth_after_create",
                          "virtual_offload", "virtual_link", "endpoint"):
            with self.subTest(operation=operation):
                self.backend.failure = operation
                with self.assertRaisesRegex(LabError, "injected"):
                    self.up()
                self.assert_restored()

    def test_cleanup_resumes_after_host_deletion_failure(self):
        self.up()
        self.backend.failure = "host_delete"
        with self.assertRaisesRegex(LabError, "host_delete"):
            self.lab.down()
        self.assertIn("eth1", self.backend.devices[None])
        self.assertTrue(self.lab.state.exists())
        self.lab.down()
        self.assert_restored()

    def test_busy_or_replaced_host_refuses_cleanup_before_port_move(self):
        self.up()
        self.backend.busy_namespaces.add(self.lab.host_namespace)
        with self.assertRaisesRegex(LabError, "running processes"):
            self.lab.down()
        self.assertIn("eth1", self.backend.devices[self.lab.namespace])
        self.backend.busy_namespaces.clear()
        self.backend.host_inodes[self.lab.host_namespace] += 1
        with self.assertRaisesRegex(LabError, "namespace identity changed"):
            self.lab.down()
        self.assertIn("eth1", self.backend.devices[self.lab.namespace])

    def test_replaced_virtual_cable_refuses_cleanup(self):
        self.up()
        device = self.backend.devices[self.lab.host_namespace]["gn0"]
        device["address"] = "02:00:ff:ff:ff:ff"
        with self.assertRaisesRegex(LabError, "virtual cable identity"):
            self.lab.down()
        self.assertIn("eth1", self.backend.devices[self.lab.namespace])

    def test_endpoint_address_and_route_drift(self):
        self.up()
        device = self.backend.devices[self.lab.host_namespace]["gn0"]
        device["addr_info"][0]["local"] = "10.0.0.99"
        with self.assertRaisesRegex(LabError, "endpoint address"):
            self.lab.check()
        device["addr_info"][0]["local"] = "10.0.0.2"
        self.backend.defaults[self.lab.host_namespace][0]["gateway"] = "10.0.0.99"
        with self.assertRaisesRegex(LabError, "endpoint route"):
            self.lab.check()
        self.lab.down()
        self.assert_restored()

    def test_alternate_subnets_configure_the_virtual_endpoint(self):
        self.networks = subnets("192.0.2.0/30", "198.51.100.0/30")
        config = self.up()
        self.assertEqual(config["interfaces"][0]["address"], "192.0.2.1/30")
        host = self.backend.port("gn0", self.lab.host_namespace)
        self.assertEqual(ipaddress.IPv4Address(host["addr_info"][0]["local"]), self.networks[0][2])
        self.lab.down()
        self.assert_restored()


if __name__ == "__main__":
    unittest.main()
