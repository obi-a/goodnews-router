//go:build linux

package dashboard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

type terminal struct {
	in, out   *os.File
	original  *unix.Termios
	color     bool
	lastFrame string
}

func openTerminal(in, out *os.File) (*terminal, error) {
	original, err := unix.IoctlGetTermios(int(in.Fd()), unix.TCGETS)
	if err != nil {
		return nil, fmt.Errorf("dashboard needs an interactive terminal (stdin): %w", err)
	}
	if _, err := unix.IoctlGetWinsize(int(out.Fd()), unix.TIOCGWINSZ); err != nil {
		return nil, fmt.Errorf("dashboard needs an interactive terminal (stdout): %w", err)
	}
	if os.Getenv("TERM") == "dumb" {
		return nil, fmt.Errorf("dashboard needs an ANSI terminal; use goodnews show for plain output")
	}
	raw := *original
	raw.Iflag &^= unix.BRKINT | unix.ICRNL | unix.INPCK | unix.ISTRIP | unix.IXON
	raw.Oflag &^= unix.OPOST
	raw.Cflag = raw.Cflag&^unix.CSIZE | unix.CS8
	raw.Lflag &^= unix.ECHO | unix.ICANON | unix.IEXTEN
	raw.Cc[unix.VMIN], raw.Cc[unix.VTIME] = 1, 0
	if err := unix.IoctlSetTermios(int(in.Fd()), unix.TCSETS, &raw); err != nil {
		return nil, err
	}
	_, noColor := os.LookupEnv("NO_COLOR")
	t := &terminal{in: in, out: out, original: original, color: !noColor}
	if _, err := io.WriteString(out, "\x1b[?1049h\x1b[?25l\x1b[?1000h\x1b[?1006h\x1b[2J\x1b[H"); err != nil {
		_ = t.Close()
		return nil, err
	}
	return t, nil
}

func (t *terminal) Size() (int, int) {
	size, err := unix.IoctlGetWinsize(int(t.out.Fd()), unix.TIOCGWINSZ)
	if err != nil || size.Col == 0 || size.Row == 0 {
		return 80, 24
	}
	return int(size.Col), int(size.Row)
}

func (t *terminal) Draw(lines []string) error {
	var frame strings.Builder
	frame.WriteString("\x1b[H")
	for i, line := range lines {
		if i > 0 {
			frame.WriteString("\r\n")
		}
		if t.color {
			switch {
			case strings.HasPrefix(line, ">"):
				frame.WriteString("\x1b[7m")
			case strings.HasPrefix(line, "goodnews"), strings.HasPrefix(line, "TOPOLOGY"), strings.HasPrefix(line, "FORWARDING"), strings.HasPrefix(line, "PACKET"):
				frame.WriteString("\x1b[36m")
			case strings.Contains(line, "drop:"), strings.Contains(line, "icmp_error:"):
				frame.WriteString("\x1b[33m")
			}
		}
		frame.WriteString(line)
		frame.WriteString("\x1b[0m\x1b[K")
	}
	if frame.String() == t.lastFrame {
		return nil
	}
	_, err := io.WriteString(t.out, frame.String())
	if err == nil {
		t.lastFrame = frame.String()
	}
	return err
}

func (t *terminal) Close() error {
	_, outputErr := io.WriteString(t.out, "\x1b[?1000l\x1b[?1006l\x1b[0m\x1b[?25h\x1b[?1049l")
	restoreErr := unix.IoctlSetTermios(int(t.in.Fd()), unix.TCSETS, t.original)
	return errors.Join(outputErr, restoreErr)
}

type keyDecoder struct{ pending []byte }

func (d *keyDecoder) Feed(input []byte) []string {
	d.pending = append(d.pending, input...)
	var keys []string
	sequences := map[string]string{"\x1b[A": "up", "\x1b[B": "down", "\x1b[H": "home", "\x1b[F": "end", "\x1bOH": "home", "\x1bOF": "end", "\x1b[1~": "home", "\x1b[4~": "end", "\x1b[7~": "home", "\x1b[8~": "end", "\x1b[5~": "pageup", "\x1b[6~": "pagedown"}
	for len(d.pending) > 0 {
		if d.pending[0] != 27 {
			key := map[byte]string{'q': "quit", 3: "quit", 'r': "reconnect", 'j': "down", 'k': "up", 'g': "home", 'G': "end", '\t': "tab", ' ': "pause", '\r': "enter", '\n': "enter"}[d.pending[0]]
			d.pending = d.pending[1:]
			if key != "" {
				keys = append(keys, key)
			}
			continue
		}
		text := string(d.pending)
		if strings.HasPrefix(text, "\x1b[<") {
			end := strings.IndexAny(text[3:], "Mm")
			if end < 0 {
				if len(d.pending) > 128 {
					d.pending = nil
				}
				break
			}
			fields := strings.Split(text[3:3+end], ";")
			if len(fields) == 3 {
				button, err := strconv.Atoi(fields[0])
				if err == nil && button&64 != 0 {
					key := "wheelup"
					if button&1 != 0 {
						key = "wheeldown"
					}
					keys = append(keys, key)
				}
			}
			d.pending = d.pending[4+end:]
			continue
		}
		matched, prefix := false, false
		for sequence, key := range sequences {
			if strings.HasPrefix(text, sequence) {
				keys = append(keys, key)
				d.pending = d.pending[len(sequence):]
				matched = true
				break
			}
			if strings.HasPrefix(sequence, text) {
				prefix = true
			}
		}
		if matched {
			continue
		}
		if prefix || text == "\x1b[<" || text == "\x1b[" {
			break
		}
		if len(text) == 1 {
			break
		}
		// Consume unknown CSI/SS3 sequences without interpreting their letters.
		if strings.HasPrefix(text, "\x1b[") || strings.HasPrefix(text, "\x1bO") {
			end := -1
			for i := 2; i < len(text); i++ {
				if text[i] >= 0x40 && text[i] <= 0x7e {
					end = i
					break
				}
			}
			if end < 0 {
				if len(text) > 128 {
					d.pending = nil
				}
				break
			}
			d.pending = d.pending[end+1:]
		} else {
			keys = append(keys, "escape")
			d.pending = d.pending[1:]
		}
	}
	return keys
}

func (d *keyDecoder) Flush() []string {
	defer func() { d.pending = nil }()
	if string(d.pending) == "\x1b" {
		return []string{"escape"}
	}
	return nil
}

func readKeys(ctx context.Context, in *os.File, keys chan<- string) {
	var decoder keyDecoder
	send := func(events []string) bool {
		for _, key := range events {
			select {
			case keys <- key:
			case <-ctx.Done():
				return false
			}
		}
		return true
	}
	for ctx.Err() == nil {
		fds := []unix.PollFd{{Fd: int32(in.Fd()), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, 100)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			send([]string{"quit"})
			return
		}
		if n == 0 {
			if !send(decoder.Flush()) {
				return
			}
			continue
		}
		if fds[0].Revents&unix.POLLIN == 0 {
			send([]string{"quit"})
			return
		}
		var buffer [128]byte
		count, err := unix.Read(int(in.Fd()), buffer[:])
		if errors.Is(err, unix.EINTR) || errors.Is(err, unix.EAGAIN) {
			continue
		}
		if count == 0 || err != nil {
			send([]string{"quit"})
			return
		}
		if !send(decoder.Feed(buffer[:count])) {
			return
		}
	}
}
