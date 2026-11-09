# Stage 1: set up and run an IPv4 router lab

Create two simulated hosts, put our router between them, and use ping,
traceroute, captures, and traces to see what it does. Before each experiment,
make a prediction; compare it with the result afterward.

Work through the sections in order. The lab stays up between experiments.

## Prepare two terminals

Use your normal Ubuntu terminal, with sudo available. Open two terminal windows:

- **Terminal A** runs the router and shows where it saves captures.
- **Terminal B** runs setup, ping, inspection, and cleanup commands.

The commands below use Bash, including variables and Python here-documents.
If you normally use Fish, enter `bash` in each terminal first. In both terminals:

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

In Terminal B, run this command and paste the actual directory printed in
Terminal A, without the `artifacts:` label:

```sh
read -r -p 'Artifact directory from Terminal A: ' RUN_DIR
```

`RUN_DIR` is a Bash variable used by the capture and trace commands below.
Every router restart creates a new directory; update this variable each time.

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
ICMP fields to compare a request on both sides.

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

Inspect counters, send one packet with an invalid IPv4 checksum, and inspect
counters again:

```sh
sudo ./bin/goodnews show counters --socket /run/goodnews-lab-stage1/r1.sock
sudo ip netns exec gn-h1 ./bin/goodnews craft --interface gn0 --bad-checksum
sudo ./bin/goodnews show counters --socket /run/goodnews-lab-stage1/r1.sock
sudo tail -n 1 "$RUN_DIR/trace.jsonl" | python3 -m json.tool --json-lines
```

Expect `invalid_checksum` to increase by one and the trace to show a drop.
The router records the corrupt packet in `left.pcap` before validation, so you
can inspect its bad checksum there. It should not be forwarded to the right.

Keep the router and namespaces available if you want to continue with the
[route and protection experiments](stage1-experiments.md). That guide shows
how to create experimental configurations, restart, and inspect the results.

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
| Capture or trace permission denied | Use the `sudo cat` or `sudo tail` commands above. Refresh `RUN_DIR` after a restart. |
| Ping fails while the router is running | Run `make lab-check`, inspect counters and recent traces, and check the intended config is running. |

If you later want to connect an observation to its implementation, the TUN
attachment is in [drivers/tun_linux.go](../drivers/tun_linux.go), forwarding
decisions are in [forward.go](../forward.go), and setup is in
[lab/stage1.sh](../lab/stage1.sh).
