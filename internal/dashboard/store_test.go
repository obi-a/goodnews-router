package dashboard

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	goodnews "github.com/obi-a/goodnews-router"
)

func demoStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore("testdata/demo")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := s.Poll(100); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestHistoricalStateAndRelatedPackets(t *testing.T) {
	s := demoStore(t)
	if s.Count() != 7 || s.Legacy {
		t.Fatalf("count=%d legacy=%v", s.Count(), s.Legacy)
	}
	old, _ := s.Event(0)
	newer, _ := s.Event(6)
	if table := s.Table(old.FIBVersion); table == nil || len(table.Routes) != 2 {
		t.Fatal("lost historical table")
	}
	if table := s.Table(newer.FIBVersion); table == nil || len(table.Routes) != 3 {
		t.Fatal("lost later table")
	}
	if counts := s.Counters(&old); counts == nil || counts.Received != 1 {
		t.Fatal("wrong old counters")
	}
	if counts := s.Counters(&newer); counts == nil || counts.Received != 5 {
		t.Fatal("wrong new counters")
	}
	parent, _ := s.Event(2)
	child, _ := s.Event(3)
	if s.Counters(&parent).Received != s.Counters(&child).Received || *s.Counters(&child).AfterPacketID != parent.PacketID {
		t.Fatal("generated packet lost parent processing snapshot")
	}
	if related := s.Related(child); len(related) != 1 || related[0].PacketID != 3 {
		t.Fatalf("%+v", related)
	}
	if row, found := s.RowForID(7); !found || row != 6 {
		t.Fatal("packet ID lookup failed")
	}
	if s.Counters(nil).Received != 0 {
		t.Fatal("initial snapshot missing")
	}
	if got := s.Peers("left", -1); len(got) != 0 {
		t.Fatal("future peer leaked into initial topology")
	}
}

func TestIncrementalHistoryBeyondCache(t *testing.T) {
	dir := t.TempDir()
	tracePath := filepath.Join(dir, "trace.jsonl")
	if err := os.WriteFile(tracePath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	temporary := s.temporary
	file, err := os.OpenFile(tracePath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	for i := 1; i <= cacheSize*3; i++ {
		event := goodnews.TraceRecord{Kind: "packet", PacketID: uint64(i), RouterID: "old-router", Action: "drop", DropReason: "invalid_checksum"}
		line, _ := json.Marshal(event)
		if _, err := file.Write(append(line, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	for range 20 {
		if err := s.Poll(50); err != nil {
			t.Fatal(err)
		}
	}
	if s.Count() != cacheSize*3 {
		t.Fatalf("count=%d", s.Count())
	}
	for row := int64(0); row < s.Count(); row++ {
		if _, err := s.Event(row); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.cache) > cacheSize {
		t.Fatal("decoded history grew beyond cache")
	}
	if first, err := s.Event(0); err != nil || first.PacketID != 1 {
		t.Fatal("earliest event unavailable after eviction")
	}
	if s.Counters(nil) != nil || s.Table(1) != nil || !s.Legacy {
		t.Fatal("invented legacy history")
	}
	// The last line is incomplete until the writer appends its newline.
	if _, err := fmt.Fprintf(file, `{"kind":"packet","packet_id":%d,"action":"forward"}`, cacheSize*3+1); err != nil {
		t.Fatal(err)
	}
	if err := s.Poll(10); err != nil {
		t.Fatal(err)
	}
	if !s.Incomplete() || s.Count() != cacheSize*3 {
		t.Fatal("partial line was published")
	}
	if _, err := file.WriteString("\n"); err != nil {
		t.Fatal(err)
	}
	if err := s.Poll(10); err != nil {
		t.Fatal(err)
	}
	if s.Incomplete() || s.Count() != cacheSize*3+1 {
		t.Fatal("completed append missing")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(temporary); !os.IsNotExist(err) {
		t.Fatal("temporary index left behind")
	}
}

func TestCorruptAndTruncatedLogs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trace.jsonl")
	data := "not json\n" + strings.Repeat("x", maxRecord+10) + "\n" + `{"kind":"packet","packet_id":1,"action":"drop"}` + "\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for range 4 {
		if err := s.Poll(100); err != nil {
			t.Fatal(err)
		}
	}
	if s.Count() != 1 || len(s.Warnings) != 2 {
		t.Fatalf("count=%d warnings=%v", s.Count(), s.Warnings)
	}
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.Poll(100); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(s.Warnings, " "), "truncated") {
		t.Fatal("silent truncation")
	}
}

func TestMissingAndBadManifest(t *testing.T) {
	if _, err := OpenStore(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing run silently accepted")
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "trace.jsonl"), nil, 0600)
	os.WriteFile(filepath.Join(dir, "run.json"), []byte("{bad}"), 0600)
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !s.Legacy || len(s.Warnings) != 1 {
		t.Fatal("bad manifest was trusted")
	}
}
