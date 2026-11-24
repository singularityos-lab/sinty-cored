package wl

import (
	"testing"
	"time"

	"github.com/singularityos-lab/sinty-cored/internal/clip"
	"github.com/singularityos-lab/sinty-cored/internal/wltest"
)

func openShare(t *testing.T, socket string) *Share {
	t.Helper()
	s, err := OpenShare(socket)
	if err != nil {
		t.Fatal(err)
	}
	go s.Run()
	t.Cleanup(func() { s.Close() })
	if !s.CanDrag() {
		t.Skip("the compositor predates carried drags")
	}
	return s
}

func outcome(t *testing.T, d *Drag) bool {
	t.Helper()
	select {
	case ok := <-d.Outcome():
		return ok
	case <-time.After(5 * time.Second):
		t.Fatal("the drag never ended")
		return false
	}
}

func TestNoDragIsReported(t *testing.T) {
	s := openShare(t, wltest.Compositor(t))
	o, err := s.CurrentDrag()
	if err != nil || o != nil {
		t.Fatalf("drag %v %v", o, err)
	}
}

func TestACarriedDragIsReadAndCancelled(t *testing.T) {
	socket := wltest.Compositor(t)
	core, reader := openShare(t, socket), openShare(t, socket)
	d, err := core.StartDrag([]clip.Item{{Mime: clip.Text, Data: []byte("dragged")}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	o, err := reader.CurrentDrag()
	if err != nil || o == nil {
		t.Fatalf("drag not seen: %v", err)
	}
	if items := o.Read(); items != nil {
		t.Fatalf("a drag from another device was read again: %v", items)
	}
	if got := string(o.receive("UTF8_STRING")); got != "dragged" {
		t.Fatalf("read %q", got)
	}
	o.Finish(false)
	if outcome(t, d) {
		t.Fatal("a cancelled drag was reported as dropped")
	}
	if o, _ := reader.CurrentDrag(); o != nil {
		t.Fatal("the drag outlived its cancellation")
	}
}

func TestADragDroppedElsewhereFinishes(t *testing.T) {
	socket := wltest.Compositor(t)
	core, reader := openShare(t, socket), openShare(t, socket)
	d, err := core.StartDrag([]clip.Item{{Mime: "text/uri-list", Data: []byte("file:///a\r\n")}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	o, err := reader.CurrentDrag()
	if err != nil || o == nil {
		t.Fatalf("drag not seen: %v", err)
	}
	o.Finish(true)
	if !outcome(t, d) {
		t.Fatal("a drop elsewhere was reported as cancelled")
	}
}

func TestCancelEndsTheDrag(t *testing.T) {
	socket := wltest.Compositor(t)
	core, reader := openShare(t, socket), openShare(t, socket)
	d, err := core.StartDrag([]clip.Item{{Mime: clip.Text, Data: []byte("x")}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	d.Cancel()
	if outcome(t, d) {
		t.Fatal("cancel reported a drop")
	}
	if o, _ := reader.CurrentDrag(); o != nil {
		t.Fatal("the drag outlived cancel")
	}
	if _, err := core.StartDrag([]clip.Item{{Mime: clip.Text, Data: []byte("y")}}, nil); err != nil {
		t.Fatal(err)
	}
	if o, _ := reader.CurrentDrag(); o == nil {
		t.Fatal("a second drag did not start after the first ended")
	}
}
