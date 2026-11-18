package goodnews

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type graphLink struct {
	a, b string
	cost uint32
}

var textbookLinks = []graphLink{{"u", "v", 2}, {"u", "x", 1}, {"u", "w", 5}, {"v", "x", 2}, {"v", "w", 3}, {"x", "y", 1}, {"x", "w", 3}, {"y", "w", 1}, {"y", "z", 2}, {"w", "z", 5}}
var graphIDs = []string{"u", "v", "w", "x", "y", "z"}

func graphConfigs(algorithm string) map[string]Config {
	configs := map[string]Config{}
	for i, id := range graphIDs {
		configs[id] = Config{Version: 1, RouterID: id, Interfaces: []Interface{{Name: "lan", Device: "lan", Address: fmt.Sprintf("10.45.%d.1/24", i), MTU: 1500, Driver: "ethernet"}}, Observability: ObservationConfig{Directory: "artifacts", Socket: "/tmp/gn4-" + id + ".sock"}, Routing: &RoutingConfig{Algorithm: algorithm}}
	}
	for i, edge := range textbookLinks {
		for side, id := range []string{edge.a, edge.b} {
			peer := edge.b
			if side == 1 {
				peer = edge.a
			}
			c := configs[id]
			name := "to-" + peer
			c.Interfaces = append(c.Interfaces, Interface{Name: name, Device: name, Address: fmt.Sprintf("10.44.0.%d/30", i*4+side+1), MTU: 1500, Driver: "ethernet"})
			c.Routing.Links = append(c.Routing.Links, RoutingLink{Interface: name, Peer: fmt.Sprintf("10.44.0.%d", i*4+2-side), Cost: edge.cost})
			configs[id] = c
		}
	}
	return configs
}

type routingSimulation struct {
	routers map[string]*Routing
	configs map[string]Config
	impair  map[string]map[string]Impairment
	stopped map[string]bool
	now     time.Time
	epoch   uint64
	events  []RoutingEvent
}

func newRoutingSimulation(t *testing.T, algorithm string) *routingSimulation {
	t.Helper()
	s := &routingSimulation{routers: map[string]*Routing{}, configs: graphConfigs(algorithm), impair: map[string]map[string]Impairment{}, stopped: map[string]bool{}, now: time.Now(), epoch: 100}
	for id, c := range s.configs {
		r, err := NewRouting(c, s.epoch)
		if err != nil {
			t.Fatal(err)
		}
		s.routers[id] = r
		s.impair[id] = map[string]Impairment{}
	}
	return s
}

func (s *routingSimulation) run(t *testing.T, duration time.Duration) {
	t.Helper()
	for end := s.now.Add(duration); s.now.Before(end); s.now = s.now.Add(20 * time.Millisecond) {
		type packet struct {
			from, to string
			message  RoutingMessage
		}
		packets := []packet{}
		for _, id := range graphIDs {
			if s.stopped[id] {
				continue
			}
			r := s.routers[id]
			r.Tick(s.now, s.impair[id])
			batch := r.Drain(s.now)
			s.events = append(s.events, batch.Events...)
			for _, tx := range batch.Transmissions {
				packets = append(packets, packet{id, strings.TrimPrefix(tx.Interface, "to-"), tx.Message})
			}
		}
		for _, p := range packets {
			if s.stopped[p.to] {
				continue
			}
			wire, err := EncodeRouting(p.message)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := DecodeRouting(wire)
			if err != nil {
				t.Fatal(err)
			}
			_ = s.routers[p.to].Receive("to-"+p.from, decoded, s.now)
		}
	}
}

func (s *routingSimulation) route(id, prefix string) (Route, bool) {
	for _, r := range s.routers[id].routes {
		if r.Prefix.String() == prefix {
			return r, true
		}
	}
	return Route{}, false
}

