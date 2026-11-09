package channel

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"testing"

	"github.com/singularityos-lab/sinty-cored/internal/pair"
	"github.com/singularityos-lab/sinty-cored/internal/wire"
)

func identity(t *testing.T) pair.Identity {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pair.Identity{Priv: priv}
}

func openPair(t *testing.T, screenRW, coreRW net.Conn, screen, core pair.Identity, coreSeenByScreen ed25519.PublicKey, trusted func(string) (string, bool)) (*Conn, *Conn, error, error) {
	t.Helper()
	type res struct {
		c   *Conn
		err error
	}
	ch := make(chan res, 1)
	go func() {
		c, err := Screen(screenRW, screen, coreSeenByScreen, "it")
		if err != nil {
			screenRW.Close()
		}
		ch <- res{c, err}
	}()
	c, req, err := Core(coreRW, core, trusted)
	if err != nil {
		coreRW.Close()
	} else if req.Layout != "it" {
		t.Errorf("layout %q", req.Layout)
	}
	s := <-ch
	return s.c, c, s.err, err
}

func TestSessionBothWays(t *testing.T) {
	screen, core := identity(t), identity(t)
	a, b := net.Pipe()
	s, c, serr, cerr := openPair(t, a, b, screen, core, core.Public(), func(id string) (string, bool) { return "Desk", id == screen.ID() })
	if serr != nil || cerr != nil {
		t.Fatal(serr, cerr)
	}
	if c.Peer().Name != "Desk" {
		t.Fatalf("peer %+v", c.Peer())
	}
	go func() {
		c.Send(1, []byte("frame"))
		c.Send(1, []byte("frame 2"))
	}()
	for _, want := range []string{"frame", "frame 2"} {
		kind, data, err := s.Recv()
		if err != nil || kind != 1 || string(data) != want {
			t.Fatalf("%d %q %v", kind, data, err)
		}
	}
	go s.Send(2, []byte("key"))
	if kind, data, err := c.Recv(); err != nil || kind != 2 || string(data) != "key" {
		t.Fatalf("%d %q %v", kind, data, err)
	}
}

func TestRevokedScreenIsRefused(t *testing.T) {
	screen, core := identity(t), identity(t)
	a, b := net.Pipe()
	_, _, serr, cerr := openPair(t, a, b, screen, core, core.Public(), func(string) (string, bool) { return "", false })
	if !errors.Is(serr, ErrUntrusted) || !errors.Is(cerr, ErrUntrusted) {
		t.Fatal(serr, cerr)
	}
}

func TestScreenRefusesAnImpostorCore(t *testing.T) {
	screen, core, impostor := identity(t), identity(t), identity(t)
	a, b := net.Pipe()
	_, _, serr, _ := openPair(t, a, b, screen, impostor, core.Public(), func(string) (string, bool) { return "x", true })
	if !errors.Is(serr, ErrIdentity) {
		t.Fatal(serr)
	}
}

func TestEditedAndReplayedFramesAreRejected(t *testing.T) {
	screen, core := identity(t), identity(t)
	a, b := net.Pipe()
	s, c, serr, cerr := openPair(t, a, b, screen, core, core.Public(), func(string) (string, bool) { return "x", true })
	if serr != nil || cerr != nil {
		t.Fatal(serr, cerr)
	}
	sealed := c.send.Seal(nil, nonceFor(c.sendN), []byte{1, 'h', 'i'}, nil)
	c.sendN++
	edited := append([]byte(nil), sealed...)
	edited[3] ^= 0x40
	go wire.Write(b, msgData, edited)
	if _, _, err := s.Recv(); !errors.Is(err, ErrDamaged) {
		t.Fatalf("edited: %v", err)
	}
	s.recvN = 0
	go func() {
		wire.Write(b, msgData, sealed)
		wire.Write(b, msgData, sealed)
	}()
	if _, data, err := s.Recv(); err != nil || string(data) != "hi" {
		t.Fatalf("first: %q %v", data, err)
	}
	if _, _, err := s.Recv(); !errors.Is(err, ErrDamaged) {
		t.Fatalf("replay: %v", err)
	}
}
