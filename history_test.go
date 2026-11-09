package goodnews

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHistoricalObservation(t *testing.T) {
	c := testConfig()
	c.Observability.Directory = t.TempDir()
	o, err := OpenObserver(c)
	if err != nil {
		t.Fatal(err)
	}
	r, _, _ := testRuntime(t, o)
	packet := testPacket(t, "10.0.0.2", "10.0.1.2", 1, 64, testEcho())
	if err := r.process("left", packet); err != nil {
		t.Fatal(err)
	}
	// A different control-plane table must not replace the historical version.
	changed := testConfig()
	changed.Routes = []StaticRoute{{Prefix: "192.0.2.0/24", Interface: "right"}}
	table, _ := BuildTable(changed, 2)
	r.Engine.FIB.Store(table)
	packet = testPacket(t, "10.0.0.2", "192.0.2.1", 1, 1, testEcho())
	if err := r.process("left", packet); err != nil {
		t.Fatal(err)
	}
	if err := o.Close(); err != nil {
		t.Fatal(err)
	}
	var manifest RunInfo
	data, _ := os.ReadFile(filepath.Join(o.Directory, "run.json"))
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Version != 1 || !filepath.IsAbs(manifest.ArtifactDirectory) || len(manifest.Interfaces) != 2 || !manifest.Protections["decrement_ttl"] {
		t.Fatalf("%+v", manifest)
	}
	decodeLines := func(name string, consume func([]byte)) {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(o.Directory, name))
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			consume([]byte(line))
		}
	}
	var tables []FIBSnapshot
	decodeLines("fib.jsonl", func(b []byte) {
		var v FIBSnapshot
		if err := json.Unmarshal(b, &v); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, v)
	})
	if len(tables) != 2 || tables[0].Version != 1 || len(tables[0].Routes) != 2 || tables[1].Version != 2 || len(tables[1].Routes) != 3 {
		t.Fatalf("%+v", tables)
	}
	var counters []CounterSnapshot
	decodeLines("counters.jsonl", func(b []byte) {
		var v CounterSnapshot
		if err := json.Unmarshal(b, &v); err != nil {
			t.Fatal(err)
		}
		counters = append(counters, v)
	})
	if len(counters) != 3 || *counters[0].AfterPacketID != 0 || *counters[1].AfterPacketID != 1 || *counters[2].AfterPacketID != 2 || counters[2].Dropped != 1 {
		t.Fatalf("%+v", counters)
	}
	var traces []TraceRecord
	decodeLines("trace.jsonl", func(b []byte) {
		var v TraceRecord
		if err := json.Unmarshal(b, &v); err != nil {
			t.Fatal(err)
		}
		traces = append(traces, v)
	})
	if len(traces) != 3 || traces[0].ICMP == nil || traces[0].ICMP.Type != 8 || traces[0].ICMP.Identifier == nil || *traces[0].ICMP.Identifier != 42 {
		t.Fatalf("%+v", traces)
	}
	if traces[1].FIBVersion != 2 || traces[2].ICMP.Type != 11 || traces[2].ParentID == nil || *traces[2].ParentID != 2 {
		t.Fatalf("%+v", traces)
	}
	response := httptest.NewRecorder()
	r.ManagementHandler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/status", nil))
	var info RunInfo
	if err := json.Unmarshal(response.Body.Bytes(), &info); err != nil || info.RunID != manifest.RunID {
		t.Fatalf("%s %v", response.Body, err)
	}
	for _, name := range []string{"run.json", "fib.jsonl", "trace.jsonl", "counters.jsonl"} {
		stat, err := os.Stat(filepath.Join(o.Directory, name))
		if err != nil || stat.Mode().Perm() != 0600 {
			t.Fatalf("%s mode: %v %v", name, stat, err)
		}
	}
}

func TestICMPDescriptionBounds(t *testing.T) {
	for length := 0; length < 9; length++ {
		p := make([]byte, length)
		if length > 0 {
			p[0] = 8
		}
		h, _ := ParseIPv4(testPacket(t, "10.0.0.2", "10.0.1.2", 1, 64, p), true)
		fields := DescribeICMP(h)
		if length < 2 && fields != nil {
			t.Fatal("decoded absent ICMP header")
		}
		if length >= 2 && (fields == nil || fields.Type != 8) {
			t.Fatal("missing type/code")
		}
		if fields != nil && ((length >= 8) != (fields.Sequence != nil)) {
			t.Fatal("decoded absent echo sequence")
		}
	}
	if DescribeICMP(IPv4{Protocol: 1, HeaderLength: 100}) != nil {
		t.Fatal("unsafe header accepted")
	}
}
