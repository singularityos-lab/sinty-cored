package service

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/singularityos-lab/sinty-cored/internal/clip"
	"github.com/singularityos-lab/sinty-cored/internal/pasteboard"
	"github.com/singularityos-lab/sinty-cored/internal/stream"
	"github.com/singularityos-lab/sinty-cored/internal/wl"
	"github.com/singularityos-lab/sinty-cored/internal/wltest"
)

type nullBoard struct{}

func (nullBoard) Read() ([]clip.Item, error)                                { return nil, nil }
func (nullBoard) SetWith([]clip.Item, func(mime string, data []byte) []byte) {}

func TestADragFromTheScreenRunsOnTheCoreUntilThePointerLeaves(t *testing.T) {
	socket := wltest.Compositor(t)
	sh, err := wl.OpenShare(socket)
	if err != nil {
		t.Fatal(err)
	}
	defer sh.Close()
	go sh.Run()
	if !sh.CanDrag() {
		t.Skip("the compositor predates carried drags")
	}
	watcher, err := wl.OpenShare(socket)
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close()
	go watcher.Run()

	var screen, core *pasteboard.Link
	back := make(chan []clip.Item, 1)
	screen = pasteboard.New(nullBoard{}, func(kind byte, data []byte) error { go core.Receive(kind, data); return nil }, filepath.Join(t.TempDir(), "s"), nil)
	core = pasteboard.New(nullBoard{}, func(kind byte, data []byte) error {
		if kind == stream.DragBack {
			items, _ := clip.Decode(data)
			back <- items
		}
		go screen.Receive(kind, data)
		return nil
	}, filepath.Join(t.TempDir(), "c"), nil)
	defer carryDrags(sh, core)()

	if err := screen.OfferDrag([]clip.Item{{Mime: clip.Text, Data: []byte("over the edge")}}); err != nil {
		t.Fatal(err)
	}
	var running *wl.DragOffer
	for range 100 {
		if running, _ = watcher.CurrentDrag(); running != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if running == nil {
		t.Fatal("the drag never started on the Core")
	}
	running.Close()
	screen.LeaveDrag()
	select {
	case items := <-back:
		if len(items) != 0 {
			t.Fatalf("the screen's own drag came back: %v", items)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the Core did not answer the leave")
	}
	if o, _ := watcher.CurrentDrag(); o != nil {
		t.Fatal("the drag kept running on the Core after the pointer left")
	}
}
