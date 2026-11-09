package goodnews

import (
	"encoding/binary"
	"fmt"
	"net/netip"
)

type PacketError string

func (e PacketError) Error() string { return string(e) }

type IPv4 struct {
	Bytes               []byte
	HeaderLength        int
	Source, Destination netip.Addr
	TTL, Protocol       uint8
	FlagsOffset         uint16
}

// ParseIPv4 checks lengths before slicing. Disabling checksum checks does not
// disable these memory-safety checks.
func ParseIPv4(packet []byte, verifyChecksum bool) (IPv4, error) {
	var h IPv4
	if len(packet) < 20 {
		return h, PacketError("truncated_header")
	}
	if packet[0]>>4 != 4 {
		return h, PacketError("invalid_version")
	}
	headerLength := int(packet[0]&15) * 4
	if headerLength < 20 || headerLength > len(packet) {
		return h, PacketError("invalid_header_length")
	}
	length := int(binary.BigEndian.Uint16(packet[2:4]))
	if length < headerLength || length > len(packet) {
		return h, PacketError("invalid_total_length")
	}
	if verifyChecksum && Checksum(packet[:headerLength]) != 0 {
		return h, PacketError("invalid_checksum")
	}
	h = IPv4{Bytes: packet[:length], HeaderLength: headerLength,
		Source: netip.AddrFrom4([4]byte(packet[12:16])), Destination: netip.AddrFrom4([4]byte(packet[16:20])),
		TTL: packet[8], Protocol: packet[9], FlagsOffset: binary.BigEndian.Uint16(packet[6:8])}
	if h.FlagsOffset&0x8000 != 0 {
		return h, PacketError("reserved_fragment_flag")
	}
	return h, nil
}

// Checksum is the Internet one's-complement checksum, including odd final bytes.
func Checksum(data []byte) uint16 {
	var sum uint32
	for len(data) >= 2 {
		sum += uint32(binary.BigEndian.Uint16(data))
		data = data[2:]
	}
	if len(data) == 1 {
		sum += uint32(data[0]) << 8
	}
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}

func SetIPv4Checksum(packet []byte) {
	packet[10], packet[11] = 0, 0
	binary.BigEndian.PutUint16(packet[10:12], Checksum(packet[:int(packet[0]&15)*4]))
}

func BuildIPv4(source, destination netip.Addr, protocol, ttl uint8, payload []byte) ([]byte, error) {
	if !source.Is4() || !destination.Is4() || len(payload) > 65515 {
		return nil, fmt.Errorf("invalid IPv4 addresses or payload length")
	}
	packet := make([]byte, 20+len(payload))
	packet[0] = 0x45
	binary.BigEndian.PutUint16(packet[2:4], uint16(len(packet)))
	packet[8], packet[9] = ttl, protocol
	s, d := source.As4(), destination.As4()
	copy(packet[12:16], s[:])
	copy(packet[16:20], d[:])
	copy(packet[20:], payload)
	SetIPv4Checksum(packet)
	return packet, nil
}
