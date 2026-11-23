package wl

import (
	"encoding/binary"
	"errors"
	"math"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/singularityos-lab/sinty-cored/internal/fb"
	"github.com/singularityos-lab/sinty-cored/internal/input"
)

var ErrNoWindow = errors.New("the compositor cannot open a window")

const (
	seatPointer  = 1
	seatKeyboard = 2
	decorServer  = 2
	axisVertical = 0
	wheelStep    = 10.0
)

type shmBuffer struct {
	pool, id uint32
	file     *os.File
	mem      []byte
	w, h     int
	busy     bool
}

type Window struct {
	mu sync.Mutex
	c  *Client

	compositor, wmBase, decorations, seat uint32
	surface, xdgSurface, toplevel         uint32
	pointer, keyboard                     uint32

	canvas         *fb.Display
	frame          []byte
	frameW, frameH int
	pendingW       int
	pendingH       int
	configured     bool
	dirty          bool
	buffers        [2]*shmBuffer
	pressed        map[uint32]bool
	px, py         float64
	wheel          [2]float64
	closed         chan struct{}
	closeOnce      sync.Once
	OnInput        func([]input.Event)
}

func SocketPath() string {
	name := os.Getenv("WAYLAND_DISPLAY")
	if name == "" {
		name = "wayland-0"
	}
	if filepath.IsAbs(name) {
		return name
	}
	return filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), name)
}

func OpenWindow(socket, title, appID string, w, h int) (*Window, error) {
	c, err := dial(socket)
	if err != nil {
		return nil, err
	}
	win := &Window{c: c, canvas: fb.NewCanvas(w, h), pressed: map[uint32]bool{}, closed: make(chan struct{})}
	c.guard = &win.mu
	c.shm = c.bind("wl_shm", 1)
	win.compositor = c.bind("wl_compositor", 4)
	win.wmBase = c.bindWith("xdg_wm_base", 1, func(op uint16, args []byte) {
		if op == 0 && len(args) >= 4 {
			c.send(win.wmBase, 3, args[:4])
		}
	})
	win.decorations = c.bind("zxdg_decoration_manager_v1", 1)
	if c.shm == 0 || win.compositor == 0 || win.wmBase == 0 {
		c.conn.Close()
		return nil, ErrNoWindow
	}
	win.seat = c.bindWith("wl_seat", 1, win.seatEvent)

	win.surface = c.newID(func(uint16, []byte) {})
	c.send(win.compositor, 0, u32(win.surface))
	win.xdgSurface = c.newID(win.xdgSurfaceEvent)
	c.send(win.wmBase, 2, u32(win.xdgSurface), u32(win.surface))
	win.toplevel = c.newID(win.toplevelEvent)
	c.send(win.xdgSurface, 1, u32(win.toplevel))
	c.send(win.toplevel, 2, sarg(title))
	c.send(win.toplevel, 3, sarg(appID))
	c.send(win.toplevel, 8, i32(320), i32(180))
	if win.decorations != 0 {
		deco := c.newID(func(uint16, []byte) {})
		c.send(win.decorations, 1, u32(deco), u32(win.toplevel))
		c.send(deco, 1, u32(decorServer))
	}
	c.send(win.surface, 6)
	if err := c.roundtrip(); err != nil {
		c.conn.Close()
		return nil, err
	}
	return win, nil
}

func (c *Client) bindWith(iface string, want uint32, h handler) uint32 {
	g, ok := c.globals[iface]
	if !ok {
		return 0
	}
	id := c.newID(h)
	c.send(c.registry, 0, u32(g.name), sarg(iface), u32(min(g.version, want)), u32(id))
	return id
}

func (win *Window) Run() error {
	for {
		select {
		case <-win.closed:
			return nil
		default:
		}
		if err := win.c.dispatch(time.Time{}); err != nil {
			select {
			case <-win.closed:
				return nil
			default:
				return err
			}
		}
	}
}

func (win *Window) Closed() <-chan struct{} { return win.closed }

func (win *Window) Close() error {
	win.closeOnce.Do(func() { close(win.closed) })
	win.mu.Lock()
	defer win.mu.Unlock()
	for i, b := range win.buffers {
		if b != nil {
			win.freeBuffer(b)
			win.buffers[i] = nil
		}
	}
	return win.c.conn.Close()
}

func (win *Window) Size() (int, int) {
	win.mu.Lock()
	defer win.mu.Unlock()
	return win.canvas.W, win.canvas.H
}

func (win *Window) Clear() {
	win.mu.Lock()
	defer win.mu.Unlock()
	win.frame = nil
	win.canvas.Clear()
	win.present()
}

func (win *Window) Text(s string, cx, cy, scale int, c uint32) {
	win.mu.Lock()
	defer win.mu.Unlock()
	win.canvas.Text(s, cx, cy, scale, c)
	win.present()
}

func (win *Window) Draw(pix []byte, w, h int) {
	win.mu.Lock()
	defer win.mu.Unlock()
	if len(win.frame) != len(pix) {
		win.frame = make([]byte, len(pix))
	}
	copy(win.frame, pix)
	win.frameW, win.frameH = w, h
	win.canvas.Draw(pix, w, h)
	win.present()
}

func (win *Window) xdgSurfaceEvent(op uint16, args []byte) {
	if op != 0 || len(args) < 4 {
		return
	}
	win.c.send(win.xdgSurface, 4, args[:4])
	if w, h := win.pendingW, win.pendingH; w > 0 && h > 0 && (w != win.canvas.W || h != win.canvas.H) {
		win.canvas = fb.NewCanvas(w, h)
		if win.frame != nil {
			win.canvas.Draw(win.frame, win.frameW, win.frameH)
		}
	}
	win.configured = true
	win.present()
}

