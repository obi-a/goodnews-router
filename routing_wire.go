package goodnews

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"strings"
)

const (
	RoutingHello      byte = 1
	RoutingDV         byte = 2
	RoutingLSA        byte = 3
	MaxRoutingMessage      = 1024
)

type RoutingPrefix struct {
	Prefix netip.Prefix `json:"prefix"`
	Metric uint32       `json:"metric"`
	Hops   uint8        `json:"hops,omitempty"`
}
type RoutingEdge struct {
	Router string `json:"router"`
	Cost   uint32 `json:"cost"`
}

// RoutingMessage is a complete advertisement, never a partial vector or LSA.
// LSA CreatedMS uses the shared clock of the single-host namespace lab.
type RoutingMessage struct {
	Type      byte            `json:"type"`
	Origin    string          `json:"origin"`
	Epoch     uint64          `json:"incarnation"`
	Sequence  uint64          `json:"sequence"`
	Algorithm string          `json:"algorithm,omitempty"`
	Seen      string          `json:"seen,omitempty"`
	SeenEpoch uint64          `json:"seen_incarnation,omitempty"`
	CreatedMS int64           `json:"created_ms,omitempty"`
	Prefixes  []RoutingPrefix `json:"prefixes,omitempty"`
	Edges     []RoutingEdge   `json:"links,omitempty"`
}

func wireID(s string, empty bool) bool { return (empty && s == "") || (safeName(s) && len(s) <= 8) }

func EncodeRouting(m RoutingMessage) ([]byte, error) {
	if !wireID(m.Origin, false) || m.Epoch == 0 || m.Sequence == 0 || len(m.Prefixes) > MaxRoutingPrefixes || len(m.Edges) > MaxRoutingLinks {
		return nil, fmt.Errorf("invalid_routing_message")
	}
	b := make([]byte, 32)
	b[0], b[1], b[2], b[3] = 'G', 'N', 1, m.Type
	copy(b[8:16], m.Origin)
	binary.BigEndian.PutUint64(b[16:24], m.Epoch)
	binary.BigEndian.PutUint64(b[24:32], m.Sequence)
	switch m.Type {
	case RoutingHello:
		if !wireID(m.Seen, true) || (m.Seen == "") != (m.SeenEpoch == 0) || (m.Algorithm != "dv" && m.Algorithm != "ls") {
			return nil, fmt.Errorf("invalid_routing_hello")
		}
		body := make([]byte, 18)
		body[0] = 1
		if m.Algorithm == "ls" {
			body[0] = 2
		}
		copy(body[2:10], m.Seen)
		binary.BigEndian.PutUint64(body[10:18], m.SeenEpoch)
		b = append(b, body...)
	case RoutingDV:
		b = binary.BigEndian.AppendUint16(b, uint16(len(m.Prefixes)))
	case RoutingLSA:
		if m.CreatedMS <= 0 {
			return nil, fmt.Errorf("invalid_routing_age")
		}
		b = binary.BigEndian.AppendUint64(b, uint64(m.CreatedMS))
		b = binary.BigEndian.AppendUint16(b, uint16(len(m.Edges)))
		b = binary.BigEndian.AppendUint16(b, uint16(len(m.Prefixes)))
		seen := map[string]bool{}
		for _, e := range m.Edges {
			if !wireID(e.Router, false) || e.Router == m.Origin || seen[e.Router] || e.Cost < 1 || e.Cost > 1000 {
				return nil, fmt.Errorf("invalid_routing_edge")
			}
			seen[e.Router] = true
			id := make([]byte, 8)
			copy(id, e.Router)
			b = append(b, id...)
			b = binary.BigEndian.AppendUint32(b, e.Cost)
		}
	default:
		return nil, fmt.Errorf("unknown_routing_type")
	}
	if m.Type != RoutingHello {
		seen := map[netip.Prefix]bool{}
		for _, p := range m.Prefixes {
			if !p.Prefix.IsValid() || !p.Prefix.Addr().Is4() || p.Prefix != p.Prefix.Masked() || seen[p.Prefix] || p.Metric > RoutingInfinity || p.Hops >= MaxRoutingRouters || (m.Type == RoutingLSA && (p.Metric != 0 || p.Hops != 0)) {
				return nil, fmt.Errorf("invalid_routing_prefix")
			}
			seen[p.Prefix] = true
			a := p.Prefix.Addr().As4()
			b = append(b, a[:]...)
			b = append(b, byte(p.Prefix.Bits()))
			if m.Type == RoutingDV {
				b = binary.BigEndian.AppendUint32(b, p.Metric)
				b = append(b, p.Hops)
			}
		}
	}
	if len(b) > MaxRoutingMessage {
		return nil, fmt.Errorf("routing_message_too_large")
	}
	binary.BigEndian.PutUint16(b[4:6], uint16(len(b)))
	binary.BigEndian.PutUint16(b[6:8], Checksum(b))
	return b, nil
}

