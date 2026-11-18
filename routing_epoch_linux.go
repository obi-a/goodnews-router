package goodnews

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Persist the incarnation before transmitting anything. A clock moving backward
// cannot make a restart older than this router's preceding process.
func nextRoutingEpoch(path string) (uint64, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return 0, err
	}
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return 0, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	if err := unix.Flock(fd, unix.LOCK_EX); err != nil {
		return 0, err
	}
	defer unix.Flock(fd, unix.LOCK_UN)
	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || st.Uid != uint32(os.Geteuid()) {
		return 0, fmt.Errorf("unsafe routing incarnation file")
	}
	data, err := io.ReadAll(io.LimitReader(f, 33))
	if err != nil || len(data) > 32 {
		return 0, fmt.Errorf("invalid routing incarnation file")
	}
	previous := uint64(0)
	if len(data) > 0 {
		previous, err = strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
		if err != nil || previous == ^uint64(0) {
			return 0, fmt.Errorf("invalid routing incarnation")
		}
	}
	next := uint64(time.Now().UnixNano())
	if next <= previous {
		next = previous + 1
	}
	if _, err = f.Seek(0, 0); err != nil {
		return 0, err
	}
	if err = f.Truncate(0); err != nil {
		return 0, err
	}
	if _, err = fmt.Fprintln(f, next); err != nil {
		return 0, err
	}
	if err = f.Sync(); err != nil {
		return 0, err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return 0, err
	}
	defer dir.Close()
	if err = dir.Sync(); err != nil {
		return 0, err
	}
	return next, nil
}
