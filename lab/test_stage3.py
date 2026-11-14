"""Lifecycle failure/recovery tests. No root or live network changes."""
import copy
import json
from pathlib import Path
import tempfile
import unittest

from stage3 import Lab, LabError, Linux, NetworkManager, subnets
from unittest.mock import patch


class FakeLinux:
    def __init__(self):
        self.devices = {None: {"lo": {"ifname": "lo"}}}
        self.settings = {}
        self.sysctls = {}
        for index, name in enumerate(("eth0", "eth1"), 2):
            self.devices[None][name] = {"ifname": name, "ifindex": index, "link_type": "ether",
                                        "address": f"02:00:00:00:00:0{index}", "addr_info": [],
                                        "mtu": 1400 + index, "flags": ["UP"] if index == 2 else []}
            self.settings[name] = {"tx-checksumming": {"on": True, "fixed": False},
                                   "generic-receive-offload": {"on": True, "fixed": False}}
            self.sysctls[name] = {f"net.ipv6.conf.{name}.disable_ipv6": "1"}
        self.inode = None
        self.busy = False
        self.routes_by_device = {}
        self.all_routes = []
        self.virtual = set()
        self.failure = None
        self.forward = "0"

    def fail(self, operation):
        if self.failure == operation:
            self.failure = None
            raise LabError("injected " + operation)

    def links(self, namespace=None):
        return copy.deepcopy(list(self.devices[namespace].values()))

    def port(self, device, namespace=None):
        try:
            return copy.deepcopy(self.devices[namespace][device])
        except KeyError as error:
            raise LabError("missing device") from error

    def physical(self, device):
        return device not in self.virtual

    def identity(self, device, namespace=None):
        return {"mac": self.port(device, namespace)["address"], "driver": "fake", "bus": device}

    def routes(self, device=None):
        return self.routes_by_device.get(device, []) if device else self.all_routes

    def features(self, device, namespace=None):
        return copy.deepcopy(self.settings[device])

    def feature(self, device, name, enabled, namespace=None):
        self.fail("offload")
        value = self.settings[device][name]
        if "requested" in value:
            value["requested"] = enabled
            value["on"] = enabled and self.settings[device]["tx-checksumming"]["on"]
        else:
            value["on"] = enabled

    def port_sysctls(self, device):
        return copy.deepcopy(self.sysctls[device])

    def restore_sysctls(self, settings):
        self.fail("sysctl")
        for device in self.sysctls:
            if any(f".{device}." in key for key in settings):
                self.sysctls[device] = copy.deepcopy(settings)

    def link(self, device, namespace=None, *, up=None, mtu=None):
        self.fail("link")
        value = self.devices[namespace][device]
        if mtu is not None:
            value["mtu"] = mtu
        if up is not None:
            value["flags"] = ["UP"] if up else []

    def move(self, device, source, target):
        self.devices[target][device] = self.devices[source].pop(device)
        self.sysctls[device] = {f"net.ipv6.conf.{device}.disable_ipv6": "0"}
        self.fail("move")  # A failed reply may follow a successful kernel operation.

    def namespace_inode(self, name):
        return self.inode

    def namespace_create(self, name):
        self.fail("namespace")
        self.inode = 42
        self.devices[name] = {"lo": {"ifname": "lo"}}

    def namespace_delete(self, name):
        self.fail("delete")
        del self.devices[name]
        self.inode = None

    def pids(self, name):
        return ["123"] if self.busy else []

    def isolate(self, name):
        self.fail("isolate")

    def forwarding(self, name):
        return self.forward

    def isolated_port(self, device, namespace):
        return self.forward == "0" and self.sysctls[device][f"net.ipv6.conf.{device}.disable_ipv6"] == "1"

    def isolate_port(self, device, namespace):
        self.sysctls[device][f"net.ipv6.conf.{device}.disable_ipv6"] = "1"


class FakeManager:
    def __init__(self):
        self.managed = {"eth0": "yes", "eth1": "no"}
        self.autoconnect = {"eth0": "yes", "eth1": "no"}
        self.connection = ""
        self.failure = None
        self.present = False

    def available(self):
        if self.present:
            raise LabError("existing exclusion")

    def status(self, device):
        return {"managed": self.managed[device], "autoconnect": self.autoconnect[device], "connection": self.connection}

    def verify(self, ports):
        if not self.present:
            raise LabError("missing manager exclusion")

    def exclude(self, ports):
        self.present = True
        for port in ports:
            self.managed[port["device"]] = "no"
            self.autoconnect[port["device"]] = "no"
        if self.failure == "exclude":
            self.failure = None
            raise LabError("injected manager exclusion")

    def restore(self, ports):
        if self.failure == "restore":
            self.failure = None
            raise LabError("injected manager restoration")
        self.present = False
        for port in ports:
            self.managed[port["device"]] = port["network_manager"]["managed"]
            self.autoconnect[port["device"]] = port["network_manager"]["autoconnect"]


class LifecycleTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.path = Path(self.temporary.name)
        self.backend, self.manager = FakeLinux(), FakeManager()
        self.lab = Lab(self.backend, self.manager, self.path / "state", "test-r1", self.path / "evidence")
        self.original = copy.deepcopy((self.backend.devices, self.backend.settings, self.backend.sysctls,
                                       self.manager.managed, self.manager.autoconnect))
        self.networks = subnets("10.0.0.0/24", "10.0.1.0/24")

    def up(self):
        return self.lab.up(["eth0", "eth1"], self.networks)

    def assertRestored(self):
        self.assertEqual((self.backend.devices, self.backend.settings, self.backend.sysctls,
                          self.manager.managed, self.manager.autoconnect), self.original)
        self.assertFalse(self.lab.state.exists())
        self.assertFalse(self.manager.present)

    def test_successful_cycle_and_repeat(self):
        for _ in range(2):
            config = self.up()
            self.assertEqual(config["interfaces"][1]["address"], "10.0.1.1/24")
            self.assertNotIn("netns", config["interfaces"][0])
            self.lab.check()
            archive = self.lab.down()
            self.assertEqual(json.loads((archive / "result.json").read_text())["lifecycle"], "restored")
            self.assertRestored()
            self.assertIsNone(self.lab.down())

    def test_failures_roll_back(self):
        for operation in ("namespace", "isolate", "move", "offload", "link"):
            with self.subTest(operation=operation):
                self.backend.failure = operation
                with self.assertRaisesRegex(LabError, "injected"):
                    self.up()
                self.assertRestored()

    def test_interruption_after_move_rolls_back(self):
        original = self.backend.move

        def interrupted(*args):
            original(*args)
            self.backend.move = original
            raise KeyboardInterrupt()

        self.backend.move = interrupted
        with self.assertRaises(KeyboardInterrupt):
            self.up()
        self.assertRestored()

    def test_manager_partial_failure_rolls_back(self):
        self.manager.failure = "exclude"
        with self.assertRaisesRegex(LabError, "exclusion"):
            self.up()
        self.assertRestored()

    def test_cleanup_can_resume_after_failures(self):
        for operation in ("offload", "sysctl", "delete", "manager"):
            with self.subTest(operation=operation):
                self.up()
                if operation == "manager":
                    self.manager.failure = "restore"
                else:
                    self.backend.failure = operation
                with self.assertRaisesRegex(LabError, "injected"):
                    self.lab.down()
                self.assertTrue(self.lab.state.exists())
                self.lab.down()
                self.assertRestored()

    def test_refuses_busy_or_replaced_namespace(self):
        self.up()
        self.backend.busy = True
        with self.assertRaisesRegex(LabError, "running processes"):
            self.lab.down()
        self.backend.busy = False
        self.backend.inode += 1
        with self.assertRaisesRegex(LabError, "identity changed"):
            self.lab.down()
        self.assertTrue(self.lab.state.exists())

    def test_missing_adapter_and_replacement_retain_state(self):
        self.up()
        port = self.backend.devices["test-r1"].pop("eth1")
        with self.assertRaisesRegex(LabError, "missing adapter"):
            self.lab.down()
        # Neither port is moved until every identity has been checked.
        self.assertIn("eth0", self.backend.devices["test-r1"])
        self.backend.devices["test-r1"]["eth1"] = copy.deepcopy(port)
        self.backend.devices["test-r1"]["eth1"]["address"] = "02:00:00:00:ff:ff"
        with self.assertRaisesRegex(LabError, "identity changed"):
            self.lab.down()
        self.backend.devices["test-r1"]["eth1"] = port
        self.lab.down()
        self.assertRestored()

    def test_preflight_refusals_do_not_change_ports(self):
        cases = {
            "address": lambda: self.backend.devices[None]["eth0"].update(addr_info=[{"local": "192.0.2.2"}]),
            "route": lambda: self.backend.routes_by_device.update(eth0=[{"dst": "default"}]),
            "master": lambda: self.backend.devices[None]["eth0"].update(master="br0"),
            "child": lambda: self.backend.devices[None].update(vlan={"ifname": "vlan", "link_index": 2}),
            "virtual": lambda: self.backend.virtual.add("eth0"),
            "mtu": lambda: self.backend.devices[None]["eth0"].update(max_mtu=1400),
            "connection": lambda: setattr(self.manager, "connection", "some-uuid"),
            "overlap": lambda: self.backend.all_routes.append({"dst": "10.0.0.0/8"}),
            "namespace": lambda: setattr(self.backend, "inode", 999),
            "manager": lambda: setattr(self.manager, "present", True),
            "fixed-offload": lambda: self.backend.settings["eth0"].update(
                {"generic-receive-offload": {"on": True, "fixed": True}}),
        }
        for name, mutate in cases.items():
            with self.subTest(name=name):
                self.backend, self.manager = FakeLinux(), FakeManager()
                self.lab.backend, self.lab.manager = self.backend, self.manager
                mutate()
                before = copy.deepcopy((self.backend.devices, self.backend.settings, self.manager.managed))
                with self.assertRaises(LabError):
                    self.up()
                self.assertEqual((self.backend.devices, self.backend.settings, self.manager.managed), before)
                self.assertFalse(self.lab.state.exists())

    def test_alternate_subnets_and_duplicate_devices(self):
        self.networks = subnets("192.0.2.0/30", "198.51.100.0/30")
        with self.assertRaisesRegex(LabError, "distinct"):
            self.lab.preflight(["eth0", "eth0"], self.networks)
        config = self.up()
        self.assertEqual(config["interfaces"][0]["address"], "192.0.2.1/30")
        self.lab.down()
        for left, right in (("10.0.0.0/24", "10.0.0.0/25"), ("10.0.0.0/31", "10.0.1.0/24"),
                            ("127.0.0.0/24", "10.0.1.0/24")):
            with self.assertRaises(LabError):
                subnets(left, right)

    def test_check_detects_drift(self):
        self.up()
        self.backend.forward = "1"
        with self.assertRaisesRegex(LabError, "forwarding"):
            self.lab.check()
        self.backend.forward = "0"
        self.backend.devices["test-r1"]["eth0"]["addr_info"] = [{"local": "10.0.0.1"}]
        with self.assertRaisesRegex(LabError, "address-free"):
            self.lab.check()
        self.backend.devices["test-r1"]["eth0"]["addr_info"] = []
        self.backend.devices["test-r1"]["lo"]["addr_info"] = [{"local": "10.0.0.1"}]
        with self.assertRaisesRegex(LabError, "loopback"):
            self.lab.check()

    def test_state_permissions_and_existing_state(self):
        self.up()
        with self.assertRaisesRegex(LabError, "existing lab state"):
            self.up()
        self.lab.state.chmod(0o755)
        with self.assertRaisesRegex(LabError, "unsafe"):
            self.lab.down()

    def test_restores_requested_offloads_with_inactive_dependencies(self):
        self.backend.settings["eth0"]["tx-checksumming"]["on"] = False
        self.backend.settings["eth0"]["tcp-segmentation-offload"] = {"on": False, "fixed": False, "requested": True}
        expected = copy.deepcopy(self.backend.settings["eth0"])
        self.up()
        self.backend.settings["eth0"]["tcp-segmentation-offload"]["requested"] = False
        self.lab.down()
        self.assertEqual(self.backend.settings["eth0"], expected)

    def test_linux_offload_parser_retains_requested_state(self):
        output = "Features for eth0:\ntx-checksumming: off\n    tx-checksum-ip-generic: off [fixed]\ntcp-segmentation-offload: off [requested on]"
        with patch("stage3.run", return_value=output):
            features = Linux().features("eth0")
        self.assertEqual(features["tcp-segmentation-offload"], {"on": False, "fixed": False, "requested": True})
        self.assertTrue(features["tx-checksum-ip-generic"]["fixed"])

    def test_physical_predicate_rejects_wifi_and_virtual_links(self):
        port = self.path / "eth0"
        port.mkdir()
        with patch("stage3.Path", return_value=self.path):
            self.assertFalse(Linux().physical("eth0"))
            (port / "device").mkdir()
            self.assertTrue(Linux().physical("eth0"))
            (port / "wireless").mkdir()
            self.assertFalse(Linux().physical("eth0"))

    def test_unknown_state_is_retained_and_stale_writes_are_cleaned(self):
        self.up()
        unexpected = self.lab.state / "unrelated"
        unexpected.write_text("keep")
        with self.assertRaisesRegex(LabError, "unexpected lab state"):
            self.lab.down()
        self.assertIn("eth0", self.backend.devices["test-r1"])
        unexpected.unlink()
        (self.lab.state / ".write-interrupted").write_text("incomplete write")
        self.lab.down()
        self.assertRestored()

    def test_networkmanager_file_roundtrip_and_replacement(self):
        manifest = self.lab.preflight(["eth0", "eth1"], self.networks)
        manager = NetworkManager(self.path / "nm.conf")
        with patch("stage3.run", return_value=""), patch.object(manager, "status", return_value={"managed": "no", "autoconnect": "no"}):
            manager.exclude(manifest["ports"])
        text = manager.path.read_text()
        self.assertIn("match-device=mac:02:00:00:00:00:02", text)
        manager.path.write_text("replaced")
        with self.assertRaisesRegex(LabError, "exclusion.*changed"):
            manager.restore(manifest["ports"])
        manager.path.write_text(text)
        with patch("stage3.run", return_value=""), patch.object(manager, "status", side_effect=
                [port["network_manager"] for port in manifest["ports"]]):
            manager.restore(manifest["ports"])
        self.assertFalse(manager.path.exists())


if __name__ == "__main__":
    unittest.main()
