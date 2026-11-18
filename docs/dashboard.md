# Watch the router in a terminal dashboard

The dashboard combines the host/router diagram, counters, forwarding table,
and packet history. Select an older packet to see the state when it was handled.
All commands below work in both Fish and Bash.

## Try a saved example

From the project directory:

```sh
env GOTOOLCHAIN=auto make build
./bin/goodnews dashboard --run internal/dashboard/testdata/demo
```

This synthetic example includes Echo Requests and Replies, TTL expiry, an ICMP
unreachable response, and a second forwarding-table version. It needs neither
sudo nor a live lab. It is a demonstration, not real network acceptance evidence.

Press Home to select the first request: expect RX/TX counters of 1/1 and table
version 1. Press End to select the last packet: expect counters of 5/5, two
drops, and table version 2. Press Enter to inspect fields and route matches.

## Watch your running lab

Rebuild, then start a fresh router process so it records dashboard metadata.
If the lab is already up, reuse it; stop the old router with Ctrl-C before
starting the new binary. For a new lab, first run:

```sh
make lab-up
```

**Terminal A — router:**

```sh
sudo ip netns exec gn-r1 ./bin/goodnews run --config lab/stage1.json
```

**Terminal B — dashboard:**

```sh
sudo ./bin/goodnews dashboard --config lab/stage1.json
```

The dashboard asks the configuration's management socket for the actual run
directory. There is no `RUN_DIR` variable to set for live attachment.

**Terminal C — traffic:**

```sh
sudo ip netns exec gn-h1 ping -n -c 3 -W 1 10.0.1.2
```

Expect Echo Requests from `left` to `right` and Echo Replies in the opposite
direction. With host TTL 64, forwarded packets show `64>63`. Three successful
pings add six received and six transmitted packets to the totals.

Try an error and a rejected packet:

```sh
sudo ip netns exec gn-h1 ./bin/goodnews craft --interface gn0 --ttl 1
sudo ip netns exec gn-h1 ./bin/goodnews craft --interface gn0 --bad-checksum
```

Select the expired request and press Enter. Expect `ttl_expired` and a related
generated Time Exceeded response. The corrupt packet shows `invalid_checksum`;
fields validation could not establish remain unknown.

When following the [route/protection experiments](stage1-experiments.md), pass
the same experimental configuration to the router and dashboard.

To watch successful application traffic as well as errors, follow the
[UDP and HTTP exercises](traffic.md). They explain `left`/`right`, the exact
packet sent by `craft --protocol udp`, and how to start receiving applications
on h2. A forwarded ICMP Port Unreachable or TCP reset can mean the router
worked correctly while an application port was closed.

## Navigate and inspect history

| Key | Action |
| --- | --- |
| Up / Down, or `k` / `j` | Select an older/newer packet |
| Page Up / Page Down | Move by half a terminal page |
| Mouse wheel | Browse history in terminals that report wheel events |
| Home / `g` | Select the earliest indexed event |
| End / `G` | Resume following the latest event |
| Space | Pause/resume following |
| Tab | Switch focus between history and the forwarding table |
| Up / Down with table focused | Scroll routes without changing the packet |
| Enter | Open/close selected packet details |
| Escape | Return from details |
| Up / Down or Page Up / Down in details | Scroll the detail text |
| `r` | Attach to a newly detected router run |
| `q` / Ctrl-C | Exit the dashboard |

Scrolling back freezes selection while new activity continues recording. The
header reports how many newer events are waiting. Counters, table version,
observed endpoints, and the highlighted path follow the selected event. The
table marks its winning route with `*` when present in the displayed version.

Historical counters describe completed handling of an incoming packet,
including generated responses. An ICMP response shares its parent's snapshot;
it does not represent another received packet. The heading identifies the
incoming packet associated with the snapshot. LIVE mode shows current totals
and the current forwarding table.

Topology connections come from recorded configuration. Each interface shows up
to three first-observed endpoint addresses, plus any endpoint from the selected
packet. Arrows indicate that packet's direction. They do not represent physical
cable discovery.

