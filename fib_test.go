package goodnews

import (
	"net/netip"
	"sync"
	"testing"
)

func TestLongestPrefixMatch(t *testing.T) {
	routes := []Route{}
	for _, p := range []string{"0.0.0.0/0", "10.0.0.0/8", "10.1.0.0/16", "10.1.2.0/24", "10.1.2.3/32"} {
		routes = append(routes, Route{Prefix: netip.MustParsePrefix(p), Interface: "left", Origin: "static"})
	}
	table, err := NewTable(routes, 7)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		dst, want string
		count     int
	}{
		{"192.0.2.1", "0.0.0.0/0", 1}, {"10.9.0.1", "10.0.0.0/8", 2},
		{"10.1.9.1", "10.1.0.0/16", 3}, {"10.1.2.9", "10.1.2.0/24", 4}, {"10.1.2.3", "10.1.2.3/32", 5},
	} {
		route, matches, ok := table.Lookup(netip.MustParseAddr(tc.dst))
		if !ok || route.Prefix.String() != tc.want || len(matches) != tc.count {
			t.Fatalf("%s: got %v, %v", tc.dst, route, matches)
		}
	}
	// Mutating caller-owned input or returned slices cannot mutate a live table.
	routes[0].Interface = "corrupt"
	copyOfRoutes := table.Routes()
	copyOfRoutes[0].Interface = "corrupt"
	_, matches, _ := table.Lookup(netip.MustParseAddr("10.1.2.3"))
	matches[0].Interface = "corrupt"
	if table.Routes()[0].Interface != "left" {
		t.Fatal("table exposes mutable backing storage")
	}
	if _, err := NewTable(append(routes, routes[0]), 8); err == nil {
		t.Fatal("duplicate prefix accepted")
	}
}

func TestFIBConcurrentReplacement(t *testing.T) {
	engine, _ := NewEngine(testConfig())
	packet := testPacket(t, "10.0.0.2", "10.0.1.2", 1, 64, testEcho())
	var workers sync.WaitGroup
	for range 4 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for range 250 {
				d := engine.Process("left", packet)
				if d.Trace.FIBVersion == 0 || len(d.Outputs) != 1 || d.Outputs[0].Interface != "right" {
					t.Error("inconsistent snapshot")
					return
				}
			}
		}()
	}
	for i := uint64(2); i < 100; i++ {
		table, _ := BuildTable(testConfig(), i)
		engine.FIB.Store(table)
	}
	workers.Wait()
}
