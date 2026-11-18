#!/usr/bin/env python3
"""R24: compare actual JSONL with a disposable PostgreSQL import via psql."""
import argparse
from collections import Counter
import json
from pathlib import Path
import subprocess


def records(directory, name):
    paths = sorted(directory.rglob(name))
    if not paths:
        raise ValueError(f"no {name} under {directory}")
    result = [json.loads(line) for path in paths for line in path.read_text().splitlines()]
    if not result or not all(isinstance(row, dict) for row in result):
        raise ValueError(f"{name} must contain nonempty JSON objects")
    return result


def sql_for(traces, counters):
    # COPY's CSV mode preserves JSON backslashes; neither control character can
    # appear literally in a compact JSON record. No filesystem path enters SQL.
    sql = ["BEGIN;", "CREATE TEMP TABLE traces (record jsonb);",
           "CREATE TEMP TABLE counters (seq bigserial, record jsonb);"]
    for table, rows in (("traces", traces), ("counters", counters)):
        sql.append(f"COPY {table}(record) FROM STDIN WITH (FORMAT csv, DELIMITER E'\\x01', QUOTE E'\\x02');")
        sql.extend(json.dumps(row, ensure_ascii=True) for row in rows)
        sql.append("\\.")
    sql.extend([
        "SELECT json_build_object('traces', (SELECT count(*) FROM traces),",
        "'counters', (SELECT count(*) FROM counters),",
        "'groups', (SELECT json_agg(g) FROM (",
        "SELECT record->>'action' AS action, record->>'drop_reason' AS reason, count(*) AS n",
        "FROM traces GROUP BY 1,2) g),",
        "'counter_records', (SELECT json_agg(record ORDER BY seq) FROM counters));",
        "ROLLBACK;",
    ])
    return "\n".join(sql) + "\n"


def verify(directory, database):
    traces, counters = records(directory, "trace.jsonl"), records(directory, "counters.jsonl")
    result = subprocess.run(["psql", "-X", "-qAt", "-v", "ON_ERROR_STOP=1", database],
                            input=sql_for(traces, counters), capture_output=True, text=True, timeout=60)
    if result.returncode:
        raise RuntimeError("PostgreSQL import failed: " + result.stderr)
    actual = json.loads(result.stdout)
    expected = Counter((row.get("action"), row.get("drop_reason")) for row in traces)
    imported = Counter({(row["action"], row["reason"]): row["n"] for row in actual["groups"]})
    if (actual["traces"] != len(traces) or actual["counters"] != len(counters)
            or imported != expected or actual["counter_records"] != counters):
        raise AssertionError("PostgreSQL rows or aggregates differ from JSONL")
    if not any(row.get("action") == "forward" for row in traces):
        raise AssertionError("acceptance evidence contains no forwarding")
    print(json.dumps({"postgresql": "passed", "traces": len(traces), "counters": len(counters)}))


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--evidence", type=Path, required=True)
    parser.add_argument("--database", required=True, help="psql connection string; use a disposable database")
    args = parser.parse_args()
    verify(args.evidence, args.database)
