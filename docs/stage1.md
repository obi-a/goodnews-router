# Stage 1: set up and run an IPv4 router lab

Create two simulated hosts, put our router between them, and use ping,
traceroute, captures, and traces to see what it does. Before each experiment,
make a prediction; compare it with the result afterward.

Work through the sections in order. The lab stays up between experiments.

## Prepare two terminals

Use your normal Ubuntu terminal, with sudo available. Open two terminal windows:

- **Terminal A** runs the router and shows where it saves captures.
- **Terminal B** runs setup, ping, inspection, and cleanup commands.

Use either **Fish or Bash** in both terminals. Blocks labeled `sh` work in
either shell. When syntax differs, separate Fish and Bash blocks are provided;
run only the one for your shell. The Makefile runs the lab's Bash setup script
for you, so Fish users can stay in Fish.

In both terminals:

```sh
cd /home/obi/bin/goodnews-router
```

On a machine that needs the tools installed, run in Terminal B:

```sh
sudo apt-get update
sudo apt-get install golang-go build-essential iproute2 procps ethtool iputils-ping traceroute tcpdump python3
```

Build the router and validate the supplied configuration:

```sh
env GOTOOLCHAIN=auto make build
./bin/goodnews check --config lab/stage1.json
```

The first command builds `bin/goodnews`. Go may download the project-selected
toolchain on the first build. The second should print `configuration valid`;
it checks the configuration without starting the router.

Rebuild after changing Go code. You can reuse the binary for later lab sessions.

## 1. Create the network environment

In Terminal B:

```sh
make lab-up
make lab-check
```

`lab-up` creates three **network namespaces**. Each is a separate network
environment inside the laptop, with its own interfaces, addresses, and routes:

| Namespace | Role | Host interface address | Default gateway |
| --- | --- | --- | --- |
| `gn-h1` | First host | `gn0`: `10.0.0.2/24` | `10.0.0.1` |
| `gn-r1` | Runs `goodnews` | Router addresses live in Go | — |
| `gn-h2` | Second host | `gn0`: `10.0.1.2/24` | `10.0.1.1` |

Both host interfaces are called `gn0`. They can share that name because they
live in different namespaces. Each is a **TUN interface**: when the host sends
an IP packet through it, our router can read the packet; when our router writes
to it, the host receives a packet.

`left` and `right` are the Go router's logical interface names. `left` attaches
to h1's `gn0`; `right` attaches to h2's `gn0`. Each is bidirectional. For an
h1-to-h2 request, `left` is ingress and `right` is egress; for the reply, the
roles reverse. In this software lab the attachments are open TUN descriptors,
serving the role that physical network ports would serve on a hardware router.
UDP/TCP port numbers identify applications within a host and are a different
use of the word "port."

Setup brings the interfaces up, sets their MTU to 1500, and adds default routes.
For example, h1's default route sends traffic outside `10.0.0.0/24` through
`gn0` toward `10.0.0.1`.

It also disables kernel forwarding in the lab namespaces, reverse-path
filtering, ICMP redirects, IPv6, and supported mutable packet offloads. This
keeps packet movement under our program's control and makes captures easier
to interpret. The settings apply inside the lab namespaces.

`lab-check` verifies the basic setup. Expect:

```text
lab ready: gn-h1 (10.0.0.2) <-> goodnews in gn-r1 <-> gn-h2 (10.0.1.2)
```

Inspect what setup created:

```sh
sudo ip netns list
sudo ip -n gn-h1 -4 address show dev gn0
sudo ip -n gn-h1 route show
sudo ip -n gn-h2 -4 address show dev gn0
sudo ip -n gn-r1 -4 address show
sudo ip netns exec gn-r1 sysctl net.ipv4.ip_forward
```

`ip -n NAME` inspects the selected namespace's network configuration.
`ip netns exec NAME COMMAND` runs a command inside that namespace.

Expect the hosts' `.2` addresses, a default route through each `.1` gateway,
and forwarding set to `0`. The router namespace has loopback, but neither
`10.0.0.1` nor `10.0.1.1` is assigned to a kernel interface. Our program owns
those addresses through its configuration.

**Try before starting the router:** predict whether h1 can reach h2, then run:

