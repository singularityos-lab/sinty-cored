package terminal

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/singularityos-lab/sinty-cored/internal/channel"
	"github.com/singularityos-lab/sinty-cored/internal/input"
	"github.com/singularityos-lab/sinty-cored/internal/pair"
	"github.com/singularityos-lab/sinty-cored/internal/stream"
	"github.com/singularityos-lab/sinty-cored/internal/wl"
)

func identity(t *testing.T) pair.Identity {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pair.Identity{Priv: priv}
}

type screen struct {
	mu    sync.Mutex
	texts []string
	last  []byte
	draws int
}

func (s *screen) Clear() {}
func (s *screen) Text(t string, _, _, _ int, _ uint32) {
	s.mu.Lock()
	s.texts = append(s.texts, t)
	s.mu.Unlock()
}
func (s *screen) Draw(pix []byte, w, h int) {
	s.mu.Lock()
	s.last = append([]byte(nil), pix...)
	s.draws++
	s.mu.Unlock()
}

type source struct {
	frames []*wl.Frame
	i      int
}

func (s *source) Capture(cursor, onDamage bool, into []byte) (*wl.Frame, error) {
	if s.i >= len(s.frames) {
		time.Sleep(10 * time.Millisecond)
		return nil, nil
	}
	f := s.frames[s.i]
	s.i++
	return f, nil
}

type sink struct {
	mu     sync.Mutex
	events []string
}

func (k *sink) add(s string) { k.mu.Lock(); k.events = append(k.events, s); k.mu.Unlock() }
func (k *sink) MoveTo(x, y, w, h uint32) {
	if x == 65535 && y == 0 {
		k.add("move-top-right")
	}
}
func (k *sink) Button(code uint32, p bool)    { k.add("button") }
func (k *sink) Scroll(v bool, steps int32)    {}
func (k *sink) Key(code uint32, pressed bool) { k.add("key") }
func (k *sink) Flush() error                  { return nil }

func TestPairThenStreamAndControl(t *testing.T) {
	core := identity(t)
	disp := &screen{}
	term := &Terminal{Identity: identity(t), Name: "Kitchen", Layout: "us", State: t.TempDir(), Display: disp, W: 800, H: 480}

	a, b := net.Pipe()
	go term.Pair(a)
	var code string
	_, err := pair.Core(b, core, "My Core", func(name string) (string, error) {
		for {
			disp.mu.Lock()
			for _, s := range disp.texts {
				if len(s) == 7 && s[3] == ' ' {
					code = s[:3] + s[4:]
				}
			}
			disp.mu.Unlock()
			if code != "" {
				return code, nil
			}
			time.Sleep(5 * time.Millisecond)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if p, err := term.Core(); err != nil || p.Name != "My Core" {
		t.Fatalf("paired core: %+v %v", p, err)
	}

	f1 := &wl.Frame{W: 100, H: 70, Stride: 400, Pix: bytes.Repeat([]byte{1, 2, 3, 255}, 100*70)}
	f2 := &wl.Frame{W: 100, H: 70, Stride: 400, Pix: append([]byte(nil), f1.Pix...)}
	f2.Pix[69*400+99*4] = 9
	src := &source{frames: []*wl.Frame{f1, f2}}
	k := &sink{}
	c, d := net.Pipe()
	stop := make(chan struct{})
	served := make(chan error, 1)
	go func() {
		cc, _, err := channel.Core(d, core, func(id string) (string, bool) { return "Kitchen", id == term.Identity.ID() })
		if err != nil {
			served <- err
			return
		}
		served <- stream.Serve(cc, src, k, stop)
	}()
	sess, err := term.Open(c)
	if err != nil {
		t.Fatal(err)
	}
	shown := make(chan error, 1)
	go func() { shown <- term.Show(sess) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		disp.mu.Lock()
		ok := disp.draws >= 2 && bytes.Equal(disp.last, f2.Pix)
		disp.mu.Unlock()
		if ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("frames did not arrive intact")
		}
		time.Sleep(10 * time.Millisecond)
	}
	sess.Send([]input.Event{{Kind: input.Motion, X: 65535, Y: 0}, {Kind: input.Button, Code: 0x110, Value: 1}, {Kind: input.Key, Code: 30, Value: 1}})
	for time.Now().Before(deadline) {
		k.mu.Lock()
		n := len(k.events)
		k.mu.Unlock()
		if n == 3 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(k.events) != 3 || k.events[0] != "move-top-right" {
		t.Fatalf("input on the Core: %v", k.events)
	}
	close(stop)
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	<-shown

	e, f := net.Pipe()
	go channel.Core(f, core, func(string) (string, bool) { return "", false })
	if _, err := term.Open(e); !errors.Is(err, channel.ErrUntrusted) {
		t.Fatalf("revoked screen: %v", err)
	}
}
