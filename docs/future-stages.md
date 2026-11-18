# Later stages: standard routing protocols and Wi-Fi

Status: planned work, not implemented. This roadmap adds explicit destinations
for RIPv2, OSPFv2, BGP, and Wi-Fi after the existing Stage 5 fragmentation/NAT
and Stage 6 controller milestones. Stage 3's physical hardware gate remains
independent. Stage 4 remains the custom protocol-253 DV/LS algorithm lab.

| Stage | Deliverable | Initial lab | Completion evidence |
| --- | --- | --- | --- |
| 7A | RIPv2 interoperability | Namespaces and virtual Ethernet, with an FRR peer | Standard RIP exchange, learned forwarding, withdrawal and recovery |
| 7B | Single-area OSPFv2 interoperability | Namespaces and virtual Ethernet, with an FRR peer | Adjacency/database synchronization, expected paths and failure recovery |
| 8 | IPv4 unicast BGP peering and policy | Multiple simulated autonomous systems, with FRR peers | Announcements, withdrawals, AS-path loop rejection and policy-driven forwarding |
| 9 | Wi-Fi access through goodnews | Virtual left host, router laptop acting as AP, physical wireless client | Association, bidirectional forwarding, captures, stopped-router failure and restoration |

Stages 7 and 8 need no physical Ethernet ports or Wi-Fi adapters. Stage 9 has a
physical radio/client acceptance gate. The numbers describe the default build
order; Stage 9's first isolated, statically addressed lab need not wait for BGP
or the controller. Its prerequisites are the Ethernet/ARP forwarding and
reversible physical-device lifecycle established in Stages 2 and 3. Internet
sharing is a later extension using Stage 5's NAT.

## Stage 7: standard intra-network routing protocols

Build RIPv2 first, then OSPFv2. Reuse the forwarding-table publication and
observability interfaces from Stage 4, but implement each standard's own packet
formats and behavior. The existing custom `dv` and `ls` modes remain useful for
algorithm comparisons; they are not renamed into standard protocol support.

### Stage 7A: RIPv2

