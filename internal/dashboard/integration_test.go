package dashboard

import (
	"context"
	"encoding/binary"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	goodnews "github.com/obi-a/goodnews-router"
)

type memoryLink struct {
	input chan []byte
	done  chan struct{}
	once  sync.Once
}

func (l *memoryLink) Receive() ([]byte, error) {
	select {
	case p := <-l.input:
		return p, nil
	case <-l.done:
		return nil, os.ErrClosed
	}
}
func (l *memoryLink) Send([]byte, netip.Addr) error { return nil }
func (l *memoryLink) Close() error                  { l.once.Do(func() { close(l.done) }); return nil }

// Exercise the real router worker, recorder, Unix HTTP API and history reader
// without network namespaces or root privileges.
func TestLiveRouterHistoryIntegration(t *testing.T) {
	socketDir, err := os.MkdirTemp("", "gn-live-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(socketDir)
	c := goodnews.Config{Version: 1, RouterID: "r1", Interfaces: []goodnews.Interface{
		{Name: "left", Address: "10.0.0.1/24", MTU: 1500, Driver: "tun", Device: "left0"},
		{Name: "right", Address: "10.0.1.1/24", MTU: 1500, Driver: "tun", Device: "right0"},
	}, Observability: goodnews.ObservationConfig{Directory: t.TempDir(), Socket: filepath.Join(socketDir, "r.sock")}}
	e, err := goodnews.NewEngine(c)
	if err != nil {
		t.Fatal(err)
	}
	o, err := goodnews.OpenObserver(c)
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	left := &memoryLink{make(chan []byte, 4), make(chan struct{}), sync.Once{}}
	right := &memoryLink{make(chan []byte, 4), make(chan struct{}), sync.Once{}}
	r, err := goodnews.NewRuntime(e, map[string]goodnews.Link{"left": left, "right": right}, o)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	completed := make(chan error, 2)
	go func() { completed <- r.Run(ctx) }()
	go func() { completed <- goodnews.ServeManagement(ctx, r, c.Observability.Socket) }()
	defer func() {
		cancel()
		for range 2 {
			select {
			case err := <-completed:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(3 * time.Second):
				t.Error("router/API goroutine did not stop")
			}
		}
	}()
	client := NewClient(c.Observability.Socket)
	defer client.Close()
	wait := func(description string, predicate func() bool) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for !predicate() {
			if time.Now().After(deadline) {
				t.Fatal(description)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	wait("management API never ready", func() bool { _, err := client.Status(ctx); return err == nil })
	icmp := []byte{8, 0, 0, 0, 0, 42, 0, 1}
	binary.BigEndian.PutUint16(icmp[2:4], goodnews.Checksum(icmp))
	packet, _ := goodnews.BuildIPv4(netip.MustParseAddr("10.0.0.2"), netip.MustParseAddr("10.0.1.2"), 1, 64, icmp)
	left.input <- packet
	wait("first packet not processed", func() bool { s := r.Snapshot(); return s.AfterPacketID != nil && *s.AfterPacketID == 1 })
	changed := c
	changed.Routes = []goodnews.StaticRoute{{Prefix: "192.0.2.0/24", Interface: "right"}}
	table, _ := goodnews.BuildTable(changed, 2)
	e.FIB.Store(table)
	left.input <- packet
	wait("second packet not processed", func() bool { s := r.Snapshot(); return s.AfterPacketID != nil && *s.AfterPacketID == 2 })
	live := client.Snapshot(ctx)
	if live.Err != nil || live.Counters.Received != 2 || live.Table.Version != 2 {
		t.Fatalf("%+v", live)
	}
	s, err := OpenStore(live.Info.ArtifactDirectory)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	wait("recorded history not readable", func() bool {
		if err := s.Poll(100); err != nil {
			t.Fatal(err)
		}
		if s.Count() != 2 {
			return false
		}
		second, _ := s.Event(1)
		return s.Counters(&second) != nil
	})
	m := NewModel()
	m.Selected, m.Follow = 0, false
	m.Observe(live, s.Info.RunID)
	view := strings.Join(m.Lines(s, 120, 30), "\n")
	if !strings.Contains(view, "RX 1  TX 1") || !strings.Contains(view, "TABLE v1") || strings.Contains(view, "TABLE v2") {
		t.Fatalf("past state incorrect:\n%s", view)
	}
	m.Key("end", s.Count(), 30)
	view = strings.Join(m.Lines(s, 120, 30), "\n")
	if !strings.Contains(view, "LIVE") || !strings.Contains(view, "RX 2  TX 2") || !strings.Contains(view, "TABLE v2") {
		t.Fatalf("live state incorrect:\n%s", view)
	}
}
