#!/usr/bin/env python3
"""Non-mutating source checks; never descend into private lab artifacts."""
import ast
from pathlib import Path
import subprocess


def main():
    root = Path(__file__).resolve().parent.parent
    sources = [*root.glob("*.go")]
    for directory in ("cmd", "drivers", "internal"):
        sources.extend((root / directory).rglob("*.go"))
    result = subprocess.run(["gofmt", "-l", *map(str, sorted(sources))],
                            check=True, text=True, capture_output=True)
    if result.stdout:
        raise SystemExit("Run gofmt on:\n" + result.stdout)
    for path in sorted((root / "lab").glob("*.sh")):
        subprocess.run(["bash", "-n", str(path)], check=True)
    for path in sorted((root / "lab").glob("*.py")):
        ast.parse(path.read_text(), filename=str(path))
    print("Go formatting, Bash syntax, and Python syntax passed.")


if __name__ == "__main__":
    main()
