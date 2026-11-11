package goodnews

import (
	"net/netip"
	"testing"
	"time"
)

func addr(s string) netip.Addr { return netip.MustParseAddr(s) }
func pending(ip string) waitingPacket {
	return waitingPacket{Output: Output{NextHop: addr(ip), Packet: []byte{1, 2, 3}}, ParentID: 42}
}

func TestARPRetriesExpiryAndQueueBounds(t *testing.T) {
	now := time.Unix(100, 0)
	a := newARPCache()
	p := pending("10.0.0.2")
	for i := 0; i < ARPMaxPendingPerNeighbor; i++ {
		mac, request, err := a.resolve(p, now)
		if err != nil || mac.Unicast() || request != (i == 0) {
			t.Fatalf("request %d: %v %v %v", i, mac, request, err)
		}
	}
	if _, _, err := a.resolve(p, now); err == nil || err.Error() != "arp_queue_full" {
		t.Fatal("unbounded neighbor queue")
	}
	for i := 1; i < ARPMaxAttempts; i++ {
		requests, expired := a.tick(now.Add(time.Duration(i) * time.Second))
		if len(requests) != 1 || requests[0] != p.Output.NextHop || len(expired) != 0 {
			t.Fatalf("retry %d", i)
		}
	}
	requests, expired := a.tick(now.Add(ARPMaxAttempts * time.Second))
	if len(requests) != 0 || len(expired) != ARPMaxPendingPerNeighbor || a.pending != 0 || len(a.snapshot(now)) != 0 {
		t.Fatal("timeout did not release all queued packets")
	}
	mac := MAC{2, 0, 0, 0, 0, 2}
	_, _, _ = a.resolve(p, now)
	p.Output.Packet[0] = 99
	waiting := a.learn(p.Output.NextHop, mac, now)
	if len(waiting) != 1 || waiting[0].Output.Packet[0] != 1 || waiting[0].ParentID != 42 {
		t.Fatal("queue aliases packet or loses correlation")
	}
	got, request, err := a.resolve(p, now.Add(ARPCacheLifetime-time.Nanosecond))
	if err != nil || got != mac || request {
		t.Fatal("cache miss before expiry")
	}
	got, request, err = a.resolve(p, now.Add(ARPCacheLifetime))
	if err != nil || got.Unicast() || !request {
		t.Fatal("expired entry was reused")
	}
}

func TestARPGlobalBounds(t *testing.T) {
	now := time.Now()
	a := newARPCache()
	for i := 0; i < ARPMaxPending; i++ {
		p := pending("10.0.0.2")
		p.Output.NextHop = netip.AddrFrom4([4]byte{10, 0, byte(i / 16), 2})
		if _, _, err := a.resolve(p, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := a.resolve(pending("10.0.1.99"), now); err == nil {
		t.Fatal("unbounded total pending")
	}
	a = newARPCache()
	for i := 0; i < ARPMaxNeighbors; i++ {
		p := pending("10.0.0.2")
		p.Output.NextHop = netip.AddrFrom4([4]byte{10, 0, 0, byte(i + 2)})
		_, _, _ = a.resolve(p, now)
	}
	if _, _, err := a.resolve(pending("10.0.0.200"), now); err == nil || err.Error() != "arp_table_full" {
		t.Fatal("unbounded neighbor table")
	}
}
