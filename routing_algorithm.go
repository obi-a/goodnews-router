package goodnews

import (
	"net/netip"
	"sort"
	"time"
)

// RoutingAlgorithm is registered at build time. It never opens a socket or
// changes the live FIB; the runtime publishes its complete calculation.
type RoutingAlgorithm interface {
	Calculate(RoutingInput) RoutingCalculation
}

type AlgorithmNeighbor struct {
	Router    string
	Interface string
	Peer      netip.Addr
	Cost      uint32
	Vector    []RoutingPrefix
}

type RoutingInput struct {
	RouterID  string
	Local     []RoutingPrefix
	Neighbors []AlgorithmNeighbor
	Database  []RoutingMessage
}

type RoutingCalculation struct {
	Routes []Route
	Events []RoutingEvent
}

type RoutingEvent struct {
	Timestamp      time.Time         `json:"timestamp"`
	RouterID       string            `json:"router_id"`
	Algorithm      string            `json:"algorithm"`
	Event          string            `json:"event"`
	Interface      string            `json:"interface,omitempty"`
	Peer           string            `json:"peer,omitempty"`
	Reason         string            `json:"reason,omitempty"`
	Prefix         *netip.Prefix     `json:"prefix,omitempty"`
	Cost           uint32            `json:"cost,omitempty"`
	Selected       bool              `json:"selected,omitempty"`
	PreviousCost   *uint32           `json:"previous_cost,omitempty"`
	LinkCost       uint32            `json:"link_cost,omitempty"`
	AdvertisedCost uint32            `json:"advertised_cost,omitempty"`
	Settled        []string          `json:"settled,omitempty"`
	Distances      map[string]uint32 `json:"distances,omitempty"`
	Predecessors   map[string]string `json:"predecessors,omitempty"`
}

type distanceVector struct{}
type linkState struct{}

var routingAlgorithms = map[string]RoutingAlgorithm{"dv": distanceVector{}, "ls": linkState{}}

func routingSum(a, b uint32) uint32 {
	if a >= RoutingInfinity || b >= RoutingInfinity || a+b >= RoutingInfinity {
		return RoutingInfinity
	}
	return a + b
}

func sortedNeighbors(input []AlgorithmNeighbor) []AlgorithmNeighbor {
	n := append([]AlgorithmNeighbor(nil), input...)
	sort.Slice(n, func(i, j int) bool {
		if n[i].Router != n[j].Router {
			return n[i].Router < n[j].Router
		}
		return n[i].Interface < n[j].Interface
	})
	return n
}

func (distanceVector) Calculate(in RoutingInput) RoutingCalculation {
	result := RoutingCalculation{}
	local := map[netip.Prefix]bool{}
	for _, p := range in.Local {
		local[p.Prefix] = true
	}
	best := map[netip.Prefix]Route{}
	for _, n := range sortedNeighbors(in.Neighbors) {
		for _, p := range n.Vector {
			if local[p.Prefix] {
				continue
			}
			cost := routingSum(n.Cost, p.Metric)
			if p.Hops+1 >= MaxRoutingRouters {
				cost = RoutingInfinity
			}
			old, exists := best[p.Prefix]
			selected := cost < RoutingInfinity && (!exists || cost < old.Metric)
			prefix := p.Prefix
			previous := RoutingInfinity
			if exists {
				previous = old.Metric
			}
			result.Events = append(result.Events, RoutingEvent{Event: "bellman_ford", Interface: n.Interface, Peer: n.Router, Prefix: &prefix, Cost: cost, Selected: selected, PreviousCost: &previous, LinkCost: n.Cost, AdvertisedCost: p.Metric})
			if selected {
				best[p.Prefix] = Route{Prefix: p.Prefix, Interface: n.Interface, NextHop: n.Peer, Origin: "dv", Metric: cost, Hops: p.Hops + 1}
			}
		}
	}
	for _, r := range best {
		result.Routes = append(result.Routes, r)
	}
	sortLearned(result.Routes)
	return result
}

func (linkState) Calculate(in RoutingInput) RoutingCalculation {
	result := RoutingCalculation{}
	db := map[string]RoutingMessage{}
	for _, m := range in.Database {
		db[m.Origin] = m
	}
	dist, first, pred, settled := map[string]uint32{}, map[string]string{}, map[string]string{}, map[string]bool{}
	for id := range db {
		dist[id] = RoutingInfinity
	}
	dist[in.RouterID] = 0
	order := []string{}
	for {
		chosen := ""
		for id, cost := range dist {
			if !settled[id] && cost < RoutingInfinity && (chosen == "" || cost < dist[chosen] || cost == dist[chosen] && id < chosen) {
				chosen = id
			}
		}
		if chosen == "" {
			break
		}
		settled[chosen] = true
		order = append(order, chosen)
		for _, edge := range db[chosen].Edges {
			other, ok := db[edge.Router]
			if !ok || settled[edge.Router] {
				continue
			}
			confirmed := false
			for _, reverse := range other.Edges {
				if reverse.Router == chosen {
					confirmed = true
				}
			}
			if !confirmed {
				continue
			}
			hop := first[chosen]
			if chosen == in.RouterID {
				hop = edge.Router
			}
			cost := routingSum(dist[chosen], edge.Cost)
			if cost < dist[edge.Router] || cost == dist[edge.Router] && cost < RoutingInfinity && hop < first[edge.Router] {
				dist[edge.Router], first[edge.Router], pred[edge.Router] = cost, hop, chosen
			}
		}
		d, p := map[string]uint32{}, map[string]string{}
		for id, v := range dist {
			d[id] = v
		}
		for id, v := range pred {
			p[id] = v
		}
		result.Events = append(result.Events, RoutingEvent{Event: "dijkstra", Settled: append([]string(nil), order...), Distances: d, Predecessors: p})
	}
	neighbors := map[string]AlgorithmNeighbor{}
	for _, n := range sortedNeighbors(in.Neighbors) {
		if _, ok := neighbors[n.Router]; !ok {
			neighbors[n.Router] = n
		}
	}
	local := map[netip.Prefix]bool{}
	for _, p := range in.Local {
		local[p.Prefix] = true
	}
	best, via := map[netip.Prefix]Route{}, map[netip.Prefix]string{}
	ids := make([]string, 0, len(db))
	for id := range db {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if id == in.RouterID || dist[id] >= RoutingInfinity {
			continue
		}
		n, ok := neighbors[first[id]]
		if !ok {
			continue
		}
		for _, p := range db[id].Prefixes {
			if local[p.Prefix] {
				continue
			}
			old, exists := best[p.Prefix]
			if !exists || dist[id] < old.Metric || dist[id] == old.Metric && first[id] < via[p.Prefix] {
				best[p.Prefix] = Route{Prefix: p.Prefix, Interface: n.Interface, NextHop: n.Peer, Origin: "ls", Metric: dist[id]}
				via[p.Prefix] = first[id]
			}
		}
	}
	for _, r := range best {
		result.Routes = append(result.Routes, r)
	}
	sortLearned(result.Routes)
	return result
}

func sortLearned(routes []Route) {
	sort.Slice(routes, func(i, j int) bool { return routes[i].Prefix.String() < routes[j].Prefix.String() })
}
