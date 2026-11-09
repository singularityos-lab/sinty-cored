package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/singularityos-lab/sinty-cored/internal/channel"
	"github.com/singularityos-lab/sinty-cored/internal/fb"
	"github.com/singularityos-lab/sinty-cored/internal/pair"
	"github.com/singularityos-lab/sinty-cored/internal/service"
	"github.com/singularityos-lab/sinty-cored/internal/terminal"
	"github.com/singularityos-lab/sinty-cored/internal/v4l2"
)

type found struct {
	Name string
	ID   string
	Port int
	Addr string
}

func discover(wait time.Duration) []found {
	pc, err := net.ListenPacket("udp4", ":0")
	if err != nil {
		return nil
	}
	defer pc.Close()
	if raw, err := pc.(*net.UDPConn).SyscallConn(); err == nil {
		raw.Control(func(fd uintptr) { syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1) })
	}
	pc.WriteTo([]byte(service.Probe), &net.UDPAddr{IP: net.IPv4bcast, Port: service.DiscoveryPort})
	pc.SetReadDeadline(time.Now().Add(wait))
	var out []found
	buf := make([]byte, 512)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			return out
		}
		var f found
		if json.Unmarshal(buf[:n], &f) != nil || f.Port <= 0 {
			continue
		}
		f.Addr = net.JoinHostPort(from.(*net.UDPAddr).IP.String(), fmt.Sprint(f.Port))
		out = append(out, f)
	}
}

func locate(fixed, want string) (string, error) {
	if fixed != "" {
		return fixed, nil
	}
	for _, f := range discover(2 * time.Second) {
		if want == "" || f.ID == want {
			return f.Addr, nil
		}
	}
	return "", errors.New("no Sinty Core found on this network")
}

type link struct {
	mu   sync.Mutex
	sess *terminal.Session
	ptr  *fb.Pointer
}

func (l *link) feed(dev int, typ, code uint16, value int32) {
	l.mu.Lock()
	events := l.ptr.Feed(dev, typ, code, value)
	s := l.sess
	l.mu.Unlock()
	if s != nil && len(events) > 0 {
		s.Send(events)
	}
}

func (l *link) use(s *terminal.Session) {
	l.mu.Lock()
	l.sess = s
	l.mu.Unlock()
}

func hostname() string {
	if data, err := os.ReadFile("/etc/hostname"); err == nil {
		if h := strings.TrimSpace(string(data)); h != "" {
			return h
		}
	}
	h, _ := os.Hostname()
	return h
}

func main() {
	state := flag.String("state", "/var/lib/sinty-screen", "identity and paired Core")
	fbPath := flag.String("fb", "/dev/fb0", "framebuffer to draw on")
	inputs := flag.String("input", "/dev/input", "directory with keyboards and pointers")
	name := flag.String("name", "", "name shown on the Core, the host name when empty")
	layout := flag.String("layout", "us", "keyboard layout of this screen")
	core := flag.String("core", "", "address of the Core, found on the network when empty")
	repair := flag.Bool("pair", false, "forget the paired Core and pair again")
	size := flag.String("size", "", "draw into a plain file of this size, as WIDTHxHEIGHT")
	codecDir := flag.String("codecs", "/dev", "directory with the video codec devices, none when empty")
	flag.Parse()
	log.SetFlags(0)
	log.SetPrefix("sinty-screen: ")
	if *name == "" {
		*name = hostname()
	}
	id, err := pair.LoadIdentity(filepath.Join(*state, "identity"))
	if err != nil {
		log.Fatalf("identity: %v", err)
	}
	disp, err := openDisplay(*fbPath, *size)
	if err != nil {
		log.Fatalf("display: %v", err)
	}
	defer disp.Close()
	term := &terminal.Terminal{Identity: id, Name: *name, Layout: *layout, State: *state, Display: disp, W: disp.W, H: disp.H}
	if *codecDir != "" {
		decoders := v4l2.Find(*codecDir, false)
		for _, c := range decoders {
			term.Codecs = append(term.Codecs, v4l2.Name(c.Format))
		}
		term.Decoders = func(format uint32, w, h int) (terminal.Decoder, error) {
			for _, c := range decoders {
				if c.Format == format {
					return v4l2.NewDecoder(c, w, h)
				}
			}
			return nil, v4l2.ErrNoDevice
		}
	}
	if *repair {
		term.Forget()
	}
	l := &link{ptr: fb.NewPointer(disp.W, disp.H)}
	term.OnFrame = func(w, h int) {
		l.mu.Lock()
		l.ptr.SetFit(disp.FitFor(w, h))
		l.mu.Unlock()
	}
	for _, d := range fb.OpenInputs(*inputs, l.ptr) {
		go d.Run(l.feed)
	}
	go func() {
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
		<-sigs
		disp.Clear()
		os.Exit(0)
	}()

	for {
		if err := run(term, l, *core); err != nil {
			log.Print(err)
		}
		time.Sleep(2 * time.Second)
	}
}

func openDisplay(path, size string) (*fb.Display, error) {
	if size == "" {
		return fb.Open(path)
	}
	var w, h int
	if _, err := fmt.Sscanf(size, "%dx%d", &w, &h); err != nil || w <= 0 || h <= 0 {
		return nil, fmt.Errorf("size %q is not WIDTHxHEIGHT", size)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	return fb.OpenFile(f, w, h)
}

func run(term *terminal.Terminal, l *link, fixed string) error {
	paired, err := term.Core()
	if err != nil {
		disp := term.Display
		disp.Clear()
		disp.Text("Looking for a Sinty Core", term.W/2, term.H/2, max(term.W/200, 2), 0xffffffff)
		addr, err := locate(fixed, "")
		if err != nil {
			return err
		}
		c, err := net.Dial("tcp", addr)
		if err != nil {
			return err
		}
		defer c.Close()
		_, err = term.Pair(c)
		return err
	}
	addr, err := locate(fixed, paired.ID)
	if err != nil {
		return err
	}
	c, err := net.Dial("tcp", addr)
	if err != nil {
		return err
	}
	sess, err := term.Open(c)
	if errors.Is(err, channel.ErrUntrusted) {
		term.Forget()
		return err
	}
	if err != nil {
		c.Close()
		return err
	}
	l.use(sess)
	defer l.use(nil)
	defer sess.Close()
	return term.Show(sess)
}
