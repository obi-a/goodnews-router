package goodnews

import (
	"fmt"
	"net/netip"
	"sort"
	"sync/atomic"
)

type Route struct {
	Prefix    netip.Prefix `json:"prefix"`
	Interface string       `json:"interface"`
	NextHop   netip.Addr   `json:"next_hop,omitempty"`
	Origin    string       `json:"origin"`
}

// Table is immutable after construction. Readers never wait for the control plane.
type Table struct {
	version uint64
	routes  []Route
}

type FIB struct{ current atomic.Pointer[Table] }

func (f *FIB) Load() *Table      { return f.current.Load() }
func (f *FIB) Store(t *Table)    { f.current.Store(t) }
func (t *Table) Version() uint64 { return t.version }
func (t *Table) Routes() []Route { return append([]Route{}, t.routes...) }

func NewTable(routes []Route, version uint64) (*Table, error) {
	copyOfRoutes := append([]Route{}, routes...)
	seen := map[netip.Prefix]bool{}
	for i := range copyOfRoutes {
		r := &copyOfRoutes[i]
		if !r.Prefix.IsValid() || !r.Prefix.Addr().Is4() || r.Interface == "" {
			return nil, fmt.Errorf("invalid route")
		}
		r.Prefix = r.Prefix.Masked()
		if seen[r.Prefix] {
			return nil, fmt.Errorf("duplicate route prefix %s", r.Prefix)
		}
		seen[r.Prefix] = true
	}
	sort.Slice(copyOfRoutes, func(i, j int) bool {
		a, b := copyOfRoutes[i].Prefix, copyOfRoutes[j].Prefix
		if a.Bits() != b.Bits() {
			return a.Bits() > b.Bits()
		}
		return a.Addr().Less(b.Addr())
	})
	return &Table{version: version, routes: copyOfRoutes}, nil
}

// Lookup deliberately examines every prefix so the trace can explain LPM.
func (t *Table) Lookup(destination netip.Addr) (Route, []Route, bool) {
	matches := []Route{}
	for _, route := range t.routes {
		if route.Prefix.Contains(destination) {
			matches = append(matches, route)
		}
	}
	if len(matches) == 0 {
		return Route{}, matches, false
	}
	return matches[0], matches, true
}

func BuildTable(c Config, version uint64) (*Table, error) {
	routes := []Route{}
	interfaces := map[string]netip.Prefix{}
	for _, iface := range c.Interfaces {
		prefix, err := netip.ParsePrefix(iface.Address)
		if err != nil || !prefix.Addr().Is4() {
			return nil, fmt.Errorf("invalid interface prefix %q", iface.Address)
		}
		interfaces[iface.Name] = prefix
		routes = append(routes, Route{Prefix: prefix.Masked(), Interface: iface.Name, Origin: "connected"})
	}
	for _, route := range c.Routes {
		iface, ok := interfaces[route.Interface]
		if !ok {
			return nil, fmt.Errorf("route references missing interface %q", route.Interface)
		}
		prefix, err := netip.ParsePrefix(route.Prefix)
		if err != nil || !prefix.Addr().Is4() {
			return nil, fmt.Errorf("invalid route prefix %q", route.Prefix)
		}
		var nextHop netip.Addr
		if route.NextHop != "" {
			nextHop, err = netip.ParseAddr(route.NextHop)
			if err != nil || !usableUnicast(nextHop) || !iface.Contains(nextHop) || nextHop == iface.Addr() || isSubnetEndpoint(nextHop, iface.Masked()) {
				return nil, fmt.Errorf("next hop %q must be a usable neighbor on %s", route.NextHop, route.Interface)
			}
		}
		routes = append(routes, Route{Prefix: prefix.Masked(), Interface: route.Interface, NextHop: nextHop, Origin: "static"})
	}
	return NewTable(routes, version)
}