## Reopen a past run

Use the actual directory printed by your router, for example:

```sh
./bin/goodnews dashboard --run artifacts/runs/r1-885399003
```

Replace the example number with your real run directory. Saved-run mode reads
files without contacting the router. Old runs remain readable, with unrecorded
historical tables, correlated counters, and ICMP fields labeled unavailable.
The current configuration is never substituted for missing historical state.

Temporary on-disk offset indexes provide access to the whole run. Only 256
decoded events are cached; older events remain accessible. Large files index
incrementally, and temporary indexes are removed on exit. Original logs stay
intact.

## Connection and terminal behavior

- LIVE follows a connected router; scrolling switches to HISTORY. Saved runs
  use REPLAY while following their latest record.
- History stays open if the router stops. If it restarts on the same socket,
  press `r` when a new run is reported. The runs stay separate.
- Use a terminal at least 80 columns wide and 24 rows high. Smaller windows
  show a resize message while history keeps indexing. Resize at any time.
- `NO_COLOR` disables color; text labels and selection markers remain.
- New artifacts belong to the account that invoked `sudo`, with directories
  set to `700` and files to `600`. Saved-run browsing needs no sudo for your
  own runs. Live attachment still uses sudo for the restricted management
  socket. Direct root runs keep root-owned artifacts.
- Partial trailing JSON lines wait for completion. Malformed records produce
  a notice while valid history stays readable. Reopen a run if a file is truncated.

If attachment reports `/v1/status` unavailable, rebuild and restart the router.
A process running the old binary cannot supply new metadata. Use `--run` to
inspect its saved logs instead.

When finished, quit the dashboard, stop the router, and run `make lab-down`.

## Inspect Stage 4 routers

Attach using any Stage 4 router configuration:

```sh
sudo ./bin/goodnews dashboard --config /run/goodnews-lab-stage4/u.json
```

Press **t** for the router inspector. It discovers router sockets in the attached
socket's directory when opened. **n/p** selects the next/previous router;
**Tab** cycles through forwarding routes, neighbors, link-state database,
counters, interfaces, ARP, and impairments. Routes use a full-height table;
other sections show the management API's structured fields. Up/Down and
Page Up/Page Down scroll. The selected section refreshes about once per second;
shorter-lived changes can occur between samples. Unavailable routers or
unsupported sections show their errors in the inspector.

**Space** freezes the displayed snapshot; press it again to resume updates.
Routers continue processing traffic and exchanging routing messages while the
display is paused. Resume before switching to a section you have not loaded.
This view shows sampled current state, not a recorded timeline of route changes.
Press **t** to return to packet history, or **q** to exit the dashboard.

### Read the state before changing it

Start a fresh lab if necessary (`make lab4-up ALGORITHM=dv`, then
`make lab4-start`). For an existing lab, attach directly. In the inspector,
select u and find `10.45.5.0/24` in routes. At convergence its cost is **4**,
with next hop x (`10.44.0.6`) on `to-x`: u → x → y → z.
The destination host is `10.45.5.2`; its LAN attachment adds zero cost.
Cost is the sum of link weights; hops counts router links. Connected routes
normally have zero cost. A forwarding-table version identifies an installed
table, not the number of routing messages received.

Use these sections together:

| Section | What to look for |
| --- | --- |
| routes | Destination prefix, chosen next hop/interface, origin, cost, hops, and table version |
| neighbors | Peer router, algorithm, adjacency state, outgoing cost, and milliseconds since last hello |
| lsdb | Advertised links/prefixes and advertisement identity; most useful in LS mode |
| counters | Receive/transmit/drop totals and per-interface activity |
| interfaces | Configured interfaces and addressing |
| arp | Neighbor IP-to-MAC resolution used to send Ethernet frames |
| impairments | Active runtime changes that can explain altered behavior |

