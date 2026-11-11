//go:build linux

package drivers

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"

	goodnews "github.com/obi-a/goodnews-router"
	"golang.org/x/sys/unix"
)

type Ethernet struct {
	file *os.File
	mac  goodnews.MAC
}

// OpenEthernet binds an AF_PACKET socket to one Ethernet interface. Nonblocking
// mode lets Go's poller wake Receive when Close is called during shutdown.
func OpenEthernet(device string, mtu int) (*Ethernet, error) {
	iface, err := net.InterfaceByName(device)
	if err != nil {
		return nil, err
	}
	if len(iface.HardwareAddr) != 6 || iface.Flags&net.FlagUp == 0 || iface.MTU < mtu {
		return nil, fmt.Errorf("%s must be an up Ethernet device with MTU >= %d", device, mtu)
	}
	addresses, err := iface.Addrs()
	if err != nil {
		return nil, err
	}
	for _, address := range addresses {
		p, err := netip.ParsePrefix(address.String())
		if err == nil && p.Addr().Is4() {
			return nil, fmt.Errorf("%s has a kernel IPv4 address; remove it before starting goodnews", device)
		}
	}
	protocol := binary.NativeEndian.Uint16([]byte{0, unix.ETH_P_ALL})
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, int(protocol))
	if err != nil {
		return nil, fmt.Errorf("AF_PACKET (CAP_NET_RAW required): %w", err)
	}
	defer func() {
		if err != nil {
			_ = unix.Close(fd)
		}
	}()
	if err = unix.SetsockoptInt(fd, unix.SOL_PACKET, unix.PACKET_IGNORE_OUTGOING, 1); err != nil {
		return nil, err
	}
	if err = unix.Bind(fd, &unix.SockaddrLinklayer{Ifindex: iface.Index, Protocol: protocol}); err != nil {
		return nil, err
	}
	return &Ethernet{file: os.NewFile(uintptr(fd), device), mac: goodnews.MAC(iface.HardwareAddr)}, nil
}

func (e *Ethernet) HardwareAddr() goodnews.MAC { return e.mac }
func (e *Ethernet) Receive() ([]byte, error) {
	frame := make([]byte, 65549)
	n, err := e.file.Read(frame)
	return frame[:n], err
}
func (e *Ethernet) Send(frame []byte, _ netip.Addr) error {
	n, err := e.file.Write(frame)
	if err == nil && n != len(frame) {
		return io.ErrShortWrite
	}
	return err
}
func (e *Ethernet) Close() error { return e.file.Close() }
