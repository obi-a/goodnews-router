package goodnews

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// artifactOwner changes ownership only when root was invoked through sudo.
// Ordinary users and direct root invocations retain normal process ownership.
type artifactOwner struct {
	uid, gid int
	set      bool
}

func resolveArtifactOwner(euid int, sudoUID, sudoGID string) (artifactOwner, error) {
	if euid != 0 || (sudoUID == "" && sudoGID == "") {
		return artifactOwner{}, nil
	}
	parseID := func(name, value string) (int, error) {
		id, err := strconv.ParseUint(value, 10, 32)
		// All-bits-one is chown's "leave unchanged" sentinel, not an owner.
		if err != nil || id == 1<<32-1 {
			return 0, fmt.Errorf("artifacts: invalid %s %q", name, value)
		}
		return int(id), nil
	}
	uid, err := parseID("SUDO_UID", sudoUID)
	if err != nil {
		return artifactOwner{}, err
	}
	gid, err := parseID("SUDO_GID", sudoGID)
	if err != nil {
		return artifactOwner{}, err
	}
	return artifactOwner{uid: uid, gid: gid, set: true}, nil
}

func (owner artifactOwner) apply(f *os.File) error {
	if !owner.set {
		return nil
	}
	if err := f.Chown(owner.uid, owner.gid); err != nil {
		return fmt.Errorf("artifacts: set owner of %s: %w", f.Name(), err)
	}
	return nil
}

// mkdirAll owns only directories it creates, never existing ancestors such as
// the repository root or /tmp. This also covers newly created artifacts/runs
// parents, which must be traversable by the invoking user.
func (owner artifactOwner) mkdirAll(path string) error {
	path = filepath.Clean(path)
	err := os.Mkdir(path, 0700)
	if os.IsNotExist(err) {
		if err := owner.mkdirAll(filepath.Dir(path)); err != nil {
			return err
		}
		err = os.Mkdir(path, 0700)
	}
	if err != nil {
		if info, statErr := os.Stat(path); statErr == nil && info.IsDir() {
			return nil
		}
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return owner.apply(f)
}

func (owner artifactOwner) create(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	if err := owner.apply(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}
