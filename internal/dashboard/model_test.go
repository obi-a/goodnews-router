package dashboard

import (
	"fmt"
	"strings"
	"testing"

	goodnews "github.com/obi-a/goodnews-router"
)

func TestNavigationFreezesHistoricalState(t *testing.T) {
	s := demoStore(t)
	m := NewModel()
	m.Sync(s.Count())
	if m.Selected != 6 || !m.Follow {
		t.Fatal("not following latest")
	}
	m.Key("home", s.Count(), 24)
	m.Sync(s.Count() + 100)
	if m.Selected != 0 || m.Follow {
		t.Fatal("new arrivals moved historical selection")
	}
	m.Live = &LiveSnapshot{Info: s.Info, Counters: goodnews.CounterSnapshot{Received: 999}, Table: goodnews.FIBSnapshot{Version: 999}}
	view := strings.Join(m.Lines(s, 120, 30), "\n")
	if !strings.Contains(view, "RX 1  TX 1") || !strings.Contains(view, "TABLE v1") || strings.Contains(view, "TABLE v999") {
		t.Fatalf("historical summary drifted:\n%s", view)
	}
	m.Key("tab", s.Count(), 24)
	m.Key("down", s.Count(), 24)
	if m.Selected != 0 || m.RouteOffset != 1 {
		t.Fatal("route scrolling moved packet selection")
	}
	m.Key("enter", s.Count(), 24)
	if !m.Details {
		t.Fatal("details did not open")
	}
	m.Key("pagedown", s.Count(), 24)
	if m.DetailOffset == 0 {
		t.Fatal("details did not scroll")
	}
	m.Key("escape", s.Count(), 24)
	m.Key("end", s.Count(), 24)
	if !m.Follow || m.Selected != 6 || m.Details {
		t.Fatal("End failed to resume following")
	}
}

func TestRenderingAndPacketDetails(t *testing.T) {
	s := demoStore(t)
	m := NewModel()
	m.Selected, m.Follow = 3, false
	for _, size := range [][2]int{{80, 24}, {120, 40}, {20, 4}, {0, 0}} {
		lines := m.Lines(s, size[0], size[1])
		if len(lines) > size[1] {
			t.Fatal("screen overflow")
		}
		for _, line := range lines {
			if len([]rune(line)) > max(1, size[0]-1) {
				t.Fatalf("width overflow: %q", line)
			}
		}
	}
	m.Details = true
	view := strings.Join(m.Lines(s, 120, 40), "\n")
	for _, expected := range []string{"Time exceeded", "ICMP type=11 code=0", "ingress #3", "Related packets:", "#3 Echo request"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("missing %q:\n%s", expected, view)
		}
	}
	if clean("evil\x1b[2J\r\n") != "evil?[2J??" {
		t.Fatal("terminal escape not sanitized")
	}
	if packetName(goodnews.TraceRecord{}) != "unknown" {
		t.Fatal("invented protocol")
	}
}

func TestKeyDecoderFragmentedKeysAndMouse(t *testing.T) {
	var d keyDecoder
	if keys := d.Feed([]byte("\x1b[")); len(keys) != 0 {
		t.Fatal(keys)
	}
	if keys := d.Feed([]byte("A\x1b[6~\x1b[<64;20;10M\x1b[<65;20;10M")); fmt.Sprint(keys) != "[up pagedown wheelup wheeldown]" {
		t.Fatal(keys)
	}
	d.Feed([]byte("\x1b"))
	if keys := d.Flush(); fmt.Sprint(keys) != "[escape]" {
		t.Fatal(keys)
	}
	if keys := d.Feed([]byte("\x1b[Cq")); fmt.Sprint(keys) != "[quit]" {
		t.Fatalf("unknown sequence leaked: %v", keys)
	}
}

func TestRouteScrollingDoesNotAccumulateBeyondTheEnd(t *testing.T) {
	s := demoStore(t)
	m := NewModel()
	m.Sync(s.Count())
	m.RouteFocus, m.RouteOffset = true, 1000
	m.Lines(s, 80, 24)
	if m.RouteOffset != 0 {
		t.Fatal("route offset accumulated beyond the visible table")
	}
	m.Key("up", s.Count(), 24)
	if m.RouteOffset != 0 {
		t.Fatal("scrolling above the first route")
	}
}
