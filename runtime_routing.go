package goodnews

import (
	"fmt"
	"net/netip"
	"slices"
	"time"
)

func (r *Runtime) routingDecision(ingress string, bytes []byte, now time.Time) Decision {
	table := r.Engine.FIB.Load()
	d := Decision{Table: table, Trace: TraceRecord{Kind: "packet", RouterID: r.Engine.Config.RouterID, Ingress: ingress, Length: len(bytes), FIBVersion: table.Version(), Matches: []Route{}, Action: "drop"}}
	drop := func(reason string) Decision { d.Trace.DropReason = reason; return d }
	h, err := ParseIPv4(bytes, true)
	if err != nil {
		return drop(err.Error())
	}
	d.Trace.Source, d.Trace.Destination, d.Trace.Protocol, d.Trace.TTLBefore = &h.Source, &h.Destination, &h.Protocol, &h.TTL
	iface, ok := r.Engine.interfaces[ingress]
	if !ok || h.TTL != 1 || h.HeaderLength != 20 || h.FlagsOffset&0x3fff != 0 || h.Destination != netip.MustParsePrefix(iface.Address).Addr() {
		return drop("routing_invalid_envelope")
	}
	peer := ""
	for _, link := range r.Engine.Config.Routing.Links {
		if link.Interface == ingress {
			peer = link.Peer
		}
	}
	if peer == "" || h.Source != netip.MustParseAddr(peer) {
		return drop("routing_wrong_peer")
	}
	m, err := DecodeRouting(h.Bytes[h.HeaderLength:])
	if err != nil {
		return drop(err.Error())
	}
	if err = r.routing.Receive(ingress, m, now); err != nil {
		return drop(err.Error())
	}
	d.Trace.Action = "deliver"
	return d
}

func (r *Runtime) tickRouting(now time.Time) error {
	if r.routing == nil {
		return nil
	}
	r.routing.Tick(now, r.Impairments())
	b := r.routing.Drain(now)
	if b.Changed {
		current := r.Engine.FIB.Load()
		base, err := BuildTable(r.Engine.Config, current.Version()+1)
		if err != nil {
			return err
		}
		routes := base.Routes()
		seen := map[netip.Prefix]bool{}
		for _, route := range routes {
			seen[route.Prefix] = true
		}
		for _, route := range b.Routes {
			if !seen[route.Prefix] {
				routes = append(routes, route)
				seen[route.Prefix] = true
			}
		}
		next, err := NewTable(routes, current.Version()+1)
		if err != nil {
			return err
		}
		if !slices.Equal(current.Routes(), next.Routes()) {
			if history, ok := r.observer.(interface{ RecordFIB(*Table) error }); ok {
				if err := history.RecordFIB(next); err != nil {
					return err
				}
			}
			r.Engine.FIB.Store(next)
		}
	}
	for _, event := range b.Events {
		if err := r.observer.(interface{ Routing(RoutingEvent) error }).Routing(event); err != nil {
			return fmt.Errorf("routing export: %w", err)
		}
	}
	for _, tx := range b.Transmissions {
		if err := r.sendRouting(tx, now); err != nil {
			return err
		}
	}
	return r.flushARPRequests()
}

func (r *Runtime) sendRouting(tx RoutingTransmission, now time.Time) error {
	iface := r.Engine.interfaces[tx.Interface]
	var peer netip.Addr
	for _, link := range r.Engine.Config.Routing.Links {
		if link.Interface == tx.Interface {
			peer = netip.MustParseAddr(link.Peer)
		}
	}
	payload, err := EncodeRouting(tx.Message)
	if err != nil {
		return fmt.Errorf("routing encode: %w", err)
	}
	packet, err := BuildIPv4(netip.MustParsePrefix(iface.Address).Addr(), peer, RoutingProtocol, 1, payload)
	if err != nil {
		return err
	}
	if len(packet) > iface.MTU {
		return fmt.Errorf("routing message exceeds MTU on %s", iface.Name)
	}
	table := r.Engine.FIB.Load()
	route := Route{Prefix: netip.MustParsePrefix(iface.Address).Masked(), Interface: iface.Name, Origin: "connected"}
	id := r.nextID.Add(1)
	output := Output{Interface: iface.Name, NextHop: peer, Packet: packet, Generated: true, Route: route, Matches: []Route{route}, FIBVersion: table.Version()}
	queued, sendErr := r.sendIP(waitingPacket{Output: output, ParentID: id}, now)
	if fatal := observationError(sendErr); fatal != nil {
		return fatal
	}
	h, _ := ParseIPv4(packet, true)
	record := TraceRecord{Kind: "packet", RouterID: r.Engine.Config.RouterID, PacketID: id, Timestamp: now.UTC(), Egress: iface.Name, Length: len(packet), Source: &h.Source, Destination: &h.Destination, Protocol: &h.Protocol, TTLAfter: &h.TTL, FIBVersion: table.Version(), Matches: output.Matches, Winner: &route, Action: "generated"}
	r.mu.Lock()
	if sendErr != nil {
		record.Action, record.DropReason, record.Error = "drop", sendDropReason(sendErr), sendErr.Error()
		r.counters.Dropped++
		r.counters.DropReasons[record.DropReason]++
	} else if queued != "" {
		record.Action = queued
	} else {
		r.counters.Transmitted++
		ic := r.counters.Interfaces[iface.Name]
		ic.Transmitted++
		r.counters.Interfaces[iface.Name] = ic
	}
	r.mu.Unlock()
	return r.exportEvent(record)
}
