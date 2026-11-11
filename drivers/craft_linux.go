//go:build linux

package drivers

import (
	"encoding/binary"
	"fmt"
	goodnews "github.com/obi-a/goodnews-router"
	"net"

	"golang.org/x/sys/unix"
)

// SendRawIP deliberately bypasses AF_INET, whose IP_HDRINCL path repairs the
// IPv4 checksum and total length, hiding the corruption we want to study.
func SendRawIP(device string, packet []byte, destination ...goodnews.MAC) error {
	iface, err := net.InterfaceByName(device)
	if err != nil {
		return err
	}
	if len(iface.HardwareAddr) == 6 {
		if len(destination) != 1 {
			return fmt.Errorf("Ethernet crafting requires --dst-mac for the next hop")
		}
		packet = goodnews.BuildEthernet(destination[0], goodnews.MAC(iface.HardwareAddr), goodnews.EtherTypeIPv4, packet)
	} else if len(destination) != 0 {
		return fmt.Errorf("--dst-mac requires an Ethernet device")
	}
	// sockaddr_ll protocol and socket protocol are network-byte-order values.
	protocol := uint16(unix.ETH_P_IP)
	bytes := [2]byte{byte(protocol >> 8), byte(protocol)}
	networkOrder := binary.NativeEndian.Uint16(bytes[:])
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC, int(networkOrder))
	if err != nil {
		return fmt.Errorf("AF_PACKET (CAP_NET_RAW required): %w", err)
	}
	defer unix.Close(fd)
	return unix.Sendto(fd, packet, 0, &unix.SockaddrLinklayer{Ifindex: iface.Index, Protocol: networkOrder})
}
