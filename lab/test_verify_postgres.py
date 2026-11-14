"""Check JSONL import framing and independent expected aggregates."""
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

from verify_postgres import records, sql_for, verify


class PostgresTests(unittest.TestCase):
    def test_copy_preserves_json_escapes(self):
        rows = [{"action": "forward", "message": "backslash\\ newline\n control\x01 quote\""}]
        sql = sql_for(rows, [{"kind": "counters"}])
        imported = sql.split("QUOTE E'\\x02');\n", 1)[1].split("\n\\.", 1)[0]
        self.assertEqual(json.loads(imported), rows[0])
        self.assertNotIn("\x01", imported)

    def test_rejects_missing_empty_or_nonobject_evidence(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary)
            for content in (None, "", "[]\n"):
                if content is not None:
                    (path / "trace.jsonl").write_text(content)
                with self.assertRaises(ValueError):
                    records(path, "trace.jsonl")

    def test_compares_imported_counts_groups_and_counters(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary)
            (path / "trace.jsonl").write_text('{"action":"forward"}\n{"action":"drop","drop_reason":"invalid_checksum"}\n')
            counters = [{"kind": "counters", "received": 2}]
            (path / "counters.jsonl").write_text(json.dumps(counters[0]) + "\n")
            result = {"traces": 2, "counters": 1, "counter_records": counters,
                      "groups": [{"action": "forward", "reason": None, "n": 1},
                                 {"action": "drop", "reason": "invalid_checksum", "n": 1}]}
            with patch("verify_postgres.subprocess.run", return_value=subprocess.CompletedProcess([], 0, json.dumps(result), "")):
                verify(path, "unused-test-database")
            result["groups"][0]["n"] = 2
            with patch("verify_postgres.subprocess.run", return_value=subprocess.CompletedProcess([], 0, json.dumps(result), "")):
                with self.assertRaises(AssertionError):
                    verify(path, "unused-test-database")
            with patch("verify_postgres.subprocess.run", return_value=subprocess.CompletedProcess([], 1, "", "SQL failed")):
                with self.assertRaisesRegex(RuntimeError, "SQL failed"):
                    verify(path, "unused-test-database")


if __name__ == "__main__":
    unittest.main()
