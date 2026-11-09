package service

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/singularityos-lab/sinty-cored/internal/bustest"
	"github.com/singularityos-lab/sinty-cored/internal/channel"
	"github.com/singularityos-lab/sinty-cored/internal/input"
	"github.com/singularityos-lab/sinty-cored/internal/pair"
	"github.com/singularityos-lab/sinty-cored/internal/stream"
	"github.com/singularityos-lab/sinty-cored/internal/terminal"
	"github.com/singularityos-lab/sinty-cored/internal/wl"
)

func identity(t *testing.T) pair.Identity {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pair.Identity{Priv: priv}
}

type trust struct {
	mu sync.Mutex
	m  map[string]string
}

func (t *trust) Screens() (map[string]string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := map[string]string{}
	for k, v := range t.m {
		out[k] = v
	}
	return out, nil
}
func (t *trust) Add(id, name string) error { t.mu.Lock(); t.m[id] = name; t.mu.Unlock(); return nil }
func (t *trust) Remove(id string) error    { t.mu.Lock(); delete(t.m, id); t.mu.Unlock(); return nil }

type power struct {
	mu        sync.Mutex
	streaming []bool
	form      string
}

func (p *power) SetStreaming(on bool) error {
	p.mu.Lock()
	p.streaming = append(p.streaming, on)
	p.mu.Unlock()
	return nil
}
func (p *power) SetForm(f string) error { p.mu.Lock(); p.form = f; p.mu.Unlock(); return nil }
func (p *power) Form() string           { p.mu.Lock(); defer p.mu.Unlock(); return p.form }
func (p *power) states() []bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]bool{}, p.streaming...)
}

type source struct{ n int }

func (s *source) Capture(cursor, onDamage bool, into []byte) (*wl.Frame, error) {
	if s.n > 0 {
		time.Sleep(10 * time.Millisecond)
		return nil, nil
	}
	s.n++
	return &wl.Frame{W: 64, H: 48, Stride: 256, Pix: bytes.Repeat([]byte{7, 8, 9, 255}, 64*48)}, nil
}

type sink struct {
	mu   sync.Mutex
	keys []uint32
}

func (k *sink) MoveTo(x, y, w, h uint32) {}
func (k *sink) Button(uint32, bool)      {}
func (k *sink) Scroll(bool, int32)       {}
func (k *sink) Key(code uint32, p bool)  { k.mu.Lock(); k.keys = append(k.keys, code); k.mu.Unlock() }
func (k *sink) Flush() error             { return nil }
func (k *sink) got() []uint32            { k.mu.Lock(); defer k.mu.Unlock(); return append([]uint32{}, k.keys...) }

type desktop struct {
	owner  uint32
	sink   *sink
	layout chan string
}

func (d *desktop) Owner() (uint32, bool) { return d.owner, true }
func (d *desktop) Open(layout string) (stream.Source, stream.Sink, func(), error) {
	d.layout <- layout
	return &source{}, d.sink, func() {}, nil
}

type display struct {
	mu    sync.Mutex
	texts []string
	draws int
}

func (d *display) Clear() {}
func (d *display) Text(s string, _, _, _ int, _ uint32) {
	d.mu.Lock()
	d.texts = append(d.texts, s)
	d.mu.Unlock()
}
func (d *display) Draw([]byte, int, int) { d.mu.Lock(); d.draws++; d.mu.Unlock() }
func (d *display) code() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, s := range d.texts {
		if len(s) == 7 && s[3] == ' ' {
			return s[:3] + s[4:]
		}
	}
	return ""
}
func (d *display) frames() int { d.mu.Lock(); defer d.mu.Unlock(); return d.draws }

type rig struct {
	t      *testing.T
	svc    *Service
	obj    dbus.BusObject
	client *dbus.Conn
	addr   string
	tr     *trust
	pw     *power
	dk     *desktop
	sigs   chan *dbus.Signal
}

