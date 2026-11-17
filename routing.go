package goodnews

import (
	"fmt"
	"net/netip"
	"reflect"
	"slices"
	"sort"
	"sync"
	"time"
)

type routingNeighbor struct {
	link                                 RoutingLink
	id                                   string
	epoch, helloSequence, vectorSequence uint64
	lastHello, vectorAt                  time.Time
	up, down, mismatch                   bool
	cost                                 uint32
	vector                               []RoutingPrefix
}
type routingFreshness struct{ epoch, sequence uint64 }
type RoutingTransmission struct {
	Interface string
	Message   RoutingMessage
}
type RoutingBatch struct {
	Changed       bool
	Routes        []Route
	Transmissions []RoutingTransmission
	Events        []RoutingEvent
}

// Routing owns bounded protocol state. Receive and Tick are called by the
// packet worker; snapshots take the same lock and return detached values.
type Routing struct {
	mu                             sync.Mutex
	config                         Config
	options                        RoutingConfig
	epoch, sequence                uint64
	neighbors                      map[string]*routingNeighbor
	database                       map[string]RoutingMessage
	fresh                          map[string]routingFreshness // tombstones persist for this bounded lab
	identities                     map[string]bool
	local                          []RoutingPrefix
	routes                         []Route
	dirty, originate               bool
	nextHello, nextUpdate, trigger time.Time
	batch                          RoutingBatch
}

