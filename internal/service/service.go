package service

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"
	"github.com/godbus/dbus/v5/prop"

	"github.com/singularityos-lab/sinty-cored/internal/channel"
	"github.com/singularityos-lab/sinty-cored/internal/link"
	"github.com/singularityos-lab/sinty-cored/internal/p2p"
	"github.com/singularityos-lab/sinty-cored/internal/pair"
	"github.com/singularityos-lab/sinty-cored/internal/stream"
)

const (
	Name          = "dev.sinty.Core1"
	Path          = dbus.ObjectPath("/dev/sinty/Core1")
	Iface         = "dev.sinty.Core1"
	errorPfx      = Iface + ".Error."
	ScreenPort    = 7412
	DiscoveryPort = 7413
	Probe         = "sinty-core?"
)

var (
	errCancelled = errors.New("pairing was cancelled")
	errNoDesktop = errors.New("nobody is signed in on the Core")
)

type Screen struct {
	ID        string
	Name      string
	Connected bool
}

type Trust interface {
	Screens() (map[string]string, error)
	Add(id, name string) error
	Remove(id string) error
}

type Power interface {
	SetStreaming(on bool) error
	SetEncoder(device string) error
	SetForm(form string) error
	Form() string
}

type Desktop interface {
	Open(layout string) (stream.Source, stream.Sink, func(), error)
	Owner() (uint32, bool)
}

type USB interface {
	Apply(mode string) error
	Cable() bool
}

type Codecs interface {
	Available() bool
	Pick(accept []string) (stream.Hardware, string)
}

type Direct interface {
	Supported() bool
	Start(timeout time.Duration) (p2p.Group, error)
	Stop(g p2p.Group) error
}

type Vault interface {
	Adopt(seed []byte) (bool, error)
}

const (
	IdentityTPM      = "tpm"
	IdentityFile     = "file"
	IdentityReplaced = "replaced"
)

type Config struct {
	Identity   pair.Identity
	Kept       string
	Vault      Vault
	Name       string
	Trust      Trust
	Power      Power
	Desktop    Desktop
	USB        USB
	Direct     Direct
	Codecs     Codecs
	LAN        func() bool
	PairWindow time.Duration
	OnPresence func([]byte)
}

type session struct {
	conn *channel.Conn
	stop chan struct{}
	once sync.Once
}

func (s *session) end() { s.once.Do(func() { close(s.stop) }) }

type Service struct {
	cfg   Config
	conn  *dbus.Conn
	props *prop.Properties

	mu       sync.Mutex
	pairing  bool
	busy     bool
	cancel   chan struct{}
	code     chan string
	timer    *time.Timer
	sessions map[string]*session
	group    *p2p.Group
	usbMode  string
	id       pair.Identity
	kept     string
}

func New(cfg Config) *Service {
	if cfg.PairWindow == 0 {
		cfg.PairWindow = 2 * time.Minute
	}
	if cfg.LAN == nil {
		cfg.LAN = func() bool { return link.OnLAN(link.System()) }
	}
	if cfg.Kept == "" {
		cfg.Kept = IdentityFile
	}
	return &Service{cfg: cfg, sessions: map[string]*session{}, usbMode: "off", id: cfg.Identity, kept: cfg.Kept}
}

type object struct{ s *Service }

func Export(conn *dbus.Conn, s *Service) error {
	s.conn = conn
	o := &object{s}
	if err := conn.Export(o, Path, Iface); err != nil {
		return err
	}
	form := ""
	if s.cfg.Power != nil {
		form = s.cfg.Power.Form()
	}
	props, err := prop.Export(conn, Path, prop.Map{Iface: {
		"Name":          {Value: s.cfg.Name, Emit: prop.EmitTrue},
		"ID":            {Value: s.identity().ID(), Emit: prop.EmitTrue},
		"Identity":      {Value: s.kept, Emit: prop.EmitTrue},
		"Screens":       {Value: []Screen{}, Emit: prop.EmitTrue},
		"Pairing":       {Value: false, Emit: prop.EmitTrue},
		"PairingScreen": {Value: "", Emit: prop.EmitTrue},
		"Route":         {Value: string(link.None), Emit: prop.EmitTrue},
		"Direct":        {Value: "", Emit: prop.EmitTrue},
		"Phone":         {Value: "", Emit: prop.EmitTrue},
		"UsbMode":       {Value: "off", Emit: prop.EmitTrue},
		"Encoder":       {Value: s.encoderKind(), Emit: prop.EmitFalse},
		"Form":          {Value: form, Emit: prop.EmitTrue},
	}})
	if err != nil {
		return err
	}
	s.props = props
	node := &introspect.Node{
		Name: string(Path),
		Interfaces: []introspect.Interface{introspect.IntrospectData, prop.IntrospectData, {
			Name:       Iface,
			Methods:    introspect.Methods(o),
			Properties: props.Introspection(Iface),
			Signals: []introspect.Signal{
				{Name: "PairingRequest", Args: []introspect.Arg{{Name: "screen", Type: "s"}}},
				{Name: "PairingFinished", Args: []introspect.Arg{{Name: "screen", Type: "s"}, {Name: "result", Type: "s"}}},
				{Name: "PhoneCommand", Args: []introspect.Arg{{Name: "device", Type: "s"}, {Name: "command", Type: "s"}}},
			},
		}},
	}
	if err := conn.Export(introspect.NewIntrospectable(node), Path, "org.freedesktop.DBus.Introspectable"); err != nil {
		return err
	}
	s.refreshScreens()
	return nil
}

