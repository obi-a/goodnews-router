package goodnews

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"net/netip"
	"time"
)

type ethernetPort struct {
	mac    MAC
	prefix netip.Prefix
	arp    *ARPCache
}

type delayedPacket struct {
	packet waitingPacket
	mac    MAC
	due    time.Time
}

type arpRequest struct {
	name string
	ip   netip.Addr
}

// Requests are emitted after their parent IP decision so trace IDs stay in
// order and the dashboard can index the history without losing packet records.
func (r *Runtime) flushARPRequests() error {
	for _, request := range r.arpRequests {
		if err := r.sendARPRequest(request.name, request.ip); err != nil {
			return err
		}
	}
	r.arpRequests = nil
	return nil
}

type captureFailure struct{ error }
type linkDrop string

func (d linkDrop) Error() string { return string(d) }
func observationError(err error) error {
	var capture captureFailure
	if errors.As(err, &capture) {
		return fmt.Errorf("link observation: %w", capture.error)
	}
	return nil
}
func sendDropReason(err error) string {
	var drop linkDrop
	if errors.As(err, &drop) {
		return string(drop)
	}
	return "send_failed"
}

func (r *Runtime) impairment(name string) Impairment {
	r.impairmentMu.RLock()
	defer r.impairmentMu.RUnlock()
	return r.impairments[name]
}

func (r *Runtime) ARPSnapshot() map[string][]ARPEntryView {
	result := map[string][]ARPEntryView{}
	for name, port := range r.ports {
		result[name] = port.arp.snapshot(time.Now())
	}
	return result
}

func (r *Runtime) hardwareAddresses() map[string]MAC {
	result := map[string]MAC{}
	for name, port := range r.ports {
		result[name] = port.mac
	}
	return result
}

// processEthernet terminates Ethernet/ARP locally. Only unicast IPv4 addressed
// to this port reaches the existing IP decision flow; no L2 broadcast can cause
// an ICMP error. VLAN tags and other EtherTypes have explicit drop reasons.
func (r *Runtime) processEthernet(name string, bytes []byte) error {
	if err := r.observer.Capture(name, time.Now().UTC(), bytes); err != nil {
		return err
	}
	port := r.ports[name]
	f, err := ParseEthernet(bytes)
	drop := func(reason string) error { return r.linkTrace(name, f, "drop", reason, true) }
	if err != nil {
		return drop(err.Error())
	}
	if r.impairment(name).Down {
		return drop("link_down")
	}
	if !f.Source.Unicast() || f.Source == port.mac {
		return drop("invalid_ethernet_source")
	}
	if f.Destination != port.mac && f.Destination != BroadcastMAC {
		return drop("ethernet_not_for_us")
	}
	switch f.EtherType {
	case EtherTypeIPv4:
		if f.Destination != port.mac {
			return drop("unsupported_ethernet_broadcast")
		}
		return r.processIP(name, f.Payload)
	case EtherTypeARP:
		arp, err := ParseARP(f.Payload)
		if err != nil {
			return drop(err.Error())
		}
		if arp.SenderMAC != f.Source || !arp.SenderMAC.Unicast() {
			return drop("invalid_arp_sender")
		}
		if arp.TargetIP != port.prefix.Addr() {
			return drop("arp_not_for_us")
		}
		// A probe (sender 0.0.0.0) may ask about our address, but is never learned.
		probe := arp.SenderIP == netip.IPv4Unspecified() && arp.Operation == 1
		if !probe && (!usableUnicast(arp.SenderIP) || !port.prefix.Contains(arp.SenderIP) || isSubnetEndpoint(arp.SenderIP, port.prefix.Masked()) || r.Engine.addresses[arp.SenderIP] != "") {
			return drop("invalid_arp_sender_ip")
		}
		if arp.Operation == 2 && (f.Destination != port.mac || arp.TargetMAC != port.mac || !port.arp.awaiting(arp.SenderIP)) {
			return drop("unsolicited_arp_reply")
		}
		if err := r.linkTrace(name, f, "arp_receive", "", true); err != nil {
			return err
		}
		var waiting []waitingPacket
		if !probe {
			waiting = port.arp.learn(arp.SenderIP, arp.SenderMAC, time.Now())
		}
		if arp.Operation == 1 {
			reply := BuildEthernet(arp.SenderMAC, port.mac, EtherTypeARP, BuildARP(ARPPacket{Operation: 2, SenderMAC: port.mac, SenderIP: port.prefix.Addr(), TargetMAC: arp.SenderMAC, TargetIP: arp.SenderIP}))
			if err := r.sendARPFrame(name, reply, "arp_reply"); err != nil {
				return err
			}
		}
		for _, packet := range waiting {
			queued, err := r.transmitReady(packet, arp.SenderMAC, time.Now())
			if err := r.completeWaiting(packet, queued, err); err != nil {
				return err
			}
		}
		return nil
	default:
		return drop("unsupported_ethertype")
	}
}

