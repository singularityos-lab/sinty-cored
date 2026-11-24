package service

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/singularityos-lab/sinty-cored/internal/carry"
	"github.com/singularityos-lab/sinty-cored/internal/pasteboard"
	"github.com/singularityos-lab/sinty-cored/internal/wl"
	"github.com/singularityos-lab/sinty-cored/internal/wltest"
)

type device struct {
	socket string
	board  *wl.Clipboard
	link   *pasteboard.Link
}

func clipboardTool(t *testing.T, name string) string {
	t.Helper()
	dir := os.Getenv("WL_CLIPBOARD")
	if dir == "" {
		t.Skip("set WL_CLIPBOARD to a directory holding wl-copy and wl-paste")
	}
	return filepath.Join(dir, name)
}

func pairDevices(t *testing.T) (pc, phone *device) {
	t.Helper()
	pc = &device{socket: wltest.Compositor(t)}
	phone = &device{socket: wltest.Compositor(t)}
	for _, d := range []*device{pc, phone} {
		k, err := wl.OpenClipboard(d.socket)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { k.Close() })
		d.board = k
	}
	pc.link = pasteboard.New(pc.board, func(kind byte, data []byte) error { go phone.link.Receive(kind, data); return nil }, filepath.Join(t.TempDir(), "pc"), nil)
	phone.link = pasteboard.New(phone.board, func(kind byte, data []byte) error { go pc.link.Receive(kind, data); return nil }, filepath.Join(t.TempDir(), "phone"), nil)
	go shareClipboard(pc.board, pc.link)
	go shareClipboard(phone.board, phone.link)
	return pc, phone
}

func (d *device) copy(t *testing.T, mime string, data []byte) {
	t.Helper()
	cmd := exec.Command(clipboardTool(t, "wl-copy"), "--type", mime)
	cmd.Env = append(os.Environ(), "WAYLAND_DISPLAY="+d.socket, "XDG_RUNTIME_DIR="+filepath.Dir(d.socket))
	cmd.Stdin = bytes.NewReader(data)
	if err := cmd.Run(); err != nil {
		t.Fatalf("wl-copy: %v", err)
	}
}

func (d *device) paste(t *testing.T, mime string, want func([]byte) bool) []byte {
	t.Helper()
	var last []byte
	for range 50 {
		cmd := exec.Command(clipboardTool(t, "wl-paste"), "--no-newline", "--type", mime)
		cmd.Env = append(os.Environ(), "WAYLAND_DISPLAY="+d.socket, "XDG_RUNTIME_DIR="+filepath.Dir(d.socket))
		out, err := cmd.Output()
		if err == nil && want(out) {
			return out
		}
		last = out
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("pasted %q", last)
	return nil
}

func TestTextCopiedOnOneDeviceIsPastedOnTheOther(t *testing.T) {
	pc, phone := pairDevices(t)
	pc.copy(t, "text/plain;charset=utf-8", []byte("copied on the pc"))
	phone.paste(t, "text/plain", func(b []byte) bool { return string(b) == "copied on the pc" })
	phone.copy(t, "text/plain;charset=utf-8", []byte("and back from the phone"))
	pc.paste(t, "text/plain", func(b []byte) bool { return string(b) == "and back from the phone" })
}

func TestACopiedFileArrivesWhenPasted(t *testing.T) {
	pc, phone := pairDevices(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "report.txt")
	if err := os.WriteFile(src, []byte("the file body"), 0o600); err != nil {
		t.Fatal(err)
	}
	pc.copy(t, "text/uri-list", []byte(carry.FileURI(src)+"\r\n"))
	out := phone.paste(t, "text/uri-list", func(b []byte) bool {
		return strings.HasPrefix(string(b), "file://") && !strings.Contains(string(b), dir)
	})
	local, ok := carry.LocalPath(strings.TrimSpace(string(out)))
	if !ok {
		t.Fatalf("pasted %q", out)
	}
	body, err := os.ReadFile(local)
	if err != nil || string(body) != "the file body" || filepath.Base(local) != "report.txt" {
		t.Fatalf("received %s: %q %v", local, body, err)
	}
}
