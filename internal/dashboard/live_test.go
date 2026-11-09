package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	goodnews "github.com/obi-a/goodnews-router"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestClientDiscoveryAndRestart(t *testing.T) {
	client := NewClient("/unused.sock")
	defer client.Close()
	statusCalls, restart := 0, false
	client.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var value any
		switch request.URL.Path {
		case "/v1/status":
			statusCalls++
			id := "run-one"
			if restart && statusCalls%2 == 0 {
				id = "run-two"
			}
			value = goodnews.RunInfo{Version: 1, RunID: id, RouterID: "r1", ArtifactDirectory: "/some/private/run"}
		case "/v1/counters":
			value = goodnews.CounterSnapshot{Received: 6}
		case "/v1/routes":
			value = goodnews.FIBSnapshot{Version: 3, Routes: []goodnews.Route{}}
		default:
			t.Fatalf("unexpected endpoint %s", request.URL.Path)
		}
		data, _ := json.Marshal(value)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(data)))}, nil
	})
	snapshot := client.Snapshot(context.Background())
	if snapshot.Err != nil || snapshot.Counters.Received != 6 || snapshot.Table.Version != 3 {
		t.Fatalf("%+v", snapshot)
	}
	restart = true
	if snapshot := client.Snapshot(context.Background()); snapshot.Err == nil {
		t.Fatal("combined state from different runs")
	}
	client.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 404, Status: "404 Not Found", Body: io.NopCloser(strings.NewReader("not found"))}, nil
	})
	if _, err := client.Status(context.Background()); err == nil || !strings.Contains(err.Error(), "restart the rebuilt router") {
		t.Fatalf("unhelpful old-router error: %v", err)
	}
}

func TestConnectionUpdatesKeepHistoryPinned(t *testing.T) {
	m := NewModel()
	m.Follow, m.Selected = false, 3
	m.Observe(LiveSnapshot{Info: goodnews.RunInfo{RunID: "old"}}, "old")
	if m.Connection != "connected" || m.Selected != 3 || m.Follow {
		t.Fatal("connection update changed history")
	}
	m.Observe(LiveSnapshot{Err: errors.New("stopped")}, "old")
	if !strings.Contains(m.Connection, "disconnected") || m.Selected != 3 {
		t.Fatal("disconnect lost history")
	}
	m.Observe(LiveSnapshot{Info: goodnews.RunInfo{RunID: "new"}}, "old")
	if m.Connection != "new run available" || m.Selected != 3 || !strings.Contains(m.Message, "Press r") {
		t.Fatal("restart switched history silently")
	}
}