func (r *Runtime) sendIP(packet waitingPacket, now time.Time) (string, error) {
	name := packet.Output.Interface
	if r.impairment(name).Down {
		return "", linkDrop("link_down")
	}
	var mac MAC
	if port := r.ports[name]; port != nil {
		var request bool
		var err error
		mac, request, err = port.arp.resolve(packet, now)
		if err != nil {
			return "", linkDrop(err.Error())
		}
		if request {
			r.arpRequests = append(r.arpRequests, arpRequest{name, packet.Output.NextHop})
		}
		if !mac.Unicast() {
			return "queue_arp", nil
		}
	}
	return r.transmitReady(packet, mac, now)
}

func (r *Runtime) transmitReady(packet waitingPacket, mac MAC, now time.Time) (string, error) {
	i := r.impairment(packet.Output.Interface)
	if i.Down {
		return "", linkDrop("link_down")
	}
	if rand.Float64()*100 < i.LossPercent {
		return "", linkDrop("impaired_loss")
	}
	if len(packet.Output.Packet) >= 20 && rand.Float64()*100 < i.CorruptPercent {
		packet.Output.Packet = append([]byte(nil), packet.Output.Packet...)
		packet.Output.Packet[10] ^= 1
	}
	if i.DelayMS > 0 {
		if len(r.delayed) >= MaxDelayedPackets {
			return "", linkDrop("delay_queue_full")
		}
		r.delayed = append(r.delayed, delayedPacket{packet: packet, mac: mac, due: now.Add(time.Duration(i.DelayMS) * time.Millisecond)})
		return "queue_delay", nil
	}
	return "", r.sendWire(packet.Output, mac)
}

func (r *Runtime) sendWire(output Output, mac MAC) error {
	if r.impairment(output.Interface).Down {
		return linkDrop("link_down")
	}
	bytes := output.Packet
	if port := r.ports[output.Interface]; port != nil {
		bytes = BuildEthernet(mac, port.mac, EtherTypeIPv4, bytes)
	}
	if err := r.links[output.Interface].Send(bytes, output.NextHop); err != nil {
		return err
	}
	if err := r.observer.Capture(output.Interface, time.Now().UTC(), bytes); err != nil {
		return captureFailure{err}
	}
	return nil
}

func (r *Runtime) sendARPRequest(name string, ip netip.Addr) error {
	p := r.ports[name]
	frame := BuildEthernet(BroadcastMAC, p.mac, EtherTypeARP, BuildARP(ARPPacket{Operation: 1, SenderMAC: p.mac, SenderIP: p.prefix.Addr(), TargetIP: ip}))
	return r.sendARPFrame(name, frame, "arp_request")
}

func (r *Runtime) sendARPFrame(name string, bytes []byte, action string) error {
	f, _ := ParseEthernet(bytes)
	if r.impairment(name).Down {
		return r.linkTrace(name, f, "drop", "link_down", false)
	}
	if err := r.links[name].Send(bytes, netip.Addr{}); err != nil {
		return r.linkTrace(name, f, "drop", "send_failed", false)
	}
	if err := r.observer.Capture(name, time.Now().UTC(), bytes); err != nil {
		return err
	}
	return r.linkTrace(name, f, action, "", false)
}

