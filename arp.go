package goodnews

import (
	"fmt"
	"net/netip"
	"sort"
	"sync"
	"time"
)

const (
	ARPCacheLifetime         = 60 * time.Second
	ARPRetryInterval         = time.Second
	ARPMaxAttempts           = 3
	ARPMaxNeighbors          = 64  // Includes resolved and incomplete entries, per port.
	ARPMaxPending            = 256 // Per port, at most sixteen for any one next hop.
	ARPMaxPendingPerNeighbor = 16
)

type waitingPacket struct {
	Output   Output
	ParentID uint64
}

type arpEntry struct {
	MAC      MAC
	Expires  time.Time
	Retry    time.Time
	Attempts int
	Waiting  []waitingPacket
}

type ARPEntryView struct {
	IP       netip.Addr `json:"ip"`
	MAC      string     `json:"mac,omitempty"`
	State    string     `json:"state"`
	Expires  time.Time  `json:"expires,omitempty"`
	Attempts int        `json:"attempts,omitempty"`
	Pending  int        `json:"pending"`
}

// ARPCache implements soft state: resolved mappings age out, while incomplete
// mappings retry a bounded number of times. A mutex permits management reads;
// only the runtime's forwarding worker changes entries.
type ARPCache struct {
	mu      sync.Mutex
	entries map[netip.Addr]*arpEntry
	pending int
}

func newARPCache() *ARPCache { return &ARPCache{entries: map[netip.Addr]*arpEntry{}} }

func (a *ARPCache) expire(now time.Time) {
	for ip, entry := range a.entries {
		if entry.MAC.Unicast() && !now.Before(entry.Expires) {
			delete(a.entries, ip)
		}
	}
}

// resolve returns either a cached MAC or a queued packet. request tells the
// caller to broadcast the first ARP request; later packets share that request.
func (a *ARPCache) resolve(packet waitingPacket, now time.Time) (mac MAC, request bool, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.expire(now)
	ip := packet.Output.NextHop
	entry := a.entries[ip]
	if entry != nil && entry.MAC.Unicast() {
		return entry.MAC, false, nil
	}
	if a.pending >= ARPMaxPending || entry != nil && len(entry.Waiting) >= ARPMaxPendingPerNeighbor {
		return MAC{}, false, fmt.Errorf("arp_queue_full")
	}
	if entry == nil {
		if len(a.entries) >= ARPMaxNeighbors {
			return MAC{}, false, fmt.Errorf("arp_table_full")
		}
		entry = &arpEntry{Attempts: 1, Retry: now.Add(ARPRetryInterval)}
		a.entries[ip] = entry
		request = true
	}
	packet.Output.Packet = append([]byte(nil), packet.Output.Packet...)
	entry.Waiting = append(entry.Waiting, packet)
	a.pending++
	return MAC{}, request, nil
}

func (a *ARPCache) learn(ip netip.Addr, mac MAC, now time.Time) []waitingPacket {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.expire(now)
	entry := a.entries[ip]
	if entry == nil {
		if len(a.entries) >= ARPMaxNeighbors {
			return nil
		}
		entry = &arpEntry{}
		a.entries[ip] = entry
	}
	waiting := entry.Waiting
	a.pending -= len(waiting)
	*entry = arpEntry{MAC: mac, Expires: now.Add(ARPCacheLifetime)}
	return waiting
}

func (a *ARPCache) awaiting(ip netip.Addr) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	e := a.entries[ip]
	return e != nil && len(e.Waiting) != 0
}

func (a *ARPCache) drain() []waitingPacket {
	a.mu.Lock()
	defer a.mu.Unlock()
	var waiting []waitingPacket
	for _, entry := range a.entries {
		waiting = append(waiting, entry.Waiting...)
	}
	a.entries = map[netip.Addr]*arpEntry{}
	a.pending = 0
	return waiting
}

func (a *ARPCache) tick(now time.Time) (requests []netip.Addr, expired []waitingPacket) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.expire(now)
	for ip, entry := range a.entries {
		if entry.MAC.Unicast() || now.Before(entry.Retry) {
			continue
		}
		if entry.Attempts >= ARPMaxAttempts {
			expired = append(expired, entry.Waiting...)
			a.pending -= len(entry.Waiting)
			delete(a.entries, ip)
			continue
		}
		entry.Attempts++
		entry.Retry = now.Add(ARPRetryInterval)
		requests = append(requests, ip)
	}
	return
}

func (a *ARPCache) snapshot(now time.Time) []ARPEntryView {
	a.mu.Lock()
	defer a.mu.Unlock()
	views := []ARPEntryView{}
	for ip, entry := range a.entries {
		v := ARPEntryView{IP: ip, State: "incomplete", Pending: len(entry.Waiting), Attempts: entry.Attempts}
		if entry.MAC.Unicast() {
			if !now.Before(entry.Expires) {
				continue
			}
			v.MAC, v.State, v.Expires, v.Attempts = entry.MAC.String(), "reachable", entry.Expires, 0
		}
		views = append(views, v)
	}
	sort.Slice(views, func(i, j int) bool { return views[i].IP.Less(views[j].IP) })
	return views
}