func (win *Window) toplevelEvent(op uint16, args []byte) {
	switch op {
	case 0:
		if len(args) >= 8 {
			win.pendingW = int(int32(binary.LittleEndian.Uint32(args)))
			win.pendingH = int(int32(binary.LittleEndian.Uint32(args[4:])))
		}
	case 1:
		win.closeOnce.Do(func() { close(win.closed) })
	}
}

func (win *Window) buffer() *shmBuffer {
	w, h := win.canvas.W, win.canvas.H
	for i, b := range win.buffers {
		if b != nil && !b.busy && (b.w != w || b.h != h) {
			win.freeBuffer(b)
			win.buffers[i] = nil
		}
	}
	for _, b := range win.buffers {
		if b != nil && !b.busy {
			return b
		}
	}
	for i, b := range win.buffers {
		if b != nil {
			continue
		}
		f, mem, err := shmFile(w * h * 4)
		if err != nil {
			return nil
		}
		nb := &shmBuffer{file: f, mem: mem, w: w, h: h}
		nb.pool = win.c.newID(func(uint16, []byte) {})
		win.c.sendFD(win.c.shm, 0, int(f.Fd()), u32(nb.pool), i32(int32(w*h*4)))
		nb.id = win.c.newID(func(op uint16, _ []byte) {
			if op != 0 {
				return
			}
			nb.busy = false
			if win.dirty {
				win.present()
			}
		})
		win.c.send(nb.pool, 0, u32(nb.id), i32(0), i32(int32(w)), i32(int32(h)), i32(int32(w*4)), u32(FormatXRGB8888))
		win.buffers[i] = nb
		return nb
	}
	return nil
}

func (win *Window) freeBuffer(b *shmBuffer) {
	win.c.send(b.id, 0)
	win.c.send(b.pool, 1)
	syscall.Munmap(b.mem)
	b.file.Close()
}

func (win *Window) present() {
	if !win.configured {
		win.dirty = true
		return
	}
	b := win.buffer()
	if b == nil {
		win.dirty = true
		return
	}
	win.dirty = false
	copy(b.mem, win.canvas.Pixels())
	b.busy = true
	win.c.send(win.surface, 1, u32(b.id), i32(0), i32(0))
	win.c.send(win.surface, 9, i32(0), i32(0), i32(int32(b.w)), i32(int32(b.h)))
	win.c.send(win.surface, 6)
}

func (win *Window) seatEvent(op uint16, args []byte) {
	if op != 0 || len(args) < 4 {
		return
	}
	caps := binary.LittleEndian.Uint32(args)
	if caps&seatPointer != 0 && win.pointer == 0 {
		win.pointer = win.c.newID(win.pointerEvent)
		win.c.send(win.seat, 0, u32(win.pointer))
	}
	if caps&seatKeyboard != 0 && win.keyboard == 0 {
		win.keyboard = win.c.newID(win.keyboardEvent)
		win.c.send(win.seat, 1, u32(win.keyboard))
	}
}

func fixedArg(b []byte) float64 { return float64(int32(binary.LittleEndian.Uint32(b))) / 256 }

func (win *Window) emit(events ...input.Event) {
	if win.OnInput != nil && len(events) > 0 {
		win.OnInput(events)
	}
}

func (win *Window) motion() input.Event {
	fit := win.canvas.FitFor(win.frameW, win.frameH)
	if fit.W == 0 {
		fit = fb.Fit{W: win.canvas.W, H: win.canvas.H}
	}
	nx := (win.px - float64(fit.X)) / float64(max(fit.W-1, 1))
	ny := (win.py - float64(fit.Y)) / float64(max(fit.H-1, 1))
	nx, ny = min(max(nx, 0), 1), min(max(ny, 0), 1)
	return input.Event{Kind: input.Motion, X: uint16(nx * 65535), Y: uint16(ny * 65535)}
}

func (win *Window) pointerEvent(op uint16, args []byte) {
	switch op {
	case 0:
		if len(args) >= 16 {
			win.c.send(win.pointer, 0, args[:4], u32(0), i32(0), i32(0))
			win.px, win.py = fixedArg(args[8:]), fixedArg(args[12:])
			win.emit(win.motion())
		}
	case 2:
		if len(args) >= 12 {
			win.px, win.py = fixedArg(args[4:]), fixedArg(args[8:])
			win.emit(win.motion())
		}
	case 3:
		if len(args) >= 16 {
			win.emit(input.Event{Kind: input.Button, Code: binary.LittleEndian.Uint32(args[8:]), Value: int32(binary.LittleEndian.Uint32(args[12:]))})
		}
	case 4:
		if len(args) >= 12 {
			axis := binary.LittleEndian.Uint32(args[4:]) & 1
			win.wheel[axis] += fixedArg(args[8:])
			if steps := math.Trunc(win.wheel[axis] / wheelStep); steps != 0 {
				win.wheel[axis] -= steps * wheelStep
				win.emit(input.Event{Kind: input.Wheel, Code: axis, Value: int32(steps)})
			}
		}
	}
}

func (win *Window) keyboardEvent(op uint16, args []byte) {
	switch op {
	case 2:
		var up []input.Event
		for code := range win.pressed {
			up = append(up, input.Event{Kind: input.Key, Code: code})
		}
		clear(win.pressed)
		win.emit(up...)
	case 3:
		if len(args) >= 16 {
			code := binary.LittleEndian.Uint32(args[8:])
			down := binary.LittleEndian.Uint32(args[12:]) == 1
			if down {
				win.pressed[code] = true
			} else {
				delete(win.pressed, code)
			}
			value := int32(0)
			if down {
				value = 1
			}
			win.emit(input.Event{Kind: input.Key, Code: code, Value: value})
		}
	}
}
