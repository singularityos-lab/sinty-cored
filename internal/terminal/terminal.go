package terminal

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/singularityos-lab/sinty-cored/internal/channel"
	"github.com/singularityos-lab/sinty-cored/internal/input"
	"github.com/singularityos-lab/sinty-cored/internal/pair"
	"github.com/singularityos-lab/sinty-cored/internal/stream"
	"github.com/singularityos-lab/sinty-cored/internal/tiles"
)

var ErrNotPaired = errors.New("this screen is not paired with a Core")

type Display interface {
	Clear()
	Text(s string, cx, cy, scale int, c uint32)
	Draw(pix []byte, w, h int)
}

type Decoder interface {
	Decode(data []byte) ([]byte, error)
	Close() error
}

type Terminal struct {
	Identity pair.Identity
	Name     string
	Layout   string
	State    string
	Display  Display
	W, H     int
	OnFrame  func(w, h int)
	Codecs   []string
	Decoders func(format uint32, w, h int) (Decoder, error)
}

type paired struct {
	ID     string
	Name   string
	Public []byte
}

func (t *Terminal) corePath() string { return filepath.Join(t.State, "core.json") }

func (t *Terminal) Core() (pair.Peer, error) {
	data, err := os.ReadFile(t.corePath())
	if err != nil {
		return pair.Peer{}, ErrNotPaired
	}
	var p paired
	if json.Unmarshal(data, &p) != nil || len(p.Public) != ed25519.PublicKeySize {
		return pair.Peer{}, ErrNotPaired
	}
	return pair.Peer{ID: p.ID, Name: p.Name, Public: p.Public}, nil
}

func (t *Terminal) Forget() error { return os.Remove(t.corePath()) }

func (t *Terminal) message(lines ...string) {
	if t.Display == nil {
		return
	}
	t.Display.Clear()
	scale := max(t.W/200, 2)
	for i, l := range lines {
		s := scale
		if i == 1 {
			s = scale * 3
		}
		t.Display.Text(l, t.W/2, t.H/2+(i-1)*scale*16, s, 0xffffffff)
	}
}

func (t *Terminal) Pair(conn io.ReadWriter) (pair.Peer, error) {
	peer, err := pair.Screen(conn, t.Identity, t.Name, func(code string) {
		t.message("Enter this code on your Sinty Core", code[:3]+" "+code[3:], t.Name)
	})
	if err != nil {
		t.message("Pairing did not finish", "", "Try again from Settings on the Core")
		return pair.Peer{}, err
	}
	data, _ := json.Marshal(paired{peer.ID, peer.Name, peer.Public})
	if err := os.MkdirAll(t.State, 0o700); err != nil {
		return pair.Peer{}, err
	}
	tmp := t.corePath() + ".new"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return pair.Peer{}, err
	}
	t.message("Paired with", peer.Name, "")
	return peer, os.Rename(tmp, t.corePath())
}

type Session struct {
	c  *channel.Conn
	mu sync.Mutex
}

func (s *Session) Send(events []input.Event) error {
	if len(events) == 0 {
		return nil
	}
	return s.c.Send(stream.Input, input.Encode(events))
}

func (t *Terminal) Open(rw io.ReadWriteCloser) (*Session, error) {
	core, err := t.Core()
	if err != nil {
		return nil, err
	}
	c, err := channel.Screen(rw, t.Identity, core.Public, t.Layout, t.Codecs)
	if err != nil {
		if errors.Is(err, channel.ErrUntrusted) {
			t.message("This screen was removed from", core.Name, "Pair it again from Settings on the Core")
		}
		return nil, err
	}
	return &Session{c: c}, nil
}

func (t *Terminal) Show(s *Session) error {
	var dec tiles.Decoder
	var hw Decoder
	var hwFormat uint32
	var hwW, hwH int
	defer func() {
		if hw != nil {
			hw.Close()
		}
	}()
	for {
		kind, data, err := s.c.Recv()
		if err != nil {
			return err
		}
		switch kind {
		case stream.Coded:
			format, w, h, payload, ok := stream.ParseCoded(data)
			if !ok || t.Decoders == nil {
				return tiles.ErrBadFrame
			}
			if hw == nil || format != hwFormat || w != hwW || h != hwH {
				if hw != nil {
					hw.Close()
				}
				hw, err = t.Decoders(format, w, h)
				if err != nil {
					t.Codecs = nil
					return err
				}
				hwFormat, hwW, hwH = format, w, h
			}
			pix, err := hw.Decode(payload)
			if err != nil {
				t.Codecs = nil
				return err
			}
			if pix == nil {
				continue
			}
			if t.Display != nil {
				t.Display.Draw(pix, w, h)
			}
			if t.OnFrame != nil {
				t.OnFrame(w, h)
			}
		case stream.Video:
			if _, err := dec.Decode(data); err != nil {
				return err
			}
			if t.Display != nil {
				t.Display.Draw(dec.Pix, dec.W, dec.H)
			}
			if t.OnFrame != nil {
				t.OnFrame(dec.W, dec.H)
			}
		case stream.Notice:
			t.message("The Core stopped showing this screen", "", "")
			return io.EOF
		}
	}
}

func (s *Session) Close() error { return s.c.Close() }
