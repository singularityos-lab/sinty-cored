package service

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/singularityos-lab/libsingularity/src/system/core/go/netkind"
)

func TestOnAPublicNetworkTheCoreCannotBeFoundOrReached(t *testing.T) {
	dir := t.TempDir()
	setKind := func(k netkind.Kind) {
		os.WriteFile(filepath.Join(dir, "network-kind"), []byte(k), 0o644)
	}
	wifi := func(net.IP) string { return "wlan0" }
	svc := New(Config{
		Identity: identity(t), Name: "Desk Core", LAN: func() bool { return true },
		Admit: netkind.Admit(dir, wifi), AdmitPeer: netkind.AdmitPeer(dir, wifi),
	})
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go svc.Serve(l)
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	go svc.Discovery(pc, 7412)

	found := func() bool {
		c, err := net.Dial("udp", pc.LocalAddr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.Write([]byte(Probe))
		c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		buf := make([]byte, 512)
		n, err := c.Read(buf)
		return err == nil && n > 0
	}
	open := func() bool {
		c, err := net.Dial("tcp", l.Addr().String())
		if err != nil {
			return false
		}
		defer c.Close()
		c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		_, err = c.Read(make([]byte, 1))
		var ne net.Error
		return errors.As(err, &ne) && ne.Timeout()
	}

	setKind(netkind.Home)
	if !found() || !open() {
		t.Fatal("not reachable on a home network")
	}
	setKind(netkind.Public)
	if found() {
		t.Fatal("answered a probe on a public network")
	}
	if open() {
		t.Fatal("kept a connection on a public network")
	}
	setKind(netkind.Work)
	if !found() || !open() {
		t.Fatal("not reachable on a work network")
	}
}
