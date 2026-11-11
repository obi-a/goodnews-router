package goodnews

import (
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
)

const (
	EtherTypeIPv4 = 0x0800
	EtherTypeARP  = 0x0806
)

// MAC is a six-byte Ethernet address, kept as bytes so packet layout is visible.
type MAC [6]byte

var BroadcastMAC = MAC{255, 255, 255, 255, 255, 255}

func (m MAC) String() string               { return net.HardwareAddr(m[:]).String() }
func (m MAC) MarshalText() ([]byte, error) { return []byte(m.String()), nil }
func (m *MAC) UnmarshalText(text []byte) error {
	parsed, err := net.ParseMAC(string(text))
	if err != nil || len(parsed) != 6 {
		return fmt.Errorf("invalid six-byte MAC address %q", text)
	}
	copy(m[:], parsed)
	return nil
}
func (m MAC) Unicast() bool { return m != (MAC{}) && m[0]&1 == 0 }

type EthernetFrame struct {
	Destination, Source MAC
	EtherType           uint16
	Payload             []byte
}

func ParseEthernet(frame []byte) (EthernetFrame, error) {
	if len(frame) < 14 {
		return EthernetFrame{}, fmt.Errorf("short_ethernet_frame")
	}
	f := EthernetFrame{EtherType: binary.BigEndian.Uint16(frame[12:14]), Payload: frame[14:]}
	copy(f.Destination[:], frame[:6])
	copy(f.Source[:], frame[6:12])
	return f, nil
}

// Ethernet II has a 14-byte header and at least 46 payload bytes. The NIC adds
// the FCS; AF_PACKET does not include it in transmitted or received buffers.
func BuildEthernet(destination, source MAC, etherType uint16, payload []byte) []byte {
	frame := make([]byte, max(60, 14+len(payload)))
	copy(frame[:6], destination[:])
	copy(frame[6:12], source[:])
	binary.BigEndian.PutUint16(frame[12:14], etherType)
	copy(frame[14:], payload)
	return frame
}

type ARPPacket struct {
	Operation uint16     `json:"operation"`
	SenderMAC MAC        `json:"sender_mac"`
	TargetMAC MAC        `json:"target_mac"`
	SenderIP  netip.Addr `json:"sender_ip"`
	TargetIP  netip.Addr `json:"target_ip"`
}

func ParseARP(payload []byte) (ARPPacket, error) {
	if len(payload) < 28 {
		return ARPPacket{}, fmt.Errorf("short_arp_packet")
	}
	if binary.BigEndian.Uint16(payload[:2]) != 1 || binary.BigEndian.Uint16(payload[2:4]) != EtherTypeIPv4 || payload[4] != 6 || payload[5] != 4 {
		return ARPPacket{}, fmt.Errorf("unsupported_arp_format")
	}
	a := ARPPacket{Operation: binary.BigEndian.Uint16(payload[6:8]), SenderIP: netip.AddrFrom4([4]byte(payload[14:18])), TargetIP: netip.AddrFrom4([4]byte(payload[24:28]))}
	copy(a.SenderMAC[:], payload[8:14])
	copy(a.TargetMAC[:], payload[18:24])
	if a.Operation != 1 && a.Operation != 2 {
		return ARPPacket{}, fmt.Errorf("unsupported_arp_operation")
	}
	return a, nil
}

func BuildARP(a ARPPacket) []byte {
	p := make([]byte, 28)
	binary.BigEndian.PutUint16(p[:2], 1)
	binary.BigEndian.PutUint16(p[2:4], EtherTypeIPv4)
	p[4], p[5] = 6, 4
	binary.BigEndian.PutUint16(p[6:8], a.Operation)
	copy(p[8:14], a.SenderMAC[:])
	copy(p[14:18], a.SenderIP.AsSlice())
	copy(p[18:24], a.TargetMAC[:])
	copy(p[24:28], a.TargetIP.AsSlice())
	return p
}