Implement the selected baseline from [RFC 2453](https://www.rfc-editor.org/rfc/rfc2453.html):
RIP request/response messages, UDP port 520, multicast handling, metric rules
with infinity 16, periodic/triggered updates, expiration/withdrawal, and split
horizon/poisoned reverse. Define checksum validation, timer behavior and the
supported authentication profile before implementation. Do not reuse the
custom protocol's weighted infinity or hello handshake as RIP behavior.

Start with goodnews and an independently configured FRR RIP peer. Exchange
prefixes in both directions and install learned routes in goodnews' Go FIB.
FRR is a reference peer, not a replacement for the algorithm in goodnews.

Exit gate: Wireshark decodes the exchange as RIPv2; both peers learn the expected
routes; endpoint ping/traceroute cross goodnews; withdrawing a prefix or stopping
a peer removes the route and reconnection restores it. Compare metrics with
hand-worked RIP expectations, not Stage 4's weighted-cost example. Retain
malformed/stale-message, timer, loop-prevention and stopped-goodnews tests.

### Stage 7B: OSPFv2

Begin with an explicitly bounded [RFC 2328](https://www.rfc-editor.org/rfc/rfc2328.html)
profile: IPv4, area 0, point-to-point router links, and passive endpoint LANs.
Implement protocol-89 packets, hello and neighbor state transitions, database
description/exchange, LSA requests/updates/acknowledgments, freshness/aging,
retransmission, and Dijkstra-derived routes. Specify the supported LSA and
authentication types before coding. A custom hello plus Dijkstra is not enough
to claim OSPF interoperability.

Exit gate: goodnews reaches the intended full adjacency state with FRR, relevant
LSDB contents agree, explicit link costs produce the predicted routes, and a
failed link/peer causes withdrawal and alternate-path recovery. Wireshark must
decode standard OSPF packets, and captured data packets must follow the learned
Go FIB. Multi-area routing and broadcast-network DR/BDR behavior are separate
follow-up scope, not implicit requirements of this first point-to-point lab.

Before either checkpoint, design standard multicast/control-packet dispatch,
transport checksums, bounded state/queues, protocol-specific route preference,
and config/inspection views. Keep kernel forwarding disabled in goodnews
namespaces. Reference peers may use their own networking stack; stopping
goodnews must still break every test path intended to cross it.

## Stage 8: BGP peering, autonomous systems and policy

Promote the previously informal BGP follow-up project into this stage. Start
with a small Go BGP speaker interoperating with FRR, using the baseline from
[RFC 4271](https://www.rfc-editor.org/rfc/rfc4271.html). The initial scope is IPv4
unicast eBGP between simulated autonomous systems, with OPEN, KEEPALIVE, UPDATE,
NOTIFICATION, session timers, AS_PATH, NEXT_HOP, announcements, and withdrawals.
Add explicit import/export policy and observable route selection.

Use at least three simulated ASes so two paths to a destination can be compared.
Demonstrate a deliberate policy choosing a different path from a simple
shortest-path calculation. Reject an announcement containing the local AS in
its AS path. IPv6, route reflectors, advanced extensions, and Internet peering
are later scope; document the negotiated capabilities actually supported.

Before implementation, settle how TCP sessions attach to the control plane,
how advertised BGP next hops resolve to goodnews data interfaces, and how BGP
routes interact with connected/static/IGP routes. Kernel TCP for a separate
control connection must not introduce kernel IP forwarding through goodnews.
Do not assume a management-session address is a usable data-plane next hop.

Exit gate: goodnews establishes a session with FRR, exchanges and withdraws
prefixes, rejects AS-path loops, applies the predicted policy, and forwards
endpoint traffic using the resulting Go FIB. Disconnect/restart a peer and
require correct withdrawal and recovery. Save session/attribute/policy logs,
packet captures and route/FIB snapshots alongside ping/traceroute evidence.

The existing Go standard-library-plus-x/sys constraint applies to goodnews.
FRR supplies an independent test peer, not a library hiding our protocol logic.

## Stage 9: Wi-Fi access-point lab

Make the router laptop expose a lab SSID that a physical client can join. The
first topology follows the Stage 3 hybrid layout with its physical leg changed
to Wi-Fi:

```text
Virtual host namespace -- veth -- goodnews -- AP-capable Wi-Fi radio )) client laptop
```

Use hostapd or equivalent AP management for association and link security;
goodnews continues to own IPv4 forwarding, ARP, TTL/checksum handling, traces
and the FIB. [Linux wireless documentation](https://wireless.docs.kernel.org/en/latest/en/users/documentation/hostapd.html)
describes the AP/driver boundary. Configure static client addresses and routes
first, so this isolated experiment does not depend on DHCP or Internet access.

Check the actual radio/driver's AP mode, usable channels, and interface
combinations before choosing hardware. Start with the built-in radio if it
supports the intended setup. A dedicated USB Wi-Fi adapter is an option if AP
support or simultaneous management connectivity requires it; buying one is
not a prerequisite for planning this stage.

Before setup, specify PHY/interface namespace ownership, AP-service lifecycle,
credentials handling, and restoration of driver, NetworkManager and link
settings. Do not assume an AP interface can be moved independently of a home
Wi-Fi connection on the same radio. Concurrent client/AP use must be verified
with the intended topology and its channel restrictions. Keep a separate
management path when needed.

Ordinary connection sharing is not the acceptance test: NetworkManager's
[IPv4 shared mode](https://networkmanager.dev/docs/api/latest/settings-ipv4.html)
enables system forwarding. The Wi-Fi setup must keep kernel forwarding
disabled in goodnews and force the client/virtual-host path through our process.

Exit gate:

1. The physical client joins the lab SSID and reaches its goodnews gateway.
2. Client and virtual host exchange ping and application traffic in both
   directions. Captures prove ARP, the selected route, TTL decrement, and valid
   checksums across the router.
3. Stopping goodnews breaks remote-endpoint traffic even while Wi-Fi stays
   associated; restarting restores it. This proves association alone is not
   providing an IP-forwarding bypass.
4. Disconnect/reconnect the client or radio and verify diagnosable failure and
   recovery. Clean up and restore management connectivity and original settings.
5. Retain OS/radio/driver details, setup/restoration records and packet evidence.
   Synthetic CI can test lifecycle behavior; real radio acceptance stays separate.

DHCP address assignment and Internet sharing can be added after the isolated
static-address lab. The former requires its own DHCP design; the latter must
use goodnews' Stage 5 NAT and retain a stopped-goodnews test. Neither is needed
to prove the first wireless forwarding milestone.

## Before implementation

Each checkpoint needs a short protocol/design review, concrete supported
features, independent-peer configurations, resource bounds, failure tests,
cleanup, and a hands-on guide. Add implementation and empirical gates
separately. Planned stage numbers do not mean the code, CLI commands, or
hardware compatibility are already available.
