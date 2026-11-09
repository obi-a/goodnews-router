package goodnews

import (
	"encoding/binary"
	"time"
)

// RunInfo makes a saved run self-describing, independent of later config edits.
type RunInfo struct {
	Version           int             `json:"version"`
	RunID             string          `json:"run_id"`
	RouterID          string          `json:"router_id"`
	StartedAt         time.Time       `json:"started_at"`
	ArtifactDirectory string          `json:"artifact_directory"`
	Interfaces        []Interface     `json:"interfaces"`
	Protections       map[string]bool `json:"protections"`
}

type FIBSnapshot struct {
	Version uint64  `json:"version"`
	Routes  []Route `json:"routes"`
}

// ICMPFields contains only fields present in the captured transport header.
type ICMPFields struct {
	Type       uint8   `json:"type"`
	Code       uint8   `json:"code"`
	Identifier *uint16 `json:"identifier,omitempty"`
	Sequence   *uint16 `json:"sequence,omitempty"`
}

func DescribeICMP(h IPv4) *ICMPFields {
	if h.Protocol != 1 || h.HeaderLength < 20 || h.HeaderLength > len(h.Bytes) {
		return nil
	}
	p := h.Bytes[h.HeaderLength:]
	if len(p) < 2 {
		return nil
	}
	fields := &ICMPFields{Type: p[0], Code: p[1]}
	if (p[0] == 0 || p[0] == 8) && len(p) >= 8 {
		id, seq := binary.BigEndian.Uint16(p[4:6]), binary.BigEndian.Uint16(p[6:8])
		fields.Identifier, fields.Sequence = &id, &seq
	}
	return fields
}

func initialCounters(c Config) CounterSnapshot {
	zero := uint64(0)
	s := CounterSnapshot{Kind: "counters", RouterID: c.RouterID, Timestamp: time.Now().UTC(),
		AfterPacketID: &zero, DropReasons: map[string]uint64{}, Interfaces: map[string]InterfaceCounters{}}
	for _, iface := range c.Interfaces {
		s.Interfaces[iface.Name] = InterfaceCounters{}
	}
	return s
}
