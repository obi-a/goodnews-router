package goodnews

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"time"
)

type TraceRecord struct {
	Kind            string      `json:"kind"`
	RouterID        string      `json:"router_id"`
	PacketID        uint64      `json:"packet_id"`
	ParentID        *uint64     `json:"parent_id,omitempty"`
	CounterPacketID *uint64     `json:"counter_packet_id,omitempty"`
	Timestamp       time.Time   `json:"timestamp"`
	Ingress         string      `json:"ingress,omitempty"`
	Egress          string      `json:"egress,omitempty"`
	Length          int         `json:"length"`
	Source          *netip.Addr `json:"source"`
	Destination     *netip.Addr `json:"destination"`
	Protocol        *uint8      `json:"protocol"`
	ICMP            *ICMPFields `json:"icmp,omitempty"`
	TTLBefore       *uint8      `json:"ttl_before"`
	TTLAfter        *uint8      `json:"ttl_after"`
	FIBVersion      uint64      `json:"fib_version"`
	Matches         []Route     `json:"matching_routes"`
	Winner          *Route      `json:"selected_route"`
	Action          string      `json:"action"`
	DropReason      string      `json:"drop_reason,omitempty"`
	ICMPSuppressed  string      `json:"icmp_suppressed,omitempty"`
	Error           string      `json:"error,omitempty"`
	EtherType       uint16      `json:"ether_type,omitempty"`
	SourceMAC       string      `json:"source_mac,omitempty"`
	DestinationMAC  string      `json:"destination_mac,omitempty"`
	ARP             *ARPPacket  `json:"arp,omitempty"`
}

type Output struct {
	Interface  string
	NextHop    netip.Addr
	Packet     []byte
	Generated  bool
	Route      Route
	Matches    []Route
	FIBVersion uint64
}

type Decision struct {
	Trace   TraceRecord
	Outputs []Output
	Table   *Table // The exact immutable snapshot used for this decision.
}

type Engine struct {
	Config     Config
	FIB        FIB
	interfaces map[string]Interface
	addresses  map[netip.Addr]string
	prefixes   []netip.Prefix
}

func NewEngine(c Config) (*Engine, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	table, err := BuildTable(c, 1)
	if err != nil {
		return nil, err
	}
	e := &Engine{Config: c, interfaces: map[string]Interface{}, addresses: map[netip.Addr]string{}}
	for _, iface := range c.Interfaces {
		p := netip.MustParsePrefix(iface.Address)
		e.interfaces[iface.Name], e.addresses[p.Addr()] = iface, iface.Name
		e.prefixes = append(e.prefixes, p.Masked())
	}
	e.FIB.Store(table)
	return e, nil
}

// Process follows the textbook's decision flow. It never performs link I/O and
// never retains or changes its caller's packet buffer.
func (e *Engine) Process(ingress string, packet []byte) Decision {
	table := e.FIB.Load() // One snapshot for forwarding and any resulting response.
	d := Decision{Table: table, Trace: TraceRecord{Kind: "packet", RouterID: e.Config.RouterID, Ingress: ingress,
		Length: len(packet), FIBVersion: table.Version(), Matches: []Route{}, Action: "drop"}}
	drop := func(reason string) Decision { d.Trace.DropReason = reason; return d }
	if _, ok := e.interfaces[ingress]; !ok {
		return drop("unknown_ingress")
	}
	h, err := ParseIPv4(packet, enabled(e.Config.Protections.VerifyIPv4Checksum))
	if err != nil {
		return drop(err.Error())
	}
	d.Trace.Source, d.Trace.Destination, d.Trace.Protocol = &h.Source, &h.Destination, &h.Protocol
	d.Trace.ICMP = DescribeICMP(h)
	d.Trace.TTLBefore, d.Trace.Length = &h.TTL, len(h.Bytes)
	if !usableUnicast(h.Source) || e.subnetEndpoint(h.Source) {
		return drop("invalid_source")
	}
	if !usableUnicast(h.Destination) || e.subnetEndpoint(h.Destination) {
		return drop("unsupported_destination")
	}
	if h.HeaderLength != 20 {
		return drop("unsupported_ip_options")
	}
	if _, local := e.addresses[h.Destination]; local {
		if h.FlagsOffset&0x3fff != 0 {
			return drop("local_reassembly_not_implemented")
		}
		return e.deliverLocally(d, h, table)
	}
	if h.TTL <= 1 {
		return e.sendICMPError(d, h, table, "ttl_expired", 11, 0, 0)
	}
	route, matches, ok := table.Lookup(h.Destination)
	d.Trace.Matches = matches
	if !ok {
		return e.sendICMPError(d, h, table, "no_route", 3, 0, 0)
	}
	d.Trace.Winner = &route
	iface, ok := e.interfaces[route.Interface]
	if !ok {
		return drop("route_interface_unavailable")
	}
	if len(h.Bytes) > iface.MTU {
		if h.FlagsOffset&0x4000 != 0 {
			return e.sendICMPError(d, h, table, "mtu_exceeded", 3, 4, uint16(iface.MTU))
		}
		return drop("fragmentation_not_implemented")
	}
	forwarded := append([]byte(nil), h.Bytes...)
	if enabled(e.Config.Protections.DecrementTTL) {
		forwarded[8]--
	}
	SetIPv4Checksum(forwarded)
	ttl := forwarded[8]
	d.Trace.TTLAfter, d.Trace.Egress, d.Trace.Action = &ttl, route.Interface, "forward"
	d.Outputs = []Output{makeOutput(route, matches, h.Destination, forwarded, false)}
	d.Outputs[0].FIBVersion = table.Version()
	return d
}

