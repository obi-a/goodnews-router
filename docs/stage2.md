# Stage 2: Ethernet, ARP, and badnews

This stage replaces the TUN attachments with two virtual Ethernet cables. The
IPv4 forwarding decisions stay the same; the router now reads Ethernet headers,
answers ARP, and resolves the next hop before sending an IPv4 packet.

Stage 1 remains available in the same binary: use `lab/stage1.json` and
`make lab-up`, `make lab-check`, `make lab-down`, and `make integration` for
the TUN lab. Stage 2 has separate configs, scripts, sockets, and namespaces.
Both stages use the shared IPv4 engine and remain covered by the test suite.

Run commands from the project directory containing `Makefile` and `go.mod`.
The command blocks work in Bash and Fish. Use the prerequisites in the README;
this lab needs Linux namespaces and sudo but does not need `/dev/net/tun`.

## Build and create the cables

```sh
make build
./bin/goodnews check --config lab/stage2.json
make lab2-up
```

The setup creates three namespaces with these devices:

```text
gn2-h1                         gn2-r1                          gn2-h2
10.0.0.2/24                    goodnews                       10.0.1.2/24
gn0 <-- veth cable --> left0   (.1 addresses in Go)   right0 <-- veth cable --> gn0
```

The router's `left0` MAC is `02:00:00:00:00:01`; `right0` is
`02:00:00:00:01:01`. Each host's MAC ends in `02` instead of `01`.
These are local lab addresses, assigned by the setup script. The Ethernet driver
reads the actual device MAC; no MAC needs to be configured in the router JSON.

The script disables kernel IPv4 forwarding and IPv6 in all three namespaces,
leaves router ports without kernel IP addresses, and disables mutable checksum
and segmentation offloads on both ends of each cable. All changes stay inside
the namespaces. Stage 1 uses different namespace names and can coexist.
`make lab2-check` verifies namespace identity, forwarding settings, host
addresses, and router ports. Cleanup refuses occupied or replaced namespaces.

## Start the router and predict the first ping

In terminal 1:

```sh
sudo ip netns exec gn2-r1 ./bin/goodnews run --config lab/stage2.json
```

In terminal 2, optionally watch the router with:

```sh
sudo ./bin/goodnews dashboard --config lab/stage2.json
```

In terminal 3:

```sh
sudo ip netns exec gn2-h1 ping -n -c 3 10.0.1.2
sudo ip netns exec gn2-h2 ping -n -c 3 10.0.0.2
sudo ip netns exec gn2-h1 traceroute -n 10.0.1.2
sudo ip netns exec gn2-h2 traceroute -n 10.0.0.2
```

Before the first ping, predict who asks for whose MAC. H1 ARPs for its default
gateway, `10.0.0.1`. Goodnews replies with the left-port MAC and learns H1's
mapping from that request. The router receives the echo request, decrements
its TTL, and queues it while ARPing for H2 on the right cable. H2's reply
releases that packet. The echo reply crosses the same forwarding engine in
reverse. Later pings use the cached mappings. Traceroute should list the `.1`
router address on the first hop and the destination on the next hop.

Inspect the live state:

```sh
sudo ./bin/goodnews show interfaces --socket /run/goodnews-lab-stage2/r1.sock
sudo ./bin/goodnews show arp --socket /run/goodnews-lab-stage2/r1.sock
sudo ./bin/goodnews show counters --socket /run/goodnews-lab-stage2/r1.sock
```

`show interfaces` includes actual Ethernet MACs. `show arp` separates entries
by logical interface and reports `reachable` or `incomplete`, expiry, attempts,
and the number of waiting packets. Next hops come from the selected route:
an explicit gateway is resolved instead of the final destination.

## Watch the bytes

For a live capture in another terminal:

```sh
sudo ip netns exec gn2-r1 tcpdump -eni left0 'arp or icmp'
```

Each run also saves `left.pcap` and `right.pcap` in the artifact directory printed
at startup. Open them in Wireshark and use `arp || icmp`. Unlike Stage 1, these
captures use Ethernet link type 1 and include both Ethernet and ARP headers.
Outgoing frames have at least 60 bytes, excluding the NIC's frame checksum.
Padding is outside the IPv4 total length.

Compare an echo request on both cables: the IP endpoints stay the same, TTL
falls by one, the IPv4 checksum stays valid, and the Ethernet source/destination
change to the addresses for that cable. See the [Wireshark guide](wireshark.md)
for opening captures; its raw-IP examples describe Stage 1.

To create a corrupt IPv4 packet while preserving its bytes on Ethernet:

```sh
sudo ip netns exec gn2-h1 ./bin/goodnews craft --interface gn0 --dst-mac 02:00:00:00:00:01 --bad-checksum
```