The database view is not a complete view of DV's received vectors. For the
actual Bellman–Ford candidates or Dijkstra steps, use the algorithm logs below.
Each section is fetched separately; observations across routers and sections
are not one atomic network-wide snapshot. Pause is for examining a loaded
snapshot, not stopping routing, and does not provide history navigation in the
inspector. The packet dashboard retains its separate recorded-event history.

### Experiment: change a cost, then break a link

Keep a continuous endpoint ping running in another terminal:

```sh
sudo ip netns exec gn4-h-u ping -n -i 0.2 10.45.5.2
```

With u's routes live in the inspector, raise u's outgoing cost toward x:

```sh
sudo ./bin/badnews --socket /run/goodnews-lab-stage4/u.sock link to-x cost 60
```

After convergence, the route to `10.45.5.0/24` should use `to-v`, next hop
`10.44.0.2`, at cost **7**. Pause to read it. Resume, then inspect neighbors
and impairments to connect the changed route to the changed outgoing cost.
This is directional: x's outgoing cost toward u is independent.

Restore the baseline, wait for cost 4, then disable the same link:

```sh
sudo ./bin/badnews --socket /run/goodnews-lab-stage4/u.sock clear
sudo ./bin/badnews --socket /run/goodnews-lab-stage4/u.sock link to-x down
```

Expect the route through v at cost **7** again. Inspect u's neighbors, then
switch to x to watch the other side expire u after missed hellos. Detection
and convergence take time; temporary ping loss is possible. This disables
u's link behavior through the lab's fault controls; x itself is still running.
Restore the configured state and confirm cost 4 and ping replies return:

```sh
sudo ./bin/badnews --socket /run/goodnews-lab-stage4/u.sock clear
```

`clear` removes all ordinary impairments on that router, including cost
overrides. Change one condition at a time so each observation has a clear cause.

### See packets and the reason for a route change

Capture forwarded pings at u in another terminal:

```sh
sudo ip netns exec gn4-u tcpdump -ni any 'icmp and host 10.45.5.2'
```

Watch routing messages separately:

```sh
sudo ip netns exec gn4-u tcpdump -ni any 'ip proto 253'
```

Forwarded pings appear on ingress and egress interfaces. Protocol 253 packets
are router control messages; their continued arrival does not mean the
forwarding table is changing. Press t to return to packet history and Enter
on a selected event for recorded packet details and route matching.

For calculation details, locate u's current run under the artifact directory
printed by lab setup (`runs/u-*/routing.jsonl`). Use its actual path:

```sh
tail -f /path/to/current/u-run/routing.jsonl
```

This path is a placeholder. Select the current run rather than an older run
from before a restart. Look for `cost_change` or `neighbor_down`, then
`bellman_ford` and `route_update` in DV mode. Bellman–Ford events expose the
neighbor's advertised cost, local link cost, total candidate cost, and selection.
In LS mode, compare `lsa_receive`/`lsa_originate`, `dijkstra` distances and
predecessors, and `route_update`. These logs explain intermediate calculations
that a once-per-second display may miss.

### Compare DV and LS, and run acceptance checks

Quit the dashboard and stop the continuous ping before rebuilding the lab:

```sh
make lab4-down
make lab4-up ALGORITHM=ls
make lab4-start
sudo ./bin/goodnews dashboard --config /run/goodnews-lab-stage4/u.json
```

Repeat the same cost and link-failure experiments. The settled baseline and
alternate costs should agree; the messages and calculation logs differ.
In LS mode, compare the effective topology in each router's lsdb section.
Quit and reopen the inspector after router restarts if a socket disappeared
from discovery; reopening discovers the sockets currently present.

For the project's automated convergence/failure acceptance suite, first quit
the dashboard, stop the ping, and remove the interactive lab:

```sh
make lab4-down
make integration4
```

This privileged suite exercises both algorithms and retains evidence under
`artifacts/stage4-*`. It checks router behavior; it does not replace manual
inspection of the interactive dashboard. See [Stage 4](stage4.md) for router
restart experiments, evidence interpretation, and cleanup details.