func TestRoutingTextbookAndRecovery(t *testing.T) {
	for _, algorithm := range []string{"dv", "ls"} {
		t.Run(algorithm, func(t *testing.T) {
			s := newRoutingSimulation(t, algorithm)
			s.run(t, 5*time.Second)
			check := func() {
				t.Helper()
				for id, want := range map[string]uint32{"v": 2, "x": 1, "y": 2, "w": 3, "z": 4} {
					index := 0
					for graphIDs[index] != id {
						index++
					}
					r, ok := s.route("u", fmt.Sprintf("10.45.%d.0/24", index))
					hop := "to-x"
					if id == "v" {
						hop = "to-v"
					}
					if !ok || r.Metric != want || r.Interface != hop {
						t.Fatalf("u->%s: %+v, want cost %d via %s", id, r, want, hop)
					}
				}
			}
			check()
			// Directed cost changes alter the first hop without restarting a router.
			s.impair["u"]["to-x"] = Impairment{Cost: 60}
			s.run(t, 4*time.Second)
			r, ok := s.route("u", "10.45.5.0/24")
			if !ok || r.Metric != 7 || r.Interface != "to-v" {
				t.Fatalf("cost change: %+v", r)
			}
			s.impair["u"]["to-x"] = Impairment{}
			s.run(t, 4*time.Second)
			check()
			// A stopped neighbor expires and the surviving graph routes around it.
			s.stopped["x"] = true
			s.run(t, 9*time.Second)
			r, ok = s.route("u", "10.45.5.0/24")
			if !ok || r.Metric != 8 || r.Interface != "to-v" {
				t.Fatalf("router failure: %+v", r)
			}
			if _, ok := s.route("u", "10.45.3.0/24"); ok {
				t.Fatal("retained stopped router LAN")
			}
			// Restart while its old advertisements/tombstones still exist.
			s.epoch++
			fresh, err := NewRouting(s.configs["x"], s.epoch)
			if err != nil {
				t.Fatal(err)
			}
			s.routers["x"] = fresh
			s.stopped["x"] = false
			s.run(t, 6*time.Second)
			check()
			if algorithm == "ls" {
				var baseline []RoutingMessage
				for _, id := range graphIDs {
					db := s.routers[id].Database()
					if len(db) != 6 {
						t.Fatalf("%s LSDB has %d records", id, len(db))
					}
					if baseline == nil {
						baseline = db
					} else {
						// Refreshes in flight can differ in sequence, but topology must agree.
						for i := range db {
							if db[i].Origin != baseline[i].Origin || !reflect.DeepEqual(db[i].Edges, baseline[i].Edges) || !reflect.DeepEqual(db[i].Prefixes, baseline[i].Prefixes) {
								t.Fatal("LSDB topology disagreement")
							}
						}
					}
				}
			}
			// Cut z completely; no stale route is allowed to survive.
			s.impair["z"]["to-y"] = Impairment{Down: true}
			s.impair["z"]["to-w"] = Impairment{Down: true}
			s.run(t, 20*time.Second)
			if _, ok := s.route("u", "10.45.5.0/24"); ok {
				t.Fatal("partition retained a route")
			}
		})
	}
}

