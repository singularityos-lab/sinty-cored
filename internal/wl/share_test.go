package wl

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/singularityos-lab/sinty-cored/internal/input"
)

func startCompositor(t *testing.T) string {
	bin := os.Getenv("LABWC")
	if bin == "" {
		t.Skip("set LABWC to a labwc binary to run the compositor tests")
	}
	dir, err := os.MkdirTemp("", "wl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+dir, "XDG_CONFIG_HOME="+filepath.Join(dir, "config"),
		"WLR_BACKENDS=headless", "WLR_HEADLESS_OUTPUTS=1", "WAYLAND_DISPLAY=")
	log, err := os.Create(filepath.Join(dir, "labwc.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})
	for range 100 {
		if found, _ := filepath.Glob(filepath.Join(dir, "wayland-[0-9]")); len(found) > 0 {
			return found[0]
		}
		time.Sleep(50 * time.Millisecond)
	}
	out, _ := os.ReadFile(log.Name())
	t.Fatalf("the compositor did not start:\n%s", out)
	return ""
}

func collect(sh *Share, stop chan struct{}) <-chan EdgeEvent {
	out := make(chan EdgeEvent, 64)
	go func() {
		for {
			batch := sh.Events(stop)
			if batch == nil {
				return
			}
			for _, e := range batch {
				if e.Kind != EdgePressing && e.Kind != EdgeMotion {
					out <- e
				}
			}
		}
	}()
	return out
}

func next(t *testing.T, events <-chan EdgeEvent, want EdgeKind) EdgeEvent {
	t.Helper()
	select {
	case e := <-events:
		if e.Kind != want {
			t.Fatalf("got event %d, want %d", e.Kind, want)
		}
		return e
	case <-time.After(5 * time.Second):
		t.Fatalf("no event %d", want)
	}
	return EdgeEvent{}
}

func quiet(t *testing.T, events <-chan EdgeEvent, what string) {
	t.Helper()
	select {
	case e := <-events:
		t.Fatalf("%s, but got event %d", what, e.Kind)
	case <-time.After(700 * time.Millisecond):
	}
}

func TestEdgeEventsAndWindowDrag(t *testing.T) {
	socket := startCompositor(t)
	sh, err := OpenShare(socket)
	if err != nil {
		t.Fatal(err)
	}
	defer sh.Close()
	go sh.Run()
	outs := sh.Outputs()
	if len(outs) != 1 || outs[0].W == 0 {
		t.Fatalf("outputs %+v", outs)
	}
	o := outs[0]
	w, h := uint32(o.W), uint32(o.H)
	edge := sh.Edge(o.Name, SideRight, 0, o.H)
	stop := make(chan struct{})
	defer close(stop)
	events := collect(sh, stop)

	in, err := Connect(socket)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	if err := in.StartInput("us"); err != nil {
		t.Fatal(err)
	}

	in.MoveTo(w-1, h/2, w, h)
	for range 4 {
		in.Move(10, 0)
	}
	e := next(t, events, EdgeEntered)
	if e.X < float64(h/2)-2 || e.X > float64(h/2)+2 {
		t.Fatalf("crossed at %v, want %v", e.X, h/2)
	}
	sh.Release(edge, float64(h/2))
	next(t, events, EdgeLeft)

	win, err := OpenWindow(socket, "drag me", "dev.sinty.DragTest", 400, 300)
	if err != nil {
		t.Fatal(err)
	}
	defer win.Close()
	var mu sync.Mutex
	touched := false
	win.OnInput = func(events []input.Event) {
		mu.Lock()
		touched = true
		mu.Unlock()
	}
	go win.Run()
	time.Sleep(500 * time.Millisecond)
	over := func(x, y uint32) bool {
		mu.Lock()
		touched = false
		mu.Unlock()
		in.MoveTo(x, y, w, h)
		in.Move(1, 0)
		in.Move(-1, 0)
		time.Sleep(60 * time.Millisecond)
		mu.Lock()
		defer mu.Unlock()
		return touched
	}
	var gx, gy uint32
	for y := uint32(40); y < h && gx == 0; y += 40 {
		for x := uint32(40); x < w/2; x += 40 {
			if over(x, y) {
				gx, gy = x, y
				break
			}
		}
	}
	if gx == 0 {
		t.Fatal("the window never got the pointer")
	}
	in.MoveTo(gx, gy, w, h)
	in.Key(125, true)
	in.Button(272, true)
	push := func(dx float64, n int) {
		for range n {
			in.Move(dx, 0)
			time.Sleep(5 * time.Millisecond)
		}
	}
	push(25, int(w/25)+10)
	e = next(t, events, WindowEntered)
	if e.AppID != "dev.sinty.DragTest" || int(e.PID) != os.Getpid() {
		t.Fatalf("dragged %q pid %d, want dev.sinty.DragTest pid %d", e.AppID, e.PID, os.Getpid())
	}
	push(-30, 3)
	next(t, events, WindowLeft)
	push(25, 12)
	next(t, events, WindowEntered)
	in.Button(272, false)
	in.Key(125, false)
	next(t, events, WindowDropped)
	time.Sleep(200 * time.Millisecond)
	if !over(gx, gy) {
		t.Fatal("the dropped window did not go back where the move started")
	}

	in.MoveTo(gx, gy, w, h)
	in.Key(125, true)
	in.Button(272, true)
	push(25, int((w-1-gx+24)/25)+2)
	time.Sleep(100 * time.Millisecond)
	in.Button(272, false)
	in.Key(125, false)
	quiet(t, events, "a window only touching the edge must not be thrown")
}
