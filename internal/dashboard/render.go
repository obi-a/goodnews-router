package dashboard

import (
	"fmt"
	"net/netip"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	goodnews "github.com/obi-a/goodnews-router"
)

// clean prevents packet/error strings from becoming terminal control sequences.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || !unicode.IsPrint(r) {
			return '?'
		}
		return r
	}, s)
}

func clip(s string, width int) string {
	r := []rune(clean(s))
	if width <= 0 {
		return ""
	}
	if len(r) > width {
		if width == 1 {
			return "~"
		}
		return string(r[:width-1]) + "~"
	}
	return string(r)
}

func address(a *netip.Addr) string {
	if a == nil {
		return "?"
	}
	return a.String()
}
func number(n *uint8) string {
	if n == nil {
		return "-"
	}
	return fmt.Sprint(*n)
}

func packetName(t goodnews.TraceRecord) string {
	if t.ICMP != nil {
		switch t.ICMP.Type {
		case 0:
			return "Echo reply"
		case 8:
			return "Echo request"
		case 3:
			return "Unreachable"
		case 11:
			return "Time exceeded"
		default:
			return fmt.Sprintf("ICMP %d/%d", t.ICMP.Type, t.ICMP.Code)
		}
	}
	if t.Protocol == nil {
		return "unknown"
	}
	switch *t.Protocol {
	case 1:
		return "ICMP"
	case 6:
		return "TCP"
	case 17:
		return "UDP"
	}
	return fmt.Sprintf("IP %d", *t.Protocol)
}

func eventAction(t goodnews.TraceRecord) string {
	if t.DropReason != "" {
		return t.Action + ": " + t.DropReason
	}
	return t.Action
}

