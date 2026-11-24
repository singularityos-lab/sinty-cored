package main

import (
	"bufio"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/singularityos-lab/sinty-cored/internal/channel"
	"github.com/singularityos-lab/sinty-cored/internal/input"
	"github.com/singularityos-lab/sinty-cored/internal/pair"
	"github.com/singularityos-lab/sinty-cored/internal/wl"
	"github.com/singularityos-lab/sinty-cored/internal/wltest"
)

type lines struct {
	t  *testing.T
	ch chan map[string]any
}

func readLines(t *testing.T, r io.Reader) *lines {
	l := &lines{t: t, ch: make(chan map[string]any, 16)}
	go func() {
		scan := bufio.NewScanner(r)
		for scan.Scan() {
			var m map[string]any
			if json.Unmarshal(scan.Bytes(), &m) == nil {
				l.ch <- m
			}
		}
		close(l.ch)
	}()
	return l
}

func (l *lines) next(event string) map[string]any {
	l.t.Helper()
	select {
	case m, ok := <-l.ch:
		if !ok {
			l.t.Fatalf("the helper stopped before %q", event)
		}
		if m["event"] != event {
			l.t.Fatalf("got %v, want %q", m, event)
		}
		return m
	case <-time.After(15 * time.Second):
		l.t.Fatalf("no %q", event)
	}
	return nil
}

type inbox struct {
	mu     sync.Mutex
	events []input.Event
}

func (b *inbox) add(e []input.Event) {
	b.mu.Lock()
	b.events = append(b.events, e...)
	b.mu.Unlock()
}

func (b *inbox) wait(t *testing.T, match func(input.Event) bool, what string) {
	t.Helper()
	end := time.Now().Add(5 * time.Second)
	for time.Now().Before(end) {
		b.mu.Lock()
		for i, e := range b.events {
			if match(e) {
				b.events = b.events[i+1:]
				b.mu.Unlock()
				return
			}
		}
		b.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the thrown window never got %s", what)
}

func colours(w, h int, left, right uint32) []byte {
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
	return pix
}

func at(f *wl.Frame, x, y int) uint32 {
	o := y*f.Stride + x*4
	return uint32(f.Pix[o]) | uint32(f.Pix[o+1])<<8 | uint32(f.Pix[o+2])<<16
}

func TestCastAcrossDevices(t *testing.T) {
	socket := wltest.Compositor(t)
	t.Setenv("WAYLAND_DISPLAY", socket)
	const red, blue = 0xff0000, 0x0000ff
	thrown, err := wl.OpenWindow(socket, "thrown", "dev.sinty.Thrown", 320, 180)
	if err != nil {
		t.Fatal(err)
	}
	got := &inbox{}
	thrown.OnInput = got.add
	go thrown.Run()
	time.Sleep(400 * time.Millisecond)
	thrown.Draw(colours(320, 180, red, blue), 320, 180)
	time.Sleep(300 * time.Millisecond)

	serveIn, toServe := io.Pipe()
	fromServe, serveOut := io.Pipe()
	served := make(chan int, 1)
	go func() { served <- serveCast("dev.sinty.Thrown", os.Getpid(), "127.0.0.1:0", serveIn, serveOut) }()
	srv := readLines(t, fromServe)
	ready := srv.next("ready")
	if ready["width"] != 320.0 || ready["height"] != 180.0 {
		t.Fatalf("the cast offered %vx%v, want the window's 320x180", ready["width"], ready["height"])
	}
	addr := fmt.Sprintf("127.0.0.1:%v", ready["port"])
	coreKey, _ := base64.StdEncoding.DecodeString(ready["key"].(string))

	intruder, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	refused := make(chan error, 1)
	go func() {
		_, priv, _ := ed25519.GenerateKey(rand.Reader)
		_, err := channel.Screen(intruder, pair.Identity{Priv: priv}, coreKey, "us", nil)
		refused <- err
	}()

	showIn, showOut := io.Pipe()
	shown := make(chan int, 1)
	go func() {
		shown <- showCast(addr, ready["key"].(string), "Thrown", "dev.sinty.Shown", "320x180", "us", showOut)
	}()
	show := readLines(t, showIn)
	id := show.next("identity")
	fmt.Fprintf(toServe, "{\"cmd\":\"trust\",\"screen\":%q}\n", id["key"])
	if err := <-refused; !errors.Is(err, channel.ErrUntrusted) {
		t.Fatalf("a device that was not named got the cast: %v", err)
	}
	srv.next("showing")

	var viewer *wl.Cast
	end := time.Now().Add(10 * time.Second)
	for viewer == nil {
		viewer, err = wl.OpenCast(socket, "dev.sinty.Shown", os.Getpid())
		if err != nil && time.Now().After(end) {
			t.Fatalf("the cast window did not open: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	defer viewer.Close()
	var f *wl.Frame
	for {
		f, err = viewer.Capture(false, false, nil)
		if err != nil {
			t.Fatal(err)
		}
		if f.W == 320 && f.H == 180 && at(f, 20, 90) == red && at(f, 300, 90) == blue {
			break
		}
		if time.Now().After(end) {
			t.Fatalf("the other device shows %dx%d %06x %06x, want the thrown window exactly", f.W, f.H, at(f, 20, 90), at(f, 300, 90))
		}
		time.Sleep(100 * time.Millisecond)
	}

	viewer.MoveTo(65535/4, 65535/2, 65535, 65535)
	got.wait(t, func(e input.Event) bool {
		x, y := float64(e.X)/65535, float64(e.Y)/65535
		return e.Kind == input.Motion && x > 0.23 && x < 0.27 && y > 0.48 && y < 0.52
	}, "the pointer where it was moved on the other device")
	viewer.Button(272, true)
	got.wait(t, func(e input.Event) bool { return e.Kind == input.Button && e.Code == 272 && e.Value == 1 }, "the click")
	viewer.Button(272, false)
	got.wait(t, func(e input.Event) bool { return e.Kind == input.Button && e.Code == 272 && e.Value == 0 }, "the release")
	viewer.Key(30, true)
	got.wait(t, func(e input.Event) bool { return e.Kind == input.Key && e.Code == 30 && e.Value == 1 }, "the key")
	viewer.Key(30, false)
	got.wait(t, func(e input.Event) bool { return e.Kind == input.Key && e.Code == 30 && e.Value == 0 }, "the key release")

	thrown.Close()
	if m := srv.next("ended"); m["reason"] != "closed" {
		t.Fatalf("the cast ended with %v, want the window closed", m)
	}
	if code := <-served; code != 0 {
		t.Fatalf("the casting side exited %d", code)
	}
	show.next("ended")
	if code := <-shown; code != 0 {
		t.Fatalf("the showing side exited %d", code)
	}
	toServe.Close()
}