func dbusErr(name string, err error) *dbus.Error {
	return dbus.NewError(errorPfx+name, []interface{}{err.Error()})
}

func (s *Service) set(name string, v any) {
	if s.props != nil {
		s.props.SetMust(Iface, name, v)
	}
}

func (s *Service) emit(name string, args ...any) {
	if s.conn != nil {
		s.conn.Emit(Path, Iface+"."+name, args...)
	}
}

func (s *Service) allowed(sender dbus.Sender) bool {
	if s.conn == nil {
		return false
	}
	var uid uint32
	if err := s.conn.BusObject().Call("org.freedesktop.DBus.GetConnectionUnixUser", 0, string(sender)).Store(&uid); err != nil {
		return false
	}
	if uid == 0 {
		return true
	}
	if s.cfg.Desktop == nil {
		return false
	}
	owner, ok := s.cfg.Desktop.Owner()
	return ok && owner == uid
}

var errNotAllowed = errors.New("only the person signed in on the Core can change this")

func (s *Service) route() link.Route {
	return link.Choose(link.State{
		Cable:  s.cfg.USB != nil && s.cfg.USB.Cable(),
		LAN:    s.cfg.LAN(),
		Direct: s.cfg.Direct != nil && s.cfg.Direct.Supported(),
	})
}