`--dst-mac` is the next-hop MAC. Crafting bypasses ARP and the kernel IPv4
transmit path, so Wireshark and the router see the deliberate corruption.

## Change one link at runtime

`badnews` talks to the same owner-only Unix socket as `show`. Flags precede
the command. Commands use logical port names, `left` and `right`.

```sh
sudo ./bin/badnews show
sudo ./bin/badnews link right delay 200
sudo ip netns exec gn2-h1 ping -n -c 3 10.0.1.2
sudo ./bin/badnews clear
sudo ./bin/badnews link right loss 100
sudo ip netns exec gn2-h1 ping -n -c 3 -W 1 10.0.1.2
sudo ./bin/badnews clear
sudo ./bin/badnews link right corruption 100
sudo ip netns exec gn2-h1 ping -n -c 3 -W 1 10.0.1.2
sudo ./bin/badnews clear
sudo ./bin/badnews link right down
sudo ip netns exec gn2-h1 ping -n -c 3 -W 1 10.0.1.2
sudo ./bin/badnews link right up
sudo ./bin/badnews clear
```

Expect roughly 200 ms added RTT when delaying only the right egress. Loss
raises `impaired_loss`; corruption flips a bit in the outgoing IPv4 checksum,
so the receiving host drops the packet and the bad checksum is visible in the
right capture. Goodnews counts that frame as transmitted. Link down raises
`link_down`. Clear restores normal ping without restarting the router.

Settings accumulate until changed or cleared. `up` changes only the down flag.
Delay accepts integer milliseconds from 0 to 10000; loss and corruption accept
percentages from 0 to 100. For another socket, put `--socket PATH` before the
command. Cost changes are reserved for Stage 4's routing protocols.

Delay, loss, and corruption affect egress IPv4, including generated replies.
ARP is exempt so experiments isolate the IP effect. Ethernet link down blocks
ingress and egress, including ARP. Settings also work on TUN egress when its
socket is selected. They change the router's behavior, not kernel qdiscs or
interface administration state. Settings reset on router restart.

## Defined limits and failure behavior

- ARP mappings expire 60 seconds after learning. Cache hits do not extend that
  lifetime; the next packet after expiry resolves again.
- Resolution sends one request immediately and retries after one and two
  seconds. After three unanswered attempts, at about three seconds, waiting
  packets get correlated `arp_timeout` drops. Other destinations keep working.
- Each Ethernet port holds at most 64 ARP entries, including incomplete ones,
  and 256 waiting packets, with at most 16 for one next hop. Full queues/tables
  report `arp_queue_full` or `arp_table_full`.
- At most 256 IPv4 packets wait for configured delay across the router.
  Overflow reports `delay_queue_full`. Clear makes delayed packets eligible at
  the next timer tick (about 20 ms). A packet already corrupted keeps those
  bytes. ARP waiting queues continue their normal resolution lifecycle.
  Graceful router shutdown records `router_shutdown` for packets still waiting
  for ARP or delay.
- ARP learns requests for that port's own IP and replies to outstanding
  requests. Sender MACs must match their Ethernet headers, and sender IPs must
  be usable neighbors on that subnet. Probes from `0.0.0.0` are answered but
  never cached. Unsolicited replies, mismatches, and off-subnet senders have
  named drops. The lab does not implement authenticated ARP.
- VLANs, IPv6, other EtherTypes, and broadcast IPv4 are explicitly dropped.
  IPv4 fragmentation and MTU limitations remain as in Stage 1. An ARP timeout
  is traced and counted; this stage does not generate an additional ICMP error
  for that timeout.

Ingress decisions can now say `queue_arp` or `queue_delay`. A later `sent` or
drop record links back through `parent_id` and retains the selected FIB version.
ARP and Ethernet drops also appear in dashboard history with MAC/ARP details.
Received/transmitted counters include ARP frames; queued packets count as
transmitted only after a successful send. Packet captures show the bytes
actually received or successfully sent, including intentional corruption.

## Exit test and cleanup

```sh
make integration2
```

Run acceptance with no existing Stage 2 lab: stop the manual router and run
`make lab2-down` first. The harness refuses to adopt a lab and retains evidence
under `artifacts/stage2-*`. It checks bidirectional gateway/transit ping and
traceroute, ARP requests/replies and reuse, TTL/checksums, corrupt crafting,
runtime delay/loss/corruption/down/clear, unresolved-neighbor timeout, dashboard
history metadata, artifact permissions, no forwarding after stopping the
process, and repeated setup/cleanup. A result file is written only on success.

For a manual session, stop the router with Ctrl-C, quit the dashboard, then:

```sh
make lab2-down
```

The empirical gate remains pending until the privileged harness passes on the
Ubuntu lab host. [The verification record](verification.md) distinguishes Go
tests from that networking result.
