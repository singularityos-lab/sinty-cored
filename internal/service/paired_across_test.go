package service

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/singularityos-lab/sinty-cored/internal/carry"
	"github.com/singularityos-lab/sinty-cored/internal/pasteboard"
	"github.com/singularityos-lab/sinty-cored/internal/terminal"
	"github.com/singularityos-lab/sinty-cored/internal/wl"
	"github.com/singularityos-lab/sinty-cored/internal/wltest"
)

type clipboardDesk struct {
	*desktop
	socket string
}

func (d *clipboardDesk) Clipboard() (Clipboard, error) { return wl.OpenClipboard(d.socket) }

func pairOverTheNetwork(t *testing.T) (core, screen *device) {
	t.Helper()
	core = &device{socket: wltest.Compositor(t)}
	screen = &device{socket: wltest.Compositor(t)}
	r := setupWith(t, uint32(os.Getuid()), func(d *desktop) Desktop { return &clipboardDesk{desktop: d, socket: core.socket} })
	disp := &display{}
	term := &terminal.Terminal{Identity: identity(t), Name: "Phone", Layout: "us", State: t.TempDir(), Display: disp, W: 640, H: 480}
	if err := r.obj.Call(Iface+".StartPairing", 0).Err; err != nil {
		t.Fatal(err)
	}
	paired := make(chan error, 1)
	go func() {
		c := r.dial()
		defer c.Close()
		_, err := term.Pair(c)
		paired <- err
	}()
	r.signal("PairingRequest")
	eventually(t, "code on the screen", func() bool { return disp.code() != "" })
	if err := r.obj.Call(Iface+".ConfirmPairing", 0, disp.code()).Err; err != nil {
		t.Fatal(err)
	}
	if err := <-paired; err != nil {
		t.Fatal(err)
	}
	term.InputOnly = true
	sess, err := term.Open(r.dial())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })
	board, err := wl.OpenClipboard(screen.socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { board.Close() })
	screen.board = board
	screen.link = pasteboard.New(board, sess.SendMessage, filepath.Join(t.TempDir(), "screen"), nil)
	go shareClipboard(board, screen.link)
	go sess.Wait(func(int, int) {}, screen.link.Receive)
	return core, screen
}

func TestClipboardCrossesThePairedChannel(t *testing.T) {
	core, screen := pairOverTheNetwork(t)
	core.copy(t, "text/plain;charset=utf-8", []byte("copied on the Core"))
	screen.paste(t, "text/plain;charset=utf-8", func(b []byte) bool { return string(b) == "copied on the Core" })
	screen.copy(t, "text/plain;charset=utf-8", []byte("copied on the phone"))
	core.paste(t, "text/plain;charset=utf-8", func(b []byte) bool { return string(b) == "copied on the phone" })
}

func TestFileCrossesThePairedChannel(t *testing.T) {
	core, screen := pairOverTheNetwork(t)
	src := filepath.Join(t.TempDir(), "report.txt")
	body := bytes.Repeat([]byte("quarterly numbers\n"), 4000)
	if err := os.WriteFile(src, body, 0o644); err != nil {
		t.Fatal(err)
	}
	core.copy(t, "text/uri-list", []byte(carry.FileURI(src)+"\r\n"))
	got := screen.paste(t, "text/uri-list", func(b []byte) bool {
		return strings.HasPrefix(string(b), "file://") && !strings.Contains(string(b), filepath.Dir(src))
	})
	local, ok := carry.LocalPath(strings.TrimSpace(string(got)))
	if !ok {
		t.Fatalf("pasted %q", got)
	}
	data, err := os.ReadFile(local)
	if err != nil || !bytes.Equal(data, body) || filepath.Base(local) != "report.txt" {
		t.Fatalf("received %s: %d bytes, %v", local, len(data), err)
	}
}