// Lines returns plain, width-bounded text. The terminal controller owns colors
// and cursor movement, keeping screen rendering testable without a real TTY.
func (m *Model) Lines(s *Store, width, height int) []string {
	mode := "HISTORY"
	if m.Follow {
		mode = "REPLAY"
		if m.Connection == "connected" {
			mode = "LIVE"
		}
	}
	lines := []string{fmt.Sprintf("goodnews | %s | router %s | %s", mode, s.Info.RouterID, m.Connection)}
	var selected *goodnews.TraceRecord
	if m.Selected >= 0 {
		if event, err := s.Event(m.Selected); err == nil {
			selected = &event
		}
	}
	context := fmt.Sprintf("Run %s | %d events", s.Info.RunID, s.Count())
	if selected != nil {
		context += fmt.Sprintf(" | #%d %s", selected.PacketID, selected.Timestamp.Local().Format("15:04:05.000 MST"))
		if !m.Follow {
			context += fmt.Sprintf(" | %d newer", s.Count()-m.Selected-1)
		}
	}
	lines = append(lines, context)
	counters := s.Counters(selected)
	version := uint64(1)
	if selected != nil {
		version = selected.FIBVersion
	}
	table := s.Table(version)
	if m.Follow && m.Live != nil && m.Live.Err == nil && m.Live.Info.RunID == s.Info.RunID {
		counters, table = &m.Live.Counters, &m.Live.Table
	}
	if counters == nil {
		lines = append(lines, "Counters: historical snapshot unavailable")
	} else {
		when := "initial state"
		if counters.AfterPacketID != nil && *counters.AfterPacketID != 0 {
			when = fmt.Sprintf("after handling ingress #%d", *counters.AfterPacketID)
		}
		if m.Follow && m.Live != nil && m.Live.Err == nil && m.Live.Info.RunID == s.Info.RunID {
			when = "live totals"
		}
		lines = append(lines, fmt.Sprintf("RX %d  TX %d  Delivered %d  Drops %d | %s", counters.Received, counters.Transmitted, counters.Delivered, counters.Dropped, when))
	}
	protections := "Protections: unavailable in this legacy run"
	if s.Info.Protections != nil {
		protections = fmt.Sprintf("Protections: IPv4 checksum=%t  TTL decrement=%t", s.Info.Protections["verify_ipv4_checksum"], s.Info.Protections["decrement_ttl"])
	}
	lines = append(lines, protections)
	if m.Details {
		lines = append(lines, "PACKET DETAILS | arrows/PgUp/PgDn scroll; Esc returns")
		details := detailLines(s, selected, counters)
		room := max(1, height-len(lines)-2)
		start := min(m.DetailOffset, max(0, len(details)-room))
		m.DetailOffset = start
		lines = append(lines, details[start:min(len(details), start+room)]...)
	} else {
		lines = append(lines, "TOPOLOGY | configured links; observed endpoints at the selected event")
		if len(s.Info.Interfaces) == 0 {
			lines = append(lines, "  Interface configuration unavailable in this legacy run")
		}
		for i, iface := range s.Info.Interfaces {
			if i >= 3 {
				lines = append(lines, fmt.Sprintf("  +%d more interfaces (see run.json)", len(s.Info.Interfaces)-3))
				break
			}
			peers := s.Peers(iface.Name, m.Selected)
			// Always include the current packet's endpoint, even after the bounded
			// first-observed peer list fills up.
			if selected != nil {
				prefix, err := netip.ParsePrefix(iface.Address)
				if err == nil {
					for _, a := range []*netip.Addr{selected.Source, selected.Destination} {
						if a != nil && prefix.Contains(*a) && *a != prefix.Addr() && !contains(peers, a.String()) {
							peers = append(peers, a.String())
						}
					}
				}
			}
			peerLabel := "no host traffic yet"
			if len(peers) > 0 {
				peerLabel = strings.Join(peers, ",")
			}
			name := filepath.Base(iface.NetNS)
			if iface.NetNS == "" {
				name = "hosts"
			}
			arrow := "<-->"
			if selected != nil {
				switch {
				case selected.Ingress == iface.Name && selected.Egress == iface.Name:
					arrow = "<==>"
				case selected.Ingress == iface.Name:
					arrow = "--->"
				case selected.Egress == iface.Name:
					arrow = "<---"
				}
			}
			counts := ""
			if counters != nil {
				c := counters.Interfaces[iface.Name]
				counts = fmt.Sprintf(" RX%d/TX%d", c.Received, c.Transmitted)
			}
			lines = append(lines, fmt.Sprintf("  %s [%s] %s %s (%s) %s [router]%s", name, peerLabel, arrow, iface.Name, iface.Address, arrow, counts))
		}
		if selected != nil {
			lines = append(lines, "Selected: "+packetName(*selected)+" | "+eventAction(*selected))
		} else {
			lines = append(lines, "Waiting for packets...")
		}
		focus := ""
		if m.RouteFocus {
			focus = " [focused: arrows scroll routes]"
		}
		if table == nil {
			lines = append(lines, "FORWARDING TABLE: historical snapshot unavailable"+focus)
		} else {
			lines = append(lines, fmt.Sprintf("FORWARDING TABLE v%d | %d routes%s", table.Version, len(table.Routes), focus))
			lines = append(lines, "  Prefix              Next hop         Interface      Installed by")
			start := min(m.RouteOffset, max(0, len(table.Routes)-3))
			m.RouteOffset = start
			for _, route := range table.Routes[start:min(len(table.Routes), start+3)] {
				hop := "direct"
				if route.NextHop.IsValid() {
					hop = route.NextHop.String()
				}
				marker := " "
				if selected != nil && selected.FIBVersion == table.Version && selected.Winner != nil && route.Prefix == selected.Winner.Prefix && route.Interface == selected.Winner.Interface {
					marker = "*"
				}
				lines = append(lines, fmt.Sprintf("%s %-19s %-16s %-14s %s", marker, route.Prefix, hop, route.Interface, route.Origin))
			}
		}
		lines = append(lines, "PACKET HISTORY | Enter inspects selection; Tab changes focus")
		lines = append(lines, "  Time         ID   Source          Destination     Packet / TTL / Action")
		room := max(1, height-len(lines)-2)
		start := max(int64(0), m.Selected-int64(room)+1)
		for row := start; row < min(s.Count(), start+int64(room)); row++ {
			event, err := s.Event(row)
			if err != nil {
				lines = append(lines, "  unreadable event: "+err.Error())
				continue
			}
			marker := " "
			if row == m.Selected {
				marker = ">"
			}
			lines = append(lines, fmt.Sprintf("%s %-12s %-4d %-15s %-15s %s %s>%s %s", marker,
				event.Timestamp.Local().Format("15:04:05.000"), event.PacketID, address(event.Source), address(event.Destination), packetName(event), number(event.TTLBefore), number(event.TTLAfter), eventAction(event)))
		}
	}
	notice := m.Message
	if notice == "" && len(s.Warnings) > 0 {
		notice = s.Warnings[len(s.Warnings)-1]
	}
	if notice == "" && s.Incomplete() {
		notice = "Incomplete trailing record: waiting for a complete write (or saved run ended mid-record)"
	}
	if notice == "" && s.CatchingUp() {
		notice = fmt.Sprintf("Indexing history... %d events available so far; navigation stays active.", s.Count())
	}
	if notice == "" && s.Legacy {
		notice = "Legacy run: unrecorded historical state is shown as unavailable"
	}
	if notice == "" {
		notice = "Space pause | Home first | End live/latest | r reconnect | q quit | wheel scroll"
	}
	footer := "Up/Down select | PgUp/PgDn history | Enter details | Esc back | Tab focus"
	if width < 80 || height < 24 {
		lines = []string{"goodnews dashboard", "Resize terminal to at least 80 columns x 24 rows.", "History continues indexing. Press q to quit."}
	}
	lines = lines[:min(len(lines), max(0, height-2))]
	for len(lines) < max(0, height-2) {
		lines = append(lines, "")
	}
	lines = append(lines, notice, footer)
	for i := range lines {
		lines[i] = clip(lines[i], max(1, width-1))
	}
	return lines[:min(len(lines), max(0, height))]
}

