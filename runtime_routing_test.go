package goodnews

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRoutingRuntimeEthernetARPAndFIBHistory(t *testing.T) {
	c := testConfig()
	for i := range c.Interfaces {
		c.Interfaces[i].Driver = "ethernet"
	}
	c.Routing = &RoutingConfig{Algorithm: "dv", Links: []RoutingLink{{Interface: "left", Peer: "10.0.0.2", Cost: 2}}}
	c.Observability = ObservationConfig{Directory: t.TempDir(), Socket: filepath.Join(t.TempDir(), "r.sock")}
	e, err := NewEngine(c)
	if err != nil {
		t.Fatal(err)
	}
	o, err := OpenObserver(c)
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	left := &fakeEthernet{newFakeLink(), MAC{2, 0, 0, 0, 0, 1}}
	right := &fakeEthernet{newFakeLink(), MAC{2, 0, 0, 0, 1, 1}}
	r, err := NewRuntime(e, map[string]Link{"left": left, "right": right}, o)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.tickRouting(time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(left.sent) != 1 {
		t.Fatal("hello did not start ARP resolution")
	}
	frame, _ := ParseEthernet(left.sent[0])
	if frame.EtherType != EtherTypeARP {
		t.Fatal("hello bypassed ARP")
	}
	arp := ARPPacket{Operation: 2, SenderMAC: hostLeft, SenderIP: addr("10.0.0.2"), TargetMAC: left.mac, TargetIP: addr("10.0.0.1")}
	if err := r.process("left", BuildEthernet(left.mac, hostLeft, EtherTypeARP, BuildARP(arp))); err != nil {
		t.Fatal(err)
	}
	frame, _ = ParseEthernet(left.sent[1])
	ip, err := ParseIPv4(frame.Payload, true)
	if err != nil || ip.Protocol != RoutingProtocol || ip.TTL != 1 {
		t.Fatal("hello was not sent as link-local IPv4", err)
	}
	message, err := DecodeRouting(ip.Bytes[20:])
	if err != nil || message.Type != RoutingHello {
		t.Fatal("invalid wire hello", err)
	}
	send := func(source string, m RoutingMessage) {
		t.Helper()
		wire, err := EncodeRouting(m)
		if err != nil {
			t.Fatal(err)
		}
		p := testPacket(t, source, "10.0.0.1", RoutingProtocol, 1, wire)
		if err := r.process("left", BuildEthernet(left.mac, hostLeft, EtherTypeIPv4, p)); err != nil {
			t.Fatal(err)
		}
	}
	hello := RoutingMessage{Type: RoutingHello, Origin: "v", Epoch: 10, Sequence: 1, Algorithm: "dv"}
	send("10.0.0.2", hello)
	if r.routing.neighbors["left"].up {
		t.Fatal("unconfirmed hello became up")
	}
	hello.Sequence = 2
	hello.Seen = c.RouterID
	hello.SeenEpoch = r.routing.epoch
	send("10.0.0.2", hello)
	vector := RoutingMessage{Type: RoutingDV, Origin: "v", Epoch: 10, Sequence: 3, Prefixes: []RoutingPrefix{{Prefix: netip.MustParsePrefix("192.0.2.0/24"), Metric: 3, Hops: 1}}}
	send("10.0.0.2", vector)
	route, _, ok := e.FIB.Load().Lookup(addr("192.0.2.7"))
	if !ok || route.Metric != 5 || route.Interface != "left" || route.NextHop != addr("10.0.0.2") {
		t.Fatalf("learned FIB: %+v", route)
	}
	packet := testPacket(t, "10.0.1.2", "192.0.2.7", 1, 64, testEcho())
	if err := r.process("right", BuildEthernet(right.mac, hostRight, EtherTypeIPv4, packet)); err != nil {
		t.Fatal(err)
	}
	frame, _ = ParseEthernet(left.sent[len(left.sent)-1])
	forwarded, err := ParseIPv4(frame.Payload, true)
	if err != nil || forwarded.TTL != 63 || frame.Destination != hostLeft {
		t.Fatal("data did not follow learned route", err)
	}
	send("10.0.0.99", vector)
	if r.Snapshot().DropReasons["routing_wrong_peer"] != 1 {
		t.Fatal("accepted control from wrong peer")
	}
	control, _ := EncodeRouting(vector)
	for _, ttl := range []uint8{0, 2, 64} {
		p := testPacket(t, "10.0.0.2", "10.0.0.1", RoutingProtocol, ttl, control)
		if err := r.process("left", BuildEthernet(left.mac, hostLeft, EtherTypeIPv4, p)); err != nil {
			t.Fatal(err)
		}
	}
	if r.Snapshot().DropReasons["routing_invalid_envelope"] != 3 {
		t.Fatal("accepted control outside link-local envelope")
	}
	checksumProtection := false
	e.Config.Protections.VerifyIPv4Checksum = &checksumProtection
	corrupt := testPacket(t, "10.0.0.2", "10.0.0.1", RoutingProtocol, 1, control)
	corrupt[10] ^= 1
	if err := r.process("left", BuildEthernet(left.mac, hostLeft, EtherTypeIPv4, corrupt)); err != nil {
		t.Fatal(err)
	}
	if r.Snapshot().DropReasons["invalid_checksum"] != 1 {
		t.Fatal("control checksum validation followed data-plane opt-out")
	}
	// Management changes are picked up by the packet worker, not applied by the
	// HTTP goroutine to mutable algorithm state.
	handler := r.ManagementHandler()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/impairments", strings.NewReader(`{"interface":"left","setting":"cost","value":60}`)))
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	if err := r.tickRouting(time.Now()); err != nil {
		t.Fatal(err)
	}
	route, _, _ = e.FIB.Load().Lookup(addr("192.0.2.7"))
	if route.Metric != 63 {
		t.Fatal("live cost did not recalculate route")
	}
	for path, status := range map[string]int{"neighbors": 200, "lsdb": 501, "routes": 200} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/"+path, nil))
		if response.Code != status || !json.Valid(response.Body.Bytes()) {
			t.Fatal(path, response.Code, response.Body.String())
		}
	}
	if err := r.changeImpairment(impairmentCommand{Interface: "left", Setting: "down"}); err != nil {
		t.Fatal(err)
	}
	if err := r.tickRouting(time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := e.FIB.Load().Lookup(addr("192.0.2.7")); ok {
		t.Fatal("dead adjacency retained learned route")
	}
	data, err := os.ReadFile(filepath.Join(o.Directory, "fib.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	versions := map[uint64]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var snapshot FIBSnapshot
		if err := json.Unmarshal([]byte(line), &snapshot); err != nil {
			t.Fatal(err)
		}
		versions[snapshot.Version] = true
	}
	data, err = os.ReadFile(filepath.Join(o.Directory, "trace.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	previous := uint64(0)
	known := map[uint64]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var trace TraceRecord
		if err := json.Unmarshal([]byte(line), &trace); err != nil {
			t.Fatal(err)
		}
		if trace.PacketID <= previous || !versions[trace.FIBVersion] {
			t.Fatal("invalid FIB/ID history", trace)
		}
		if trace.ParentID != nil && !known[*trace.ParentID] {
			t.Fatal("missing parent", trace)
		}
		previous = trace.PacketID
		known[trace.PacketID] = true
	}
	data, err = os.ReadFile(filepath.Join(o.Directory, "routing.jsonl"))
	if err != nil || !strings.Contains(string(data), "bellman_ford") {
		t.Fatal("algorithm history missing", err)
	}
}

type routingWireLink struct {
	input  chan []byte
	closed chan struct{}
	once   sync.Once
	mac    MAC
	peer   *routingWireLink
}

func (l *routingWireLink) HardwareAddr() MAC { return l.mac }
func (l *routingWireLink) Receive() ([]byte, error) {
	select {
	case p := <-l.input:
		return p, nil
	case <-l.closed:
		return nil, io.EOF
	}
}
func (l *routingWireLink) Send(p []byte, _ netip.Addr) error {
	select {
	case <-l.peer.closed:
		return io.ErrClosedPipe
	default:
	}
	select {
	case l.peer.input <- append([]byte(nil), p...):
		return nil
	default:
		return fmt.Errorf("test wire queue full")
	}
}
func (l *routingWireLink) Close() error { l.once.Do(func() { close(l.closed) }); return nil }

type routingTestObserver struct{ memoryObserver }

func (*routingTestObserver) Routing(RoutingEvent) error { return nil }

func TestRoutingWorkersExchangeOverVirtualEthernet(t *testing.T) {
	for _, algorithm := range []string{"dv", "ls"} {
		t.Run(algorithm, func(t *testing.T) {
			config := graphConfigs(algorithm)
			root := t.TempDir()
			attached := map[string]map[string]Link{}
			routers := map[string]*Runtime{}
			for i, id := range graphIDs {
				attached[id] = map[string]Link{"lan": &fakeEthernet{newFakeLink(), MAC{2, 45, 0, 0, byte(i), 1}}}
			}
			for i, edge := range textbookLinks {
				a := &routingWireLink{input: make(chan []byte, 256), closed: make(chan struct{}), mac: MAC{2, 44, 0, byte(i), 0, 1}}
				b := &routingWireLink{input: make(chan []byte, 256), closed: make(chan struct{}), mac: MAC{2, 44, 0, byte(i), 0, 2}}
				a.peer, b.peer = b, a
				attached[edge.a]["to-"+edge.b] = a
				attached[edge.b]["to-"+edge.a] = b
			}
			ctx, cancel := context.WithCancel(context.Background())
			results := make(chan error, len(graphIDs))
			for _, id := range graphIDs {
				c := config[id]
				c.Observability.Socket = filepath.Join(root, id+".sock")
				e, err := NewEngine(c)
				if err != nil {
					cancel()
					t.Fatal(err)
				}
				r, err := NewRuntime(e, attached[id], &routingTestObserver{})
				if err != nil {
					cancel()
					t.Fatal(err)
				}
				routers[id] = r
			}
			for _, id := range graphIDs {
				go func() { results <- routers[id].Run(ctx) }()
			}
			t.Cleanup(func() {
				cancel()
				for range graphIDs {
					select {
					case err := <-results:
						if err != nil {
							t.Error(err)
						}
					case <-time.After(3 * time.Second):
						t.Error("router worker failed to stop")
					}
				}
			})
			wait := func(cost uint32, iface string) {
				t.Helper()
				deadline := time.Now().Add(7 * time.Second)
				for time.Now().Before(deadline) {
					route, _, ok := routers["u"].Engine.FIB.Load().Lookup(addr("10.45.5.2"))
					if ok && route.Metric == cost && route.Interface == iface {
						return
					}
					time.Sleep(20 * time.Millisecond)
				}
				t.Fatalf("runtime %s did not converge to cost %d via %s: %+v", algorithm, cost, iface, routers["u"].Engine.FIB.Load().Routes())
			}
			wait(4, "to-x")
			if err := routers["u"].changeImpairment(impairmentCommand{Interface: "to-x", Setting: "cost", Value: 60}); err != nil {
				t.Fatal(err)
			}
			wait(7, "to-v")
			if err := routers["u"].changeImpairment(impairmentCommand{Setting: "clear"}); err != nil {
				t.Fatal(err)
			}
			wait(4, "to-x")
		})
	}
}

func TestRoutingObserverFailureIsFatal(t *testing.T) {
	c := graphConfigs("ls")["u"]
	c.Observability = ObservationConfig{Directory: t.TempDir(), Socket: filepath.Join(t.TempDir(), "r.sock")}
	e, err := NewEngine(c)
	if err != nil {
		t.Fatal(err)
	}
	o, err := OpenObserver(c)
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	links := map[string]Link{}
	for i, iface := range c.Interfaces {
		links[iface.Name] = &fakeEthernet{newFakeLink(), MAC{2, 0, 0, 0, 0, byte(i + 1)}}
	}
	r, err := NewRuntime(e, links, o)
	if err != nil {
		t.Fatal(err)
	}
	if err := o.routing.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.tickRouting(time.Now()); err == nil || !strings.Contains(err.Error(), "routing export") {
		t.Fatal("routing export failure was swallowed", err)
	}
}
