package goodnews

import (
	"fmt"
	"net/netip"
)

const (
	RoutingProtocol           = 253
	RoutingInfinity    uint32 = 65535
	MaxRoutingRouters         = 16
	MaxRoutingPrefixes        = 64
	MaxRoutingLinks           = 16
)

type RoutingConfig struct {
	Algorithm       string        `json:"algorithm"`
	HelloMS         int           `json:"hello_ms,omitempty"`
	DeadMS          int           `json:"dead_ms,omitempty"`
	UpdateMS        int           `json:"update_ms,omitempty"`
	MaxAgeMS        int           `json:"max_age_ms,omitempty"`
	PoisonedReverse *bool         `json:"poisoned_reverse,omitempty"`
	Links           []RoutingLink `json:"links"`
}

type RoutingLink struct {
	Interface string `json:"interface"`
	Peer      string `json:"peer"`
	Cost      uint32 `json:"cost"`
}

func (c RoutingConfig) Effective() RoutingConfig {
	if c.HelloMS == 0 {
		c.HelloMS = 500
	}
	if c.DeadMS == 0 {
		c.DeadMS = 2000
	}
	if c.UpdateMS == 0 {
		c.UpdateMS = 1000
	}
	if c.MaxAgeMS == 0 {
		c.MaxAgeMS = 6000
	}
	if c.Algorithm == "dv" && c.PoisonedReverse == nil {
		value := true
		c.PoisonedReverse = &value
	}
	return c
}

func validateRouting(c Config) error {
	if c.Routing == nil {
		return nil
	}
	r := c.Routing.Effective()
	if routingAlgorithms[r.Algorithm] == nil {
		return fmt.Errorf("routing algorithm must be dv or ls")
	}
	if len(c.RouterID) > 8 {
		return fmt.Errorf("routing router_id must fit 8 ASCII bytes")
	}
	if r.HelloMS < 100 || r.HelloMS > 10000 || r.DeadMS < 3*r.HelloMS || r.DeadMS > 60000 || r.UpdateMS < 100 || r.UpdateMS > 10000 || r.MaxAgeMS < 3*r.UpdateMS || r.MaxAgeMS < r.DeadMS || r.MaxAgeMS > 120000 {
		return fmt.Errorf("routing timers require hello/update 100..10000ms, dead >= 3*hello (<=60000), max_age >= max(3*update, dead) (<=120000)")
	}
	if r.Algorithm == "ls" && r.PoisonedReverse != nil {
		return fmt.Errorf("poisoned_reverse applies only to dv")
	}
	if len(r.Links) == 0 || len(r.Links) > MaxRoutingLinks || len(c.Interfaces) > MaxRoutingPrefixes {
		return fmt.Errorf("routing requires 1..16 links and at most 64 connected prefixes")
	}
	seen := map[string]bool{}
	for _, link := range r.Links {
		var iface *Interface
		for i := range c.Interfaces {
			if c.Interfaces[i].Name == link.Interface {
				iface = &c.Interfaces[i]
			}
		}
		if iface == nil || iface.Driver != "ethernet" || iface.MTU < 1044 || seen[link.Interface] {
			return fmt.Errorf("routing link %s requires a unique Ethernet interface with MTU >= 1044", link.Interface)
		}
		seen[link.Interface] = true
		p := netip.MustParsePrefix(iface.Address)
		peer, err := netip.ParseAddr(link.Peer)
		if err != nil || !usableUnicast(peer) || !p.Contains(peer) || peer == p.Addr() || isSubnetEndpoint(peer, p.Masked()) {
			return fmt.Errorf("routing peer %s must be a usable on-link neighbor", link.Peer)
		}
		for _, local := range c.Interfaces {
			if peer == netip.MustParsePrefix(local.Address).Addr() {
				return fmt.Errorf("routing peer cannot be a local address")
			}
		}
		if link.Cost < 1 || link.Cost > 1000 {
			return fmt.Errorf("routing cost must be 1..1000")
		}
	}
	return nil
}