func setup(t *testing.T, owner uint32) *rig {
	bus := bustest.Start(t)
	server := bustest.Connect(t, bus)
	r := &rig{t: t, tr: &trust{m: map[string]string{}}, pw: &power{form: "laptop"}}
	r.dk = &desktop{owner: owner, sink: &sink{}, layout: make(chan string, 4)}
	r.svc = New(Config{
		Identity: identity(t), Name: "Desk Core", Trust: r.tr, Power: r.pw, Desktop: r.dk,
		LAN: func() bool { return true }, PairWindow: 5 * time.Second,
	})
	if err := Export(server, r.svc); err != nil {
		t.Fatal(err)
	}
	server.RequestName(Name, dbus.NameFlagDoNotQueue)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go r.svc.Serve(l)
	r.addr = l.Addr().String()
	r.client = bustest.Connect(t, bus)
	r.obj = r.client.Object(Name, Path)
	r.client.AddMatchSignal(dbus.WithMatchInterface(Iface))
	r.sigs = make(chan *dbus.Signal, 16)
	r.client.Signal(r.sigs)
	return r
}

func (r *rig) signal(name string) []any {
	r.t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case s := <-r.sigs:
			if s.Name == Iface+"."+name {
				return s.Body
			}
		case <-deadline:
			r.t.Fatalf("no %s signal", name)
		}
	}
}

func (r *rig) screens() []Screen {
	var v []Screen
	if err := r.obj.StoreProperty(Iface+".Screens", &v); err != nil {
		r.t.Fatal(err)
	}
	return v
}