func TestRoutingHandWorkedGraphs(t *testing.T) {
	for _, algorithm := range []string{"dv", "ls"} {
		for _, tc := range []struct {
			name   string
			edges  []RoutingEdge
			vector []RoutingPrefix
			want   uint32
			via    string
		}{
			{"chain", []RoutingEdge{{"a", 2}}, []RoutingPrefix{{Prefix: netip.MustParsePrefix("192.0.2.0/24"), Metric: 3}}, 5, "a"},
			{"tie", []RoutingEdge{{"b", 2}, {"a", 2}}, []RoutingPrefix{{Prefix: netip.MustParsePrefix("192.0.2.0/24")}}, 2, "a"},
			{"disconnected", nil, nil, 0, ""},
		} {
			t.Run(algorithm+"/"+tc.name, func(t *testing.T) {
				in := RoutingInput{RouterID: "self"}
				local := RoutingMessage{Origin: "self", Edges: tc.edges}
				for i, e := range tc.edges {
					in.Neighbors = append(in.Neighbors, AlgorithmNeighbor{Router: e.Router, Interface: e.Router, Peer: netip.AddrFrom4([4]byte{10, 0, 0, byte(i + 2)}), Cost: e.Cost, Vector: tc.vector})
					m := RoutingMessage{Origin: e.Router, Edges: []RoutingEdge{{"self", e.Cost}}}
					if tc.name == "chain" {
						m.Edges = append(m.Edges, RoutingEdge{"c", 3})
						in.Database = append(in.Database, RoutingMessage{Origin: "c", Edges: []RoutingEdge{{e.Router, 3}}, Prefixes: []RoutingPrefix{{Prefix: netip.MustParsePrefix("192.0.2.0/24")}}})
					} else {
						m.Prefixes = tc.vector
					}
					in.Database = append(in.Database, m)
				}
				in.Database = append(in.Database, local)
				result := routingAlgorithms[algorithm].Calculate(in)
				if tc.via == "" {
					if len(result.Routes) != 0 {
						t.Fatal(result.Routes)
					}
					return
				}
				if len(result.Routes) != 1 || result.Routes[0].Metric != tc.want || result.Routes[0].Interface != tc.via {
					t.Fatalf("%+v", result.Routes)
				}
			})
		}
	}
	if routingSum(65534, 10) != RoutingInfinity || routingSum(RoutingInfinity, 1) != RoutingInfinity {
		t.Fatal("metric overflow")
	}
}

func TestRoutingWireAndValidation(t *testing.T) {
	for _, m := range []RoutingMessage{
		{Type: RoutingHello, Origin: "u", Epoch: 1, Sequence: 2, Algorithm: "dv", Seen: "x", SeenEpoch: 3},
		{Type: RoutingDV, Origin: "x", Epoch: 3, Sequence: 4, Prefixes: []RoutingPrefix{{Prefix: netip.MustParsePrefix("192.0.2.0/24"), Metric: RoutingInfinity}}},
		{Type: RoutingLSA, Origin: "x", Epoch: 3, Sequence: 5, CreatedMS: 1000, Edges: []RoutingEdge{{"u", 1}}, Prefixes: []RoutingPrefix{{Prefix: netip.MustParsePrefix("192.0.2.0/24")}}},
	} {
		wire, err := EncodeRouting(m)
		if err != nil {
			t.Fatal(err)
		}
		got, err := DecodeRouting(wire)
		if err != nil || !reflect.DeepEqual(got, m) {
			t.Fatalf("%+v %v", got, err)
		}
		for i := range wire {
			corrupt := append([]byte(nil), wire...)
			corrupt[i] ^= 1
			if _, err := DecodeRouting(corrupt); err == nil {
				t.Fatalf("accepted corruption at %d", i)
			}
		}
		for i := range wire {
			if _, err := DecodeRouting(wire[:i]); err == nil {
				t.Fatal("accepted truncation")
			}
		}
	}
	bad := RoutingMessage{Type: RoutingDV, Origin: "u", Epoch: 1, Sequence: 1, Prefixes: []RoutingPrefix{{Prefix: netip.MustParsePrefix("192.0.2.1/24")}}}
	if _, err := EncodeRouting(bad); err == nil {
		t.Fatal("accepted noncanonical prefix")
	}
	c := graphConfigs("dv")["u"]
	for _, mutate := range []func(*Config){
		func(c *Config) { c.Routing.Algorithm = "bogus" }, func(c *Config) { c.Routing.DeadMS = 1 }, func(c *Config) { c.Routing.Links[0].Cost = 0 }, func(c *Config) { c.Routing.Links[0].Peer = "192.0.2.4" }, func(c *Config) { c.Routing.Links[0].Interface = "lan-missing" }, func(c *Config) { c.RouterID = "too-long-id" }, func(c *Config) { c.Routing.Algorithm = "ls"; b := true; c.Routing.PoisonedReverse = &b },
	} {
		clone := c
		options := *c.Routing
		options.Links = append([]RoutingLink(nil), c.Routing.Links...)
		clone.Routing = &options
		mutate(&clone)
		if err := clone.Validate(); err == nil {
			t.Fatal("accepted bad routing config")
		}
	}
}