func (o *object) StartPairing(sender dbus.Sender) (string, *dbus.Error) {
	s := o.s
	if !s.allowed(sender) {
		return "", dbusErr("NotAllowed", errNotAllowed)
	}
	r := s.route()
	s.set("Route", string(r))
	if r == link.Direct {
		if err := s.startDirect(); err != nil {
			return "", dbusErr("NoLink", err)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.pairing {
		s.pairing = true
		s.cancel = make(chan struct{})
		s.timer = time.AfterFunc(s.cfg.PairWindow, s.closePairing)
		s.set("Pairing", true)
	} else {
		s.timer.Reset(s.cfg.PairWindow)
	}
	return string(r), nil
}

func (o *object) ConfirmPairing(sender dbus.Sender, code string) *dbus.Error {
	s := o.s
	if !s.allowed(sender) {
		return dbusErr("NotAllowed", errNotAllowed)
	}
	s.mu.Lock()
	ch := s.code
	s.mu.Unlock()
	if ch == nil {
		return dbusErr("NotPairing", errors.New("no screen is waiting for a code"))
	}
	select {
	case ch <- code:
	default:
	}
	return nil
}

func (o *object) CancelPairing(sender dbus.Sender) *dbus.Error {
	if !o.s.allowed(sender) {
		return dbusErr("NotAllowed", errNotAllowed)
	}
	o.s.closePairing()
	return nil
}

func (o *object) RevokeScreen(sender dbus.Sender, id string) *dbus.Error {
	s := o.s
	if !s.allowed(sender) {
		return dbusErr("NotAllowed", errNotAllowed)
	}
	if err := s.cfg.Trust.Remove(id); err != nil {
		return dbusErr("Failed", err)
	}
	s.DropScreen(id)
	return nil
}

func (o *object) SetUsbMode(sender dbus.Sender, mode string) *dbus.Error {
	s := o.s
	if !s.allowed(sender) {
		return dbusErr("NotAllowed", errNotAllowed)
	}
	if s.cfg.USB == nil {
		return dbusErr("NotSupported", errors.New("this Core has no USB-C port that can act as a device"))
	}
	if mode != "off" && mode != "network" && mode != "disk" {
		return dbusErr("Failed", errors.New("unknown USB mode "+mode))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.cfg.USB.Apply(mode); err != nil {
		return dbusErr("Failed", err)
	}
	s.usbMode = mode
	s.set("UsbMode", mode)
	return nil
}

func (o *object) SetForm(sender dbus.Sender, form string) *dbus.Error {
	s := o.s
	if !s.allowed(sender) {
		return dbusErr("NotAllowed", errNotAllowed)
	}
	if s.cfg.Power == nil {
		return dbusErr("NotSupported", errors.New("power service not available"))
	}
	if err := s.cfg.Power.SetForm(form); err != nil {
		return dbusErr("Failed", err)
	}
	s.set("Form", s.cfg.Power.Form())
	return nil
}

func (o *object) DirectNetwork(sender dbus.Sender) (string, string, *dbus.Error) {
	s := o.s
	if !s.allowed(sender) {
		return "", "", dbusErr("NotAllowed", errNotAllowed)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.group == nil {
		return "", "", dbusErr("NoLink", errors.New("no direct connection is open"))
	}
	return s.group.SSID, s.group.Passphrase, nil
}

func (s *Service) startDirect() error {
	s.mu.Lock()
	open := s.group != nil
	s.mu.Unlock()
	if open {
		return nil
	}
	g, err := s.cfg.Direct.Start(30 * time.Second)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.group = &g
	s.mu.Unlock()
	s.set("Direct", g.SSID)
	return nil
}

func (s *Service) stopDirectLocked() {
	if s.group == nil || s.pairing || len(s.sessions) > 0 {
		return
	}
	g := *s.group
	s.group = nil
	go s.cfg.Direct.Stop(g)
	s.set("Direct", "")
}

func (s *Service) closePairing() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.pairing {
		return
	}
	s.pairing = false
	close(s.cancel)
	s.timer.Stop()
	s.set("Pairing", false)
	s.set("PairingScreen", "")
	s.stopDirectLocked()
}

func (s *Service) refreshScreens() {
	trusted, err := s.cfg.Trust.Screens()
	if err != nil {
		log.Printf("screens not read: %v", err)
		trusted = map[string]string{}
	}
	s.mu.Lock()
	list := make([]Screen, 0, len(trusted))
	for id, name := range trusted {
		_, on := s.sessions[id]
		list = append(list, Screen{id, name, on})
	}
	n := len(s.sessions)
	s.mu.Unlock()
	sort.Slice(list, func(i, j int) bool {
		return list[i].Name < list[j].Name || list[i].Name == list[j].Name && list[i].ID < list[j].ID
	})
	s.set("Screens", list)
	if s.cfg.OnPresence != nil {
		data, _ := json.Marshal(struct {
			ID      string
			Name    string
			Showing int
		}{s.identity().ID(), s.cfg.Name, n})
		s.cfg.OnPresence(data)
	}
}

func (s *Service) DropScreen(id string) {
	s.mu.Lock()
	if sess := s.sessions[id]; sess != nil {
		sess.end()
	}
	s.mu.Unlock()
	s.refreshScreens()
}

func (s *Service) Phone(device, command string) {
	s.set("Phone", device)
	s.emit("PhoneCommand", device, command)
}

func (s *Service) Follow(conn *dbus.Conn) {
	conn.AddMatchSignal(dbus.WithMatchInterface("dev.sinty.Key1"), dbus.WithMatchMember("Revoked"))
	ch := make(chan *dbus.Signal, 16)
	conn.Signal(ch)
	for sig := range ch {
		if sig.Name != "dev.sinty.Key1.Revoked" || len(sig.Body) != 2 {
			continue
		}
		kind, _ := sig.Body[0].(string)
		id, _ := sig.Body[1].(string)
		if kind == KindScreen {
			s.DropScreen(id)
		}
	}
}

type rwc struct {
	io.Reader
	net.Conn
}

func (r rwc) Read(p []byte) (int, error) { return r.Reader.Read(p) }

func (s *Service) Serve(l net.Listener) error {
	for {
		c, err := l.Accept()
		if err != nil {
			return err
		}
		go s.handle(c)
	}
}

func (s *Service) handle(c net.Conn) {
	c.SetDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(c)
	head, err := br.Peek(5)
	if err != nil {
		c.Close()
		return
	}
	rw := rwc{br, c}
	switch head[4] {
	case 1:
		s.pair(rw)
	case 10:
		s.session(rw)
	default:
		c.Close()
	}
}

func (s *Service) pair(rw rwc) {
	defer rw.Close()
	s.mu.Lock()
	if !s.pairing || s.busy {
		s.mu.Unlock()
		return
	}
	s.busy = true
	cancel := s.cancel
	code := make(chan string, 1)
	s.code = code
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.busy = false
		s.code = nil
		s.mu.Unlock()
	}()
	rw.SetDeadline(time.Now().Add(s.cfg.PairWindow + 10*time.Second))
	screen := ""
	peer, err := pair.Core(rw, s.identity(), s.cfg.Name, func(name string) (string, error) {
		screen = name
		s.set("PairingScreen", name)
		s.emit("PairingRequest", name)
		select {
		case typed := <-code:
			return typed, nil
		case <-cancel:
			return "", errCancelled
		}
	})
	result := "paired"
	switch {
	case err == nil:
		if err := s.cfg.Trust.Add(peer.ID, peer.Name); err != nil {
			log.Printf("screen %s not remembered: %v", peer.Name, err)
			result = "failed"
		}
	case errors.Is(err, pair.ErrWrongCode):
		result = "wrong-code"
	case errors.Is(err, errCancelled):
		result = "cancelled"
	default:
		result = "failed"
	}
	s.set("PairingScreen", "")
	s.emit("PairingFinished", screen, result)
	if result == "paired" {
		s.closePairing()
		s.refreshScreens()
	}
}

func (s *Service) session(rw rwc) {
	defer rw.Close()
	trusted, err := s.cfg.Trust.Screens()
	if err != nil {
		return
	}
	conn, req, err := channel.Core(rw, s.identity(), func(id string) (string, bool) {
		name, ok := trusted[id]
		return name, ok
	})
	if err != nil {
		return
	}
	rw.SetDeadline(time.Time{})
	if s.cfg.Desktop == nil {
		conn.Send(stream.Notice, []byte(errNoDesktop.Error()))
		return
	}
	src, sink, done, err := s.cfg.Desktop.Open(req.Layout)
	if err != nil {
		conn.Send(stream.Notice, []byte(err.Error()))
		return
	}
	defer done()
	var hw stream.Hardware
	device := ""
	if s.cfg.Codecs != nil {
		hw, device = s.cfg.Codecs.Pick(req.Codecs)
	}
	id := req.Peer.ID
	sess := &session{conn: conn, stop: make(chan struct{})}
	s.mu.Lock()
	if old := s.sessions[id]; old != nil {
		old.end()
	}
	s.sessions[id] = sess
	first := len(s.sessions) == 1
	s.mu.Unlock()
	if first && s.cfg.Power != nil {
		if err := s.cfg.Power.SetStreaming(true); err != nil {
			log.Printf("streaming state not set: %v", err)
		}
	}
	if device != "" && s.cfg.Power != nil {
		if err := s.cfg.Power.SetEncoder(device); err != nil {
			log.Printf("encoder not reported: %v", err)
		}
	}
	s.refreshScreens()
	stream.Serve(conn, src, sink, hw, sess.stop)
	s.mu.Lock()
	if s.sessions[id] == sess {
		delete(s.sessions, id)
	}
	last := len(s.sessions) == 0
	if last {
		s.stopDirectLocked()
	}
	s.mu.Unlock()
	if last && s.cfg.Power != nil {
		if err := s.cfg.Power.SetStreaming(false); err != nil {
			log.Printf("streaming state not cleared: %v", err)
		}
		if s.cfg.Codecs != nil && s.cfg.Codecs.Available() {
			if err := s.cfg.Power.SetEncoder(""); err != nil {
				log.Printf("encoder not cleared: %v", err)
			}
		}
	}
	s.refreshScreens()
}

func (s *Service) encoderKind() string {
	if s.cfg.Codecs != nil && s.cfg.Codecs.Available() {
		return "hardware"
	}
	return "software"
}

type announce struct {
	Name string
	ID   string
	Port int
}

func (s *Service) Discovery(pc net.PacketConn, port int) error {
	buf := make([]byte, 64)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			return err
		}
		if string(buf[:n]) == Probe {
			reply, _ := json.Marshal(announce{s.cfg.Name, s.identity().ID(), port})
			pc.WriteTo(reply, from)
		}
	}
}
