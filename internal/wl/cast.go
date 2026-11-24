package wl

import (
	"encoding/binary"
	"errors"
	"net"
	"sync"
	"time"
)

var (
	ErrNotOpen    = errors.New("the window is not open on this desktop")
	ErrWindowGone = errors.New("the window was closed")
)

const (
	windowMotion uint16 = iota + 1
	windowButton
	windowAxis
	windowKey
)

type Rect struct{ X, Y, W, H int }

type Cast struct {
	c       *Client
	wmu     sync.Mutex
	window  uint32
	session uint32

	identifier string
	geometry   Rect
	closed     bool
	stopped    bool

	bufW, bufH int
	formats    []uint32
	settled    bool
}

func OpenCast(socket, appID string, pid int) (*Cast, error) {
	c, err := dial(socket)
	if err != nil {
		return nil, err
	}
	k := &Cast{c: c}
	if err := k.open(appID, pid); err != nil {
		c.conn.Close()
		return nil, err
	}
	return k, nil
}

func (k *Cast) open(appID string, pid int) error {
	c := k.c
	if g, ok := c.globals["zsingularity_input_share_manager_v1"]; !ok || g.version < 3 {
		return ErrNoShare
	}
	manager := c.bind("zsingularity_input_share_manager_v1", 3)
	c.shm = c.bind("wl_shm", 1)
	list := c.bind("ext_foreign_toplevel_list_v1", 1)
	sources := c.bind("ext_foreign_toplevel_image_capture_source_manager_v1", 1)
	copier := c.bind("ext_image_copy_capture_manager_v1", 1)
	if c.shm == 0 || list == 0 || sources == 0 || copier == 0 {
		return ErrMissing
	}
	handles := map[string]uint32{}
	c.handlers[list] = func(op uint16, args []byte) {
		if op != 0 || len(args) < 4 {
			return
		}
		h := binary.LittleEndian.Uint32(args)
		c.handlers[h] = func(op uint16, args []byte) {
			if op == 4 {
				handles[str(args)] = h
			}
		}
	}
	k.window = c.newID(func(op uint16, args []byte) {
		switch {
		case op == 0:
			k.identifier = str(args)
		case op == 1 && len(args) >= 16:
			k.geometry = Rect{
				int(int32(binary.LittleEndian.Uint32(args))),
				int(int32(binary.LittleEndian.Uint32(args[4:]))),
				int(int32(binary.LittleEndian.Uint32(args[8:]))),
				int(int32(binary.LittleEndian.Uint32(args[12:]))),
			}
		case op == 2:
			k.closed = true
		}
	})
	c.send(manager, 3, u32(k.window), sarg(appID), i32(int32(pid)))
	for range 2 {
		if err := c.roundtrip(); err != nil {
			return err
		}
	}
	handle, ok := handles[k.identifier]
	if k.closed || k.identifier == "" || !ok {
		return ErrNotOpen
	}
	source := c.newID(func(uint16, []byte) {})
	c.send(sources, 0, u32(source), u32(handle))
	k.session = c.newID(func(op uint16, args []byte) {
		switch {
		case op == 0 && len(args) >= 8:
			k.bufW = int(binary.LittleEndian.Uint32(args))
			k.bufH = int(binary.LittleEndian.Uint32(args[4:]))
			k.formats = k.formats[:0]
			k.settled = false
		case op == 1 && len(args) >= 4:
			k.formats = append(k.formats, binary.LittleEndian.Uint32(args))
		case op == 4:
			k.settled = true
		case op == 5:
			k.stopped = true
		}
	})
	c.send(copier, 0, u32(k.session), u32(source), u32(0))
	deadline := time.Now().Add(5 * time.Second)
	for !k.settled && !k.stopped {
		if err := c.dispatch(deadline); err != nil {
			return err
		}
	}
	if k.stopped {
		return ErrWindowGone
	}
	return nil
}

func (k *Cast) Close() error { return k.c.Close() }

func (k *Cast) format() uint32 {
	var best uint32
	for _, f := range k.formats {
		if rank(f) > rank(best) || best == 0 && rank(f) > 0 {
			best = f
		}
	}
	return best
}

func (k *Cast) Geometry() Rect {
	k.c.mu.Lock()
	defer k.c.mu.Unlock()
	return k.view()
}

