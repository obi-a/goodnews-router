# Stage 4 design: routing protocols over virtual Ethernet

Status: implemented design; privileged namespace acceptance remains a separate
gate. See [the lab guide](stage4.md), [exact wire contract](stage4-protocol.md),
and [verification record](verification.md). Stage 3 physical acceptance stays
pending. Stage 4 uses Linux namespaces and veth cables on one Ubuntu machine;
it does not need physical Ethernet adapters, VM images, or additional computers.

## Learning outcome and scope

Run six independent goodnews processes, watch them exchange routing packets,
and compare their calculations with the textbook. Learn neighbor discovery,
distance vector, link state, convergence, and the separation between route
calculation and packet forwarding.

The deliverable includes both distance vector (DV, Bellman–Ford) and link state
(LS, Dijkstra), selected in config. Implement and verify them in separate
checkpoints. No OSPF/RIP interoperability, dynamic shared libraries, Internet
gateway, DHCP, Wi-Fi forwarding, NAT, fragmentation, or central controller is
included in this stage.

## Lab topology and evidence

Use the existing textbook graph:

| Link | Cost |
| --- | ---: |
| u–v | 2 |
| u–x | 1 |
| u–w | 5 |
| v–x | 2 |
| v–w | 3 |
| x–y | 1 |
| x–w | 3 |
| y–w | 1 |
| y–z | 2 |
| w–z | 5 |

Each router runs in its own `gn4-<router>` namespace. Every graph edge is a veth
pair with a unique /30 subnet. Each router also has a host-facing /24 LAN and
one endpoint namespace. These six small endpoints provide stable destinations
for checking the route to every router's LAN; they are ordinary Linux network
namespaces, not full virtual machines.

Allocate transit subnets from `10.44.0.0/16` and host LANs from
`10.45.0.0/16`. Check for conflicting existing routes before setup. Router LAN
addresses end in `.1` and endpoint addresses end in `.2`. Router addresses
exist only in Go. Endpoint kernels receive addresses and routes normally.

No namespace runs kernel forwarding. Router data interfaces have no kernel IP
addresses. Setup disables the mutable checksum and segmentation offloads that
would obscure packet captures. All interfaces are created inside owned lab
namespaces. No host network, firewall, or NetworkManager settings are changed.

At convergence, u's cost to the LAN attached to each router must be:

| Router | Cost | First hop from u |
| --- | ---: | --- |
| v | 2 | v |
| x | 1 | x |
| y | 2 | x |
| w | 3 | x |
| z | 4 | x |

Host LAN attachment adds zero to the graph distance. Verify paths with ping,
ICMP traceroute, routing tables, and captures. The protocol must learn these
paths; generated configs must not contain the graph's end-to-end static routes.

## Configuration

Add an optional `routing` section to config version 1. Its absence preserves
the existing static router behavior. Example fragment for u:

```json
{
  "routing": {
    "algorithm": "dv",
    "hello_ms": 500,
    "dead_ms": 2000,
    "update_ms": 1000,
    "max_age_ms": 6000,
    "poisoned_reverse": true,
    "links": [
      {"interface": "to-v", "peer": "10.44.0.2", "cost": 2},
      {"interface": "to-x", "peer": "10.44.0.6", "cost": 1},
      {"interface": "to-w", "peer": "10.44.0.10", "cost": 5}
    ]
  }
}
```

The interface entries elsewhere in the config still specify device, logical
address, and MTU. A routing link describes a directly connected peer, not an
end-to-end route. Neighbor identity is learned through hellos. Interfaces not
listed in `routing.links` are passive LANs: advertise their connected prefixes,
but never start a routing adjacency on them.

Validate algorithm, timer relationships, duplicate links, Ethernet attachments,
usable on-link peer addresses, positive integral costs, and resource limits.
Use `ls` to select link state. Reject algorithm-specific switches where they
would have no effect. Expose effective defaults in startup metadata.

The initial supported envelope is at most 16 routers, 16 routing links per
router, and 64 destination prefixes. Limit a link cost to 1–1000; represent
infinity as 65535 with saturating arithmetic. Reject oversized advertisements
and state growth beyond these limits with an observable reason.
DV also carries a hop count and rejects paths requiring 16 hops, bounding
multi-router count-to-infinity cases within the supported 16-router envelope.

## Control packets and neighbor lifecycle

Use a small custom protocol carried directly in IPv4 protocol 253, as the
project plan specifies. It is a teaching protocol, not an implementation of
RIP or OSPF. Send unicast packets to the configured on-link peer, with TTL 1,
through the existing Ethernet/ARP path. No kernel IP socket or multicast
implementation is needed.

