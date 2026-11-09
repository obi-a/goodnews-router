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
	Kind        string                       `json:"kind"`
	RouterID    string                       `json:"router_id"`
	Timestamp   time.Time                    `json:"timestamp"`
	Received    uint64                       `json:"received"`
	Transmitted uint64                       `json:"transmitted"`
	Delivered   uint64                       `json:"delivered"`
	Dropped     uint64                       `json:"dropped"`
	DropReasons map[string]uint64            `json:"drop_reasons"`
	Interfaces  map[string]InterfaceCounters `json:"interfaces"`
}

type Runtime struct {
	Engine   *Engine
	links    map[string]Link
	observer PacketObserver
	nextID   atomic.Uint64
	mu       sync.RWMutex
	counters CounterSnapshot
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
	r := &Runtime{Engine: engine, links: owned, observer: observer,
		counters: CounterSnapshot{Kind: "counters", RouterID: engine.Config.RouterID, DropReasons: map[string]uint64{}, Interfaces: map[string]InterfaceCounters{}}}
	for name := range owned {
		r.counters.Interfaces[name] = InterfaceCounters{}
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
			return nil
		case p := <-packets:
			if ctx.Err() != nil {
				return nil
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
	at := time.Now().UTC()
	if err := r.observer.Capture(ingress, at, packet); err != nil {
		return fmt.Errorf("ingress capture: %w", err)
	}
	d := r.Engine.Process(ingress, packet)
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
		err := r.links[output.Interface].Send(output.Packet, output.NextHop)
		if !output.Generated {
			if err != nil {
				d.Trace.Action, d.Trace.DropReason, d.Trace.Error = "drop", "send_failed", err.Error()
			}
		} else {
			h, _ := ParseIPv4(output.Packet, true)
			parent := d.Trace.PacketID
			record := TraceRecord{Kind: "packet", RouterID: r.Engine.Config.RouterID, PacketID: r.nextID.Add(1), ParentID: &parent,
				Timestamp: time.Now().UTC(), Egress: output.Interface, Length: len(output.Packet), Source: &h.Source, Destination: &h.Destination,
				Protocol: &h.Protocol, TTLAfter: &h.TTL, FIBVersion: d.Trace.FIBVersion, Matches: output.Matches, Winner: &output.Route, Action: "generated"}
			if err != nil {
				record.Action, record.DropReason, record.Error = "drop", "send_failed", err.Error()
			}
			generated = append(generated, record)
		}
		r.mu.Lock()
		if err != nil {
			r.counters.Dropped++
			r.counters.DropReasons["send_failed"]++
		} else {
			r.counters.Transmitted++
			ic := r.counters.Interfaces[output.Interface]
			ic.Transmitted++
			r.counters.Interfaces[output.Interface] = ic
		}
		r.mu.Unlock()
		if err == nil {
			if err := r.observer.Capture(output.Interface, time.Now().UTC(), output.Packet); err != nil {
				return fmt.Errorf("egress capture: %w", err)
			}
		}
	}
	if err := r.observer.Trace(d.Trace); err != nil {
		return fmt.Errorf("packet trace: %w", err)
	}
	for _, record := range generated {
		if err := r.observer.Trace(record); err != nil {
			return fmt.Errorf("generated trace: %w", err)
		}
	}
	if err := r.observer.Counters(r.Snapshot()); err != nil {
		return fmt.Errorf("counter export: %w", err)
	}
	return nil
}

func CloseLinks(links map[string]Link) error {
	var errs []error
	for _, link := range links {
		errs = append(errs, link.Close())
	}
	return errors.Join(errs...)
}
