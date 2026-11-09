package pair

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"testing"

	"github.com/singularityos-lab/sinty-cored/internal/wire"
)

func identity(t *testing.T) Identity {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return Identity{priv}
}

type result struct {
	peer Peer
	err  error
}

func run(t *testing.T, screenConn, coreConn net.Conn, typed func(code string) string) (result, result, string) {
	t.Helper()
	screen, core := identity(t), identity(t)
	codes := make(chan string, 1)
	sr := make(chan result, 1)
	go func() {
		p, err := Screen(screenConn, screen, "Living Room", func(c string) { codes <- c })
		screenConn.Close()
		sr <- result{p, err}
	}()
	shown := ""
	p, err := Core(coreConn, core, "Core", func(name string) (string, error) {
		if name != "Living Room" {
			t.Errorf("screen name: %q", name)
		}
		shown = <-codes
		return typed(shown), nil
	})
	coreConn.Close()
	return <-sr, result{p, err}, shown
}

func TestPairWithTheShownCode(t *testing.T) {
	a, b := net.Pipe()
	s, c, code := run(t, a, b, func(code string) string { return code })
	if s.err != nil || c.err != nil {
		t.Fatalf("screen %v core %v", s.err, c.err)
	}
	if len(code) != 6 {
		t.Fatalf("code %q", code)
	}
	if s.peer.Name != "Core" || c.peer.Name != "Living Room" || c.peer.ID != ID(c.peer.Public) {
		t.Fatalf("peers %+v %+v", s.peer, c.peer)
	}
}

func TestWrongCodeIsRefusedOnBothSides(t *testing.T) {
	a, b := net.Pipe()
	s, c, _ := run(t, a, b, func(code string) string {
		if code == "000000" {
			return "000001"
		}
		return "000000"
	})
	if !errors.Is(c.err, ErrWrongCode) || !errors.Is(s.err, ErrRejected) {
		t.Fatalf("screen %v core %v", s.err, c.err)
	}
}

func TestScreenCannotSwapItsKeyAfterSeeingTheCore(t *testing.T) {
	screenSide, relayA := net.Pipe()
	relayB, coreSide := net.Pipe()
	go func() {
		for {
			kind, data, err := wire.Read(relayA)
			if err != nil {
				relayB.Close()
				return
			}
			if kind == msgReveal {
				var r reveal
				json.Unmarshal(data, &r)
				r.Nonce[0] ^= 1
				data, _ = json.Marshal(r)
			}
			if wire.Write(relayB, kind, data) != nil {
				return
			}
		}
	}()
	go func() {
		for {
			kind, data, err := wire.Read(relayB)
			if err != nil {
				relayA.Close()
				return
			}
			if wire.Write(relayA, kind, data) != nil {
				return
			}
		}
	}()
	go Screen(screenSide, identity(t), "x", func(string) {})
	_, err := Core(coreSide, identity(t), "Core", func(string) (string, error) { return "", nil })
	if !errors.Is(err, ErrCommit) {
		t.Fatalf("tampered reveal: %v", err)
	}
}

func TestIdentityPersists(t *testing.T) {
	p := filepath.Join(t.TempDir(), "id", "core.key")
	a, err := LoadIdentity(p)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadIdentity(p)
	if err != nil || a.ID() != b.ID() {
		t.Fatalf("identity changed: %v", err)
	}
}
