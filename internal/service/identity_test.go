package service

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/godbus/dbus/v5"

	"github.com/singularityos-lab/sinty-cored/internal/pair"
)

type keeper struct {
	mu    sync.Mutex
	seeds [][]byte
}

func (k *keeper) Adopt(seed []byte) (bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.seeds = append(k.seeds, append([]byte{}, seed...))
	return true, nil
}

func (k *keeper) count() int { k.mu.Lock(); defer k.mu.Unlock(); return len(k.seeds) }

func (r *rig) property(name string) string {
	r.t.Helper()
	var v string
	if err := r.obj.StoreProperty(Iface+"."+name, &v); err != nil {
		r.t.Fatal(err)
	}
	return v
}

func backupFile(t *testing.T) *os.File {
	f, err := os.OpenFile(filepath.Join(t.TempDir(), "core.backup"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestBackupAndRestoreTheCore(t *testing.T) {
	old := setup(t, uint32(os.Getuid()))
	old.tr.Add("aa11", "Living Room TV")
	if got := old.property("Identity"); got != IdentityFile {
		t.Fatalf("identity kept as %q", got)
	}
	f := backupFile(t)
	var code string
	if err := old.obj.Call(Iface+".Backup", 0, dbus.UnixFD(f.Fd())).Store(&code); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(f.Name())
	if len(code) != 29 || len(data) < 64 {
		t.Fatalf("code %q, %d bytes", code, len(data))
	}

	fresh := setup(t, uint32(os.Getuid()))
	k := &keeper{}
	fresh.svc.cfg.Vault = k
	before := fresh.property("ID")
	in, _ := os.Open(f.Name())
	defer in.Close()
	err := fresh.obj.Call(Iface+".Restore", 0, dbus.UnixFD(in.Fd()), "00000-00000-00000-00000-00000").Err
	var de dbus.Error
	if !errors.As(err, &de) || de.Name != errorPfx+"WrongCode" || fresh.property("ID") != before {
		t.Fatalf("wrong code: %v", err)
	}
	in.Seek(0, io.SeekStart)
	if err := fresh.obj.Call(Iface+".Restore", 0, dbus.UnixFD(in.Fd()), code).Err; err != nil {
		t.Fatal(err)
	}
	if got, want := fresh.property("ID"), old.property("ID"); got != want {
		t.Fatalf("restored id %s, want %s", got, want)
	}
	if fresh.property("Identity") != IdentityTPM || k.count() != 1 {
		t.Fatalf("identity not kept in the TPM: %q %d", fresh.property("Identity"), k.count())
	}
	if pair.ID(pair.Identity{Priv: old.svc.identity().Priv}.Public()) != fresh.svc.identity().ID() {
		t.Fatal("the restored key differs")
	}
	if l := fresh.screens(); len(l) != 1 || l[0].ID != "aa11" || l[0].Name != "Living Room TV" {
		t.Fatalf("screens after restore: %+v", l)
	}

	junk := backupFile(t)
	junk.WriteString("not a backup at all, just some text that is long enough")
	junk.Seek(0, io.SeekStart)
	err = fresh.obj.Call(Iface+".Restore", 0, dbus.UnixFD(junk.Fd()), code).Err
	if !errors.As(err, &de) || de.Name != errorPfx+"NotABackup" {
		t.Fatalf("junk: %v", err)
	}
}

func TestBackupNeedsTheSignedInPerson(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root may always back up")
	}
	r := setup(t, uint32(os.Getuid())+1)
	f := backupFile(t)
	err := r.obj.Call(Iface+".Backup", 0, dbus.UnixFD(f.Fd())).Err
	var de dbus.Error
	if !errors.As(err, &de) || de.Name != errorPfx+"NotAllowed" {
		t.Fatalf("stranger backup: %v", err)
	}
	if data, _ := os.ReadFile(f.Name()); len(data) != 0 {
		t.Fatal("a stranger got a backup")
	}
}
