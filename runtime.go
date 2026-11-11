package goodnews

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

type Link interface {
	Receive() ([]byte, error)
	Send([]byte, netip.Addr) error
	Close() error
}

type PacketObserver interface {
	Capture(string, time.Time, []byte) error
	Trace(TraceRecord) error
	Counters(CounterSnapshot) error
}

type InterfaceCounters struct {
	Received    uint64 `json:"received"`
	Transmitted uint64 `json:"transmitted"`
}

type CounterSnapshot struct {
	Kind          string                       `json:"kind"`
	RouterID      string                       `json:"router_id"`
	Timestamp     time.Time                    `json:"timestamp"`
	AfterPacketID *uint64                      `json:"after_packet_id,omitempty"`
	Received      uint64                       `json:"received"`
	Transmitted   uint64                       `json:"transmitted"`
	Delivered     uint64                       `json:"delivered"`
	Dropped       uint64                       `json:"dropped"`
	DropReasons   map[string]uint64            `json:"drop_reasons"`
	Interfaces    map[string]InterfaceCounters `json:"interfaces"`
}

type Runtime struct {
	Engine       *Engine
	Info         RunInfo
	links        map[string]Link
	observer     PacketObserver
	nextID       atomic.Uint64
	mu           sync.RWMutex
	counters     CounterSnapshot
	ports        map[string]*ethernetPort
	impairmentMu sync.RWMutex
	impairments  map[string]Impairment
	delayed      []delayedPacket
	arpRequests  []arpRequest
}

func NewRuntime(engine *Engine, links map[string]Link, observer PacketObserver) (*Runtime, error) {
	if observer == nil {
		return nil, fmt.Errorf("observer is required")
	}
	owned := make(map[string]Link, len(links))
	for _, iface := range engine.Config.Interfaces {
		if links[iface.Name] == nil {
			return nil, fmt.Errorf("missing link %s", iface.Name)
		}
		owned[iface.Name] = links[iface.Name]
	}
	r := &Runtime{Engine: engine, links: owned, observer: observer, counters: initialCounters(engine.Config), ports: map[string]*ethernetPort{}, impairments: map[string]Impairment{}}
	for _, iface := range engine.Config.Interfaces {
		r.impairments[iface.Name] = Impairment{}
		if iface.Driver == "ethernet" {
			link, ok := owned[iface.Name].(interface{ HardwareAddr() MAC })
			if !ok || !link.HardwareAddr().Unicast() {
				return nil, fmt.Errorf("interface %s requires an Ethernet link with a unicast MAC", iface.Name)
			}
			r.ports[iface.Name] = &ethernetPort{mac: link.HardwareAddr(), prefix: netip.MustParsePrefix(iface.Address), arp: newARPCache()}
		}
	}
	if history, ok := observer.(interface{ RunInfo() RunInfo }); ok {
		r.Info = history.RunInfo()
	}
	return r, nil
}

func (r *Runtime) Snapshot() CounterSnapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s := r.counters
	s.Timestamp = time.Now().UTC()
	s.DropReasons = make(map[string]uint64, len(r.counters.DropReasons))
	for k, v := range r.counters.DropReasons {
		s.DropReasons[k] = v
	}
	s.Interfaces = make(map[string]InterfaceCounters, len(r.counters.Interfaces))
	for k, v := range r.counters.Interfaces {
		s.Interfaces[k] = v
	}
	return s
}

func (r *Runtime) Run(ctx context.Context) error {
	type received struct {
		iface  string
		packet []byte
		err    error
	}
	ctx, cancel := context.WithCancel(ctx)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	packets := make(chan received, 256)
	var workers sync.WaitGroup
	for name, link := range r.links {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				packet, err := link.Receive()
				select {
				case packets <- received{name, packet, err}:
				case <-ctx.Done():
					return
				}
				if err != nil {
					return
				}
			}
		}()
	}
	defer func() {
		cancel()
		for _, link := range r.links {
			_ = link.Close()
		}
		workers.Wait()
	}()
	for {
		select {
		case <-ctx.Done():
			return r.shutdownPending()
		case now := <-ticker.C:
			if err := r.tickLinks(now); err != nil {
				return err
			}
		case p := <-packets:
			if ctx.Err() != nil {
				return r.shutdownPending()
			}
			if p.err != nil {
				return fmt.Errorf("receive on %s: %w", p.iface, p.err)
			}
			if err := r.process(p.iface, p.packet); err != nil {
				return err
			}
		}
	}
}