func (k *Cast) view() Rect {
	g := k.geometry
	if g.W <= 0 || g.H <= 0 || g.X < 0 || g.Y < 0 || g.X+g.W > k.bufW || g.Y+g.H > k.bufH {
		return Rect{0, 0, k.bufW, k.bufH}
	}
	return g
}

func (k *Cast) Capture(cursor, onDamage bool, into []byte) (*Frame, error) {
	c := k.c
	c.mu.Lock()
	defer c.mu.Unlock()
	for attempt := 0; ; attempt++ {
		if k.closed || k.stopped {
			return nil, ErrWindowGone
		}
		format := k.format()
		if rank(format) == 0 {
			return nil, ErrFormat
		}
		fr := Frame{W: k.bufW, H: k.bufH, Stride: k.bufW * 4, Format: format}
		if err := c.ensureBuffer(fr.Format, fr.W, fr.H, fr.Stride); err != nil {
			return nil, err
		}
		ready, failed, reason := false, false, uint32(0)
		frame := c.newID(func(op uint16, args []byte) {
			switch op {
			case 3:
				ready = true
			case 4:
				failed = true
				if len(args) >= 4 {
					reason = binary.LittleEndian.Uint32(args)
				}
			}
		})
		c.send(k.session, 0, u32(frame))
		c.send(frame, 1, u32(c.buffer))
		c.send(frame, 2, i32(0), i32(0), i32(int32(fr.W)), i32(int32(fr.H)))
		c.send(frame, 3)
		wait := 3 * time.Second
		if onDamage {
			wait = time.Second
		}
		deadline := time.Now().Add(wait)
		var err error
		for !ready && !failed && err == nil {
			err = c.dispatch(deadline)
		}
		c.send(frame, 0)
		delete(c.handlers, frame)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() && onDamage {
				return nil, nil
			}
			return nil, err
		}
		if failed {
			if reason == 1 && attempt < 3 {
				if err := c.roundtrip(); err != nil {
					return nil, err
				}
				continue
			}
			if reason == 2 {
				return nil, ErrWindowGone
			}
			return nil, ErrFailed
		}
		c.take(&fr, nil)
		return k.crop(&fr, into), nil
	}
}

func (k *Cast) crop(fr *Frame, into []byte) *Frame {
	g := k.view()
	if g == (Rect{0, 0, fr.W, fr.H}) {
		return fr
	}
	out := &Frame{W: g.W, H: g.H, Stride: g.W * 4, Format: fr.Format}
	size := out.Stride * out.H
	if cap(into) >= size {
		out.Pix = into[:size]
	} else {
		out.Pix = make([]byte, size)
	}
	for y := 0; y < g.H; y++ {
		src := fr.Pix[(g.Y+y)*fr.Stride+g.X*4:]
		copy(out.Pix[y*out.Stride:(y+1)*out.Stride], src[:out.Stride])
	}
	return out
}

func (k *Cast) MoveTo(x, y, w, h uint32) {
	g := k.Geometry()
	px := float64(g.X) + float64(x)/float64(max(w, 1))*float64(max(g.W-1, 0))
	py := float64(g.Y) + float64(y)/float64(max(h, 1))*float64(max(g.H-1, 0))
	k.wmu.Lock()
	defer k.wmu.Unlock()
	k.c.send(k.window, windowMotion, fixed(px), fixed(py))
}

func (k *Cast) Button(code uint32, pressed bool) {
	k.wmu.Lock()
	defer k.wmu.Unlock()
	k.c.send(k.window, windowButton, u32(code), u32(state(pressed)))
}

func (k *Cast) Scroll(vertical bool, steps int32) {
	axis := uint32(1)
	if vertical {
		axis = 0
	}
	k.wmu.Lock()
	defer k.wmu.Unlock()
	k.c.send(k.window, windowAxis, u32(axis), i32(steps))
}

func (k *Cast) Key(code uint32, pressed bool) {
	k.wmu.Lock()
	defer k.wmu.Unlock()
	k.c.send(k.window, windowKey, u32(code), u32(state(pressed)))
}

func (k *Cast) Flush() error { return nil }

func state(pressed bool) uint32 {
	if pressed {
		return 1
	}
	return 0
}
