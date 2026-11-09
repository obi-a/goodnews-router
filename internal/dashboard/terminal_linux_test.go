//go:build linux

package dashboard

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestTerminalRestoreOnQuitAndCancellation(t *testing.T) {
	for _, cancelExit := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelExit), func(t *testing.T) {
			fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
			if err != nil {
				t.Skipf("pseudo terminals unavailable: %v", err)
			}
			master := os.NewFile(uintptr(fd), "pty-master")
			defer master.Close()
			if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
				t.Fatal(err)
			}
			number, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
			if err != nil {
				t.Fatal(err)
			}
			slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|unix.O_NOCTTY, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer slave.Close()
			before, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
			if err != nil {
				t.Fatal(err)
			}
			if err := unix.IoctlSetWinsize(int(slave.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: 24, Col: 80}); err != nil {
				t.Fatal(err)
			}
			t.Setenv("TERM", "xterm-256color")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- Run(ctx, Options{Directory: "testdata/demo", Input: slave, Output: slave}) }()
			deadline := time.Now().Add(2 * time.Second)
			for {
				mode, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
				if err != nil {
					t.Fatal(err)
				}
				if mode.Lflag&unix.ICANON == 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("dashboard never entered terminal mode")
				}
				time.Sleep(5 * time.Millisecond)
			}
			// Drain output so redraw cannot block on a full pseudo-terminal buffer.
			drained := make(chan struct{})
			go func() {
				defer close(drained)
				var b [8192]byte
				for ctx.Err() == nil {
					fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
					if n, _ := unix.Poll(fds, 50); n > 0 {
						_, _ = unix.Read(fd, b[:])
					}
				}
			}()
			if cancelExit {
				cancel()
			} else {
				if _, err := master.Write([]byte("q")); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("dashboard did not exit")
			}
			cancel()
			<-drained
			after, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatal("terminal settings were not restored")
			}
		})
	}
}

func TestRejectNonInteractiveTerminal(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "not-a-terminal")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := openTerminal(f, f); err == nil {
		t.Fatal("noninteractive input accepted")
	}
}