// process is called only by the forwarding worker; all observer writes have a
// stable order. Management queries read independent counter snapshots.
func (r *Runtime) process(ingress string, packet []byte) error {
	if r.ports[ingress] != nil {
		return r.processEthernet(ingress, packet)
	}
	return r.processIP(ingress, packet)
}

func (r *Runtime) processIP(ingress string, packet []byte) error {
	at := time.Now().UTC()
	if r.ports[ingress] == nil {
		if err := r.observer.Capture(ingress, at, packet); err != nil {
			return fmt.Errorf("ingress capture: %w", err)
		}
	}
	d := r.Engine.Process(ingress, packet)
	if history, ok := r.observer.(interface{ RecordFIB(*Table) error }); ok {
		if err := history.RecordFIB(d.Table); err != nil {
			return fmt.Errorf("FIB snapshot: %w", err)
		}
	}
	d.Trace.PacketID, d.Trace.Timestamp = r.nextID.Add(1), at
	r.mu.Lock()
	r.counters.Received++
	ic := r.counters.Interfaces[ingress]
	ic.Received++
	r.counters.Interfaces[ingress] = ic
	if d.Trace.Action == "deliver" {
		r.counters.Delivered++
	}
	if d.Trace.DropReason != "" {
		r.counters.Dropped++
		r.counters.DropReasons[d.Trace.DropReason]++
	}
	r.mu.Unlock()
	generated := []TraceRecord{}
	for _, output := range d.Outputs {
		queued, err := r.sendIP(waitingPacket{Output: output, ParentID: d.Trace.PacketID}, time.Now())
		if fatal := observationError(err); fatal != nil {
			return fatal
		}
		if !output.Generated {
			if err != nil {
				d.Trace.Action, d.Trace.DropReason, d.Trace.Error = "drop", sendDropReason(err), err.Error()
			} else if queued != "" {
				d.Trace.Action = queued
			}
		} else {
			h, _ := ParseIPv4(output.Packet, true)
			parent := d.Trace.PacketID
			record := TraceRecord{Kind: "packet", RouterID: r.Engine.Config.RouterID, PacketID: r.nextID.Add(1), ParentID: &parent,
				Timestamp: time.Now().UTC(), Egress: output.Interface, Length: len(output.Packet), Source: &h.Source, Destination: &h.Destination,
				Protocol: &h.Protocol, ICMP: DescribeICMP(h), TTLAfter: &h.TTL, FIBVersion: d.Trace.FIBVersion, Matches: output.Matches, Winner: &output.Route, Action: "generated"}
			if err != nil {
				record.Action, record.DropReason, record.Error = "drop", sendDropReason(err), err.Error()
			} else if queued != "" {
				record.Action = queued
			}
			generated = append(generated, record)
		}
		r.mu.Lock()
		if err != nil {
			r.counters.Dropped++
			r.counters.DropReasons[sendDropReason(err)]++
		} else if queued == "" {
			r.counters.Transmitted++
			ic := r.counters.Interfaces[output.Interface]
			ic.Transmitted++
			r.counters.Interfaces[output.Interface] = ic
		}
		r.mu.Unlock()
	}
	if err := r.observer.Trace(d.Trace); err != nil {
		return fmt.Errorf("packet trace: %w", err)
	}
	for _, record := range generated {
		if err := r.observer.Trace(record); err != nil {
			return fmt.Errorf("generated trace: %w", err)
		}
	}
	r.mu.Lock()
	id := d.Trace.PacketID
	r.counters.AfterPacketID = &id
	r.mu.Unlock()
	if err := r.observer.Counters(r.Snapshot()); err != nil {
		return fmt.Errorf("counter export: %w", err)
	}
	return r.flushARPRequests()
}

func CloseLinks(links map[string]Link) error {
	var errs []error
	for _, link := range links {
		errs = append(errs, link.Close())
	}
	return errors.Join(errs...)
}