func TestRoutingNeighborAndFreshness(t *testing.T) {
	s := newRoutingSimulation(t, "dv")
	r := s.routers["u"]
	now := s.now
	m := RoutingMessage{Type: RoutingHello, Origin: "x", Epoch: 100, Sequence: 1, Algorithm: "dv"}
	if err := r.Receive("to-x", m, now); err != nil {
		t.Fatal(err)
	}
	if r.Neighbors(now)[2].State == "up" {
		t.Fatal("one-way hello established adjacency")
	}
	m.Sequence = 2
	m.Seen = "u"
	m.SeenEpoch = 100
	if err := r.Receive("to-x", m, now); err != nil {
		t.Fatal(err)
	}
	if err := r.Receive("to-x", m, now.Add(time.Second)); err == nil {
		t.Fatal("stale hello refreshed timer")
	}
	v := RoutingMessage{Type: RoutingDV, Origin: "x", Epoch: 100, Sequence: 3, Prefixes: []RoutingPrefix{{Prefix: netip.MustParsePrefix("192.0.2.0/24"), Metric: 2}}}
	if err := r.Receive("to-x", v, now); err != nil {
		t.Fatal(err)
	}
	r.Tick(now, nil)
	r.Drain(now)
	if len(r.routes) == 0 {
		t.Fatal("vector not installed")
	}
	if err := r.Receive("to-x", v, now); err == nil {
		t.Fatal("accepted stale vector")
	}
	// Hellos stay alive while vector ages out.
	for i := 1; i <= 7; i++ {
		m.Sequence += 10
		if err := r.Receive("to-x", m, now.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
		r.Tick(now.Add(time.Duration(i)*time.Second), nil)
		r.Drain(now)
	}
	if len(r.routes) != 0 {
		t.Fatal("vector survived max age")
	}
	r.Tick(now.Add(10*time.Second), nil)
	if r.neighbors["to-x"].up {
		t.Fatal("neighbor survived dead timer")
	}
	wrong := m
	wrong.Algorithm = "ls"
	if err := r.Receive("to-x", wrong, now); err == nil {
		t.Fatal("accepted mismatched algorithm")
	}
	// Maximum LS prefix count, stale age and tombstones are enforced.
	ls := newRoutingSimulation(t, "ls")
	ls.run(t, 3*time.Second)
	receiver := ls.routers["u"]
	old := ls.routers["x"].Database()[0]
	old.Origin = "new"
	old.Epoch = 1
	old.Sequence = 1
	old.CreatedMS = ls.now.Add(-7 * time.Second).UnixMilli()
	if err := receiver.Receive("to-x", old, ls.now); err == nil {
		t.Fatal("accepted expired LSA")
	}
	old.CreatedMS = ls.now.UnixMilli()
	old.Prefixes = nil
	old.Edges = nil
	if err := receiver.Receive("to-x", old, ls.now); err != nil {
		t.Fatal(err)
	}
	at := ls.now.Add(7 * time.Second)
	n := receiver.neighbors["to-x"]
	keepAlive := RoutingMessage{Type: RoutingHello, Origin: n.id, Epoch: n.epoch, Sequence: n.helloSequence + 1, Algorithm: "ls", Seen: "u", SeenEpoch: receiver.epoch}
	if err := receiver.Receive("to-x", keepAlive, at); err != nil {
		t.Fatal(err)
	}
	receiver.Tick(at, nil)
	old.CreatedMS = at.UnixMilli()
	if err := receiver.Receive("to-x", old, at); err == nil || err.Error() != "routing_stale_lsa" {
		t.Fatal("resurrected old LSA", err)
	}
	tooMany := old
	tooMany.Origin = "limit"
	tooMany.Sequence++
	for i := range 16 {
		tooMany.Edges = append(tooMany.Edges, RoutingEdge{Router: fmt.Sprintf("r%d", i), Cost: 1})
	}
	if err := receiver.Receive("to-x", tooMany, at); err == nil || err.Error() != "routing_state_limit" {
		t.Fatal("accepted excessive identities", err)
	}
	dv := newRoutingSimulation(t, "dv")
	dv.run(t, time.Second)
	tooLarge := RoutingMessage{Type: RoutingDV, Origin: "x", Epoch: 100, Sequence: 100000}
	for i := range 64 {
		tooLarge.Prefixes = append(tooLarge.Prefixes, RoutingPrefix{Prefix: netip.PrefixFrom(netip.AddrFrom4([4]byte{192, 0, 2, byte(i)}), 32)})
	}
	if err := dv.routers["u"].Receive("to-x", tooLarge, dv.now); err == nil || err.Error() != "routing_state_limit" {
		t.Fatal("accepted excessive prefixes", err)
	}
}

func TestRoutingIncarnationPersistenceAndSnapshots(t *testing.T) {
	path := filepath.Join(t.TempDir(), "epoch")
	a, err := nextRoutingEpoch(path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := nextRoutingEpoch(path)
	if err != nil || b <= a {
		t.Fatal(a, b, err)
	}
	future := uint64(time.Now().Add(time.Hour).UnixNano())
	if err := os.WriteFile(path, []byte(fmt.Sprint(future)), 0600); err != nil {
		t.Fatal(err)
	}
	b, err = nextRoutingEpoch(path)
	if err != nil || b != future+1 {
		t.Fatal("clock rollback", b, err)
	}
	s := newRoutingSimulation(t, "ls")
	s.run(t, time.Second)
	r := s.routers["u"]
	var workers sync.WaitGroup
	for range 3 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for range 100 {
				r.Neighbors(s.now)
				db := r.Database()
				if len(db) > 0 {
					db[0].Prefixes = nil
				}
			}
		}()
	}
	for range 100 {
		r.Tick(s.now, nil)
		r.Drain(s.now)
	}
	workers.Wait()
}

