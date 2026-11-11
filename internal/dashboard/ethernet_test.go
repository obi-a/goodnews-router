package dashboard

import (
	"encoding/json"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	goodnews "github.com/obi-a/goodnews-router"
)

func TestEthernetAndDeferredHistory(t *testing.T) {
	dir := t.TempDir()
	trace, err := os.Create(filepath.Join(dir, "trace.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	counts, err := os.Create(filepath.Join(dir, "counters.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	parent := uint64(1)
	completion := uint64(cacheSize*3 + 1)
	encode := json.NewEncoder(trace)
	for id := uint64(1); id <= completion; id++ {
		event := goodnews.TraceRecord{Kind: "packet", PacketID: id, Action: "queue_arp"}
		if id == 2 {
			event.Kind = "ethernet"
			event.Action = "arp_request"
			event.SourceMAC = "02:00:00:00:00:01"
			event.ARP = &goodnews.ARPPacket{Operation: 1, SenderIP: netip.MustParseAddr("10.0.0.1"), TargetIP: netip.MustParseAddr("10.0.0.2")}
		}
		if id == completion {
			event.Action = "sent"
			event.ParentID = &parent
			event.CounterPacketID = &completion
		}
		if err := encode.Encode(event); err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range []goodnews.CounterSnapshot{{Kind: "counters", AfterPacketID: &parent, Transmitted: 0}, {Kind: "counters", AfterPacketID: &completion, Transmitted: 1}} {
		if err := json.NewEncoder(counts).Encode(row); err != nil {
			t.Fatal(err)
		}
	}
	if err := trace.Close(); err != nil {
		t.Fatal(err)
	}
	if err := counts.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Poll(1000); err != nil {
		t.Fatal(err)
	}
	if s.Count() != int64(completion) || len(s.Warnings) != 0 {
		t.Fatalf("bad history: %d %v", s.Count(), s.Warnings)
	}
	initial, _ := s.Event(0)
	last, _ := s.Event(int64(completion - 1))
	arp, _ := s.Event(1)
	if s.Counters(&initial).Transmitted != 0 || s.Counters(&last).Transmitted != 1 {
		t.Fatal("completion borrowed queued parent's counters")
	}
	related := s.Related(initial)
	if len(related) != 1 || related[0].PacketID != completion {
		t.Fatal("lost nonadjacent completion")
	}
	related = s.Related(last)
	if len(related) != 1 || related[0].PacketID != parent {
		t.Fatal("lost deferred parent")
	}
	if packetName(arp) != "ARP request" || !strings.Contains(strings.Join(detailLines(s, &arp, nil), "\n"), "10.0.0.2") {
		t.Fatal("ARP details not rendered")
	}
}
