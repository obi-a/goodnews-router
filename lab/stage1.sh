#!/usr/bin/env bash
# Stage 1 uses only owned namespaces. It never changes the host's forwarding,
# addresses, routes, NetworkManager settings, or firewall.
set -euo pipefail
state=/run/goodnews-lab-stage1

die() { echo "lab: $*" >&2; exit 1; }
[[ $EUID -eq 0 ]] || die "run with sudo"
command -v ip >/dev/null || die "install iproute2"

check_owner() {
    [[ -d $state && ! -L $state && -f $state/namespaces ]] || die "no owned lab state at $state"
    [[ $(stat -c %u "$state") == 0 && $(stat -c %a "$state") == 700 ]] || die "unsafe lab state ownership or permissions"
}

down() {
    [[ -e $state ]] || return 0
    check_owner
    # Refuse to strand a running router or interrupt somebody's namespace shell.
    while read -r name inode; do
        case "$name" in gn-h1|gn-r1|gn-h2) ;; *) die "unexpected namespace in ownership manifest" ;; esac
        if [[ -e /run/netns/$name ]]; then
            [[ $(stat -Lc %i "/run/netns/$name") == "$inode" ]] || die "namespace $name was replaced; refusing cleanup"
            [[ -z $(ip netns pids "$name") ]] || die "processes still use $name; stop them before cleanup"
        fi
    done < "$state/namespaces"
    while read -r name inode; do
        if [[ -e /run/netns/$name ]]; then ip netns delete "$name"; fi
    done < "$state/namespaces"
    # Only these known files are created by this script/router. No recursive rm.
    if [[ -S $state/r1.sock ]]; then unlink "$state/r1.sock"; fi
    unlink "$state/namespaces"
    rmdir "$state"
}

disable_offloads() {
    local ns=$1 feature description settings
    # Disable each mutable offload separately; never hide an ethtool failure.
    settings=$(ip netns exec "$ns" ethtool -k gn0)
    while read -r feature description; do
        if [[ $description == on* && $description != *'[fixed]'* ]]; then
            case "$feature" in
                rx-checksumming:|tx-checksumming:|scatter-gather:|tcp-segmentation-offload:|generic-segmentation-offload:|generic-receive-offload:|large-receive-offload:)
                    ip netns exec "$ns" ethtool -K gn0 "${feature%:}" off
                    ;;
            esac
        fi
    done <<< "$settings"
}

up() {
    [[ -c /dev/net/tun ]] || die "/dev/net/tun is unavailable; enable the tun module and expose the device on the Ubuntu lab host"
    command -v ethtool >/dev/null || die "install ethtool"
    command -v sysctl >/dev/null || die "install procps"
    [[ ! -e $state ]] || die "$state already exists; inspect the lab or run down first"
    for name in gn-h1 gn-r1 gn-h2; do
        [[ ! -e /run/netns/$name ]] || die "namespace $name already exists; refusing to adopt it"
    done
    mkdir -m 700 "$state"
    : > "$state/namespaces"
    # EXIT also runs for an explicit diagnostic exit in a nested setup helper.
    trap 'result=$?; trap - EXIT; if (( result != 0 )); then echo "lab setup failed; cleaning owned namespaces" >&2; down; fi; exit "$result"' EXIT
    for name in gn-h1 gn-r1 gn-h2; do
        ip netns add "$name"
        printf '%s %s\n' "$name" "$(stat -Lc %i "/run/netns/$name")" >> "$state/namespaces"
        ip -n "$name" link set lo up
        ip netns exec "$name" sysctl -qw net.ipv4.ip_forward=0
        ip netns exec "$name" sysctl -qw net.ipv4.conf.all.rp_filter=0 net.ipv4.conf.default.rp_filter=0
        ip netns exec "$name" sysctl -qw net.ipv4.conf.all.send_redirects=0 net.ipv4.conf.all.accept_redirects=0
        ip netns exec "$name" sysctl -qw net.ipv6.conf.all.disable_ipv6=1
    done
    for side in 0 1; do
        name=gn-h$((side+1))
        ip netns exec "$name" ip tuntap add dev gn0 mode tun
        ip -n "$name" address add "10.0.$side.2/24" dev gn0
        ip -n "$name" link set gn0 mtu 1500 up
        ip -n "$name" route add default via "10.0.$side.1" dev gn0
        ip netns exec "$name" sysctl -qw net.ipv4.conf.gn0.rp_filter=0
        disable_offloads "$name"
    done
    check
    trap - EXIT
}

check() {
    check_owner
    for name in gn-h1 gn-r1 gn-h2; do
        [[ -e /run/netns/$name ]] || die "missing namespace $name"
        [[ $(ip netns exec "$name" sysctl -n net.ipv4.ip_forward) == 0 ]] || die "kernel forwarding enabled in $name"
    done
    [[ -z $(ip -n gn-r1 -4 -o address show scope global) ]] || die "router namespace has kernel IPv4 addresses"
    for side in 0 1; do
        name=gn-h$((side+1))
        ip -n "$name" -4 address show dev gn0 | grep -q "10.0.$side.2/24" || die "wrong address in $name"
    done
    echo "lab ready: gn-h1 (10.0.0.2) <-> goodnews in gn-r1 <-> gn-h2 (10.0.1.2)"
}

case "${1:-}" in up) up ;; down) down ;; check) check ;; *) die "usage: $0 up|check|down" ;; esac