The [protocol contract](stage4-protocol.md) specifies exact offsets and encoding
rules, exercised by encoder/decoder tests. The common envelope contains magic, version, message
type, explicit length, payload checksum, origin router ID, startup incarnation,
and sequence number. Use bounded binary fields and canonical IPv4 prefixes;
control messages must fit the configured link MTU without fragmentation.

There are three message types:

- **Hello:** identifies this router and its incarnation, algorithm, and the peer
  incarnation it has heard. A neighbor becomes up only after bidirectional
  confirmation. Send immediately at startup, then every 500 ms.
- **DV update:** a complete vector for this sender, with infinity entries for
  poisoned reverse and omission withdrawing removed destinations. A newer vector replaces the previous
  vector; it is not merged into it. Sequence numbers reject reordered updates.
- **LS advertisement:** one origin's directly attached prefixes and live
  neighbors with outgoing costs. Flood it without rewriting its origin,
  incarnation, or sequence number. Relay it in a new link-local IP packet.

Only a validated hello refreshes the neighbor dead timer. After 2000 ms without
one, remove the adjacency and its DV vector or originate a new local LSA.
Updates are accepted only from an established neighbor running the same
algorithm. A local administrative `badnews down` removes the adjacency promptly;
its peer discovers the loss through its timer. Delay and loss can therefore
cause genuine neighbor expiration as well as data-packet loss.

Validate IPv4 checksum, exact local destination, configured peer source,
TTL 1, no IP options or fragmentation, message length/checksum, field bounds,
and algorithm before state changes. Do not treat protocol 253 transit traffic
as routing input or generate ICMP errors in response to malformed control
packets. Different algorithms on opposite ends remain unestablished and report
the mismatch. This isolated lab protocol provides no authentication.

Order restarts through an owner-only persistent incarnation file next to the
management socket, fsynced before sending, and a monotonic sequence within that
incarnation. The next value exceeds the saved value even across a backward
clock change. Preserve it across individual restarts. Algorithm tests restart
an origin while old advertisements remain; privileged acceptance checks the
actual process/file lifecycle separately.

## Distance vector

Maintain the latest complete vector per live neighbor. Recompute each prefix
using Bellman–Ford: local link cost plus the neighbor's advertised distance.
Connected prefixes are locally reachable at cost zero. Keep connected and
explicit static routes ahead of learned routes for the same prefix; do not
redistribute static routes in this stage.

Choose equal-cost next hops deterministically by router ID and interface name;
install one next hop, with no ECMP. Advertise infinity back to the neighbor
selected as next hop when poisoned reverse is enabled. The opt-out switch is
an explicit learning experiment, enabled protection by default.

Send full vectors every second and triggered updates after changes. Coalesce
triggered updates for 100 ms, so several changes do not create an update storm.
Expire vectors after 6000 ms even if hellos continue. Withdraw removed routes
and remove every route using a dead neighbor. Use saturating addition and
explicit infinity handling.

Log each candidate and selected Bellman–Ford update, including old/new cost,
neighbor contribution, and poison decisions. Poisoned reverse does not prevent
all multi-router count-to-infinity cases; document that limitation and include
a small controlled exercise, separate from the normal convergence gate.

## Link state

Originate an LSA on adjacency or local-cost change and refresh every second.
Retain the newest incarnation/sequence per origin. Suppress duplicate and old
LSAs without refreshing their age. Flood new records to all established peers
except the incoming link. Synchronize the current LSDB when an adjacency
becomes established; periodic refresh repairs losses without reliable-transport
machinery.

Expire an unrefreshed LSA after 6000 ms and recompute. Keep freshness metadata
for the process lifetime under the 16-identity budget, preventing old records
from being reintroduced after expiration. Creation timestamps use the shared
host clock and survive flooding unchanged. Only use graph edges confirmed by
both endpoints' current advertisements. Preserve directional outgoing costs.

Run Dijkstra from the local router after an effective topology change. Derive
the first hop from the calculated path, then resolve its local interface and
peer IP. Apply the same deterministic tie-break and route preference as DV.
Log every iteration's settled set, tentative distances, and predecessors in a
format that can be compared with the textbook's step table.

## Integration with the existing router

Introduce a statically registered Go routing interface selected by config.
Implementations consume validated neighbor/advertisement events and time,
produce control advertisements, algorithm trace events, and learned routes.
Keep calculations and timers testable without sockets or root privileges.

The runtime owns control-packet I/O, neighbor state, and timer dispatch on the
same worker that handles packets. Management requests enqueue changes or use
immutable snapshots; they must not mutate algorithm state concurrently.

Merge learned routes with connected/static routes and publish a complete
immutable FIB through the existing atomic pointer. Increment the FIB version
only when the effective route set changes. Add learned metric and origin to
route output. Record the new FIB before a packet can reference its version.
Define queued-packet behavior explicitly: an existing queued output retains
its original route/version, matching the current runtime's history contract.