```sh
sudo ip netns exec gn-h1 ping -n -c 1 -W 1 10.0.1.2
```

Expect failure. The interfaces and routes exist, but our program is not running
to carry packets between the hosts. `-c 1` sends one request; `-W 1` limits how
long ping waits for its reply.

## 2. Start the router and ping its addresses

In Terminal A:

```sh
sudo ip netns exec gn-r1 ./bin/goodnews run --config lab/stage1.json
```

The program runs in the router namespace, attaches to each host's TUN, and
begins handling packets. Leave it running in this terminal.

It prints a fresh output directory, such as:

```text
artifacts: artifacts/runs/r1-885399003
```

### Save the capture directory

The numbered directory above is an example. In Terminal B, run the command for
your shell exactly as written. When the prompt appears, paste the **actual
directory printed in Terminal A**, without the `artifacts:` label, and press
Enter.

**Fish:**

```fish
read -P 'Artifact directory from Terminal A: ' RUN_DIR
```

**Bash:**

```bash
read -r -p 'Artifact directory from Terminal A: ' RUN_DIR
```

Keep `RUN_DIR` in the command: it is the variable name, not a placeholder for
the path. Enter the path at the prompt, rather than replacing `RUN_DIR` with
it. In both shells, `$RUN_DIR` retrieves the saved path for the commands below.
Every router restart creates a new directory; repeat this step each time.

Now ping each gateway from its host:

```sh
sudo ip netns exec gn-h1 ping -n -c 3 -W 1 10.0.0.1
sudo ip netns exec gn-h2 ping -n -c 3 -W 1 10.0.1.1
```

Expect replies. These requests are addressed to the router itself, so it
generates ICMP Echo Replies rather than forwarding requests to the other host.

Inspect its configured interfaces, routes, and counters:

```sh
sudo ./bin/goodnews show interfaces --socket /run/goodnews-lab-stage1/r1.sock
sudo ./bin/goodnews show routes --socket /run/goodnews-lab-stage1/r1.sock
sudo ./bin/goodnews show counters --socket /run/goodnews-lab-stage1/r1.sock
```

The route view should contain connected routes for `10.0.0.0/24` through
`left` and `10.0.1.0/24` through `right`. `left` and `right` are logical names
in the router config; both attach to a host-side device named `gn0`.

The socket path is how inspection commands contact the running router.
Adding `--json` gives compact JSON output.

For a combined view with scrollback, run in Terminal B:

```sh
sudo ./bin/goodnews dashboard --config lab/stage1.json
```

It discovers the current run directory automatically and displays topology,
counters, the forwarding table, and packet history. Use a third terminal to
send pings while watching, or press `q` to return to the commands below.
Arrow keys/Page Up/Page Down browse past events; Enter opens details and End
returns to live activity. See the [dashboard guide](dashboard.md) for saved-run
browsing and a synthetic example that works without sudo.

## 3. Forward traffic and inspect its TTL

Predict what will change as a packet crosses the router. In Terminal B:

```sh
sudo ip netns exec gn-h1 ping -n -c 3 -W 1 10.0.1.2
sudo ip netns exec gn-h2 ping -n -c 3 -W 1 10.0.0.2
```

Expect replies in both directions. With the normal host TTL of 64, ping should
show reply TTL 63 because the reply also crosses the router once.

Each interface's pcap includes packets received and successfully sent. Decode
the h1-to-h2 requests from each side:

```sh
sudo cat "$RUN_DIR/left.pcap" | tcpdump -nn -vv -r - 'icmp and src host 10.0.0.2 and dst host 10.0.1.2'
sudo cat "$RUN_DIR/right.pcap" | tcpdump -nn -vv -r - 'icmp and src host 10.0.0.2 and dst host 10.0.1.2'
```

`sudo cat` opens the private capture; the pipe gives tcpdump the already-open
data. `-r -` reads from standard input, `-nn` keeps addresses numeric, and `-vv`
shows details including TTL.

Find the same request using its ICMP ID and sequence number. Expect TTL 64 on
the left and 63 on the right. The source, destination, and ICMP payload stay
the same. Changing TTL requires a new IPv4 header checksum.

View recent decisions:

