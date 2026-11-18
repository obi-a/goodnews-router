# Stage 3: physical Ethernet hosts

This lab connects endpoints through the Ubuntu goodnews router. Use two physical
hosts, or choose **hybrid mode** for one physical host and one virtual host on
the router laptop. Physical hosts can run Linux, Windows, or macOS. Stage 1 TUN
and Stage 2 veth labs keep their existing commands and configurations.

Stage 3 uses **wired Ethernet for forwarding**. Keep Wi-Fi connected for Internet
access, SSH, and management. Stage 5 adds fragmentation/NAT, and planned
[Stage 9](future-stages.md#stage-9-wi-fi-access-point-lab) adds a Wi-Fi forwarding
lab. DHCP is a subsequent exercise. Stage 4's custom DV/LS algorithm lab is
separate from the standard protocol interoperability planned for Stage 7.

The physical exit gate remains pending until the hardware results are recorded
in [verification.md](verification.md). `make integration3` exercises both modes
with real Linux namespaces and veth devices, using a simulated NetworkManager
backend and synthetic physical-port fixtures. It does not certify physical
adapters or Windows/macOS hosts.

## Choose the topology

| Mode | Router laptop ports | Left endpoint | Right endpoint |
| --- | --- | --- | --- |
| `physical` (default) | Two specified physical Ethernet ports | Physical host | Physical host |
| `hybrid` | One specified physical Ethernet port | Virtual host namespace on this laptop | Physical host |

Mode is chosen at setup, never from cable carrier state. An unplugged cable in
physical mode remains a failed physical link; it does not turn into a virtual
host. Changing topology requires stopping the router, running `make lab3-down`,
then setting up the other mode. Omitting `LEFT_DEVICE` in the default mode is
an error. Hybrid mode must be selected with `MODE=hybrid` and uses only
`RIGHT_DEVICE`; leave `LEFT_DEVICE` unset or empty.

## Two-laptop hybrid lab

Use this procedure when you have this Ubuntu router laptop and one other
Ubuntu laptop. Each needs one Ethernet port or adapter, connected by one cable.
No third computer or full virtual machine is required.

```text
This Ubuntu laptop                                             Other Ubuntu laptop
gn3-h1: 10.0.0.2/24 -- veth -- goodnews in gn3-r1 -- Ethernet -- 10.0.1.2/24
                              left: 10.0.0.1
                              right: 10.0.1.1
```

### Prepare the router laptop

Run from this repository on the router laptop:

```sh
make build
ip -br address
nmcli device status
```

Identify the dedicated wired port connected to the other laptop. In the examples
below it is `enp1s0`; replace that name with your device. The selected port must
have no existing addresses, routes, or active NetworkManager connection. If
NetworkManager automatically connected that dedicated port, disconnect only
that port first:

```sh
sudo nmcli device disconnect enp1s0
```

Keep Wi-Fi connected for management. Check that `10.0.0.0/24` and
`10.0.1.0/24` do not overlap LAN, VPN, or container routes on either laptop.
Preflight is read-only; setup repeats its checks:

```sh
make lab3-preflight MODE=hybrid RIGHT_DEVICE=enp1s0
make lab3-up MODE=hybrid RIGHT_DEVICE=enp1s0
make lab3-check
```

Setup moves the selected physical port into `gn3-r1`, creates an owned virtual
Ethernet pair (`left0` in `gn3-r1`, `gn0` in `gn3-h1`), and configures the virtual
host as `10.0.0.2/24` with gateway `10.0.0.1`. Both router interfaces stay free
of kernel addresses and kernel forwarding stays disabled. NetworkManager
exclusion and physical settings snapshots apply only to the selected real port.
The generated config is `/run/goodnews-lab-stage3/router.json`.

For alternate subnets, pass the same `LEFT_SUBNET` and `RIGHT_SUBNET` values to
both preflight and up, then adjust all endpoint addresses/routes below. Setup
prints the chosen physical endpoint address and virtual namespace.

### Configure the other Ubuntu laptop

These commands run on the **other** laptop. Identify its dedicated wired device
with `ip -br address` and `nmcli device status`. Here it is also named `enp1s0`;
its actual name can differ from the router laptop's port.

Ensure the profile name `goodnews-stage3` is unused and the selected port has no
existing configuration to preserve. This lab-owned profile sets no default
route or DNS and does not autoconnect:

```sh
nmcli connection show
sudo nmcli connection add type ethernet ifname enp1s0 con-name goodnews-stage3 connection.autoconnect no ipv4.method manual ipv4.addresses 10.0.1.2/24 ipv4.routes '10.0.0.0/24 10.0.1.1' ipv4.never-default yes ipv6.method disabled
sudo nmcli connection up goodnews-stage3
ip -4 route get 10.0.0.2
```

The route should use `10.0.1.1` through the selected wired interface. A local
firewall must permit the lab's ICMP echo requests from `10.0.0.0/24` on that
interface. Use a scoped rule in your existing firewall if needed, keep the
firewall enabled, and remove the lab rule during cleanup.

### Start the router and test both directions

Back on the router laptop, leave this command running in terminal 1:

```sh
sudo ip netns exec gn3-r1 ./bin/goodnews run --config /run/goodnews-lab-stage3/router.json
```

In terminal 2 on the router laptop, test from the virtual host:

```sh
sudo ip netns exec gn3-h1 ping -n -c 3 10.0.0.1
sudo ip netns exec gn3-h1 ping -n -c 3 10.0.1.2
sudo ip netns exec gn3-h1 traceroute -I -n -q 1 -m 3 10.0.1.2
sudo ./bin/goodnews show arp --socket /run/goodnews-lab-stage3/r1.sock
sudo ./bin/goodnews show counters --socket /run/goodnews-lab-stage3/r1.sock
```

On the other laptop:

```sh
ping -n -c 3 10.0.1.1
ping -n -c 3 10.0.0.2
traceroute -I -n -q 1 -m 3 10.0.0.2
```

Expect the local `.1` router address, then the opposite endpoint. Compare the
router's `left.pcap` and `right.pcap` captures: ARP on both legs, the same echo
payload and IP endpoints, TTL lower by one, and a valid IPv4 checksum.

Stop goodnews with Ctrl-C and repeat the remote-endpoint ping: it must fail.
Restart goodnews and require recovery. While pinging, unplug and reconnect the
physical cable; require loss and recovery without a topology change. Record
the adapter names, command output, and artifact directory for this physical
Linux endpoint run. It supplies useful mixed virtual/physical evidence; the
original two-physical-endpoint and Windows/macOS gate remains separate.

### Restore both laptops

Stop goodnews, any endpoint ping, and any shell running inside a lab namespace.
On the other laptop, remove the lab-owned profile and any lab firewall rule:

```sh
sudo nmcli connection down goodnews-stage3
sudo nmcli connection delete goodnews-stage3
```

On the router laptop:

```sh
make lab3-down
ip -br link
nmcli device status
```

Cleanup restores the physical port's original MTU, administrative state,
offloads, interface sysctls, and NetworkManager managed/autoconnect settings.
It removes both owned namespaces and their virtual cable. A running virtual
endpoint process, replaced namespace, or replaced virtual cable is reported
before moving the physical port. Keep the state manifest if cleanup reports a
problem, resolve its diagnostic, and rerun cleanup. Repeated cleanup is harmless.

## Three-laptop physical lab: equipment and addresses

Use two dedicated Ethernet ports on Ubuntu (built-in or USB adapters), two
cables, and an Ethernet port or adapter on each endpoint. Physical port names
are discovered at setup; they are never hard-coded into router source.

```text
left host                 Ubuntu: goodnews in gn3-r1                 right host
10.0.0.2/24 -- Ethernet -- left (10.0.0.1)   right (10.0.1.1) -- Ethernet -- 10.0.1.2/24
```

The `.1` addresses exist only in Go. The router namespace has no addresses on
its Ethernet ports and has kernel forwarding disabled. Each endpoint receives
a route to the opposite subnet through its local `.1` gateway. No default
gateway or DNS change is needed. Check that these subnets do not overlap any
existing LAN, VPN, or container route on **all three machines**.

All Ubuntu commands below run from the repository root. They work in Bash and
Fish unless specifically labeled. Ubuntu needs the README prerequisites and
a running NetworkManager with `nmcli`. Endpoint Ethernet interfaces must also
be dedicated to this lab. Record their original settings before changing them.

## Prepare the Ubuntu ports

Build and inspect available hardware:

```sh
make check build
ip -br link
ip -br address
ip -4 route show table all
nmcli device status
```

Use `ethtool -i DEVICE` to identify a driver and USB/bus attachment. Replace
`enp1s0` and `enx001122334455` below with the two actual wired port names.
Wi-Fi, virtual devices, bridges, bonds, devices with VLAN children, and ports
carrying an active connection are refused. Both selected ports must have no
IPv4/IPv6 addresses or routes. Setup does not flush existing configuration.

If a dedicated endpoint cable triggers an automatic NetworkManager connection,
disconnect that selected port before preflight. Verify again that it has no
addresses or routes. Do this only for a port you have identified as unused:

```sh
sudo nmcli device disconnect enp1s0
sudo nmcli device disconnect enx001122334455
ip -br address
```

Preflight is read-only. Setup repeats the checks while holding a lifecycle lock:

```sh
make lab3-preflight LEFT_DEVICE=enp1s0 RIGHT_DEVICE=enx001122334455
make lab3-up LEFT_DEVICE=enp1s0 RIGHT_DEVICE=enx001122334455
make lab3-check
```

For conflicting subnets, pass an override to both preflight and up, and adjust
the endpoint commands accordingly:

```sh
make lab3-up LEFT_DEVICE=enp1s0 RIGHT_DEVICE=enx001122334455 LEFT_SUBNET=192.0.2.0/24 RIGHT_SUBNET=198.51.100.0/24
```

Setup records identities, administrative link state, MTUs, offloads, per-interface sysctls, and
NetworkManager managed/autoconnect state in a root-only manifest. It installs a MAC-specific
NetworkManager exclusion under `/run`, moves the ports into `gn3-r1`, and writes
`/run/goodnews-lab-stage3/router.json`. Names and hardware MAC addresses stay the
same. All forwarding/sysctl changes are confined to the router namespace.

Mutable checksum, scatter/gather, and aggregation offloads are disabled and
checked individually. Fixed checksum features are recorded; an aggregation
feature fixed on is refused. Offloads and their dependency changes are restored
after the lab. Endpoint checksum offload can affect an endpoint-local capture;
use the router's captures to compare received and forwarded packets.

Inspect carrier and negotiated speed for each selected port:

```sh
sudo ip netns exec gn3-r1 ip -br link
sudo ip netns exec gn3-r1 ethtool enp1s0
sudo ip netns exec gn3-r1 ethtool enx001122334455
```

`lab3-check` checks namespace identity, kernel forwarding, port addresses, MTU,
offloads, and exclusion-file integrity. A cable can be unplugged during setup;
verify `Link detected: yes` before traffic tests.

## Start the router

In terminal 1:

```sh
sudo ip netns exec gn3-r1 ./bin/goodnews run --config /run/goodnews-lab-stage3/router.json
```

In terminal 2:

```sh
sudo ./bin/goodnews dashboard --config /run/goodnews-lab-stage3/router.json
sudo ./bin/goodnews show interfaces --socket /run/goodnews-lab-stage3/r1.sock
sudo ./bin/goodnews show arp --socket /run/goodnews-lab-stage3/r1.sock
sudo ./bin/goodnews show counters --socket /run/goodnews-lab-stage3/r1.sock
```

The printed artifact directory contains Ethernet pcaps, traces, counter
snapshots, and the run manifest. Save that directory name for acceptance.

## Configure a Linux endpoint

This example puts a Linux host on the **left**. For a right-side Linux host,
swap `10.0.0` and `10.0.1` in the address and route arguments.

On a Linux desktop with NetworkManager, identify an unused wired interface:

```sh
ip -br address
nmcli device status
nmcli connection show
```

Verify that the profile name `goodnews-stage3` is unused. Replace `enp1s0` with
the endpoint's device name. This creates a lab-owned profile with no default
route, no autoconnection, and no changes to Wi-Fi or other profiles:

```sh
sudo nmcli connection add type ethernet ifname enp1s0 con-name goodnews-stage3 connection.autoconnect no ipv4.method manual ipv4.addresses 10.0.0.2/24 ipv4.routes '10.0.1.0/24 10.0.0.1' ipv4.never-default yes ipv6.method disabled
sudo nmcli connection up goodnews-stage3
ip -4 route get 10.0.1.2
ping -n -c 3 10.0.0.1
ping -n -c 3 10.0.1.2
traceroute -I -n -q 1 -m 3 10.0.1.2
```

On Linux without NetworkManager, first save `ip address` and `ip route` output
and ensure the selected port is unmanaged and address-free. These are temporary
kernel settings; restore the original link administrative state after cleanup:

```sh
sudo ip link set enp1s0 up
sudo ip address add 10.0.0.2/24 dev enp1s0
sudo ip route add 10.0.1.0/24 via 10.0.0.1 dev enp1s0
```

If a local firewall blocks echo requests, add an inbound ICMP echo allowance
on the selected interface from the opposite lab subnet using that firewall's
existing ruleset. Keep the firewall enabled and record the rule for removal.

## Configure a Windows endpoint

This example puts Windows on the **left**. Use an elevated PowerShell window
and keep it open through cleanup. Identify an unused wired adapter; replace
`Ethernet` with its alias. Save the printed original DHCP state and configuration.

```powershell
Get-NetAdapter
$labAdapter = Get-NetAdapter -Name 'Ethernet'
$labIf = $labAdapter.ifIndex
$labDhcp = (Get-NetIPInterface -InterfaceIndex $labIf -AddressFamily IPv4).Dhcp
if ($labDhcp -ne 'Enabled') { throw 'Select an unused DHCP adapter; preserve existing static configuration' }
Get-NetIPConfiguration -InterfaceIndex $labIf | Format-List
Get-NetIPAddress -InterfaceIndex $labIf -AddressFamily IPv4
$labDhcp
```

Use an unused adapter originally configured for DHCP, with no static address
or default route to preserve. Refuse an existing lab firewall rule or route
instead of replacing it. Add an active-session address, a nonpersistent route,
and a scoped echo-request allowance:

```powershell
if (Get-NetFirewallRule -Name 'goodnews-stage3-icmp' -ErrorAction SilentlyContinue) { throw 'Lab firewall rule already exists' }
if (Get-NetRoute -DestinationPrefix '10.0.1.0/24' -ErrorAction SilentlyContinue) { throw 'Lab route already exists' }
New-NetIPAddress -InterfaceIndex $labIf -IPAddress '10.0.0.2' -PrefixLength 24 -PolicyStore ActiveStore
New-NetRoute -InterfaceIndex $labIf -DestinationPrefix '10.0.1.0/24' -NextHop '10.0.0.1' -PolicyStore ActiveStore
New-NetFirewallRule -Name 'goodnews-stage3-icmp' -DisplayName 'goodnews Stage 3 ICMP' -Direction Inbound -Action Allow -Protocol ICMPv4 -IcmpType 8 -InterfaceAlias $labAdapter.Name -LocalAddress '10.0.0.2' -RemoteAddress '10.0.1.0/24' -Profile Any
ping -n 3 10.0.0.1
ping -n 3 10.0.1.2
tracert -d -h 3 10.0.1.2
```

## Configure a macOS endpoint

This example puts the Mac on the **right**. Discover its Ethernet device and
record the original administrative state and addresses:

```sh
networksetup -listallhardwareports
ifconfig en5
netstat -rn -f inet
```

Replace `en5` with the dedicated Ethernet device. Verify that the lab address
and subnet route are absent. An address alias avoids editing a saved network
service or changing the default gateway:

```sh
sudo ifconfig en5 inet 10.0.1.2 netmask 255.255.255.0 alias up
sudo route -n add -net 10.0.0.0/24 10.0.1.1
route -n get 10.0.0.2
ping -n -c 3 10.0.1.1
ping -n -c 3 10.0.0.2
sudo traceroute -I -n -q 1 -m 3 10.0.0.2
```

If ping is blocked by macOS stealth mode or a managed firewall policy, inspect
the current firewall settings and obtain a permitted ICMP lab configuration.
Record and restore any setting changed for the experiment; the guide does not
disable the firewall. ICMP traceroute avoids needing UDP port allowances.

## Prove the physical router works

Save endpoint command output with operating system versions, adapter names,
driver/bus information, and the selected subnets. The completion evidence must
include the original Windows/macOS pairing and a run with a physical Linux
endpoint. A Linux namespace fixture is a separate automated regression result.

1. From each endpoint, ping its gateway and then the remote endpoint. Predict
   which MAC each ARP request should resolve, then inspect `goodnews show arp`.
2. Run the OS-specific traceroute above. Expect the local `.1` router address
   followed by the remote host.
3. Open both router pcaps in Wireshark with `arp || icmp`. Match the same echo
   request across the cables: unchanged IP endpoints and payload, TTL lower by
   one, a valid IPv4 checksum, and Ethernet addresses changed for the next link.
4. Stop goodnews with Ctrl-C. Repeat remote ping and require failure. Start it
   again and require recovery. This proves the kernel is not providing a bypass.
5. While pinging, unplug one cable and reconnect it. Require loss followed by
   recovery. Inspect link state, ARP, counters, and traces when diagnosing loss.
6. Stop the router and dashboard, perform cleanup, and verify restored settings
   and working management connectivity. Keep the lifecycle evidence with the
   packet captures and endpoint transcripts.

If only the gateway responds, inspect the remote route and endpoint firewall.
If the gateway fails, check carrier, endpoint addressing, MACs, and router state.
An unplugged USB adapter can reappear in the host namespace. Cleanup matches
hardware identity and retains its manifest if the adapter is missing or replaced;
reconnect the original adapter at its original bus location and rerun cleanup.

## Restore settings

On a Linux endpoint using the lab-owned NetworkManager profile:

```sh
sudo nmcli connection down goodnews-stage3
sudo nmcli connection delete goodnews-stage3
```

For the unmanaged Linux example, remove only the entries added above, then
restore the recorded original link state:

```sh
sudo ip route del 10.0.1.0/24 via 10.0.0.1 dev enp1s0
sudo ip address del 10.0.0.2/24 dev enp1s0
```

In the original Windows PowerShell session:

```powershell
Remove-NetFirewallRule -Name 'goodnews-stage3-icmp'
Remove-NetRoute -InterfaceIndex $labIf -DestinationPrefix '10.0.1.0/24' -NextHop '10.0.0.1' -Confirm:$false
Remove-NetIPAddress -InterfaceIndex $labIf -IPAddress '10.0.0.2' -Confirm:$false
Set-NetIPInterface -InterfaceIndex $labIf -AddressFamily IPv4 -Dhcp $labDhcp
```

On macOS, remove the alias and route, then restore the recorded original link
administrative state if it was down:

```sh
sudo route -n delete -net 10.0.0.0/24 10.0.1.1
sudo ifconfig en5 inet 10.0.1.2 -alias
```

Remove any additional lab firewall allowance on Linux. Stop all processes in
`gn3-r1`, unplug the endpoint cables, and restore Ubuntu's ports:

```sh
make lab3-down
ip -br link
nmcli device status
```

Cleanup returns both ports explicitly, restores their MTU, administrative state, sysctls and
offloads, removes only the owned NetworkManager exclusion, and restores the
previous managed and autoconnect states. A fresh `artifacts/stage3-lifecycle-*` directory records
the saved settings and restoration result. It does not claim physical acceptance.
Repeated cleanup is harmless. If cleanup reports incomplete recovery, keep
`/run/goodnews-lab-stage3` and follow its diagnostic; do not delete the namespace
or manifest to force success.
