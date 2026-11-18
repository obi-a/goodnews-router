//go:build linux

package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	goodnews "github.com/obi-a/goodnews-router"
	"path/filepath"
	"strings"
	"time"
)

type inspection struct {
	socket, resource string
	lines            []string
	at               time.Time
}

// inspect polls only the selected router. Pausing freezes the displayed snapshot;
// requests and router operation never depend on the display being live.
func inspect(ctx context.Context, terminal *terminal, keys <-chan string, socket string) (bool, error) {
	sockets, _ := filepath.Glob(filepath.Join(filepath.Dir(socket), "*.sock"))
	if !contains(sockets, socket) {
		sockets = append(sockets, socket)
	}
	resources := []string{"routes", "neighbors", "lsdb", "counters", "interfaces", "arp", "impairments"}
	router, section, offset := 0, 0, 0
	for i, s := range sockets {
		if s == socket {
			router = i
		}
	}
	paused := false
	var shown inspection
	requests := make(chan inspection, 1)
	results := make(chan inspection, 1)
	workerCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	defer func() { cancel(); <-done }()
	go func() {
		defer close(done)
		for {
			select {
			case <-workerCtx.Done():
				return
			case request := <-requests:
				client := NewClient(request.socket)
				var value any
				err := client.get(workerCtx, request.resource, &value)
				client.Close()
				request.at = time.Now()
				if err != nil {
					request.lines = []string{err.Error()}
				} else {
					if request.resource == "routes" {
						data, _ := json.Marshal(value)
						var table goodnews.FIBSnapshot
						if err := json.Unmarshal(data, &table); err != nil {
							request.lines = []string{err.Error()}
						} else {
							request.lines = inspectionRoutes(table)
						}
					} else {
						data, _ := json.MarshalIndent(value, "", "  ")
						request.lines = strings.Split(string(data), "\n")
					}
				}
				select {
				case results <- request:
				case <-workerCtx.Done():
					return
				}
			}
		}
	}()
	poll := func() {
		if paused {
			return
		}
		request := inspection{socket: sockets[router], resource: resources[section]}
		select {
		case requests <- request:
		default:
		}
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	poll()
	for {
		width, height := terminal.Size()
		mode := "LIVE"
		if paused {
			mode = "PAUSED (routers continue running)"
		}
		lines := []string{fmt.Sprintf("goodnews | ROUTER INSPECTOR | %s", mode), "Router: " + filepath.Base(sockets[router]) + " | " + resources[section], "Routers: " + strings.Join(socketNames(sockets), "  "), "Sections: " + strings.Join(resources, " | ")}
		body := []string{"Waiting for snapshot..."}
		if shown.socket == sockets[router] && shown.resource == resources[section] {
			lines = append(lines, "Snapshot: "+shown.at.Local().Format("15:04:05.000"))
			body = shown.lines
		}
		room := max(1, height-len(lines)-2)
		offset = min(offset, max(0, len(body)-room))
		lines = append(lines, body[offset:min(len(body), offset+room)]...)
		for len(lines) < max(0, height-2) {
			lines = append(lines, "")
		}
		lines = append(lines, "n/p router | Tab section | Space pause/resume | Up/Down scroll", "PgUp/PgDn scroll | t packet dashboard | q quit")
		for i := range lines {
			lines[i] = clip(lines[i], max(1, width-1))
		}
		if err := terminal.Draw(lines[:min(len(lines), max(0, height))]); err != nil {
			return false, err
		}
		select {
		case <-ctx.Done():
			return true, nil
		case <-ticker.C:
			poll()
		case result := <-results:
			if !paused && result.socket == sockets[router] && result.resource == resources[section] {
				shown = result
			}
		case key := <-keys:
			switch key {
			case "quit":
				return true, nil
			case "tables", "escape":
				return false, nil
			case "pause":
				paused = !paused
				poll()
			case "next-router", "previous-router":
				step := 1
				if key == "previous-router" {
					step = -1
				}
				router = (router + step + len(sockets)) % len(sockets)
				offset = 0
				poll()
			case "tab":
				section = (section + 1) % len(resources)
				offset = 0
				poll()
			case "up", "wheelup":
				offset = max(0, offset-1)
			case "down", "wheeldown":
				offset++
			case "pageup":
				offset = max(0, offset-room)
			case "pagedown":
				offset += room
			case "home":
				offset = 0
			}
		}
	}
}

func socketNames(sockets []string) []string {
	names := make([]string, len(sockets))
	for i, s := range sockets {
		names[i] = filepath.Base(s)
	}
	return names
}

func inspectionRoutes(table goodnews.FIBSnapshot) []string {
	lines := []string{fmt.Sprintf("FORWARDING TABLE v%d | %d routes", table.Version, len(table.Routes)), "Prefix              Next hop         Interface      Origin     Cost Hops"}
	for _, route := range table.Routes {
		hop := "direct"
		if route.NextHop.IsValid() {
			hop = route.NextHop.String()
		}
		lines = append(lines, fmt.Sprintf("%-19s %-16s %-14s %-10s %4d %4d", route.Prefix, hop, route.Interface, route.Origin, route.Metric, route.Hops))
	}
	return lines
}
