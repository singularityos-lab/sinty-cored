package wl

import (
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/singularityos-lab/sinty-cored/internal/input"
	"github.com/singularityos-lab/sinty-cored/internal/wltest"
)

type heard struct {
	mu     sync.Mutex
	events []input.Event
}

func (h *heard) add(events []input.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, events...)
}

func (h *heard) take() []input.Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := h.events
	h.events = nil
	return out
}

func (h *heard) wait(t *testing.T, kind uint8) input.Event {
	t.Helper()
	end := time.Now().Add(3 * time.Second)
	for time.Now().Before(end) {
		h.mu.Lock()
		for i, e := range h.events {
			if e.Kind == kind {
				h.events = h.events[i+1:]
				h.mu.Unlock()
				return e
			}
		}
		h.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	h.mu.Lock()
	t.Logf("heard %+v", h.events)
	h.mu.Unlock()
	t.Fatalf("the cast window never got event %d", kind)
	return input.Event{}
}

func castWindow(t *testing.T, socket, appID string, w, h int, left, right uint32) (*Window, *heard) {
	t.Helper()
	win, err := OpenWindow(socket, appID, appID, w, h)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { win.Close() })
	got := &heard{}
	win.OnInput = got.add
	go win.Run()
	pix := make([]byte, w*h*4)
	for y := range h {
		for x := range w {
			c := left
			if x >= w/2 {
				c = right
			}
			o := (y*w + x) * 4
			pix[o], pix[o+1], pix[o+2], pix[o+3] = byte(c), byte(c>>8), byte(c>>16), 0xff
		}
	}
	time.Sleep(400 * time.Millisecond)
	win.Draw(pix, w, h)
	time.Sleep(300 * time.Millisecond)
	return win, got
}

func pixel(f *Frame, x, y int) uint32 {
	o := y*f.Stride + x*4
	return uint32(f.Pix[o]) | uint32(f.Pix[o+1])<<8 | uint32(f.Pix[o+2])<<16
}

func near(a, b uint32) bool {
	for s := 0; s < 24; s += 8 {
		d := int(a>>s&0xff) - int(b>>s&0xff)
		if d < -8 || d > 8 {
			return false
		}
	}
	return true
}

func TestCastOneWindow(t *testing.T) {
	socket := wltest.Compositor(t)
	const red, blue, green = 0xff0000, 0x0000ff, 0x00ff00
	target, got := castWindow(t, socket, "dev.sinty.CastTest", 320, 180, red, blue)
	_, other := castWindow(t, socket, "dev.sinty.Other", 320, 180, green, green)

	if _, err := OpenCast(socket, "dev.sinty.CastTest", os.Getpid()+1); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("a window of another process was cast: %v", err)
	}
	if _, err := OpenCast(socket, "dev.sinty.Missing", 0); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("a missing app was cast: %v", err)
	}
	k, err := OpenCast(socket, "dev.sinty.CastTest", os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	time.Sleep(300 * time.Millisecond)

	f, err := k.Capture(false, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if f.W != 320 || f.H != 180 {
		t.Fatalf("captured %dx%d, want the window's own 320x180", f.W, f.H)
	}
	if l, r := pixel(f, 20, 90), pixel(f, 300, 90); !near(l, red) || !near(r, blue) {
		t.Fatalf("captured %06x and %06x, want the window's red and blue, not the window above it", l, r)
	}

	other.take()
	got.take()
	k.MoveTo(65535/4, 65535/2, 65535, 65535)
	m := got.wait(t, input.Motion)
	if x, y := float64(m.X)/65535, float64(m.Y)/65535; x < 0.23 || x > 0.27 || y < 0.48 || y > 0.52 {
		t.Fatalf("the pointer reached the window at %.3f,%.3f, want 0.25,0.5", x, y)
	}
	k.Button(272, true)
	if b := got.wait(t, input.Button); b.Code != 272 || b.Value != 1 {
		t.Fatalf("button %d state %d, want 272 pressed", b.Code, b.Value)
	}
	k.Button(272, false)
	if b := got.wait(t, input.Button); b.Code != 272 || b.Value != 0 {
		t.Fatalf("button %d state %d, want 272 released", b.Code, b.Value)
	}
	k.Scroll(true, 2)
	if s := got.wait(t, input.Wheel); s.Code != 0 || s.Value < 1 {
		t.Fatalf("wheel axis %d steps %d, want vertical", s.Code, s.Value)
	}
	k.Key(30, true)
	if e := got.wait(t, input.Key); e.Code != 30 || e.Value != 1 {
		t.Fatalf("key %d state %d, want 30 pressed", e.Code, e.Value)
	}
	k.Key(30, false)
	if e := got.wait(t, input.Key); e.Code != 30 || e.Value != 0 {
		t.Fatalf("key %d state %d, want 30 released", e.Code, e.Value)
	}
	time.Sleep(200 * time.Millisecond)
	for _, e := range other.take() {
		if e.Kind == input.Button || e.Kind == input.Key || e.Kind == input.Wheel {
			t.Fatalf("the window above got %d %d meant for the cast window", e.Kind, e.Code)
		}
	}

	target.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := k.Capture(false, true, nil)
		if errors.Is(err, ErrWindowGone) {
			break
		}
		if err != nil && !errors.Is(err, ErrFailed) {
			t.Fatalf("after the window closed the cast said %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("the cast went on after the window closed")
		}
	}
}