```sh
sudo tail -n 4 "$RUN_DIR/trace.jsonl" | python3 -m json.tool --json-lines
```

Look for `action: "forward"`, the incoming and outgoing interfaces,
`ttl_before`, `ttl_after`, `matching_routes`, and `selected_route`.

For Wireshark, make user-readable copies once the pings have finished:

```sh
mkdir -p "$HOME/goodnews-captures"
sudo cat "$RUN_DIR/left.pcap" > "$HOME/goodnews-captures/left.pcap"
sudo cat "$RUN_DIR/right.pcap" > "$HOME/goodnews-captures/right.pcap"
```

Open those copies through Wireshark's **File → Open**. Expand the IPv4 and
ICMP fields to compare a request on both sides. Follow the
[Wireshark packet guide](wireshark.md) for installation, display filters,
TTL comparisons, and checksum validation.

## 4. Watch traceroute and ICMP errors

Predict the hops from h1 to h2, then run:

```sh
sudo ip netns exec gn-h1 traceroute -n -q 1 -w 1 -m 3 10.0.1.2
```

Expect the router at `10.0.0.1` as hop 1 and h2 at `10.0.1.2` as hop 2.
Traceroute's first probe reaches the router with TTL 1. The router returns
ICMP Time Exceeded, revealing its address. A later probe reaches h2, whose
kernel returns the UDP port-unreachable response that ends this traceroute.

Generate a TTL-1 packet directly:

```sh
sudo ip netns exec gn-h1 ./bin/goodnews craft --interface gn0 --ttl 1
sudo tail -n 2 "$RUN_DIR/trace.jsonl" | python3 -m json.tool --json-lines
```

Crafting is silent on success. Expect a trace with `drop_reason: "ttl_expired"`
and a generated ICMP packet. Its `parent_id` identifies the original packet.

Try a destination outside the configured routes:

```sh
sudo ip netns exec gn-h1 ping -n -c 1 -W 1 192.0.2.1
sudo ./bin/goodnews show counters --socket /run/goodnews-lab-stage1/r1.sock
```

Expect a destination-unreachable message from the router and an increase in
the `no_route` drop counter.

An ICMP error should not cause another ICMP error. Test that rule:

```sh
sudo ip netns exec gn-h1 ./bin/goodnews craft --interface gn0 --icmp-type 3 --dst 192.0.2.1
sudo tail -n 1 "$RUN_DIR/trace.jsonl" | python3 -m json.tool --json-lines
```

Expect `drop_reason: "no_route"` and `icmp_suppressed: "icmp_error"`.

## 5. Corrupt a packet

This experiment checks that the router rejects a corrupt IPv4 header. Keep
the router running in Terminal A and run these commands in Terminal B. Use
the default configuration, with IPv4 checksum verification enabled, and pause
other traffic so the changes are easy to compare.

1. Read the current counters:

   ```sh
   sudo ./bin/goodnews show counters --socket /run/goodnews-lab-stage1/r1.sock
   ```

   This asks the running router for its cumulative totals: packets received,
   transmitted, delivered locally, and dropped, including the reasons for
   drops. Keep this output to compare with the next reading.

   The `.sock` path is a local Unix socket used to communicate with the router
   process. This query does not send a packet through the lab's network.
   `sudo` provides access to the protected socket.

2. Send one packet with an invalid IPv4 header checksum:

   ```sh
   sudo ip netns exec gn-h1 ./bin/goodnews craft --interface gn0 --bad-checksum
   ```

   `ip netns exec gn-h1` runs `goodnews craft` inside host h1's network
   namespace. `craft` constructs and sends one packet; `--interface gn0`
   selects h1's TUN interface, which connects to the router's `left` side.

   With the defaults, the packet is an ICMP Echo Request from `10.0.0.2` to
   `10.0.1.2`, with TTL 64. `--bad-checksum` deliberately corrupts the IPv4
   header checksum, a value used to detect header corruption. The router
   should reject the packet during header validation, before forwarding it
   to h2. `craft` normally prints nothing when sending succeeds; that silence
   does not mean the router accepted or forwarded the packet.

