package goodnews

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestForwardAndProtections(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		c := testConfig()
		value := !disabled
		c.Protections.DecrementTTL, c.Protections.VerifyIPv4Checksum = &value, &value
		e, _ := NewEngine(c)
		p := testPacket(t, "10.0.0.2", "10.0.1.2", 1, 64, testEcho())
		if disabled {
			p[10] ^= 0xff
		}
		original := append([]byte(nil), p...)
		d := e.Process("left", p)
		if d.Trace.Action != "forward" || len(d.Outputs) != 1 {
			t.Fatalf("decision=%+v", d.Trace)
		}
		h, err := ParseIPv4(d.Outputs[0].Packet, true)
		wantTTL := uint8(63)
		if disabled {
			wantTTL = 64
		}
		if err != nil || h.TTL != wantTTL || !bytes.Equal(h.Bytes[20:], original[20:]) {
			t.Fatalf("forwarded: %+v %v", h, err)
		}
		if !bytes.Equal(p, original) {
			t.Fatal("mutated input buffer")
		}
		if d.Outputs[0].Generated || d.Outputs[0].NextHop.String() != "10.0.1.2" {
			t.Fatal("wrong next hop")
		}
	}
}

func TestICMPResponses(t *testing.T) {
	e, _ := NewEngine(testConfig())
	for _, tc := range []struct {
		name, dst              string
		proto, ttl, kind, code uint8
		reason                 string
	}{
		{"echo", "10.0.0.1", 1, 64, 0, 0, ""},
		{"other router address", "10.0.1.1", 1, 64, 0, 0, ""},
		{"TTL", "10.0.1.2", 1, 1, 11, 0, "ttl_expired"},
		{"no route", "192.0.2.1", 1, 64, 3, 0, "no_route"},
		{"local UDP", "10.0.0.1", 17, 64, 3, 3, "local_udp_port_unreachable"},
		{"local protocol", "10.0.0.1", 253, 64, 3, 2, "local_protocol_unreachable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := testPacket(t, "10.0.0.2", tc.dst, tc.proto, tc.ttl, testEcho())
			d := e.Process("left", p)
			if len(d.Outputs) != 1 || !d.Outputs[0].Generated || d.Trace.DropReason != tc.reason {
				t.Fatalf("%+v", d.Trace)
			}
			h, err := ParseIPv4(d.Outputs[0].Packet, true)
			if err != nil || h.TTL != 64 || h.Destination.String() != "10.0.0.2" || Checksum(h.Bytes[20:]) != 0 {
				t.Fatalf("bad reply: %v %+v", err, h)
			}
			if h.Bytes[20] != tc.kind || h.Bytes[21] != tc.code {
				t.Fatal("wrong ICMP type/code")
			}
			if tc.kind != 0 && !bytes.Equal(h.Bytes[28:], p[:28]) {
				t.Fatal("ICMP must quote original header + 8 bytes")
			}
			if tc.kind == 0 && h.Source.String() != tc.dst {
				t.Fatal("echo reply used wrong source address")
			}
		})
	}
}

func TestICMPSuppression(t *testing.T) {
	e, _ := NewEngine(testConfig())
	for _, kind := range []uint8{3, 4, 5, 11, 12} {
		icmp := testEcho()
		icmp[0] = kind
		p := testPacket(t, "10.0.0.2", "192.0.2.1", 1, 64, icmp)
		d := e.Process("left", p)
		if len(d.Outputs) != 0 || d.Trace.ICMPSuppressed != "icmp_error" {
			t.Fatalf("error about ICMP %d: %+v", kind, d.Trace)
		}
	}
	p := testPacket(t, "10.0.0.2", "10.0.1.2", 17, 1, []byte("fragment"))
	binary.BigEndian.PutUint16(p[6:8], 1)
	SetIPv4Checksum(p)
	d := e.Process("left", p)
	if len(d.Outputs) != 0 || d.Trace.ICMPSuppressed != "noninitial_fragment" {
		t.Fatal("error about noninitial fragment")
	}
	p = testPacket(t, "192.0.2.2", "10.0.1.2", 17, 1, []byte("payload"))
	d = e.Process("left", p)
	if len(d.Outputs) != 0 || d.Trace.ICMPSuppressed != "no_return_route" {
		t.Fatal("response without return route")
	}
}

func TestForwardingLimitations(t *testing.T) {
	c := testConfig()
	c.Interfaces[1].MTU = 100
	e, _ := NewEngine(c)
	for _, tc := range []struct {
		name   string
		mutate func([]byte) []byte
		reason string
	}{
		{"bad checksum", func(p []byte) []byte { p[10] ^= 1; return p }, "invalid_checksum"},
		{"limited broadcast", func(p []byte) []byte { copy(p[16:20], []byte{255, 255, 255, 255}); SetIPv4Checksum(p); return p }, "unsupported_destination"},
		{"directed broadcast", func(p []byte) []byte { p[19] = 255; SetIPv4Checksum(p); return p }, "unsupported_destination"},
		{"multicast", func(p []byte) []byte { p[16] = 224; SetIPv4Checksum(p); return p }, "unsupported_destination"},
		{"invalid source", func(p []byte) []byte { p[12] = 0; SetIPv4Checksum(p); return p }, "invalid_source"},
		{"options", func(p []byte) []byte { p[0] = 0x46; SetIPv4Checksum(p); return p }, "unsupported_ip_options"},
		{"local fragments", func(p []byte) []byte { p[19] = 1; p[6] = 0x20; SetIPv4Checksum(p); return p }, "local_reassembly_not_implemented"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.mutate(testPacket(t, "10.0.0.2", "10.0.1.2", 1, 64, testEcho()))
			d := e.Process("left", p)
			if d.Trace.DropReason != tc.reason || len(d.Outputs) != 0 {
				t.Fatalf("%+v", d.Trace)
			}
		})
	}
	p := testPacket(t, "10.0.0.2", "10.0.1.2", 17, 64, make([]byte, 120))
	if d := e.Process("left", p); d.Trace.DropReason != "fragmentation_not_implemented" {
		t.Fatalf("%+v", d.Trace)
	}
	p[6] = 0x40
	SetIPv4Checksum(p)
	d := e.Process("left", p)
	if len(d.Outputs) != 1 || d.Outputs[0].Packet[20] != 3 || d.Outputs[0].Packet[21] != 4 || binary.BigEndian.Uint16(d.Outputs[0].Packet[26:28]) != 100 {
		t.Fatal("missing fragmentation-needed MTU")
	}
	// Forward preexisting fragments without transport parsing or reassembly.
	p = testPacket(t, "10.0.0.2", "10.0.1.2", 17, 64, []byte("fragment"))
	p[7] = 1
	SetIPv4Checksum(p)
	if d := e.Process("left", p); d.Trace.Action != "forward" {
		t.Fatalf("%+v", d.Trace)
	}
}
