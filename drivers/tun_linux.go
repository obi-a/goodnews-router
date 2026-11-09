//go:build linux

package drivers

import (
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

type TUN struct{ file *os.File }

// OpenTUN attaches to a persistent host-side TUN. Only this bootstrap goroutine
// changes namespaces; packet workers always remain in the router namespace.
func OpenTUN(device, namespace string) (*TUN, error) {
	if namespace == "" {
		return openTUN(device)
	}
	type result struct {
		link *TUN
		err  error
	}
	results := make(chan result)
	go func() {
		runtime.LockOSThread()
		// /proc/self/ns/net refers to the thread-group leader; use this thread.
		original, err := unix.Open(fmt.Sprintf("/proc/self/task/%d/ns/net", unix.Gettid()), unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			runtime.UnlockOSThread()
			results <- result{err: err}
			return
		}
		defer unix.Close(original)
		target, err := unix.Open(namespace, unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			runtime.UnlockOSThread()
			results <- result{err: err}
			return
		}
		defer unix.Close(target)
		if err = unix.Setns(target, unix.CLONE_NEWNET); err != nil {
			runtime.UnlockOSThread()
			results <- result{err: fmt.Errorf("enter %s (CAP_SYS_ADMIN required): %w", namespace, err)}
			return
		}
		link, openErr := openTUN(device)
		if err := unix.Setns(original, unix.CLONE_NEWNET); err != nil {
			if link != nil {
				_ = link.Close()
			}
			results <- result{err: errors.Join(openErr, fmt.Errorf("restore namespace: %w", err))}
			// Exiting while locked destroys this thread, rather than returning a
			// thread in the wrong namespace to Go's scheduler.
			return
		}
		runtime.UnlockOSThread()
		results <- result{link, openErr}
	}()
	r := <-results
	return r.link, r.err
}

func openTUN(device string) (*TUN, error) {
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open TUN: %w", err)
	}
	request, err := unix.NewIfreq(device)
	if err == nil {
		request.SetUint16(unix.IFF_TUN | unix.IFF_NO_PI)
		err = unix.IoctlIfreq(fd, unix.TUNSETIFF, request)
	}
	if err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("attach TUN %s: %w", device, err)
	}
	return &TUN{file: os.NewFile(uintptr(fd), device)}, nil
}

func (t *TUN) Receive() ([]byte, error) {
	packet := make([]byte, 65535)
	n, err := t.file.Read(packet)
	return packet[:n], err
}
func (t *TUN) Send(packet []byte, _ netip.Addr) error {
	n, err := t.file.Write(packet)
	if err == nil && n != len(packet) {
		err = io.ErrShortWrite
	}
	return err
}
func (t *TUN) Close() error { return t.file.Close() }