func TestRoutingPoisonedReverseSwitch(t *testing.T) {
	for _, poison := range []bool{true, false} {
		s := newRoutingSimulation(t, "dv")
		for _, r := range s.routers {
			value := poison
			r.options.PoisonedReverse = &value
		}
		s.run(t, 3*time.Second)
		u := s.routers["u"]
		u.Tick(s.now.Add(time.Second), nil)
		batch := u.Drain(s.now)
		found := false
		for _, tx := range batch.Transmissions {
			if tx.Interface == "to-x" && tx.Message.Type == RoutingDV {
				for _, p := range tx.Message.Prefixes {
					if p.Prefix.String() == "10.45.5.0/24" {
						found = true
						want := uint32(4)
						if poison {
							want = RoutingInfinity
						}
						if p.Metric != want {
							t.Fatalf("poisoned_reverse=%v advertised %d, want %d", poison, p.Metric, want)
						}
					}
				}
			}
		}
		if !found {
			t.Fatal("missing complete vector")
		}
	}
}

func FuzzDecodeRouting(f *testing.F) {
	b, _ := EncodeRouting(RoutingMessage{Type: RoutingHello, Origin: "u", Epoch: 1, Sequence: 1, Algorithm: "ls"})
	f.Add(b)
	f.Add([]byte("GN"))
	f.Fuzz(func(t *testing.T, b []byte) {
		m, err := DecodeRouting(b)
		if err != nil {
			return
		}
		wire, err := EncodeRouting(m)
		if err != nil || string(wire) != string(b) {
			t.Fatal("noncanonical accepted message")
		}
		if binary.BigEndian.Uint16(wire[4:6]) != uint16(len(wire)) {
			t.Fatal("length mismatch")
		}
	})
}
