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
sudo ./bin/goodnews dashboard --run artifacts/runs/r1-885399003
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
- Files keep their private permissions. Use sudo for root-owned lab runs;
  user-readable copies and the demo need no sudo.
- Partial trailing JSON lines wait for completion. Malformed records produce
  a notice while valid history stays readable. Reopen a run if a file is truncated.

If attachment reports `/v1/status` unavailable, rebuild and restart the router.
A process running the old binary cannot supply new metadata. Use `--run` to
inspect its saved logs instead.

When finished, quit the dashboard, stop the router, and run `make lab-down`.
