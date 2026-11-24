package wl

import (
	"testing"
	"time"

	"github.com/singularityos-lab/sinty-cored/internal/clip"
	"github.com/singularityos-lab/sinty-cored/internal/wltest"
)

func openClipboard(t *testing.T, socket string) *Clipboard {
	t.Helper()
	k, err := OpenClipboard(socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { k.Close() })
	return k
}

func waitChange(t *testing.T, k *Clipboard) {
	t.Helper()
	select {
	case <-k.Changed():
	case <-time.After(5 * time.Second):
		t.Fatal("the selection change was not reported")
	}
}

func drain(k *Clipboard) {
	for {
		select {
		case <-k.Changed():
		case <-time.After(300 * time.Millisecond):
			return
		}
	}
}

func TestClipboardReadsAnAppSelection(t *testing.T) {
	socket := wltest.Compositor(t)
	app := openClipboard(t, socket)
	peer := openClipboard(t, socket)
	drain(peer)
	app.set([]clip.Item{{Mime: clip.Text, Data: []byte("hello")}}, []string{"UTF8_STRING", "TEXT"}, nil)
	waitChange(t, peer)
	items, err := peer.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Mime != clip.Text || string(items[0].Data) != "hello" {
		t.Fatalf("read %+v", items)
	}
}

func TestClipboardServesWhatAnotherDeviceCopied(t *testing.T) {
	socket := wltest.Compositor(t)
	app := openClipboard(t, socket)
	peer := openClipboard(t, socket)
	drain(app)
	peer.Set([]clip.Item{{Mime: clip.Text, Data: []byte("from the phone")}, {Mime: "image/png", Data: []byte{1, 2, 3}}})
	waitChange(t, app)
	items, err := app.Read()
	if err != nil {
		t.Fatal(err)
	}
	if items != nil {
		t.Fatalf("a selection that came from another device was read back: %+v", items)
	}
	app.mu.Lock()
	text, _ := app.receive(app.current, "TEXT")
	png, _ := app.receive(app.current, "image/png")
	app.mu.Unlock()
	if string(text) != "from the phone" || len(png) != 3 {
		t.Fatalf("served %q and %v", text, png)
	}
}