func contains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}

func detailLines(s *Store, t *goodnews.TraceRecord, counters *goodnews.CounterSnapshot) []string {
	if t == nil {
		return []string{"No packet selected."}
	}
	lines := []string{fmt.Sprintf("Packet #%d | %s | %s", t.PacketID, t.Timestamp.Format("2006-01-02T15:04:05.000Z07:00"), packetName(*t)),
		fmt.Sprintf("%s -> %s | %d bytes", address(t.Source), address(t.Destination), t.Length),
		fmt.Sprintf("Interface: %s -> %s | TTL: %s -> %s", t.Ingress, t.Egress, number(t.TTLBefore), number(t.TTLAfter)),
		"Action: " + t.Action, "Drop reason: " + t.DropReason, "ICMP suppression: " + t.ICMPSuppressed, "Send error: " + t.Error,
		fmt.Sprintf("FIB version: %d", t.FIBVersion)}
	if t.Protocol != nil {
		lines = append(lines, fmt.Sprintf("IP protocol number: %d", *t.Protocol))
	}
	if t.ParentID != nil {
		lines = append(lines, fmt.Sprintf("Generated while handling ingress #%d; counters include that completed operation.", *t.ParentID))
	}
	if t.ICMP != nil {
		lines = append(lines, fmt.Sprintf("ICMP type=%d code=%d", t.ICMP.Type, t.ICMP.Code))
		if t.ICMP.Identifier != nil && t.ICMP.Sequence != nil {
			lines = append(lines, fmt.Sprintf("Echo identifier=%d sequence=%d", *t.ICMP.Identifier, *t.ICMP.Sequence))
		}
	}
	lines = append(lines, "Matching prefixes (most specific first):")
	if len(t.Matches) == 0 {
		lines = append(lines, "  No matching prefixes recorded (lookup may not have been reached).")
	}
	for _, r := range t.Matches {
		lines = append(lines, fmt.Sprintf("  %s -> %s (%s)", r.Prefix, r.Interface, r.Origin))
	}
	if t.Winner != nil {
		lines = append(lines, fmt.Sprintf("Winner: %s -> %s", t.Winner.Prefix, t.Winner.Interface))
	}
	lines = append(lines, "Related packets:")
	for _, r := range s.Related(*t) {
		lines = append(lines, fmt.Sprintf("  #%d %s %s -> %s | %s", r.PacketID, packetName(r), address(r.Source), address(r.Destination), eventAction(r)))
	}
	if counters != nil {
		lines = append(lines, "Drop counters at this point:")
		keys := make([]string, 0, len(counters.DropReasons))
		for k := range counters.DropReasons {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, key := range keys {
			lines = append(lines, fmt.Sprintf("  %s: %d", key, counters.DropReasons[key]))
		}
	}
	return lines
}
