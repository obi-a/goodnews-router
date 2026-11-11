package goodnews

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestEthernetAndARPWireLayout(t *testing.T) {
	local, peer := MAC{2, 0, 0, 0, 0, 1}, MAC{2, 0, 0, 0, 0, 2}
	a := ARPPacket{Operation: 1, SenderMAC: peer, SenderIP: addr("10.0.0.2"), TargetIP: addr("10.0.0.1")}
	frame := BuildEthernet(BroadcastMAC, peer, EtherTypeARP, BuildARP(a))
	if len(frame) != 60 || binary.BigEndian.Uint16(frame[12:14]) != EtherTypeARP || !bytes.Equal(frame[14:22], []byte{0, 1, 8, 0, 6, 4, 0, 1}) {
		t.Fatalf("bad wire layout: %x", frame)
	}
	f, err := ParseEthernet(frame)
	if err != nil || f.Destination != BroadcastMAC || f.Source != peer {
		t.Fatalf("%+v %v", f, err)
	}
	got, err := ParseARP(f.Payload)
	if err != nil || got != a {
		t.Fatalf("%+v %v", got, err)
	}
	a.Operation, a.SenderMAC, a.TargetMAC = 2, local, peer
	got, err = ParseARP(BuildARP(a))
	if err != nil || got != a {
		t.Fatal("reply did not round trip")
	}
	for size := 0; size < 14; size++ {
		if _, err := ParseEthernet(frame[:size]); err == nil {
			t.Fatalf("accepted Ethernet length %d", size)
		}
	}
	for size := 0; size < 28; size++ {
		if _, err := ParseARP(BuildARP(a)[:size]); err == nil {
			t.Fatalf("accepted ARP length %d", size)
		}
	}
	for _, offset := range []int{0, 2, 4, 5, 6} {
		p := BuildARP(a)
		p[offset] ^= 0x80
		if _, err := ParseARP(p); err == nil {
			t.Fatalf("accepted unsupported ARP field %d", offset)
		}
	}
}

func FuzzEthernetARP(f *testing.F) {
	f.Add(BuildEthernet(BroadcastMAC, MAC{2, 0, 0, 0, 0, 2}, EtherTypeARP, BuildARP(ARPPacket{Operation: 1, SenderIP: addr("10.0.0.2"), TargetIP: addr("10.0.0.1")})))
	f.Fuzz(func(t *testing.T, packet []byte) {
		frame, err := ParseEthernet(packet)
		if err != nil {
			return
		}
		if frame.EtherType == EtherTypeARP {
			_, _ = ParseARP(frame.Payload)
		}
	})
}
