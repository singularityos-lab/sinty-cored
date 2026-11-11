package service

import (
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"

	"github.com/godbus/dbus/v5"

	"github.com/singularityos-lab/sinty-cored/internal/gadget"
	"github.com/singularityos-lab/sinty-cored/internal/stream"
	"github.com/singularityos-lab/sinty-cored/internal/v4l2"
	"github.com/singularityos-lab/sinty-cored/internal/wl"
)

const KindScreen = "screen"

type KeyTrust struct{ Conn *dbus.Conn }

func (k KeyTrust) obj() dbus.BusObject {
	return k.Conn.Object("dev.sinty.Key1", "/dev/sinty/Key1")
}

func (k KeyTrust) Screens() (map[string]string, error) {
	var list []struct {
		Kind  string
		ID    string
		Name  string
		Added int64
		Last  int64
	}
	if err := k.obj().Call("dev.sinty.Key1.Devices", 0).Store(&list); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, d := range list {
		if d.Kind == KindScreen {
			out[d.ID] = d.Name
		}
	}
	return out, nil
}

func (k KeyTrust) Add(id, name string) error {
	return k.obj().Call("dev.sinty.Key1.Trust", 0, KindScreen, id, name).Err
}

func (k KeyTrust) Remove(id string) error {
	return k.obj().Call("dev.sinty.Key1.Revoke", 0, KindScreen, id).Err
}

type Powerd struct{ Conn *dbus.Conn }

func (p Powerd) obj() dbus.BusObject {
	return p.Conn.Object("dev.sinty.Power1", "/dev/sinty/Power1")
}

func (p Powerd) SetStreaming(on bool) error {
	return p.obj().Call("dev.sinty.Power1.SetStreaming", 0, on).Err
}

func (p Powerd) SetEncoder(device string) error {
	return p.obj().Call("dev.sinty.Power1.SetEncoder", 0, device).Err
}

type V4L2 struct {
	Dir  string
	list []v4l2.Codec
}

func NewV4L2(dir, helper string) *V4L2 {
	return &V4L2{Dir: dir, list: v4l2.FindAll(dir, helper, true)}
}

func (v *V4L2) Available() bool { return len(v.list) > 0 }

func (v *V4L2) Pick(accept []string) (stream.Hardware, string) {
	for _, c := range v.list {
		if !slices.Contains(accept, v4l2.Name(c.Format)) {
			continue
		}
		return func(w, h int) (stream.Coder, error) { return v4l2.NewEncoder(c, w, h) }, c.Path
	}
	return nil, ""
}

func (p Powerd) SetForm(form string) error {
	return p.obj().Call("dev.sinty.Power1.SetTerminal", 0, form).Err
}

func (p Powerd) Form() string {
	v, err := p.obj().GetProperty("dev.sinty.Power1.Form")
	if err != nil {
		return ""
	}
	s, _ := v.Value().(string)
	return s
}

type Wayland struct {
	RunDir  string
	Skip    []string
	MinUID  uint32
	Connect func(socket string) (*wl.Client, error)
}

func NewWayland() *Wayland {
	return &Wayland{RunDir: "/run/user", Skip: []string{"greeter"}, MinUID: 1000, Connect: wl.Connect}
}

func (w *Wayland) socket() (string, uint32, bool) {
	matches, _ := filepath.Glob(filepath.Join(w.RunDir, "*", "wayland-[0-9]*"))
	best, bestUID, bestTime := "", uint32(0), int64(0)
	for _, m := range matches {
		if filepath.Ext(m) == ".lock" {
			continue
		}
		st, err := os.Stat(m)
		if err != nil || st.Mode()&os.ModeSocket == 0 {
			continue
		}
		uid := st.Sys().(*syscall.Stat_t).Uid
		if uid < w.MinUID || w.skipped(uid) {
			continue
		}
		if t := st.ModTime().UnixNano(); best == "" || t > bestTime {
			best, bestUID, bestTime = m, uid, t
		}
	}
	return best, bestUID, best != ""
}

func (w *Wayland) skipped(uid uint32) bool {
	u, err := user.LookupId(strconv.Itoa(int(uid)))
	if err != nil {
		return false
	}
	for _, s := range w.Skip {
		if u.Username == s {
			return true
		}
	}
	return false
}

func (w *Wayland) Owner() (uint32, bool) {
	_, uid, ok := w.socket()
	return uid, ok
}

func (w *Wayland) Open(layout string) (stream.Source, stream.Sink, func(), error) {
	path, _, ok := w.socket()
	if !ok {
		return nil, nil, nil, errNoDesktop
	}
	c, err := w.Connect(path)
	if err != nil {
		return nil, nil, nil, err
	}
	if !c.CanCapture() || !c.CanInject() {
		c.Close()
		return nil, nil, nil, errors.New("the desktop does not allow screen sharing")
	}
	if err := c.StartInput(layout); err != nil {
		c.Close()
		return nil, nil, nil, err
	}
	return c, c, func() { c.Close() }, nil
}

type GadgetUSB struct {
	G         *gadget.Gadget
	Disk      string
	Size      int64
	Serial    string
	MountInfo string
}

func (u *GadgetUSB) Cable() bool { return u.G.State() == "configured" }

func (u *GadgetUSB) Apply(mode string) error {
	cfg := gadget.Config{Serial: u.Serial, Product: "Sinty Core"}
	switch mode {
	case "network":
		cfg.Network = true
	case "disk":
		mi := u.MountInfo
		if mi == "" {
			mi = "/proc/self/mountinfo"
		}
		if gadget.InUse(u.Disk, mi) {
			return errors.New("the Core disk is open on this computer")
		}
		if err := gadget.CreateDisk(u.Disk, u.Size); err != nil {
			return err
		}
		cfg.Disk = u.Disk
	}
	return u.G.Apply(cfg)
}