func DecodeRouting(b []byte) (RoutingMessage, error) {
	bad := func() (RoutingMessage, error) { return RoutingMessage{}, fmt.Errorf("invalid_routing_message") }
	if len(b) < 32 || len(b) > MaxRoutingMessage || b[0] != 'G' || b[1] != 'N' || b[2] != 1 || int(binary.BigEndian.Uint16(b[4:6])) != len(b) || Checksum(b) != 0 {
		return bad()
	}
	id := func(raw []byte) string { return strings.TrimRight(string(raw), "\x00") }
	m := RoutingMessage{Type: b[3], Origin: id(b[8:16]), Epoch: binary.BigEndian.Uint64(b[16:24]), Sequence: binary.BigEndian.Uint64(b[24:32])}
	position, count := 32, 0
	switch m.Type {
	case RoutingHello:
		if len(b) != 50 || b[33] != 0 || b[32] < 1 || b[32] > 2 {
			return bad()
		}
		m.Algorithm = "dv"
		if b[32] == 2 {
			m.Algorithm = "ls"
		}
		m.Seen, m.SeenEpoch = id(b[34:42]), binary.BigEndian.Uint64(b[42:50])
	case RoutingDV:
		if len(b) < 34 {
			return bad()
		}
		count, position = int(binary.BigEndian.Uint16(b[32:34])), 34
	case RoutingLSA:
		if len(b) < 44 {
			return bad()
		}
		m.CreatedMS = int64(binary.BigEndian.Uint64(b[32:40]))
		edges := int(binary.BigEndian.Uint16(b[40:42]))
		count, position = int(binary.BigEndian.Uint16(b[42:44])), 44
		if edges > MaxRoutingLinks || len(b) < position+edges*12 {
			return bad()
		}
		for range edges {
			m.Edges = append(m.Edges, RoutingEdge{Router: id(b[position : position+8]), Cost: binary.BigEndian.Uint32(b[position+8 : position+12])})
			position += 12
		}
	default:
		return bad()
	}
	if m.Type != RoutingHello {
		size := 5
		if m.Type == RoutingDV {
			size = 10
		}
		if count > MaxRoutingPrefixes || len(b) != position+count*size {
			return bad()
		}
		for range count {
			p := netip.PrefixFrom(netip.AddrFrom4([4]byte(b[position:position+4])), int(b[position+4]))
			metric := uint32(0)
			hops := uint8(0)
			if size == 10 {
				metric = binary.BigEndian.Uint32(b[position+5 : position+9])
				hops = b[position+9]
			}
			m.Prefixes = append(m.Prefixes, RoutingPrefix{Prefix: p, Metric: metric, Hops: hops})
			position += size
		}
	}
	// A canonical re-encode validates all semantic bounds, padding and IDs.
	encoded, err := EncodeRouting(m)
	if err != nil || string(encoded) != string(b) {
		return bad()
	}
	return m, nil
}
