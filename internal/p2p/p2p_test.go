package p2p

import (
	"net"
	"path/filepath"
	"testing"
	"time"
)

func fakeSupplicant(t *testing.T, dir string) {
	addr := &net.UnixAddr{Name: filepath.Join(dir, "p2p-dev-wlan0"), Net: "unixgram"}
	srv, err := net.ListenUnixgram("unixgram", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	go func() {
		buf := make([]byte, 512)
		for {
			n, from, err := srv.ReadFromUnix(buf)
			if err != nil {
				return
			}
			cmd := string(buf[:n])
			srv.WriteToUnix([]byte("OK\n"), from)
			if cmd == "P2P_GROUP_ADD" {
				time.Sleep(20 * time.Millisecond)
				srv.WriteToUnix([]byte(`<3>P2P-GROUP-STARTED p2p-wlan0-0 GO ssid="DIRECT-x7 Sinty Core" freq=5180 passphrase="k3Hd9aQp" go_dev_addr=02:00:00:00:01:00`), from)
			}
		}
	}()
}

func TestStartAndStopAGroup(t *testing.T) {
	t.Chdir(t.TempDir())
	dir := "."
	c := &Control{Dir: dir}
	if c.Supported() {
		t.Fatal("supported without a P2P device")
	}
	if _, err := c.Start(time.Second); err != ErrUnsupported {
		t.Fatal(err)
	}
	fakeSupplicant(t, dir)
	g, err := c.Start(2 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if g != (Group{"p2p-wlan0-0", "DIRECT-x7 Sinty Core", "k3Hd9aQp", "5180"}) {
		t.Fatalf("group %+v", g)
	}
	if err := c.Stop(g); err != nil {
		t.Fatal(err)
	}
}

func TestClientGroupsAreIgnored(t *testing.T) {
	if _, ok := ParseStarted(`<3>P2P-GROUP-STARTED p2p-wlan0-0 client ssid="x" freq=2412`); ok {
		t.Fatal("client group taken as ours")
	}
}