3. Read the counters again:

   ```sh
   sudo ./bin/goodnews show counters --socket /run/goodnews-lab-stage1/r1.sock
   ```

   After the router processes the packet, expect these changes from the first
   reading, assuming no other traffic:

   | Counter | Change |
   |---|---|
   | `received` | +1 |
   | `dropped` | +1 |
   | `drop_reasons.invalid_checksum` | +1 |
   | `transmitted` | No change |

   These counters accumulate throughout the router run. Compare the two
   readings rather than expecting the totals themselves to equal one.

4. Inspect the most recent packet decision:

   ```sh
   sudo tail -n 1 "$RUN_DIR/trace.jsonl" | python3 -m json.tool --json-lines
   ```

   `$RUN_DIR` is the current run's artifact directory, saved in
   [Save the capture directory](#save-the-capture-directory). Update it after
   every router restart. `tail -n 1` reads the final line of `trace.jsonl`.
   This file uses JSON Lines: each line is a separate JSON object describing
   a packet event.

   The pipe (`|`) passes that line to Python, where
   `python3 -m json.tool --json-lines` formats it into readable, indented
   output. Only `tail` needs `sudo` to read the protected file; Python reads
   the piped output.
   This command reads the latest event once, rather than monitoring new ones.

   The relevant fields should look like:

   ```json
   {
     "ingress": "left",
     "action": "drop",
     "drop_reason": "invalid_checksum"
   }
   ```

The router records the corrupt packet in `left.pcap` before validation, so you
can inspect its bad checksum there. It should not be forwarded to the right.

Keep the router and namespaces available if you want to continue with the
[route and protection experiments](stage1-experiments.md). That guide shows
how to create experimental configurations, restart, and inspect the results.
For application traffic, the [UDP and HTTP guide](traffic.md) explains raw
crafting, UDP echo, HTTP over TCP, and the difference between a missing route,
a closed application port, a missing web resource, and a timeout.

## 6. Prove the router is necessary and clean up

Stop the router with **Ctrl-C in Terminal A**. In Terminal B:

```sh
sudo ip netns exec gn-h1 ping -n -c 1 -W 1 10.0.1.2
make lab-down
sudo ip netns list
```

Expect ping to fail. After cleanup, `gn-h1`, `gn-r1`, and `gn-h2` should be gone.
Cleanup removes the lab's namespaces, any leftover management socket, and its
ownership record. Captures under `artifacts/` remain available for review.

If cleanup reports processes still using a namespace, close the router or any
shell you started in that namespace, then retry `make lab-down`.

## Run the full acceptance check when needed

After cleanup, you can run:

```sh
sudo python3 lab/acceptance.py
```

This creates its own lab, runs the full set of networking checks, and cleans
up. It is useful after code changes or to verify the whole stage. You do not
need to run it before every interactive session; use `make lab-up` for that.
Its evidence is saved under `artifacts/stage1-*`.

For an optional database exercise, [lab/postgres.sql](../lab/postgres.sql)
includes a trace import and a query that counts outcomes. Set its trace path
before running it against a disposable PostgreSQL database.

## If something fails

| Symptom | What to do |
| --- | --- |
| `/dev/net/tun` unavailable | Try `sudo modprobe tun`, then `make lab-up`. A container may also need the device exposed by its host. |
| Lab state already exists | Inspect with `make lab-check`; stop the old router, then run `make lab-down` before creating a fresh lab. |
| Namespace has running processes | Stop the router or namespace shell. `sudo ip netns pids gn-r1` lists processes in the router namespace. |
| Inspection socket unavailable | Check that Terminal A is running the router and inspect its error output. |
| `read` reports an invalid variable name | Use the Fish or Bash command in [Save the capture directory](#save-the-capture-directory). Keep `RUN_DIR` as the variable name and enter the path at the prompt. |
| Capture or trace permission denied | Use the `sudo cat` or `sudo tail` commands above. Refresh `RUN_DIR` after a restart. |
| Ping fails while the router is running | Run `make lab-check`, inspect counters and recent traces, and check the intended config is running. |

If you later want to connect an observation to its implementation, the TUN
attachment is in [drivers/tun_linux.go](../drivers/tun_linux.go), forwarding
decisions are in [forward.go](../forward.go), and setup is in
[lab/stage1.sh](../lab/stage1.sh).