func (r *rig) dial() net.Conn {
	c, err := net.Dial("tcp", r.addr)
	if err != nil {
		r.t.Fatal(err)
	}
	return c
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for i := 0; i < 300; i++ {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestPairStreamControlAndRevoke(t *testing.T) {
	r := setup(t, uint32(os.Getuid()))
	disp := &display{}
	term := &terminal.Terminal{Identity: identity(t), Name: "Living Room TV", Layout: "it", State: t.TempDir(), Display: disp, W: 640, H: 480}

	c := r.dial()
	if _, err := term.Pair(c); err == nil {
		t.Fatal("paired while the Core was not accepting screens")
	}
	c.Close()

	var route string
	if err := r.obj.Call(Iface+".StartPairing", 0).Store(&route); err != nil || route != "network" {
		t.Fatalf("start pairing: %q %v", route, err)
	}
	paired := make(chan error, 1)
	go func() {
		c := r.dial()
		defer c.Close()
		_, err := term.Pair(c)
		paired <- err
	}()
	if body := r.signal("PairingRequest"); body[0] != "Living Room TV" {
		t.Fatalf("request %v", body)
	}
	eventually(t, "code on the screen", func() bool { return disp.code() != "" })
	if err := r.obj.Call(Iface+".ConfirmPairing", 0, disp.code()).Err; err != nil {
		t.Fatal(err)
	}
	if body := r.signal("PairingFinished"); body[1] != "paired" {
		t.Fatalf("finished %v", body)
	}
	if err := <-paired; err != nil {
		t.Fatal(err)
	}
	var pairing bool
	r.obj.StoreProperty(Iface+".Pairing", &pairing)
	if pairing {
		t.Fatal("pairing window still open after a screen paired")
	}
	if s := r.screens(); len(s) != 1 || s[0].Name != "Living Room TV" || s[0].ID != term.Identity.ID() || s[0].Connected {
		t.Fatalf("screens %+v", s)
	}

	sess, err := term.Open(r.dial())
	if err != nil {
		t.Fatal(err)
	}
	shown := make(chan error, 1)
	go func() { shown <- term.Show(sess) }()
	if l := <-r.dk.layout; l != "it" {
		t.Fatalf("layout %q", l)
	}
	eventually(t, "first frame", func() bool { return disp.frames() > 0 })
	if s := r.screens(); !s[0].Connected {
		t.Fatalf("screen not shown as connected: %+v", s)
	}
	if err := sess.Send([]input.Event{{Kind: input.Key, Code: 30, Value: 1}, {Kind: input.Key, Code: 30}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "keys on the Core", func() bool { return len(r.dk.sink.got()) == 2 })
	if st := r.pw.states(); len(st) != 1 || !st[0] {
		t.Fatalf("power while streaming %v", st)
	}

	if err := r.obj.Call(Iface+".RevokeScreen", 0, term.Identity.ID()).Err; err != nil {
		t.Fatal(err)
	}
	select {
	case <-shown:
	case <-time.After(5 * time.Second):
		t.Fatal("session kept running after the screen was removed")
	}
	eventually(t, "streaming off", func() bool { st := r.pw.states(); return len(st) == 2 && !st[1] })
	if s := r.screens(); len(s) != 0 {
		t.Fatalf("screens after revoke %+v", s)
	}
	if _, err := term.Open(r.dial()); !errors.Is(err, channel.ErrUntrusted) {
		t.Fatalf("removed screen opened a session: %v", err)
	}
}

func TestWrongCodeKeepsTheScreenOut(t *testing.T) {
	r := setup(t, uint32(os.Getuid()))
	disp := &display{}
	term := &terminal.Terminal{Identity: identity(t), Name: "Kitchen", State: t.TempDir(), Display: disp, W: 640, H: 480}
	r.obj.Call(Iface+".StartPairing", 0)
	go func() {
		c := r.dial()
		defer c.Close()
		term.Pair(c)
	}()
	r.signal("PairingRequest")
	eventually(t, "code on the screen", func() bool { return disp.code() != "" })
	wrong := []byte(disp.code())
	wrong[0] = '0' + (wrong[0]-'0'+1)%10
	r.obj.Call(Iface+".ConfirmPairing", 0, string(wrong))
	if body := r.signal("PairingFinished"); body[1] != "wrong-code" {
		t.Fatalf("finished %v", body)
	}
	if len(r.screens()) != 0 {
		t.Fatal("screen trusted with a wrong code")
	}
	var pairing bool
	r.obj.StoreProperty(Iface+".Pairing", &pairing)
	if !pairing {
		t.Fatal("a wrong code closed the pairing window")
	}
	r.obj.Call(Iface+".CancelPairing", 0)
	r.obj.StoreProperty(Iface+".Pairing", &pairing)
	if pairing {
		t.Fatal("cancel left the window open")
	}
}

func TestOnlyTheSignedInPersonCanChangeThings(t *testing.T) {
	r := setup(t, uint32(os.Getuid())+1)
	if os.Getuid() == 0 {
		t.Skip("root is always allowed")
	}
	for _, call := range []struct {
		m    string
		args []any
	}{{"StartPairing", nil}, {"RevokeScreen", []any{"x"}}, {"SetForm", []any{"tv"}}, {"DirectNetwork", nil}} {
		err := r.obj.Call(Iface+"."+call.m, 0, call.args...).Err
		var de dbus.Error
		if !errors.As(err, &de) || de.Name != errorPfx+"NotAllowed" {
			t.Fatalf("%s: %v", call.m, err)
		}
	}
}

func TestFormAndDiscovery(t *testing.T) {
	r := setup(t, uint32(os.Getuid()))
	if err := r.obj.Call(Iface+".SetForm", 0, "tv").Err; err != nil {
		t.Fatal(err)
	}
	var form string
	r.obj.StoreProperty(Iface+".Form", &form)
	if form != "tv" || r.pw.Form() != "tv" {
		t.Fatalf("form %q", form)
	}
	if err := r.obj.Call(Iface+".SetUsbMode", 0, "disk").Err; err == nil {
		t.Fatal("USB mode set without a USB port")
	}

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	go r.svc.Discovery(pc, ScreenPort)
	probe, err := net.Dial("udp", pc.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	probe.Write([]byte(Probe))
	probe.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 256)
	n, err := probe.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	var a announce
	if json.Unmarshal(buf[:n], &a) != nil || a.Name != "Desk Core" || a.Port != ScreenPort || a.ID != r.svc.cfg.Identity.ID() {
		t.Fatalf("announce %s", buf[:n])
	}
}
