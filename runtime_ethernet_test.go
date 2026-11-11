package goodnews

import (
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeEthernet struct {
	*fakeLink
	mac MAC
}

func (f *fakeEthernet) HardwareAddr() MAC { return f.mac }

func ethernetRuntime(t *testing.T, observer PacketObserver) (*Runtime, *fakeEthernet, *fakeEthernet) {
	t.Helper()
	c := testConfig()
	for i := range c.Interfaces {
		c.Interfaces[i].Driver = "ethernet"
	}
	e, err := NewEngine(c)
	if err != nil {
		t.Fatal(err)
	}
	l := &fakeEthernet{newFakeLink(), MAC{2, 0, 0, 0, 0, 1}}
	r := &fakeEthernet{newFakeLink(), MAC{2, 0, 0, 0, 1, 1}}
	router, err := NewRuntime(e, map[string]Link{"left": l, "right": r}, observer)
	if err != nil {
		t.Fatal(err)
	}
	return router, l, r
}

var hostLeft = MAC{2, 0, 0, 0, 0, 2}
var hostRight = MAC{2, 0, 0, 0, 1, 2}

func learnLeft(t *testing.T, r *Runtime, l *fakeEthernet) {
	t.Helper()
	frame := BuildEthernet(BroadcastMAC, hostLeft, EtherTypeARP, BuildARP(ARPPacket{Operation: 1, SenderMAC: hostLeft, SenderIP: addr("10.0.0.2"), TargetIP: addr("10.0.0.1")}))
	if err := r.process("left", frame); err != nil {
		t.Fatal(err)
	}
	reply, _ := ParseEthernet(l.sent[len(l.sent)-1])
	arp, err := ParseARP(reply.Payload)
	if err != nil || arp.Operation != 2 || arp.SenderMAC != l.mac || arp.TargetMAC != hostLeft || arp.SenderIP != addr("10.0.0.1") {
		t.Fatal("router did not answer ARP")
	}
}

func TestEthernetRuntimeResolutionAndNonblockingQueue(t *testing.T) {
	o := &memoryObserver{}
	r, left, right := ethernetRuntime(t, o)
	learnLeft(t, r, left)
	p := testPacket(t, "10.0.0.2", "10.0.1.2", 1, 64, testEcho())
	if err := r.process("left", BuildEthernet(left.mac, hostLeft, EtherTypeIPv4, p)); err != nil {
		t.Fatal(err)
	}
	if len(right.sent) != 1 {
		t.Fatal("queued IP was sent before ARP")
	}
	f, _ := ParseEthernet(right.sent[0])
	arp, _ := ParseARP(f.Payload)
	if f.Destination != BroadcastMAC || arp.TargetIP != addr("10.0.1.2") || arp.Operation != 1 {
		t.Fatal("wrong next hop resolution")
	}
	local := testPacket(t, "10.0.0.2", "10.0.0.1", 1, 64, testEcho())
	if err := r.process("left", BuildEthernet(left.mac, hostLeft, EtherTypeIPv4, local)); err != nil {
		t.Fatal(err)
	}
	if len(left.sent) != 2 {
		t.Fatal("ARP queue blocked unrelated local reply")
	}
	response := BuildEthernet(right.mac, hostRight, EtherTypeARP, BuildARP(ARPPacket{Operation: 2, SenderMAC: hostRight, SenderIP: addr("10.0.1.2"), TargetMAC: right.mac, TargetIP: addr("10.0.1.1")}))
	if err := r.process("right", response); err != nil {
		t.Fatal(err)
	}
	if len(right.sent) != 2 {
		t.Fatal("ARP reply did not release queue")
	}
	f, _ = ParseEthernet(right.sent[1])
	h, err := ParseIPv4(f.Payload, true)
	if err != nil || h.TTL != 63 || f.Destination != hostRight || f.Source != right.mac {
		t.Fatalf("invalid forwarded frame: %v", err)
	}
	if err := r.process("left", BuildEthernet(left.mac, hostLeft, EtherTypeIPv4, p)); err != nil {
		t.Fatal(err)
	}
	if len(right.sent) != 3 {
		t.Fatal("cache hit did not send directly")
	}
	for i, trace := range o.traces {
		if i > 0 && trace.PacketID <= o.traces[i-1].PacketID {
			t.Fatal("nonmonotonic history IDs")
		}
	}
	if len(r.ARPSnapshot()["right"]) != 1 || r.ARPSnapshot()["right"][0].State != "reachable" {
		t.Fatal("missing cache view")
	}
}

func TestEthernetRuntimeTimeoutAndSenderValidation(t *testing.T) {
	o := &memoryObserver{}
	r, left, right := ethernetRuntime(t, o)
	p := testPacket(t, "10.0.0.2", "10.0.1.2", 1, 64, testEcho())
	if err := r.process("left", BuildEthernet(left.mac, hostLeft, EtherTypeIPv4, p)); err != nil {
		t.Fatal(err)
	}
	base := time.Now()
	for i := 1; i <= ARPMaxAttempts; i++ {
		if err := r.tickLinks(base.Add(time.Duration(i) * time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if len(right.sent) != ARPMaxAttempts || r.Snapshot().DropReasons["arp_timeout"] != 1 {
		t.Fatal("missing bounded retries or timeout drop")
	}
	for _, tc := range []struct {
		frame  []byte
		reason string
	}{
		{[]byte{1}, "short_ethernet_frame"},
		{BuildEthernet(BroadcastMAC, hostLeft, EtherTypeIPv4, p), "unsupported_ethernet_broadcast"},
		{BuildEthernet(left.mac, hostLeft, 0x8100, []byte{1, 2, 3}), "unsupported_ethertype"},
		{BuildEthernet(left.mac, hostLeft, EtherTypeARP, BuildARP(ARPPacket{Operation: 2, SenderMAC: hostLeft, SenderIP: addr("10.0.0.2"), TargetMAC: left.mac, TargetIP: addr("10.0.0.1")})), "unsolicited_arp_reply"},
		{BuildEthernet(left.mac, hostLeft, EtherTypeARP, BuildARP(ARPPacket{Operation: 1, SenderMAC: hostRight, SenderIP: addr("10.0.0.2"), TargetIP: addr("10.0.0.1")})), "invalid_arp_sender"},
		{BuildEthernet(left.mac, hostLeft, EtherTypeARP, BuildARP(ARPPacket{Operation: 1, SenderMAC: hostLeft, SenderIP: addr("10.1.0.2"), TargetIP: addr("10.0.0.1")})), "invalid_arp_sender_ip"},
	} {
		if err := r.process("left", tc.frame); err != nil {
			t.Fatal(err)
		}
		if r.Snapshot().DropReasons[tc.reason] != 1 {
			t.Fatalf("missing %s", tc.reason)
		}
	}
}

func TestImpairmentsAndManagement(t *testing.T) {
	o := &memoryObserver{}
	r, left, right := ethernetRuntime(t, o)
	learnLeft(t, r, left)
	r.ports["right"].arp.learn(addr("10.0.1.2"), hostRight, time.Now())
	handler := r.ManagementHandler()
	post := func(body string, status int) {
		t.Helper()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/impairments", strings.NewReader(body)))
		if response.Code != status || !json.Valid(response.Body.Bytes()) {
			t.Fatalf("%d: %s", response.Code, response.Body)
		}
	}
	for _, body := range []string{`{}`, `{"interface":"missing","setting":"down"}`, `{"interface":"right","setting":"loss","value":101}`, `{"interface":"right","setting":"delay","value":0.5}`, `{"interface":"right","setting":"cost","value":10}`, `{"setting":"clear","extra":true}`, `{"setting":"clear"} {}`} {
		post(body, 400)
	}
	p := testPacket(t, "10.0.0.2", "10.0.1.2", 1, 64, testEcho())
	send := func() {
		t.Helper()
		if err := r.process("left", BuildEthernet(left.mac, hostLeft, EtherTypeIPv4, p)); err != nil {
			t.Fatal(err)
		}
	}
	post(`{"interface":"right","setting":"loss","value":100}`, 200)
	send()
	if len(right.sent) != 0 || r.Snapshot().DropReasons["impaired_loss"] != 1 {
		t.Fatal("loss had no effect")
	}
	post(`{"setting":"clear"}`, 200)
	post(`{"interface":"right","setting":"delay","value":500}`, 200)
	send()
	if len(right.sent) != 0 || len(r.delayed) != 1 {
		t.Fatal("delay did not queue")
	}
	if err := r.tickLinks(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if len(right.sent) != 0 {
		t.Fatal("delay sent too early")
	}
	post(`{"setting":"clear"}`, 200)
	if err := r.tickLinks(time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(right.sent) != 1 || len(r.delayed) != 0 {
		t.Fatal("clear did not release delayed packet")
	}
	post(`{"interface":"right","setting":"corruption","value":100}`, 200)
	send()
	f, _ := ParseEthernet(right.sent[len(right.sent)-1])
	if _, err := ParseIPv4(f.Payload, true); err == nil || err.Error() != "invalid_checksum" {
		t.Fatal("corruption did not reach wire")
	}
	post(`{"interface":"right","setting":"down"}`, 200)
	send()
	if r.Snapshot().DropReasons["link_down"] != 1 {
		t.Fatal("link down had no effect")
	}
	copy := r.Impairments()
	copy["right"] = Impairment{}
	if !r.Impairments()["right"].Down {
		t.Fatal("management snapshot aliases state")
	}
	for _, resource := range []string{"arp", "impairments"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/"+resource, nil))
		if response.Code != 200 {
			t.Fatal(resource)
		}
	}
}

func TestEthernetPCAP(t *testing.T) {
	c := testConfig()
	c.Observability.Directory = t.TempDir()
	for i := range c.Interfaces {
		c.Interfaces[i].Driver = "ethernet"
	}
	o, err := OpenObserver(c)
	if err != nil {
		t.Fatal(err)
	}
	r, left, _ := ethernetRuntime(t, o)
	learnLeft(t, r, left)
	if err := o.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(o.Directory, "left.pcap"))
	if err != nil {
		t.Fatal(err)
	}
	if binary.LittleEndian.Uint32(data[20:24]) != 1 || binary.LittleEndian.Uint32(data[32:36]) != 60 || len(data) != 24+2*(16+60) {
		t.Fatal("pcap is not complete Ethernet frames")
	}
}

func TestARPResolvesGatewayAndRetainsSelectedFIB(t *testing.T) {
	o := &memoryObserver{}
	r, left, right := ethernetRuntime(t, o)
	c := r.Engine.Config
	c.Routes = []StaticRoute{{Prefix: "192.0.2.0/24", Interface: "right", NextHop: "10.0.1.254"}}
	table, err := BuildTable(c, 2)
	if err != nil {
		t.Fatal(err)
	}
	r.Engine.FIB.Store(table)
	p := testPacket(t, "10.0.0.2", "192.0.2.10", 1, 64, testEcho())
	if err := r.process("left", BuildEthernet(left.mac, hostLeft, EtherTypeIPv4, p)); err != nil {
		t.Fatal(err)
	}
	f, _ := ParseEthernet(right.sent[0])
	a, _ := ParseARP(f.Payload)
	if a.TargetIP != addr("10.0.1.254") {
		t.Fatal("resolved destination instead of gateway")
	}
	newTable, _ := BuildTable(r.Engine.Config, 3)
	r.Engine.FIB.Store(newTable)
	reply := BuildEthernet(right.mac, hostRight, EtherTypeARP, BuildARP(ARPPacket{Operation: 2, SenderMAC: hostRight, SenderIP: a.TargetIP, TargetMAC: right.mac, TargetIP: a.SenderIP}))
	if err := r.process("right", reply); err != nil {
		t.Fatal(err)
	}
	last := o.traces[len(o.traces)-1]
	if last.Action != "sent" || last.FIBVersion != 2 || last.ParentID == nil || last.CounterPacketID == nil || *last.CounterPacketID != last.PacketID {
		t.Fatalf("lost original route or completion snapshot: %+v", last)
	}
}

func TestDelayBoundAndShutdownDrops(t *testing.T) {
	r, _, right := testRuntime(t, &memoryObserver{})
	if err := r.changeImpairment(impairmentCommand{Interface: "right", Setting: "delay", Value: 10000}); err != nil {
		t.Fatal(err)
	}
	p := testPacket(t, "10.0.0.2", "10.0.1.2", 1, 64, testEcho())
	for i := 0; i <= MaxDelayedPackets; i++ {
		if err := r.process("left", p); err != nil {
			t.Fatal(err)
		}
	}
	if len(r.delayed) != MaxDelayedPackets || len(right.sent) != 0 || r.Snapshot().DropReasons["delay_queue_full"] != 1 {
		t.Fatal("unbounded delay queue")
	}
	if err := r.shutdownPending(); err != nil {
		t.Fatal(err)
	}
	if len(r.delayed) != 0 || r.Snapshot().DropReasons["router_shutdown"] != MaxDelayedPackets {
		t.Fatal("shutdown silently discarded delayed packets")
	}
	r, left, _ := ethernetRuntime(t, &memoryObserver{})
	if err := r.process("left", BuildEthernet(left.mac, hostLeft, EtherTypeIPv4, p)); err != nil {
		t.Fatal(err)
	}
	if err := r.shutdownPending(); err != nil {
		t.Fatal(err)
	}
	if r.Snapshot().DropReasons["router_shutdown"] != 1 || len(r.ARPSnapshot()["right"]) != 0 {
		t.Fatal("shutdown silently discarded ARP queue")
	}
}
