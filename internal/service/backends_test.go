package service

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func listenUnix(t *testing.T, path string) {
	t.Helper()
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
}

func sharedDesktop(t *testing.T, pid int) (*Wayland, string) {
	t.Helper()
	root, err := os.MkdirTemp("", "w")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	run, shared := filepath.Join(root, "u"), filepath.Join(root, "s")
	home := filepath.Join(run, "me")
	for _, d := range []string{home, shared} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, "singularity-desktop-session.pid"), []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(shared, "wayland-0")
	listenUnix(t, socket)
	if err := os.WriteFile(socket+".lock", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return &Wayland{RunDir: run, SharedDir: shared}, socket
}

func TestASharedCompositorBelongsToTheSignedInPerson(t *testing.T) {
	w, socket := sharedDesktop(t, os.Getpid())
	path, uid, ok := w.socket()
	if !ok || path != socket || uid != uint32(os.Getuid()) {
		t.Fatalf("socket() = %q %d %v, want %q owned by %d", path, uid, ok, socket, os.Getuid())
	}
	if owner, ok := w.Owner(); !ok || owner != uint32(os.Getuid()) {
		t.Fatalf("Owner() = %d %v", owner, ok)
	}
}

func TestAnEndedSessionLeavesNobodySignedIn(t *testing.T) {
	w, _ := sharedDesktop(t, 1<<30)
	if owner, ok := w.Owner(); ok {
		t.Fatalf("Owner() = %d after the session ended", owner)
	}
}

func TestAPersonalCompositorComesFirst(t *testing.T) {
	w, _ := sharedDesktop(t, os.Getpid())
	personal := filepath.Join(w.RunDir, "me", "wayland-1")
	listenUnix(t, personal)
	if path, _, ok := w.socket(); !ok || path != personal {
		t.Fatalf("socket() = %q %v, want %q", path, ok, personal)
	}
}
