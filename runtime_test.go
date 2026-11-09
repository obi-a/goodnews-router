package goodnews

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type fakeLink struct {
	input   chan []byte
	closed  chan struct{}
	once    sync.Once
	sent    [][]byte
	sendErr error
}

func newFakeLink() *fakeLink {
	return &fakeLink{input: make(chan []byte, 8), closed: make(chan struct{})}
}
func (f *fakeLink) Receive() ([]byte, error) {
	select {
	case p := <-f.input:
		return p, nil
	case <-f.closed:
		return nil, io.EOF
	}
}
func (f *fakeLink) Send(p []byte, _ netip.Addr) error {
	if f.sendErr != nil {
		return f.sendErr
	}
	f.sent = append(f.sent, append([]byte(nil), p...))
	return nil
}
func (f *fakeLink) Close() error { f.once.Do(func() { close(f.closed) }); return nil }

type memoryObserver struct {
	traces   []TraceRecord
	captures int
	fail     string
}

func (o *memoryObserver) Capture(_ string, _ time.Time, _ []byte) error {
	o.captures++
	if o.fail == "capture" {
		return errors.New("disk full")
	}
	return nil
}
func (o *memoryObserver) Trace(t TraceRecord) error {
	o.traces = append(o.traces, t)
	if o.fail == "trace" {
		return errors.New("disk full")
	}
	return nil
}
func (o *memoryObserver) Counters(_ CounterSnapshot) error {
	if o.fail == "counters" {
		return errors.New("disk full")
	}
	return nil
}

func testRuntime(t *testing.T, observer PacketObserver) (*Runtime, *fakeLink, *fakeLink) {
	t.Helper()
	e, err := NewEngine(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	left, right := newFakeLink(), newFakeLink()
	r, err := NewRuntime(e, map[string]Link{"left": left, "right": right}, observer)
	if err != nil {
		t.Fatal(err)
	}
	return r, left, right
}

func TestRuntimeCountersAndSendFailure(t *testing.T) {
	o := &memoryObserver{}
	r, _, right := testRuntime(t, o)
	p := testPacket(t, "10.0.0.2", "10.0.1.2", 1, 64, testEcho())
	if err := r.process("left", p); err != nil {
		t.Fatal(err)
	}
	right.sendErr = errors.New("link down")
	if err := r.process("left", p); err != nil {
		t.Fatal(err)
	}
	bad := append([]byte(nil), p...)
	bad[10] ^= 1
	if err := r.process("left", bad); err != nil {
		t.Fatal(err)
	}
	s := r.Snapshot()
	if s.Received != 3 || s.Transmitted != 1 || s.Dropped != 2 || s.DropReasons["send_failed"] != 1 || s.DropReasons["invalid_checksum"] != 1 {
		t.Fatalf("%+v", s)
	}
	if len(o.traces) != 3 || o.traces[1].Error != "link down" || o.traces[1].Action != "drop" {
		t.Fatal("send failure trace missing")
	}
	s.DropReasons["invalid_checksum"] = 999
	if r.Snapshot().DropReasons["invalid_checksum"] != 1 {
		t.Fatal("snapshot aliases live map")
	}
}

func TestRuntimeGeneratedPacketIDs(t *testing.T) {
	o := &memoryObserver{}
	r, _, _ := testRuntime(t, o)
	if err := r.process("left", testPacket(t, "10.0.0.2", "10.0.1.2", 1, 1, testEcho())); err != nil {
		t.Fatal(err)
	}
	if len(o.traces) != 2 || o.traces[1].ParentID == nil || *o.traces[1].ParentID != o.traces[0].PacketID || o.traces[1].PacketID == o.traces[0].PacketID {
		t.Fatal("lost generated-packet relationship")
	}
	if s := r.Snapshot(); s.Dropped != 1 || s.Transmitted != 1 || s.DropReasons["ttl_expired"] != 1 {
		t.Fatalf("%+v", s)
	}
}

func TestRuntimeStopsOnObservationFailure(t *testing.T) {
	for _, where := range []string{"capture", "trace", "counters"} {
		t.Run(where, func(t *testing.T) {
			r, _, _ := testRuntime(t, &memoryObserver{fail: where})
			if err := r.process("left", testPacket(t, "10.0.0.2", "10.0.1.2", 1, 64, testEcho())); err == nil {
				t.Fatal("silently ignored observation failure")
			}
		})
	}
}

func TestRuntimeCancellationUnblocksReaders(t *testing.T) {
	r, _, _ := testRuntime(t, &memoryObserver{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked reader leaked on shutdown")
	}
}

func TestManagementQueries(t *testing.T) {
	r, _, _ := testRuntime(t, &memoryObserver{})
	handler := r.ManagementHandler()
	for _, tc := range []struct {
		resource string
		status   int
	}{{"interfaces", 200}, {"routes", 200}, {"counters", 200}, {"arp", 501}, {"neighbors", 501}, {"lsdb", 501}} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/"+tc.resource, nil))
		if response.Code != tc.status || !json.Valid(response.Body.Bytes()) {
			t.Fatalf("%s: %d %s", tc.resource, response.Code, response.Body)
		}
		if tc.resource == "interfaces" && !bytes.Contains(response.Body.Bytes(), []byte(`"decrement_ttl":true`)) {
			t.Fatal("effective protection missing")
		}
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/routes", nil))
	if response.Code != 405 {
		t.Fatal("management must be read-only")
	}
}

func TestPCAPAndJSONLArtifacts(t *testing.T) {
	c := testConfig()
	c.Observability.Directory = t.TempDir()
	o, err := OpenObserver(c)
	if err != nil {
		t.Fatal(err)
	}
	r, _, _ := testRuntime(t, o)
	p := testPacket(t, "10.0.0.2", "10.0.1.2", 1, 64, testEcho())
	if err := r.process("left", p); err != nil {
		t.Fatal(err)
	}
	if err := o.Close(); err != nil {
		t.Fatal(err)
	}
	left, err := os.ReadFile(filepath.Join(o.Directory, "left.pcap"))
	if err != nil {
		t.Fatal(err)
	}
	right, _ := os.ReadFile(filepath.Join(o.Directory, "right.pcap"))
	if binary.LittleEndian.Uint32(left[20:24]) != 101 || len(left) != 40+len(p) || !bytes.Equal(left[40:], p) {
		t.Fatal("invalid ingress pcap")
	}
	h, err := ParseIPv4(right[40:], true)
	if err != nil || h.TTL != 63 {
		t.Fatal("invalid egress pcap")
	}
	for _, name := range []string{"trace.jsonl", "counters.jsonl"} {
		data, _ := os.ReadFile(filepath.Join(o.Directory, name))
		for _, line := range bytes.Split(bytes.TrimSpace(data), []byte{'\n'}) {
			if !json.Valid(line) {
				t.Fatalf("invalid JSONL %s", name)
			}
		}
	}
}