func (r *Runtime) tickLinks(now time.Time) error {
	for name, port := range r.ports {
		requests, expired := port.arp.tick(now)
		for _, ip := range requests {
			if err := r.sendARPRequest(name, ip); err != nil {
				return err
			}
		}
		for _, packet := range expired {
			if err := r.completeWaiting(packet, "", linkDrop("arp_timeout")); err != nil {
				return err
			}
		}
	}
	pending := r.delayed[:0]
	for _, delayed := range r.delayed {
		i := r.impairment(delayed.packet.Output.Interface)
		if now.Before(delayed.due) && i.DelayMS != 0 && !i.Down {
			pending = append(pending, delayed)
			continue
		}
		if err := r.completeWaiting(delayed.packet, "", r.sendWire(delayed.packet.Output, delayed.mac)); err != nil {
			return err
		}
	}
	// Clear vacated slots so large packet buffers do not remain retained.
	clear(r.delayed[len(pending):])
	r.delayed = pending
	return nil
}

func (r *Runtime) shutdownPending() error {
	for _, port := range r.ports {
		for _, packet := range port.arp.drain() {
			if err := r.completeWaiting(packet, "", linkDrop("router_shutdown")); err != nil {
				return err
			}
		}
	}
	for _, delayed := range r.delayed {
		if err := r.completeWaiting(delayed.packet, "", linkDrop("router_shutdown")); err != nil {
			return err
		}
	}
	r.delayed = nil
	return nil
}

// An asynchronous completion has its own ID linked to the initial decision.
// Counters are updated when a frame actually leaves, never when it is queued.
func (r *Runtime) completeWaiting(packet waitingPacket, queued string, err error) error {
	if fatal := observationError(err); fatal != nil {
		return fatal
	}
	o := packet.Output
	h, _ := ParseIPv4(o.Packet, false)
	record := TraceRecord{Kind: "packet", RouterID: r.Engine.Config.RouterID, PacketID: r.nextID.Add(1), ParentID: &packet.ParentID,
		Timestamp: time.Now().UTC(), Egress: o.Interface, Length: len(o.Packet), Source: &h.Source, Destination: &h.Destination,
		Protocol: &h.Protocol, ICMP: DescribeICMP(h), TTLAfter: &h.TTL, Matches: o.Matches, Winner: &o.Route, Action: "sent"}
	// The route was selected before queuing, even if the live FIB has changed.
	record.FIBVersion = o.FIBVersion
	id := record.PacketID
	record.CounterPacketID = &id
	if queued != "" {
		record.Action = queued
	}
	if err != nil {
		record.Action, record.DropReason, record.Error = "drop", sendDropReason(err), err.Error()
	}
	r.mu.Lock()
	if err != nil {
		r.counters.Dropped++
		r.counters.DropReasons[record.DropReason]++
	} else if queued == "" {
		r.counters.Transmitted++
		ic := r.counters.Interfaces[o.Interface]
		ic.Transmitted++
		r.counters.Interfaces[o.Interface] = ic
	}
	r.mu.Unlock()
	return r.exportEvent(record)
}

func (r *Runtime) linkTrace(name string, f EthernetFrame, action, reason string, received bool) error {
	table := r.Engine.FIB.Load()
	if history, ok := r.observer.(interface{ RecordFIB(*Table) error }); ok {
		if err := history.RecordFIB(table); err != nil {
			return err
		}
	}
	record := TraceRecord{Kind: "ethernet", RouterID: r.Engine.Config.RouterID, PacketID: r.nextID.Add(1), Timestamp: time.Now().UTC(),
		Length: 14 + len(f.Payload), FIBVersion: table.Version(), Matches: []Route{}, Action: action, DropReason: reason,
		EtherType: f.EtherType, SourceMAC: f.Source.String(), DestinationMAC: f.Destination.String()}
	if f.EtherType == EtherTypeARP {
		if arp, err := ParseARP(f.Payload); err == nil {
			record.ARP = &arp
		}
	}
	r.mu.Lock()
	ic := r.counters.Interfaces[name]
	if received {
		record.Ingress = name
		r.counters.Received++
		ic.Received++
	} else {
		record.Egress = name
		if reason == "" {
			r.counters.Transmitted++
			ic.Transmitted++
		}
	}
	if reason != "" {
		r.counters.Dropped++
		r.counters.DropReasons[reason]++
	}
	r.counters.Interfaces[name] = ic
	r.mu.Unlock()
	return r.exportEvent(record)
}

func (r *Runtime) exportEvent(record TraceRecord) error {
	if err := r.observer.Trace(record); err != nil {
		return err
	}
	r.mu.Lock()
	id := record.PacketID
	r.counters.AfterPacketID = &id
	r.mu.Unlock()
	return r.observer.Counters(r.Snapshot())
}
