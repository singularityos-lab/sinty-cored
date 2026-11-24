package terminal

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/singularityos-lab/sinty-cored/internal/channel"
	"github.com/singularityos-lab/sinty-cored/internal/input"
	"github.com/singularityos-lab/sinty-cored/internal/pair"
	"github.com/singularityos-lab/sinty-cored/internal/stream"
	"github.com/singularityos-lab/sinty-cored/internal/tiles"
)

var ErrNotPaired = errors.New("this screen is not paired with a Core")

var errNoSize = errors.New("the Core did not say how large its desktop is")

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

	InputOnly bool
	Pinned    *pair.Peer
}

type paired struct {
	ID     string
	Name   string
	Public []byte
}

func (t *Terminal) corePath() string { return filepath.Join(t.State, "core.json") }

func (t *Terminal) Core() (pair.Peer, error) {
	if t.Pinned != nil {
		return *t.Pinned, nil
	}
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

func (t *Terminal) Adopt(core pair.Peer) error {
	if len(core.Public) != ed25519.PublicKeySize || pair.ID(core.Public) != core.ID {
		return errors.New("the Core key does not match its id")
	}
	data, _ := json.Marshal(paired{core.ID, core.Name, core.Public})
	if err := os.MkdirAll(t.State, 0o700); err != nil {
		return err
	}
	tmp := t.corePath() + ".new"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, t.corePath())
}

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
	features := append(slices.Clone(t.Codecs), stream.FeatureShown, stream.FeatureSplit)
	if t.InputOnly {
		features = []string{stream.FeatureInput}
	}
	c, err := channel.Screen(rw, t.Identity, core.Public, t.Layout, features)
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
			if pix != nil && t.Display != nil {
				t.Display.Draw(pix, w, h)
			}
			if pix != nil && t.OnFrame != nil {
				t.OnFrame(w, h)
			}
			if err := s.c.Send(stream.Shown, nil); err != nil {
				return err
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
			if err := s.c.Send(stream.Shown, nil); err != nil {
				return err
			}
		case stream.Notice:
			t.message("The Core stopped showing this screen", "", "")
			return io.EOF
		}
	}
}

func (s *Session) Close() error { return s.c.Close() }

func (s *Session) SendMessage(kind byte, data []byte) error { return s.c.Send(kind, data) }

func (s *Session) Size() (int, int, error) {
	for {
		kind, data, err := s.c.Recv()
		if err != nil {
			return 0, 0, err
		}
		switch kind {
		case stream.Size:
			w, h, ok := stream.ParseSize(data)
			if !ok {
				return 0, 0, errNoSize
			}
			return w, h, nil
		case stream.Notice:
			return 0, 0, io.EOF
		}
	}
}

func (s *Session) Wait(resized func(w, h int), other func(kind byte, data []byte)) error {
	for {
		kind, data, err := s.c.Recv()
		if err != nil {
			return err
		}
		switch kind {
		case stream.Notice:
			return io.EOF
		case stream.Size:
			if w, h, ok := stream.ParseSize(data); ok && resized != nil {
				resized(w, h)
			}
		case stream.Clip, stream.FileAsk, stream.FileData:
			if other != nil {
				other(kind, data)
			}
		}
	}
}
