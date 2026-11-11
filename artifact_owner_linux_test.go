//go:build linux

package goodnews

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

func TestResolveArtifactOwner(t *testing.T) {
	for _, tc := range []struct {
		name     string
		euid     int
		uid, gid string
		want     artifactOwner
		wantErr  bool
	}{
		{name: "ordinary user", euid: 1000},
		{name: "ordinary user ignores sudo environment", euid: 1000, uid: "bad", gid: "bad"},
		{name: "direct root", euid: 0},
		{name: "sudo", uid: "1000", gid: "2000", want: artifactOwner{uid: 1000, gid: 2000, set: true}},
		{name: "sudo from root", uid: "0", gid: "0", want: artifactOwner{set: true}},
		{name: "missing uid", gid: "1000", wantErr: true},
		{name: "missing gid", uid: "1000", wantErr: true},
		{name: "negative uid", uid: "-1", gid: "1000", wantErr: true},
		{name: "negative gid", uid: "1000", gid: "-1", wantErr: true},
		{name: "invalid uid", uid: "user", gid: "1000", wantErr: true},
		{name: "invalid gid", uid: "1000", gid: "group", wantErr: true},
		{name: "overflow", uid: "4294967296", gid: "1000", wantErr: true},
		{name: "uid sentinel", uid: "4294967295", gid: "1000", wantErr: true},
		{name: "gid sentinel", uid: "1000", gid: "4294967295", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveArtifactOwner(tc.euid, tc.uid, tc.gid)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("owner=%+v error=%v, want %+v error=%t", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

func assertArtifactOwnership(t *testing.T, path string, mode os.FileMode, uid, gid int) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	if info.Mode().Perm() != mode || int(stat.Uid) != uid || int(stat.Gid) != gid {
		t.Fatalf("%s: mode=%o owner=%d:%d; want mode=%o owner=%d:%d", path,
			info.Mode().Perm(), stat.Uid, stat.Gid, mode, uid, gid)
	}
}

func checkObserverArtifacts(t *testing.T, base string, uid, gid int) []string {
	t.Helper()
	c := testConfig()
	c.Observability.Directory = filepath.Join(base, "artifacts", "runs")
	o, err := OpenObserver(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = o.Close() })
	for _, path := range []string{filepath.Join(base, "artifacts"), c.Observability.Directory, o.Directory} {
		assertArtifactOwnership(t, path, 0700, uid, gid)
	}
	var files []string
	for _, name := range []string{"trace.jsonl", "counters.jsonl", "fib.jsonl", "run.json", "left.pcap", "right.pcap"} {
		path := filepath.Join(o.Directory, name)
		assertArtifactOwnership(t, path, 0600, uid, gid)
		files = append(files, path)
	}
	return files
}

func TestObserverProcessOwnership(t *testing.T) {
	t.Setenv("SUDO_UID", "")
	t.Setenv("SUDO_GID", "")
	base := t.TempDir()
	before, err := os.Stat(base)
	if err != nil {
		t.Fatal(err)
	}
	checkObserverArtifacts(t, base, os.Geteuid(), os.Getegid())
	assertArtifactOwnership(t, base, before.Mode().Perm(), os.Geteuid(), os.Getegid())
}

func TestObserverIgnoresUnprivilegedSudoEnvironment(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("exercises ordinary user execution")
	}
	t.Setenv("SUDO_UID", "invalid")
	t.Setenv("SUDO_GID", "invalid")
	checkObserverArtifacts(t, t.TempDir(), os.Geteuid(), os.Getegid())
}

func TestObserverSudoOwnership(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to transfer ownership and test access as another user")
	}
	const uid, gid = 65534, 65534
	t.Setenv("SUDO_UID", "65534")
	t.Setenv("SUDO_GID", "65534")
	// A traversable existing parent lets the child process test access to every
	// newly created directory; it must remain owned by root and keep its mode.
	base, err := os.MkdirTemp("", "goodnews-artifact-owner-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	if err := os.Chmod(base, 0755); err != nil {
		t.Fatal(err)
	}
	files := checkObserverArtifacts(t, base, uid, gid)
	assertArtifactOwnership(t, base, 0755, os.Geteuid(), os.Getegid())

	read := exec.Command("/bin/cat", files...)
	read.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uid, Gid: gid}}
	data, err := read.CombinedOutput()
	if err != nil || !bytes.Contains(data, []byte(`"router_id": "r1"`)) {
		t.Fatalf("invoking user could not read artifacts: %v\n%s", err, data)
	}
	// A different account must still be excluded from these private artifacts.
	read = exec.Command("/bin/cat", files[3])
	read.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uid - 1, Gid: gid - 1}}
	if err := read.Run(); err == nil {
		t.Fatal("another account could read the private manifest")
	}
}

func TestArtifactDirectoryRejectsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := (artifactOwner{}).mkdirAll(filepath.Join(path, "runs")); err == nil {
		t.Fatal("file accepted as an artifact directory")
	}
}