func NewRouting(c Config, epoch uint64) (*Routing, error) {
	if c.Routing == nil || epoch == 0 {
		return nil, fmt.Errorf("routing config and nonzero incarnation required")
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	r := &Routing{config: c, options: c.Routing.Effective(), epoch: epoch, neighbors: map[string]*routingNeighbor{}, database: map[string]RoutingMessage{}, fresh: map[string]routingFreshness{}, identities: map[string]bool{c.RouterID: true}, dirty: true, originate: true}
	for _, link := range r.options.Links {
		r.neighbors[link.Interface] = &routingNeighbor{link: link, cost: link.Cost}
	}
	for _, iface := range c.Interfaces {
		r.local = append(r.local, RoutingPrefix{Prefix: netip.MustParsePrefix(iface.Address).Masked()})
	}
	sort.Slice(r.local, func(i, j int) bool { return r.local[i].Prefix.String() < r.local[j].Prefix.String() })
	return r, nil
}

func (r *Routing) message(kind byte) RoutingMessage {
	r.sequence++
	return RoutingMessage{Type: kind, Origin: r.config.RouterID, Epoch: r.epoch, Sequence: r.sequence}
}

func (r *Routing) names() []string {
	names := make([]string, 0, len(r.neighbors))
	for name := range r.neighbors {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (r *Routing) event(kind, iface, peer, reason string) {
	r.batch.Events = append(r.batch.Events, RoutingEvent{Event: kind, Interface: iface, Peer: peer, Reason: reason})
}

func (r *Routing) hello(name string, now time.Time) {
	n := r.neighbors[name]
	if n.down {
		return
	}
	m := r.message(RoutingHello)
	m.Algorithm = r.options.Algorithm
	if !n.lastHello.IsZero() && now.Sub(n.lastHello) < time.Duration(r.options.DeadMS)*time.Millisecond {
		m.Seen, m.SeenEpoch = n.id, n.epoch
	}
	r.batch.Transmissions = append(r.batch.Transmissions, RoutingTransmission{name, m})
}

func (r *Routing) lose(n *routingNeighbor, reason string) {
	if n.up {
		r.event("neighbor_down", n.link.Interface, n.id, reason)
		r.dirty, r.originate = true, true
	}
	n.up = false
	n.vector = nil
	n.vectorAt = time.Time{}
}

func (r *Routing) Receive(name string, m RoutingMessage, now time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.neighbors[name]
	if n == nil || n.down {
		return fmt.Errorf("routing_inactive_link")
	}
	if m.Type == RoutingHello {
		if m.Origin == r.config.RouterID {
			return fmt.Errorf("routing_duplicate_identity")
		}
		if m.Algorithm != r.options.Algorithm {
			r.lose(n, "algorithm_mismatch")
			n.mismatch = true
			return fmt.Errorf("routing_algorithm_mismatch")
		}
		if m.Origin == n.id && (m.Epoch < n.epoch || m.Epoch == n.epoch && m.Sequence <= n.helloSequence) {
			return fmt.Errorf("routing_stale_hello")
		}
		if !r.identities[m.Origin] && len(r.identities) >= MaxRoutingRouters {
			return fmt.Errorf("routing_router_limit")
		}
		for _, other := range r.neighbors {
			if other != n && other.up && other.id == m.Origin {
				return fmt.Errorf("routing_duplicate_identity")
			}
		}
		changed := m.Origin != n.id || m.Epoch != n.epoch
		if changed {
			r.lose(n, "peer_restart")
			n.vectorSequence = 0
		}
		r.identities[m.Origin] = true
		n.id, n.epoch, n.helloSequence, n.lastHello, n.mismatch = m.Origin, m.Epoch, m.Sequence, now, false
		confirmed := m.Seen == r.config.RouterID && m.SeenEpoch == r.epoch
		if !confirmed {
			r.lose(n, "unconfirmed_hello")
		}
		if confirmed && !n.up {
			n.up = true
			r.dirty, r.originate = true, true
			r.event("neighbor_up", name, n.id, "")
			if r.options.Algorithm == "ls" {
				for _, origin := range r.databaseIDs() {
					lsa := r.database[origin]
					if now.UnixMilli()-lsa.CreatedMS < int64(r.options.MaxAgeMS) {
						r.batch.Transmissions = append(r.batch.Transmissions, RoutingTransmission{name, lsa})
					}
				}
			}
		}
		if changed {
			r.hello(name, now)
		}
		return nil
	}
	if !n.up {
		return fmt.Errorf("routing_neighbor_not_up")
	}
	switch m.Type {
	case RoutingDV:
		if r.options.Algorithm != "dv" || m.Origin != n.id || m.Epoch != n.epoch {
			return fmt.Errorf("routing_wrong_advertisement")
		}
		if m.Sequence <= n.vectorSequence {
			return fmt.Errorf("routing_stale_vector")
		}
		if !r.withinLimits(name, m) {
			return fmt.Errorf("routing_state_limit")
		}
		n.vectorSequence, n.vectorAt = m.Sequence, now
		if !reflect.DeepEqual(n.vector, m.Prefixes) {
			n.vector = append([]RoutingPrefix(nil), m.Prefixes...)
			r.dirty = true
		}
	case RoutingLSA:
		if r.options.Algorithm != "ls" {
			return fmt.Errorf("routing_wrong_advertisement")
		}
		age := now.UnixMilli() - m.CreatedMS
		if age < -1000 || age >= int64(r.options.MaxAgeMS) {
			return fmt.Errorf("routing_expired_lsa")
		}
		if m.Origin == r.config.RouterID {
			return fmt.Errorf("routing_self_lsa")
		}
		old, known := r.fresh[m.Origin]
		if known && (m.Epoch < old.epoch || m.Epoch == old.epoch && m.Sequence <= old.sequence) {
			return fmt.Errorf("routing_stale_lsa")
		}
		if !r.withinLimits(name, m) {
			return fmt.Errorf("routing_state_limit")
		}
		previous, exists := r.database[m.Origin]
		r.database[m.Origin] = m
		r.fresh[m.Origin] = routingFreshness{m.Epoch, m.Sequence}
		r.identities[m.Origin] = true
		for _, edge := range m.Edges {
			r.identities[edge.Router] = true
		}
		if !exists || !reflect.DeepEqual(previous.Edges, m.Edges) || !reflect.DeepEqual(previous.Prefixes, m.Prefixes) {
			r.dirty = true
		}
		r.flood(m, name)
		r.event("lsa_receive", name, m.Origin, "")
	default:
		return fmt.Errorf("unknown_routing_type")
	}
	return nil
}

func (r *Routing) withinLimits(name string, incoming RoutingMessage) bool {
	prefixes := map[netip.Prefix]bool{}
	ids := map[string]bool{}
	for id := range r.identities {
		ids[id] = true
	}
	for _, p := range r.local {
		prefixes[p.Prefix] = true
	}
	if incoming.Type == RoutingDV {
		for iface, n := range r.neighbors {
			if iface != name {
				for _, p := range n.vector {
					prefixes[p.Prefix] = true
				}
			}
		}
	} else {
		ids[incoming.Origin] = true
		for _, e := range incoming.Edges {
			ids[e.Router] = true
		}
		for id, m := range r.database {
			if id != incoming.Origin {
				for _, p := range m.Prefixes {
					prefixes[p.Prefix] = true
				}
			}
		}
	}
	for _, p := range incoming.Prefixes {
		prefixes[p.Prefix] = true
	}
	return len(prefixes) <= MaxRoutingPrefixes && len(ids) <= MaxRoutingRouters
}

func (r *Routing) databaseIDs() []string {
	ids := make([]string, 0, len(r.database))
	for id := range r.database {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (r *Routing) flood(m RoutingMessage, except string) {
	for _, name := range r.names() {
		if n := r.neighbors[name]; n.up && !n.down && name != except {
			r.batch.Transmissions = append(r.batch.Transmissions, RoutingTransmission{name, m})
		}
	}
}

func (r *Routing) originateLSA(now time.Time) {
	m := r.message(RoutingLSA)
	m.CreatedMS = now.UnixMilli()
	m.Prefixes = append([]RoutingPrefix(nil), r.local...)
	for _, name := range r.names() {
		if n := r.neighbors[name]; n.up && !n.down {
			m.Edges = append(m.Edges, RoutingEdge{Router: n.id, Cost: n.cost})
		}
	}
	old, exists := r.database[m.Origin]
	if !exists || !reflect.DeepEqual(old.Edges, m.Edges) {
		r.dirty = true
	}
	r.database[m.Origin] = m
	r.fresh[m.Origin] = routingFreshness{m.Epoch, m.Sequence}
	r.flood(m, "")
	r.event("lsa_originate", "", m.Origin, "")
	r.originate = false
}

func (r *Routing) Tick(now time.Time, impairments map[string]Impairment) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, name := range r.names() {
		n := r.neighbors[name]
		i := impairments[name]
		cost := i.Cost
		if cost == 0 {
			cost = n.link.Cost
		}
		if cost != n.cost {
			n.cost = cost
			r.dirty, r.originate = true, true
			r.event("cost_change", name, n.id, fmt.Sprint(cost))
		}
		if i.Down != n.down {
			n.down = i.Down
			if n.down {
				r.lose(n, "administrative_down")
			} else {
				r.nextHello = time.Time{}
			}
		}
		if !n.lastHello.IsZero() && now.Sub(n.lastHello) >= time.Duration(r.options.DeadMS)*time.Millisecond {
			r.lose(n, "hello_timeout")
			n.lastHello = time.Time{}
		}
		if !n.vectorAt.IsZero() && now.Sub(n.vectorAt) >= time.Duration(r.options.MaxAgeMS)*time.Millisecond {
			n.vector = nil
			n.vectorAt = time.Time{}
			r.dirty = true
			r.event("vector_expired", name, n.id, "")
		}
	}
	for id, m := range r.database {
		if id != r.config.RouterID && now.UnixMilli()-m.CreatedMS >= int64(r.options.MaxAgeMS) {
			delete(r.database, id)
			r.dirty = true
			r.event("lsa_expired", "", id, "")
		}
	}
	periodic := !now.Before(r.nextUpdate)
	if r.options.Algorithm == "ls" && (r.originate || periodic) {
		r.originateLSA(now)
	}
	if r.dirty {
		in := RoutingInput{RouterID: r.config.RouterID, Local: r.local}
		for _, name := range r.names() {
			n := r.neighbors[name]
			if n.up && !n.down {
				in.Neighbors = append(in.Neighbors, AlgorithmNeighbor{Router: n.id, Interface: name, Peer: netip.MustParseAddr(n.link.Peer), Cost: n.cost, Vector: n.vector})
			}
		}
		for _, id := range r.databaseIDs() {
			in.Database = append(in.Database, r.database[id])
		}
		calculated := routingAlgorithms[r.options.Algorithm].Calculate(in)
		r.batch.Events = append(r.batch.Events, calculated.Events...)
		if !slices.Equal(r.routes, calculated.Routes) {
			oldByPrefix, newByPrefix := map[netip.Prefix]Route{}, map[netip.Prefix]Route{}
			for _, route := range r.routes {
				oldByPrefix[route.Prefix] = route
			}
			for _, route := range calculated.Routes {
				newByPrefix[route.Prefix] = route
				old, exists := oldByPrefix[route.Prefix]
				if !exists || old != route {
					previous := RoutingInfinity
					if exists {
						previous = old.Metric
					}
					prefix := route.Prefix
					r.batch.Events = append(r.batch.Events, RoutingEvent{Event: "route_update", Interface: route.Interface, Prefix: &prefix, Cost: route.Metric, PreviousCost: &previous})
				}
			}
			for _, route := range r.routes {
				if _, exists := newByPrefix[route.Prefix]; !exists {
					previous, prefix := route.Metric, route.Prefix
					r.batch.Events = append(r.batch.Events, RoutingEvent{Event: "route_update", Prefix: &prefix, Cost: RoutingInfinity, PreviousCost: &previous, Reason: "withdrawn"})
				}
			}
			r.routes = calculated.Routes
			r.batch.Changed = true
			r.event("routes_changed", "", "", "")
		}
		if r.trigger.IsZero() {
			r.trigger = now.Add(100 * time.Millisecond)
		}
		r.dirty = false
	}
	if !now.Before(r.nextHello) {
		for _, name := range r.names() {
			r.hello(name, now)
		}
		r.nextHello = now.Add(time.Duration(r.options.HelloMS) * time.Millisecond)
	}
	if periodic || !r.trigger.IsZero() && !now.Before(r.trigger) {
		if r.options.Algorithm == "dv" {
			for _, name := range r.names() {
				n := r.neighbors[name]
				if !n.up || n.down {
					continue
				}
				m := r.message(RoutingDV)
				m.Prefixes = append([]RoutingPrefix(nil), r.local...)
				for _, route := range r.routes {
					metric := route.Metric
					if enabled(r.options.PoisonedReverse) && route.Interface == name {
						metric = RoutingInfinity
						prefix := route.Prefix
						r.batch.Events = append(r.batch.Events, RoutingEvent{Event: "poison_reverse", Interface: name, Peer: n.id, Prefix: &prefix, Cost: metric})
					}
					m.Prefixes = append(m.Prefixes, RoutingPrefix{Prefix: route.Prefix, Metric: metric, Hops: route.Hops})
				}
				r.batch.Transmissions = append(r.batch.Transmissions, RoutingTransmission{name, m})
			}
		}
		r.trigger = time.Time{}
	}
	if periodic {
		r.nextUpdate = now.Add(time.Duration(r.options.UpdateMS) * time.Millisecond)
	}
}

func (r *Routing) Drain(now time.Time) RoutingBatch {
	r.mu.Lock()
	defer r.mu.Unlock()
	b := r.batch
	b.Routes = append([]Route(nil), r.routes...)
	for i := range b.Events {
		b.Events[i].Timestamp = now.UTC()
		b.Events[i].RouterID = r.config.RouterID
		b.Events[i].Algorithm = r.options.Algorithm
	}
	r.batch = RoutingBatch{}
	return b
}

type NeighborView struct {
	Interface   string `json:"interface"`
	Router      string `json:"router,omitempty"`
	Peer        string `json:"peer"`
	Algorithm   string `json:"algorithm"`
	State       string `json:"state"`
	Incarnation uint64 `json:"incarnation"`
	Cost        uint32 `json:"cost"`
	LastHelloMS int64  `json:"last_hello_ms"`
}

func (r *Routing) Neighbors(now time.Time) []NeighborView {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := []NeighborView{}
	for _, name := range r.names() {
		n := r.neighbors[name]
		state := "init"
		age := int64(-1)
		if n.up {
			state = "up"
		}
		if n.down || n.lastHello.IsZero() && n.id != "" {
			state = "down"
		}
		if n.mismatch {
			state = "algorithm_mismatch"
		}
		if !n.lastHello.IsZero() {
			age = now.Sub(n.lastHello).Milliseconds()
		}
		result = append(result, NeighborView{name, n.id, n.link.Peer, r.options.Algorithm, state, n.epoch, n.cost, age})
	}
	return result
}

func (r *Routing) Database() []RoutingMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := []RoutingMessage{}
	for _, id := range r.databaseIDs() {
		m := r.database[id]
		m.Prefixes = append([]RoutingPrefix(nil), m.Prefixes...)
		m.Edges = append([]RoutingEdge(nil), m.Edges...)
		result = append(result, m)
	}
	return result
}
