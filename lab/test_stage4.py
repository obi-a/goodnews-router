"""Unprivileged checks of the generated topology and independent route oracle."""
import ipaddress
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from acceptance_stage4 import oracle
from stage3 import LabError
from stage4 import EDGES, IDS, NAMES, Lab, configs


class Stage4Tests(unittest.TestCase):
    def test_configured_cables_and_passive_lans(self):
        for algorithm in ("dv", "ls"):
            config = configs(algorithm)
            addresses = set()
            for id, value in config.items():
                self.assertEqual(value["routing"]["algorithm"], algorithm)
                self.assertEqual(value["routes"], [])
                self.assertNotIn("lan", [link["interface"] for link in value["routing"]["links"]])
                for interface in value["interfaces"]:
                    address = ipaddress.IPv4Interface(interface["address"])
                    self.assertNotIn(address.ip, addresses)
                    addresses.add(address.ip)
                for link in value["routing"]["links"]:
                    peer = link["interface"].removeprefix("to-")
                    other = next(item for item in config[peer]["interfaces"] if item["name"] == f"to-{id}")
                    self.assertEqual(link["peer"], str(ipaddress.IPv4Interface(other["address"]).ip))
            self.assertEqual(sum(len(c["routing"]["links"]) for c in config.values()), 2 * len(EDGES))
            self.assertEqual(len(NAMES), 12)

    def test_hand_worked_oracle_and_failures(self):
        self.assertEqual(oracle()["u"], {"u": (0, ""), "x": (1, "x"), "v": (2, "v"),
                                          "y": (2, "x"), "w": (3, "x"), "z": (4, "x")})
        self.assertEqual(oracle(changed={("u", "x"): 60})["u"]["z"], (7, "v"))
        self.assertEqual(oracle(stopped=("x",))["u"]["z"], (8, "v"))
        self.assertNotIn("z", oracle(partition=("z",))["u"])
        self.assertEqual(oracle(blocked=(("u", "x"),))["u"]["z"], (7, "v"))

    def test_preexisting_state_is_never_adopted(self):
        with tempfile.TemporaryDirectory() as directory:
            with patch("stage4.STATE", Path(directory)):
                with self.assertRaisesRegex(LabError, "exists"):
                    Lab().up()

    def test_replaced_namespace_refuses_cleanup(self):
        lab = Lab()
        lab.manifest = {"namespaces": {"gn4-u": 42}}
        with patch("stage4.inode", return_value=43):
            with self.assertRaisesRegex(LabError, "replaced"):
                lab.verify_namespaces()

    def test_cleanup_refuses_foreign_process(self):
        lab = Lab()
        expected = {"pid": 123, "start": "456", "namespace": 7, "executable": "goodnews"}
        lab.manifest = {"namespaces": {}, "processes": {"u": expected}}
        with patch.object(lab, "load"), patch.object(lab, "verify_namespaces"), \
                patch("stage4.process_identity", return_value={**expected, "start": "789"}), \
                patch("stage4.os.kill") as kill:
            with self.assertRaisesRegex(LabError, "identity changed"):
                lab.stop("u")
            kill.assert_not_called()

    def test_generated_configs_are_json(self):
        self.assertEqual(set(json.loads(json.dumps(configs()))), set(IDS))

    def test_pid_reuse_during_cleanup_is_not_signalled(self):
        lab = Lab()
        expected = {"pid": 123, "start": "456", "namespace": 7, "executable": "goodnews"}
        lab.manifest = {"namespaces": {}, "processes": {"u": expected}}
        with patch.object(lab, "load"), patch.object(lab, "verify_namespaces"), \
                patch("stage4.process_identity", side_effect=[expected, {**expected, "start": "789"}]), \
                patch("stage4.os.pidfd_open", return_value=42), \
                patch("stage4.os.close"), patch("stage4.signal.pidfd_send_signal") as send:
            with self.assertRaisesRegex(LabError, "identity changed"):
                lab.stop("u")
            send.assert_not_called()


if __name__ == "__main__":
    unittest.main()