Routing packets use existing ARP, impairment, capture, trace, and counter paths.
ARP remains exempt from delay/loss/corruption as in Stage 2. Routing packets
receive those IP impairments, so protocol and forwarding failures can be
studied together. Fatal capture/export errors still stop the router.

## Inspection and experiments

- `goodnews show neighbors`: peer identity, interface/IP, algorithm, state,
  incarnation, effective cost, and time since the last valid hello.
- `goodnews show routes`: connected/static/learned origin, cost, next hop,
  interface, and FIB version. Make unreachable destinations explicit in
  algorithm history without installing them as forwarding routes.
- `goodnews show lsdb`: origin, freshness, age, advertised links and prefixes;
  explicitly unavailable for static and DV runs.
- `badnews link NAME cost N`: change an outgoing routing cost without a restart.
  Recompute and advertise immediately subject to trigger coalescing. Clear
  restores configured costs as well as Stage 2 impairments. Reject `cost` on a
  static router or passive LAN. Two directions are changed independently.
- Saved runs: per-interface Ethernet pcaps, existing packet/counter/FIB files,
  effective routing config, and a separate `routing.jsonl` algorithm/event log.
  Keep routing calculations out of packet-ID sequencing. Export failures are
  fatal. Use `ip.proto == 253` in Wireshark to find control packets.

The terminal dashboard should continue to display packets and forwarding
history. A six-router interactive topology editor is outside this stage.

## Implementation checkpoints

1. **Protocol and pure calculations.** Finalize exact wire layout, restart and
   aging rules, encoder/decoder tests, routing config, and Go interface. Verify
   Bellman–Ford and Dijkstra against hand-worked graphs before socket work.
2. **Neighbors and runtime integration.** Add control-packet dispatch, ARP-based
   sending, hello/dead timers, management snapshots, route publication, and
   routing history. Test malformed packets and timer transitions.
3. **Distance-vector lab.** Add six-router setup/config generation and run DV
   to convergence. Verify poisoned reverse, withdrawals, cost changes, and
   recovery. Make the first complete learning guide runnable here.
4. **Link-state lab.** Add flooding, LSDB synchronization/aging, Dijkstra traces,
   and the same end-to-end scenarios in LS mode.
5. **Acceptance and documentation.** Add CI coverage, evidence collection,
   cleanup/restoration verification, and the hands-on Stage 4 guide. Update
   implementation status separately from privileged empirical results.

## Verification and completion criteria

Unprivileged tests must cover both algorithms on the textbook graph and at
least two additional hand-worked graphs, including disconnected and equal-cost
cases. Cover stale/reordered/malformed packets, wrong peer/algorithm, restarting
origins, bidirectional adjacency, vector/LSA expiry, duplicate flooding,
saturating arithmetic, state bounds, cost changes, and concurrent management
reads. Exercise control messages through the runtime, not only pure algorithms.
Add a bounded decoder fuzz target and run the existing race/vet/source checks.

Provide `make lab4-up ALGORITHM=dv|ls`, `make lab4-check`, `make lab4-down`, and
`make integration4`. Setup refuses preexisting resources, records namespace
identity in a root-owned manifest, and rolls back partial setup. Cleanup
refuses replaced namespaces or namespaces containing user processes. A runner
that starts the six processes must record their identities and stop only its
own processes before teardown. Repeated cleanup is harmless.

For both algorithms, acceptance must retain evidence of:

1. Exact textbook metrics/first hops and bidirectional ping between endpoint
   LANs. Traceroute and captures show the learned multi-router path, TTL
   decrements, and valid IPv4 checksums.
2. A cost change with the expected new route, without restarting goodnews.
3. A failed active link and a stopped router, followed by withdrawal and
   alternate-path recovery. Record continuous-ping loss and measured
   convergence; assert stable final paths within an explicit deadline.
4. Restart/reconnection and restored textbook routes. In LS mode, every live
   router eventually has the same current origin records, excluding local
   receipt timestamps.
5. A deliberately partitioned destination becoming unreachable, rather than
   retaining stale routes. Stopping all goodnews processes makes remote LAN
   pings fail, proving there is no kernel bypass.
6. Successful cleanup, verified namespace removal, and a second setup/teardown.

Store command transcripts, generated configs, traces, pcaps, algorithm logs,
convergence timing, and final snapshots under a fresh `artifacts/stage4-*`
directory. Add a Stage 4 job to existing privileged CI. Record a physical Stage
3 result only after physical hardware has actually been tested; Stage 4 virtual
acceptance cannot close that gate.
