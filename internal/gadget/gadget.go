package gadget

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

var ErrNoPort = errors.New("this computer has no USB port that can act as a device")

type Gadget struct {
	Root   string
	UDCDir string
	Name   string
	rmdir  func(string) error
}

type Config struct {
	Network bool
	Disk    string
	Serial  string
	Product string
}

func New() *Gadget {
	return &Gadget{Root: "/sys/kernel/config/usb_gadget", UDCDir: "/sys/class/udc", Name: "sinty"}
}

func (g *Gadget) dir(parts ...string) string {
	return filepath.Join(append([]string{g.Root, g.Name}, parts...)...)
}

func write(path, value string) error {
	return os.WriteFile(path, []byte(value), 0o644)
}

func (g *Gadget) Port() (string, error) {
	entries, err := os.ReadDir(g.UDCDir)
	if err != nil || len(entries) == 0 {
		return "", ErrNoPort
	}
	return entries[0].Name(), nil
}

func (g *Gadget) State() string {
	port, err := g.Port()
	if err != nil {
		return "unsupported"
	}
	data, err := os.ReadFile(filepath.Join(g.UDCDir, port, "state"))
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(data))
}

func (g *Gadget) Active() bool {
	data, err := os.ReadFile(g.dir("UDC"))
	return err == nil && strings.TrimSpace(string(data)) != ""
}

var owned = map[string]bool{"ncm.usb0": true, "mass_storage.usb0": true}

func (g *Gadget) foreign() (functions, links []string) {
	entries, _ := os.ReadDir(g.dir("functions"))
	for _, e := range entries {
		if !owned[e.Name()] {
			functions = append(functions, e.Name())
		}
	}
	entries, _ = os.ReadDir(g.dir("configs", "c.1"))
	for _, e := range entries {
		if e.Type()&os.ModeSymlink != 0 && !owned[e.Name()] {
			links = append(links, e.Name())
		}
	}
	return functions, links
}

func (g *Gadget) Remove() error {
	rmdir := g.rmdir
	if rmdir == nil {
		rmdir = os.Remove
	}
	if _, err := os.Stat(g.dir()); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	port := ""
	if data, err := os.ReadFile(g.dir("UDC")); err == nil {
		port = strings.TrimSpace(string(data))
	}
	write(g.dir("UDC"), "\n")
	for name := range owned {
		os.Remove(g.dir("configs", "c.1", name))
		rmdir(g.dir("functions", name))
	}
	functions, links := g.foreign()
	if len(functions) > 0 {
		if len(links) > 0 && port != "" {
			return write(g.dir("UDC"), port)
		}
		return nil
	}
	rmdir(g.dir("configs", "c.1", "strings", "0x409"))
	rmdir(g.dir("configs", "c.1"))
	rmdir(g.dir("strings", "0x409"))
	os.Remove(g.dir("functions"))
	os.Remove(g.dir("configs"))
	os.Remove(g.dir("strings"))
	return rmdir(g.dir())
}

func (g *Gadget) Apply(cfg Config) error {
	if err := g.Remove(); err != nil {
		return err
	}
	if !cfg.Network && cfg.Disk == "" {
		return nil
	}
	port, err := g.Port()
	if err != nil {
		return err
	}
	shared, _ := g.foreign()
	write(g.dir("UDC"), "\n")
	steps := []struct{ path, value string }{}
	if len(shared) == 0 {
		steps = append(steps, []struct{ path, value string }{
			{g.dir("idVendor"), "0x1d6b"},
			{g.dir("idProduct"), "0x0104"},
			{g.dir("bcdDevice"), "0x0100"},
			{g.dir("bcdUSB"), "0x0200"},
			{g.dir("strings", "0x409", "manufacturer"), "Sinty"},
			{g.dir("strings", "0x409", "product"), cfg.Product},
			{g.dir("strings", "0x409", "serialnumber"), cfg.Serial},
			{g.dir("configs", "c.1", "strings", "0x409", "configuration"), "Sinty Core"},
			{g.dir("configs", "c.1", "MaxPower"), "250"},
		}...)
	}
	if cfg.Network {
		steps = append(steps, struct{ path, value string }{g.dir("functions", "ncm.usb0", "qmult"), "5"})
	}
	if cfg.Disk != "" {
		steps = append(steps,
			struct{ path, value string }{g.dir("functions", "mass_storage.usb0", "stall"), "1"},
			struct{ path, value string }{g.dir("functions", "mass_storage.usb0", "lun.0", "removable"), "1"},
			struct{ path, value string }{g.dir("functions", "mass_storage.usb0", "lun.0", "ro"), "0"},
			struct{ path, value string }{g.dir("functions", "mass_storage.usb0", "lun.0", "nofua"), "1"},
			struct{ path, value string }{g.dir("functions", "mass_storage.usb0", "lun.0", "file"), cfg.Disk},
		)
	}
	for _, s := range steps {
		if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
			g.Remove()
			return err
		}
		if err := write(s.path, s.value); err != nil {
			g.Remove()
			return err
		}
	}
	for _, f := range []struct {
		on   bool
		name string
	}{{cfg.Network, "ncm.usb0"}, {cfg.Disk != "", "mass_storage.usb0"}} {
		if f.on {
			if err := os.Symlink(g.dir("functions", f.name), g.dir("configs", "c.1", f.name)); err != nil {
				g.Remove()
				return err
			}
		}
	}
	if err := write(g.dir("UDC"), port); err != nil {
		g.Remove()
		return err
	}
	return nil
}

func CreateDisk(path string, size int64) error {
	if st, err := os.Stat(path); err == nil && st.Size() >= size {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Truncate(size)
}

func InUse(path, mountinfo string) bool {
	data, err := os.ReadFile(mountinfo)
	if err != nil {
		return false
	}
	return strings.Contains(string(data), " "+path+" ")
}
