# Stage 1: route and protection experiments

Start with the [main lab guide](stage1.md). These experiments use its two
terminals, existing namespaces, built binary, and `RUN_DIR` capture variable.
Keep the namespaces up and stop the router in Terminal A before each restart.

Use Fish or Bash. Shared `sh` blocks work in both shells; choose your shell's
block where a Fish or Bash label is shown. The multiline `python3 -c` commands
also work in both shells; paste the complete block, including its closing quote.

Each experiment creates a separate configuration from `lab/stage1.json`. The
supplied configuration stays unchanged. Predict the result before sending
packets, then compare the trace and captures.

## 1. Observe longest prefix match

Stop the router with **Ctrl-C in Terminal A**. In Terminal B, create a config
with three overlapping routes:

```sh
python3 -c '
import json
from pathlib import Path

config = json.loads(Path("lab/stage1.json").read_text())
config["routes"] = [
    {"prefix": "10.0.0.0/8", "interface": "left"},
    {"prefix": "10.20.0.0/16", "interface": "left"},
    {"prefix": "10.20.30.0/24", "interface": "right"},
]
Path("/tmp/goodnews-stage1-routes.json").write_text(json.dumps(config, indent=2))
'
./bin/goodnews check --config /tmp/goodnews-stage1-routes.json
```

The Python command copies the base configuration and adds these routes. The
validation command should print `configuration valid`.

In Terminal A:

```sh
sudo ip netns exec gn-r1 ./bin/goodnews run --config /tmp/goodnews-stage1-routes.json
```

In Terminal B, save the new artifact directory printed in Terminal A. Run the
command for your shell, keeping `RUN_DIR` as written, then paste the directory
when prompted:

**Fish:**

```fish
read -P 'New artifact directory from Terminal A: ' RUN_DIR
```

**Bash:**

```bash
read -r -p 'New artifact directory from Terminal A: ' RUN_DIR
```

Then inspect the table and send three lookup probes (both shells):

```sh
sudo ./bin/goodnews show routes --socket /run/goodnews-lab-stage1/r1.sock
sudo ip netns exec gn-h1 ./bin/goodnews craft --interface gn0 --dst 10.99.0.9
sudo ip netns exec gn-h1 ./bin/goodnews craft --interface gn0 --dst 10.20.99.9
sudo ip netns exec gn-h1 ./bin/goodnews craft --interface gn0 --dst 10.20.30.40
sudo cat "$RUN_DIR/trace.jsonl" | python3 -c '
import json, sys
destinations = {"10.99.0.9", "10.20.99.9", "10.20.30.40"}
for line in sys.stdin:
    record = json.loads(line)
    if record["destination"] in destinations:
        print(json.dumps(record, indent=2))
'
```

The final command selects traces for these destinations so other traffic does
not obscure the probes. Predict each winner, then compare `matching_routes`,
`selected_route`, and `egress`:

| Destination | Matching experimental prefixes | Winner | Outgoing interface |
| --- | --- | --- | --- |
| `10.99.0.9` | `/8` | `10.0.0.0/8` | `left` |
| `10.20.99.9` | `/8`, `/16` | `10.20.0.0/16` | `left` |
| `10.20.30.40` | `/8`, `/16`, `/24` | `10.20.30.0/24` | `right` |

No hosts are configured to answer at these destinations. Observe the chosen
route and outgoing interface. The most specific matching prefix wins, even
when broader matches point left. Stop this router before the next experiment.

## 2. Keep TTL unchanged

In Terminal B, create a config that disables TTL decrement:

```sh
python3 -c '
import json
from pathlib import Path

config = json.loads(Path("lab/stage1.json").read_text())
config["protections"]["decrement_ttl"] = False
Path("/tmp/goodnews-stage1-no-ttl.json").write_text(json.dumps(config, indent=2))
'
./bin/goodnews check --config /tmp/goodnews-stage1-no-ttl.json
```

In Terminal A:

```sh
sudo ip netns exec gn-r1 ./bin/goodnews run --config /tmp/goodnews-stage1-no-ttl.json
```

In Terminal B, save the new artifact directory using your shell's command and
paste the directory at the prompt:

**Fish:**

```fish
read -P 'New artifact directory from Terminal A: ' RUN_DIR
```

**Bash:**

```bash
read -r -p 'New artifact directory from Terminal A: ' RUN_DIR
```

Then run in either shell:

```sh
sudo ./bin/goodnews show interfaces --socket /run/goodnews-lab-stage1/r1.sock
sudo ip netns exec gn-h1 ping -n -c 3 -W 1 10.0.1.2
sudo cat "$RUN_DIR/right.pcap" | tcpdump -nn -vv -r - 'icmp and src host 10.0.0.2 and dst host 10.0.1.2'
```

Expect `decrement_ttl: false` in the effective settings. With host TTL 64, the
forwarded requests and received ping replies now retain TTL 64. Compare that
with TTL 63 in the normal run. This topology has no routing loop; loop
experiments become useful when distributed routing is added.

Stop the router before continuing.

## 3. Accept an invalid IPv4 checksum

In Terminal B:

```sh
python3 -c '
import json
from pathlib import Path

config = json.loads(Path("lab/stage1.json").read_text())
config["protections"]["verify_ipv4_checksum"] = False
Path("/tmp/goodnews-stage1-no-checksum.json").write_text(json.dumps(config, indent=2))
'
./bin/goodnews check --config /tmp/goodnews-stage1-no-checksum.json
```

In Terminal A:

```sh
sudo ip netns exec gn-r1 ./bin/goodnews run --config /tmp/goodnews-stage1-no-checksum.json
```

In Terminal B, save the new artifact directory using your shell's command and
paste the directory at the prompt:

**Fish:**

```fish
read -P 'New artifact directory from Terminal A: ' RUN_DIR
```

**Bash:**

```bash
read -r -p 'New artifact directory from Terminal A: ' RUN_DIR
```

Then run in either shell:

```sh
sudo ./bin/goodnews show interfaces --socket /run/goodnews-lab-stage1/r1.sock
sudo ip netns exec gn-h1 ./bin/goodnews craft --interface gn0 --bad-checksum
sudo ./bin/goodnews show counters --socket /run/goodnews-lab-stage1/r1.sock
sudo cat "$RUN_DIR/left.pcap" | tcpdump -nn -vv -r - 'icmp and src host 10.0.0.2'
sudo cat "$RUN_DIR/right.pcap" | tcpdump -nn -vv -r - 'icmp and src host 10.0.0.2'
```

Expect `verify_ipv4_checksum: false` and the invalid packet to be forwarded,
without an `invalid_checksum` drop. Its incoming checksum is wrong, but its
outgoing checksum is recomputed after TTL changes. Bounds and length checks
remain active.

## Restore normal behavior or finish

Stop the experimental router with Ctrl-C. To return to normal behavior, run in
Terminal A:

```sh
sudo ip netns exec gn-r1 ./bin/goodnews run --config lab/stage1.json
```

Refresh `RUN_DIR` in Terminal B using your shell's
[capture-directory command](stage1.md#save-the-capture-directory) before
inspecting this new run. To finish the session instead, keep the router stopped
and follow the main guide's
[cleanup steps](stage1.md#6-prove-the-router-is-necessary-and-clean-up).
