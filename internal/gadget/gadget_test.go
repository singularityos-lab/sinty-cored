package gadget

import (
	"os"
	"path/filepath"
	"testing"
)

func fake(t *testing.T) *Gadget {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "udc", "fe980000.usb"), 0o755)
	os.WriteFile(filepath.Join(root, "udc", "fe980000.usb", "state"), []byte("configured\n"), 0o644)
	os.MkdirAll(filepath.Join(root, "usb_gadget"), 0o755)
	return &Gadget{Root: filepath.Join(root, "usb_gadget"), UDCDir: filepath.Join(root, "udc"), Name: "sinty", rmdir: os.RemoveAll}
}

func read(t *testing.T, p string) string {
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestDiskAndNetworkOverOnePort(t *testing.T) {
	g := fake(t)
	disk := filepath.Join(t.TempDir(), "disk.img")
	if err := CreateDisk(disk, 1<<30); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(disk); st.Size() != 1<<30 {
		t.Fatal("disk size")
	}
	if err := g.Apply(Config{Network: true, Disk: disk, Serial: "abc", Product: "Sinty Core"}); err != nil {
		t.Fatal(err)
	}
	if read(t, g.dir("UDC")) != "fe980000.usb" || read(t, g.dir("functions", "mass_storage.usb0", "lun.0", "file")) != disk {
		t.Fatal("gadget not bound to the disk")
	}
	for _, f := range []string{"ncm.usb0", "mass_storage.usb0"} {
		if dst, err := os.Readlink(g.dir("configs", "c.1", f)); err != nil || dst != g.dir("functions", f) {
			t.Fatalf("%s not linked: %v", f, err)
		}
	}
	if !g.Active() || g.State() != "configured" {
		t.Fatal("state")
	}
	if err := g.Apply(Config{Network: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(g.dir("configs", "c.1", "mass_storage.usb0")); err == nil {
		t.Fatal("disk still shared after turning it off")
	}
}

func TestNoPort(t *testing.T) {
	g := fake(t)
	os.RemoveAll(g.UDCDir)
	if err := g.Apply(Config{Network: true}); err != ErrNoPort {
		t.Fatal(err)
	}
	if g.State() != "unsupported" {
		t.Fatal(g.State())
	}
}

func TestInUse(t *testing.T) {
	info := filepath.Join(t.TempDir(), "mountinfo")
	os.WriteFile(info, []byte("40 1 7:0 / /mnt rw - ext4 /var/lib/sinty-cored/disk.img rw\n"), 0o644)
	if !InUse("/var/lib/sinty-cored/disk.img", info) || InUse("/other.img", info) {
		t.Fatal("in use")
	}
}
