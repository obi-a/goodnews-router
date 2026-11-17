# Stage 4 protocol and integration contract

This is the custom goodnews teaching protocol, carried directly in IPv4
protocol 253. It is not RIP or OSPF. The supported lab uses namespaces on one
Linux host, with one configured peer per routing interface. See
[the lab guide](stage4.md) and [design](stage4-design.md).

## Wire format, version 1

All integers are unsigned and big-endian. Messages are at most 1024 bytes,
including their 32-byte common header. Require a routing-interface MTU of at
least 1044, so IPv4 plus the largest allowed message fits without fragmentation.
IPv4 source/destination are the sending/receiving interface's logical addresses;
TTL is 1. Ethernet and ARP use the existing Stage 2 driver. An IPv4 packet's
total length excludes any Ethernet padding.

| Byte offset | Size | Meaning |
| --- | ---: | --- |
| 0 | 2 | ASCII `GN` |
| 2 | 1 | Version, 1 |
| 3 | 1 | Type: hello 1, DV 2, LSA 3 |
| 4 | 2 | Total message length, including header |
| 6 | 2 | Internet checksum of the entire message, checksum field initially zero |
| 8 | 8 | Origin router ID, zero-padded ASCII |
| 16 | 8 | Origin startup incarnation |
| 24 | 8 | Origin sequence number |

Router IDs contain 1–8 ASCII letters, digits, underscores, or hyphens. All
padding is zero. Incarnation and sequence are nonzero. Checksum validation,
exact length, canonical encoding, and semantic bounds precede state changes.
`TestRoutingWireAndValidation` exercises round trips, corruption, truncation,
and invalid fields; `FuzzDecodeRouting` checks canonical decoder behavior.

**Hello body, 18 bytes:** algorithm byte (DV 1, LS 2); one zero reserved byte;
eight-byte seen-peer ID; eight-byte seen-peer incarnation. Unknown peer is an
empty ID and zero incarnation. A peer confirms this process only by echoing
both its ID and current incarnation. Algorithm mismatch never forms adjacency.

**DV body:** two-byte prefix count, followed by that many ten-byte entries.
Each entry has a four-byte IPv4 network address, one-byte prefix length,
four-byte weighted metric, and one-byte hop count. Prefixes are unique and
masked. Metrics are 0–65535; 65535 means infinity. Hops are 0–15. Receiving a
finite candidate that would require 16 hops makes it unreachable. This limits
multi-router counting loops while permitting every simple path in a lab of at
most 16 routers. Poisoned reverse advertises infinity to the selected next hop.
An omitted destination is withdrawn: every newer vector replaces the previous
one completely. There is no incremental-vector protocol.

**LSA body:** eight-byte origin creation time in Unix milliseconds; two-byte
edge count; two-byte prefix count; edge entries; prefix entries. Each edge is
an eight-byte neighbor ID and four-byte outgoing cost (1–1000). Each prefix is
a four-byte IPv4 network address and one-byte prefix length, locally cost zero.
Edges and prefixes are unique. Self-edges are invalid. At most 16 edges and 64
prefixes are accepted. The original header, timestamp, and topology survive
flooding unchanged; only the outer IP/Ethernet sender changes.

## Freshness, restart, and bounds

Before a process sends routing packets, it locks an owner-only incarnation
file beside its management socket (`u.sock.epoch` in this lab). Persist
`max(current Unix nanoseconds, previous incarnation + 1)` and fsync before
continuing. A restart therefore sorts after its predecessor even if the clock
moved backward. Corrupt state, symlinks, inappropriate ownership/modes, and
overflow fail startup. Preserve this file across a router restart; remove it
only with the entire lab's cleanup. Reusing an identity with erased state while
other routers still retain its old state is unsupported.

Sequence numbers increase within an incarnation. A hello must be newer than
that neighbor's last accepted hello. A DV update must come from the established
peer with its current incarnation and a newer vector sequence. For LSAs,
compare `(incarnation, sequence)` separately per origin, regardless of relay.
Equal/older records never refresh state or timers.

The shared host clock makes LSA creation time comparable across namespaces.
Reject records older than `max_age_ms` or more than one second in the future.
Never reset that timestamp during flooding. This avoids extending an expired
record's lifetime along a path. Multi-host clock synchronization and operation
across large wall-clock changes are outside this lab's guarantees.

Freshness tombstones survive LSA expiration for the lifetime of the process.
The identity budget is 16 distinct router IDs, including local, learned, and
advertised neighbors; new identities beyond it are rejected. Prefix state is
bounded at 64 distinct prefixes. Changing more than 15 remote identities over
one process lifetime requires a fresh lab. No unbounded tombstone collection
is hidden behind aging.

Default hello/dead/update/max-age timers are 500/2000/1000/6000 ms. Only valid
hellos refresh neighbor liveness. Vector and LSA expiration are independent.
Full periodic updates repair lost messages; LS adjacencies also synchronize
their current database when they become up. There is no retransmission/ACK
protocol beyond periodic refresh. DV triggered updates coalesce for 100 ms.

## Calculation and publication

`RoutingAlgorithm` has two implementations registered in Go at build time.
It consumes validated state and returns a complete calculation without I/O.
The shared controller handles timers, discovery, flooding, and bounded state.

DV uses Bellman–Ford over live neighbors' vectors, with positive weighted
costs, saturation at infinity, and the hop bound above. LS uses Dijkstra over
the current LSDB, requiring both endpoints to advertise an edge. Link weights
remain directional. Equal-cost choices use the lowest first-hop router ID,
then interface; one next hop is installed. The initial lab has no parallel
adjacencies between the same pair of router IDs.

Connected and static routes retain preference over learned routes with the
same exact prefix. Static routes are not redistributed. LPM still applies
between different prefix lengths. A cost is measured to the advertising
router, so its host LAN adds zero to the graph distance.

The runtime's packet worker handles protocol events and publishes a complete
immutable FIB. Its version changes only when effective installed routes change.
Record the new FIB before publishing it. An already queued packet retains its
selected route/FIB version; a later packet uses the new table. Management reads
detached snapshots and cost/down changes are applied by the worker on its next
tick, normally within 20 ms.

Control sends use the same ARP, capture, counters, queue bounds, delay/loss/
corruption, and link-down checks as data packets. Malformed control packets
are counted with named reasons and generate no ICMP responses. Duplicate LSAs
are visible as `routing_stale_lsa` drops, not reflooded packets. Export failures
are fatal. Separate `routing.jsonl` events never consume packet IDs.