func (e *Engine) subnetEndpoint(a netip.Addr) bool {
	for _, prefix := range e.prefixes {
		if isSubnetEndpoint(a, prefix) {
			return true
		}
	}
	return false
}

func (e *Engine) deliverLocally(d Decision, h IPv4, table *Table) Decision {
	payload := h.Bytes[h.HeaderLength:]
	if h.Protocol == 1 {
		if len(payload) < 8 || Checksum(payload) != 0 {
			d.Trace.DropReason = "invalid_icmp"
			return d
		}
		if payload[0] != 8 || payload[1] != 0 {
			d.Trace.Action = "deliver"
			return d
		}
		reply := append([]byte(nil), payload...)
		reply[0], reply[2], reply[3] = 0, 0, 0
		binary.BigEndian.PutUint16(reply[2:4], Checksum(reply))
		return e.respond(d, h, table, h.Destination, reply, "reply")
	}
	if h.Protocol == 17 {
		if len(payload) < 8 {
			d.Trace.DropReason = "invalid_udp_length"
			return d
		}
		return e.sendICMPError(d, h, table, "local_udp_port_unreachable", 3, 3, 0)
	}
	return e.sendICMPError(d, h, table, "local_protocol_unreachable", 3, 2, 0)
}

func (e *Engine) sendICMPError(d Decision, h IPv4, table *Table, reason string, kind, code uint8, mtu uint16) Decision {
	d.Trace.DropReason = reason
	// RFC 1812: never send an error about an error or a noninitial fragment.
	if h.FlagsOffset&0x1fff != 0 {
		d.Trace.ICMPSuppressed = "noninitial_fragment"
		return d
	}
	if h.Protocol == 1 {
		payload := h.Bytes[h.HeaderLength:]
		if len(payload) < 8 {
			d.Trace.ICMPSuppressed = "truncated_icmp"
			return d
		}
		switch payload[0] {
		case 3, 4, 5, 11, 12:
			d.Trace.ICMPSuppressed = "icmp_error"
			return d
		}
	}
	quoteEnd := min(len(h.Bytes), h.HeaderLength+8)
	icmp := make([]byte, 8+quoteEnd)
	icmp[0], icmp[1] = kind, code
	if kind == 3 && code == 4 {
		binary.BigEndian.PutUint16(icmp[6:8], mtu)
	}
	copy(icmp[8:], h.Bytes[:quoteEnd]) // Quote the incoming header, before TTL changes.
	binary.BigEndian.PutUint16(icmp[2:4], Checksum(icmp))
	return e.respond(d, h, table, netip.Addr{}, icmp, "icmp_error")
}

func (e *Engine) respond(d Decision, original IPv4, table *Table, source netip.Addr, icmp []byte, action string) Decision {
	route, matches, ok := table.Lookup(original.Source)
	if !ok {
		d.Trace.Action, d.Trace.ICMPSuppressed = "drop", "no_return_route"
		if d.Trace.DropReason == "" {
			d.Trace.DropReason = "no_return_route"
		}
		return d
	}
	iface, ok := e.interfaces[route.Interface]
	if !ok {
		d.Trace.DropReason = "route_interface_unavailable"
		return d
	}
	if !source.IsValid() {
		source = netip.MustParsePrefix(iface.Address).Addr()
	}
	packet, err := BuildIPv4(source, original.Source, 1, 64, icmp)
	if err != nil {
		d.Trace.DropReason, d.Trace.Error = "response_build_failed", fmt.Sprint(err)
		return d
	}
	if len(packet) > iface.MTU {
		d.Trace.Action, d.Trace.ICMPSuppressed = "drop", "response_exceeds_mtu"
		if d.Trace.DropReason == "" {
			d.Trace.DropReason = "response_exceeds_mtu"
		}
		return d
	}
	d.Trace.Action = action
	d.Outputs = []Output{makeOutput(route, matches, original.Source, packet, true)}
	d.Outputs[0].FIBVersion = table.Version()
	return d
}

func makeOutput(route Route, matches []Route, destination netip.Addr, packet []byte, generated bool) Output {
	nextHop := route.NextHop
	if !nextHop.IsValid() {
		nextHop = destination
	}
	return Output{Interface: route.Interface, NextHop: nextHop, Packet: packet, Generated: generated, Route: route, Matches: matches}
}
