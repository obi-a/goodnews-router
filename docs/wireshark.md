# Inspect Stage 1 packets with Wireshark

Use Wireshark to compare the packets entering and leaving the router. The
router already records `left.pcap` and `right.pcap`; open these files to inspect
individual fields and confirm forwarding or a drop.

First follow the [Stage 1 lab guide](stage1.md) to build the router, create the
namespaces, and start the router in Terminal A. Run the commands below from
the project directory in Terminal B. Shared `sh` blocks work in Fish and Bash.

## 1. Install Wireshark

On Ubuntu, install it if needed:

```sh
sudo apt-get update
sudo apt-get install wireshark
```

These commands use Ubuntu's packages, as described in Wireshark's
[installation instructions](https://www.wireshark.org/docs/wsug_html_chunked/ChBuildInstallUnixInstallBins.html).
If installation asks whether non-root users should capture packets, you can
choose **No** for this exercise. Opening saved captures does not require live
capture permissions. Launch the Wireshark application as your regular user.

## 2. Generate traffic and select the run directory

Leave the router running in Terminal A. In Terminal B, send three pings:

```sh
sudo ip netns exec gn-h1 ping -n -c 3 -W 1 10.0.1.2
```

Expect replies from h2. The router records each request and reply on both sides.

If you already saved `RUN_DIR` for this router run, keep using it. Otherwise,
find the artifact directory printed by the router in Terminal A, for example:

```text
artifacts: artifacts/runs/r1-885399003
```

In Terminal B, run the command for your shell. At the prompt, paste the
**actual directory from Terminal A**, without the `artifacts:` label. Keep
`RUN_DIR` in the command; it is the variable name, not a path placeholder.

**Fish:**

```fish
read -P 'Artifact directory from Terminal A: ' RUN_DIR
```

**Bash:**

```bash
read -r -p 'Artifact directory from Terminal A: ' RUN_DIR
```

Every router restart creates a new directory. Update `RUN_DIR` after a restart.
Continue using this terminal so the saved variable is available.

## 3. Open the captures

Once the pings finish, check the actual run directory from the project root:

```sh
ls -1 artifacts/runs
ls "$RUN_DIR"
```

You should see `left.pcap` and `right.pcap` in your run. New artifacts belong
to the account that invoked `sudo`: directories use `700` and files use `600`.
This keeps captures private while allowing you to open them as your regular
user. Direct root runs keep root ownership. Existing directories keep their
ownership and permissions.

Open the left capture:

```sh
wireshark -r "$RUN_DIR/left.pcap" &
```

`-r` opens a saved capture, and `&` keeps the terminal available for more lab
commands. You can also launch `wireshark` and choose
**File → Open**, as described in the
[Wireshark manual](https://www.wireshark.org/docs/man-pages/wireshark.html).
There is no need to select a live capture interface for these files.

To compare both sides side by side, open a second window:

```sh
wireshark -r "$RUN_DIR/right.pcap" &
```

`left.pcap` contains traffic received and successfully sent on the h1 side;
`right.pcap` contains traffic received and successfully sent on the h2 side.
Each file includes both directions, rather than requests in one file and
replies in the other. These TUN captures contain IPv4 packets without Ethernet
headers.

After generating more traffic, use **File → Reload** or reopen the files to
read newly recorded packets. Opening a saved capture does not start a live
capture in Wireshark.

## 4. Compare a forwarded ping request

In each Wireshark window, enter this in the display-filter bar and press Enter:

```text
icmp.type == 8 && ip.src == 10.0.0.2 && ip.dst == 10.0.1.2
```

This shows h1's Echo Requests to h2. Display filters change which recorded
packets are visible; clearing the filter shows all packets again. See
[Wireshark's display-filter guide](https://www.wireshark.org/docs/wsug_html_chunked/ChWorkDisplayFilterSection.html).

Select a request in the packet list. In the details pane, expand
**Internet Protocol Version 4** and **Internet Control Message Protocol**.
Find the corresponding request in the other capture by comparing source,
destination, ICMP identifier, sequence number, and nearby timestamps. Packet
list numbers belong to each capture and need not match between files.

With the default host TTL and router protections, expect:

| Field | Left capture: request arriving | Right capture: request forwarded |
| --- | --- | --- |
| Source | `10.0.0.2` | `10.0.0.2` |
| Destination | `10.0.1.2` | `10.0.1.2` |
| Time to Live | 64 | 63 |
| IPv4 header checksum | Valid incoming value | Recalculated after TTL changes |
| ICMP identifier, sequence, and payload | Original values | Same values |

The router decrements TTL once and updates the IPv4 header checksum. It keeps
the host addresses and ICMP message intact. The Echo Reply takes the reverse
path: it arrives on the right and leaves on the left.

Other useful display filters are:

| Filter | What to inspect |
| --- | --- |
| `icmp` | Ping messages and ICMP errors |
| `icmp.type == 0` | Echo Replies |
| `icmp.type == 11` | Time Exceeded responses, such as traceroute's first hop |
| `icmp.type == 3 && icmp.code == 3` | Port Unreachable responses from a closed UDP port |
| `udp` | UDP packets, including default traceroute probes |
| `tcp` | TCP traffic, including HTTP connections |

Use the [traffic guide](traffic.md) to generate UDP and HTTP exchanges. An
ICMP Port Unreachable from h2 can confirm that the router delivered a UDP
probe to the destination host, where its application port was closed.

## 5. Inspect a deliberately corrupt packet

Keep the default router configuration, with IPv4 checksum verification enabled.
In Terminal B, send one packet with a corrupt IPv4 header checksum:

```sh
sudo ip netns exec gn-h1 ./bin/goodnews craft --interface gn0 --bad-checksum
```

`craft` sends one ICMP Echo Request from h1 to h2 and is silent on successful
transmission. Reload or reopen both captures to see the new packet.

In Wireshark, open **Edit → Preferences → Protocols → IPv4**, enable
**Validate the IPv4 checksum if possible**, and apply the setting. This
[IPv4 preference](https://wiki.wireshark.org/Internet_Protocol#preference-settings)
controls whether Wireshark checks the header checksum. A label such as
`unverified` or `validation disabled` means validation is not enabled; it does
not mean the checksum is correct.

Select the new request in `left.pcap`, using its timestamp to distinguish it
from earlier pings. Expand **Internet Protocol Version 4 → Header Checksum**
and look for an incorrect checksum. The packet should appear on the left and
have no forwarded copy on the right: the router captures incoming packets
before validation and drops this one during validation.

For the corresponding counter changes and trace record, follow
[Corrupt a packet](stage1.md#5-corrupt-a-packet). The expected trace action is
`drop`, with reason `invalid_checksum`.

Use the [dashboard guide](dashboard.md) to connect these packet fields with
the router's forwarding decisions, drop reasons, and historical counters.
