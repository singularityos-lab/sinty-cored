package wl

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"sync"
	"syscall"
	"time"
)

var (
	ErrMissing = errors.New("the compositor does not offer screen capture or virtual input")
	ErrFailed  = errors.New("the compositor could not copy the screen")
	ErrFormat  = errors.New("the compositor offers no 32-bit screen format")
)

type handler func(op uint16, args []byte)

type global struct {
	name    uint32
	version uint32
}

type Client struct {
	conn     *net.UnixConn
	mu       sync.Mutex
	next     uint32
	handlers map[uint32]handler
	globals  map[string]global
	outputs  []global
	err      error
	start    time.Time
	pending  []byte

	registry, shm, seat, output, copier, pointers, keyboards uint32
	pointer, keyboard                                        uint32
	depressed, locked                                        uint32

	pool, buffer uint32
	shmFile      *os.File
	shmMem       []byte
	shmKey       [4]int
}

func Connect(socket string) (*Client, error) {
	addr, err := net.ResolveUnixAddr("unix", socket)
	if err != nil {
		return nil, err
	}
	conn, err := net.DialUnix("unix", nil, addr)
	if err != nil {
		return nil, err
	}
	c := &Client{conn: conn, next: 2, handlers: map[uint32]handler{}, globals: map[string]global{}, start: time.Now()}
	c.handlers[1] = func(op uint16, args []byte) {
		switch {
		case op == 0 && len(args) >= 8:
			c.err = fmt.Errorf("wayland error %d: %s", binary.LittleEndian.Uint32(args[4:]), str(args[8:]))
		case op == 1 && len(args) >= 4:
			delete(c.handlers, binary.LittleEndian.Uint32(args))
		}
	}
	c.registry = c.newID(func(op uint16, args []byte) {
		if op != 0 || len(args) < 8 {
			return
		}
		name := binary.LittleEndian.Uint32(args)
		iface, rest := strArg(args[4:])
		if len(rest) < 4 {
			return
		}
		g := global{name, binary.LittleEndian.Uint32(rest)}
		c.globals[iface] = g
		if iface == "wl_output" {
			c.outputs = append(c.outputs, g)
		}
	})
	c.send(1, 1, u32(c.registry))
	if err := c.roundtrip(); err != nil {
		conn.Close()
		return nil, err
	}
	c.shm = c.bind("wl_shm", 1)
	c.seat = c.bind("wl_seat", 1)
	c.copier = c.bind("zwlr_screencopy_manager_v1", 2)
	c.pointers = c.bind("zwlr_virtual_pointer_manager_v1", 1)
	c.keyboards = c.bind("zwp_virtual_keyboard_manager_v1", 1)
	if len(c.outputs) > 0 {
		c.output = c.bindGlobal(c.outputs[0], "wl_output", 1)
	}
	if err := c.roundtrip(); err != nil {
		conn.Close()
		return nil, err
	}
	return c, nil
}

func (c *Client) bind(iface string, want uint32) uint32 {
	g, ok := c.globals[iface]
	if !ok {
		return 0
	}
	return c.bindGlobal(g, iface, want)
}

func (c *Client) bindGlobal(g global, iface string, want uint32) uint32 {
	id := c.newID(func(uint16, []byte) {})
	c.send(c.registry, 0, u32(g.name), sarg(iface), u32(min(g.version, want)), u32(id))
	return id
}

func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropBuffer()
	return c.conn.Close()
}

func (c *Client) CanCapture() bool { return c.shm != 0 && c.copier != 0 && c.output != 0 }

func (c *Client) CanInject() bool { return c.pointers != 0 && c.keyboards != 0 && c.seat != 0 }

func (c *Client) newID(h handler) uint32 {
	id := c.next
	c.next++
	c.handlers[id] = h
	return id
}

func u32(v uint32) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, v)
	return b
}

func i32(v int32) []byte { return u32(uint32(v)) }

func fixed(v float64) []byte { return i32(int32(math.Round(v * 256))) }

func sarg(s string) []byte {
	n := len(s) + 1
	b := make([]byte, 4+(n+3)/4*4)
	binary.LittleEndian.PutUint32(b, uint32(n))
	copy(b[4:], s)
	return b
}

func strArg(b []byte) (string, []byte) {
	if len(b) < 4 {
		return "", nil
	}
	n := int(binary.LittleEndian.Uint32(b))
	pad := 4 + (n+3)/4*4
	if n == 0 || len(b) < pad {
		return "", nil
	}
	return string(b[4 : 4+n-1]), b[pad:]
}

func str(b []byte) string {
	s, _ := strArg(b)
	return s
}

func (c *Client) send(obj uint32, op uint16, args ...[]byte) {
	c.sendFD(obj, op, -1, args...)
}

func (c *Client) sendFD(obj uint32, op uint16, fd int, args ...[]byte) {
	size := 8
	for _, a := range args {
		size += len(a)
	}
	msg := make([]byte, 8, size)
	binary.LittleEndian.PutUint32(msg, obj)
	binary.LittleEndian.PutUint32(msg[4:], uint32(size)<<16|uint32(op))
	for _, a := range args {
		msg = append(msg, a...)
	}
	var oob []byte
	if fd >= 0 {
		oob = syscall.UnixRights(fd)
	}
	if _, _, err := c.conn.WriteMsgUnix(msg, oob, nil); err != nil && c.err == nil {
		c.err = err
	}
}

func (c *Client) dispatch(deadline time.Time) error {
	c.conn.SetReadDeadline(deadline)
	buf := make([]byte, 65536)
	oob := make([]byte, syscall.CmsgSpace(28*4))
	n, oobn, _, _, err := c.conn.ReadMsgUnix(buf, oob)
	if err != nil {
		return err
	}
	if oobn > 0 {
		msgs, _ := syscall.ParseSocketControlMessage(oob[:oobn])
		for _, m := range msgs {
			if fds, err := syscall.ParseUnixRights(&m); err == nil {
				for _, fd := range fds {
					syscall.Close(fd)
				}
			}
		}
	}
	data := append(c.pending, buf[:n]...)
	for len(data) >= 8 {
		size := int(binary.LittleEndian.Uint32(data[4:]) >> 16)
		if size < 8 {
			return errors.New("broken wayland message")
		}
		if size > len(data) {
			break
		}
		obj := binary.LittleEndian.Uint32(data)
		op := uint16(binary.LittleEndian.Uint32(data[4:]) & 0xffff)
		if h, ok := c.handlers[obj]; ok {
			h(op, data[8:size])
		}
		data = data[size:]
	}
	c.pending = append(c.pending[:0], data...)
	return c.err
}

func (c *Client) roundtrip() error {
	done := false
	cb := c.newID(func(uint16, []byte) { done = true })
	c.send(1, 0, u32(cb))
	deadline := time.Now().Add(5 * time.Second)
	for !done {
		if err := c.dispatch(deadline); err != nil {
			return err
		}
	}
	return c.err
}

type Frame struct {
	W, H, Stride int
	Format       uint32
	Pix          []byte
}

const (
	FormatARGB8888 = 0
	FormatXRGB8888 = 1
)

func shmFile(size int) (*os.File, []byte, error) {
	dir := "/dev/shm"
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		dir = os.Getenv("XDG_RUNTIME_DIR")
	}
	f, err := os.CreateTemp(dir, "sinty-cored-*")
	if err != nil {
		return nil, nil, err
	}
	os.Remove(f.Name())
	if err := f.Truncate(int64(size)); err != nil {
		f.Close()
		return nil, nil, err
	}
	mem, err := syscall.Mmap(int(f.Fd()), 0, size, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, mem, nil
}

func (c *Client) dropBuffer() {
	if c.buffer != 0 {
		c.send(c.buffer, 0)
		c.send(c.pool, 1)
		c.buffer, c.pool = 0, 0
	}
	if c.shmMem != nil {
		syscall.Munmap(c.shmMem)
		c.shmMem = nil
	}
	if c.shmFile != nil {
		c.shmFile.Close()
		c.shmFile = nil
	}
}

func (c *Client) ensureBuffer(format uint32, w, h, stride int) error {
	key := [4]int{int(format), w, h, stride}
	if c.buffer != 0 && c.shmKey == key {
		return nil
	}
	c.dropBuffer()
	f, mem, err := shmFile(stride * h)
	if err != nil {
		return err
	}
	c.shmFile, c.shmMem, c.shmKey = f, mem, key
	c.pool = c.newID(func(uint16, []byte) {})
	c.sendFD(c.shm, 0, int(f.Fd()), u32(c.pool), i32(int32(stride*h)))
	c.buffer = c.newID(func(uint16, []byte) {})
	c.send(c.pool, 0, u32(c.buffer), i32(0), i32(int32(w)), i32(int32(h)), i32(int32(stride)), u32(format))
	return nil
}

func (c *Client) Capture(cursor, onDamage bool, into []byte) (*Frame, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.CanCapture() {
		return nil, ErrMissing
	}
	var fr Frame
	offered, ready, failed := false, false, false
	frame := c.newID(func(op uint16, args []byte) {
		switch op {
		case 0:
			if len(args) >= 16 {
				format := binary.LittleEndian.Uint32(args)
				if format == FormatARGB8888 || format == FormatXRGB8888 || !offered {
					fr.Format = format
					fr.W = int(binary.LittleEndian.Uint32(args[4:]))
					fr.H = int(binary.LittleEndian.Uint32(args[8:]))
					fr.Stride = int(binary.LittleEndian.Uint32(args[12:]))
					offered = true
				}
			}
		case 2:
			ready = true
		case 3:
			failed = true
		}
	})
	defer func() {
		c.send(frame, 1)
		delete(c.handlers, frame)
	}()
	overlay := int32(0)
	if cursor {
		overlay = 1
	}
	c.send(c.copier, 0, u32(frame), i32(overlay), u32(c.output))
	deadline := time.Now().Add(3 * time.Second)
	if err := c.roundtrip(); err != nil {
		return nil, err
	}
	if !offered {
		return nil, ErrFailed
	}
	if fr.Format != FormatARGB8888 && fr.Format != FormatXRGB8888 {
		return nil, ErrFormat
	}
	if err := c.ensureBuffer(fr.Format, fr.W, fr.H, fr.Stride); err != nil {
		return nil, err
	}
	if onDamage {
		c.send(frame, 2, u32(c.buffer))
		deadline = time.Now().Add(time.Second)
	} else {
		c.send(frame, 0, u32(c.buffer))
	}
	for !ready && !failed {
		if err := c.dispatch(deadline); err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() && onDamage {
				return nil, nil
			}
			return nil, err
		}
	}
	if failed {
		return nil, ErrFailed
	}
	size := fr.Stride * fr.H
	if cap(into) >= size {
		fr.Pix = into[:size]
	} else {
		fr.Pix = make([]byte, size)
	}
	copy(fr.Pix, c.shmMem[:size])
	return &fr, nil
}

const keymap = `xkb_keymap {
	xkb_keycodes { include "evdev+aliases(qwerty)" };
	xkb_types { include "complete" };
	xkb_compat { include "complete" };
	xkb_symbols { include "pc+%s+inet(evdev)" };
};
`

func (c *Client) StartInput(layout string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.CanInject() {
		return ErrMissing
	}
	if c.pointer != 0 {
		return nil
	}
	if !validLayout(layout) {
		layout = "us"
	}
	c.pointer = c.newID(func(uint16, []byte) {})
	c.send(c.pointers, 0, u32(c.seat), u32(c.pointer))
	c.keyboard = c.newID(func(uint16, []byte) {})
	c.send(c.keyboards, 0, u32(c.seat), u32(c.keyboard))
	text := append([]byte(fmt.Sprintf(keymap, layout)), 0)
	f, mem, err := shmFile(len(text))
	if err != nil {
		return err
	}
	copy(mem, text)
	syscall.Munmap(mem)
	c.sendFD(c.keyboard, 0, int(f.Fd()), u32(1), u32(uint32(len(text))))
	err = c.roundtrip()
	f.Close()
	return err
}

func validLayout(s string) bool {
	if s == "" || len(s) > 16 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z') {
			return false
		}
	}
	return true
}

func (c *Client) now() []byte { return u32(uint32(time.Since(c.start).Milliseconds())) }

func (c *Client) MoveTo(x, y, w, h uint32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pointer == 0 {
		return
	}
	c.send(c.pointer, 1, c.now(), u32(x), u32(y), u32(w), u32(h))
	c.send(c.pointer, 4)
}

func (c *Client) Button(code uint32, pressed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pointer == 0 {
		return
	}
	state := uint32(0)
	if pressed {
		state = 1
	}
	c.send(c.pointer, 2, c.now(), u32(code), u32(state))
	c.send(c.pointer, 4)
}

func (c *Client) Scroll(vertical bool, steps int32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pointer == 0 {
		return
	}
	axis := uint32(1)
	if vertical {
		axis = 0
	}
	c.send(c.pointer, 7, c.now(), u32(axis), fixed(float64(steps)*15), i32(steps))
	c.send(c.pointer, 4)
}

func (c *Client) Key(code uint32, pressed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.keyboard == 0 {
		return
	}
	state := uint32(0)
	if pressed {
		state = 1
	}
	c.send(c.keyboard, 1, c.now(), u32(code), u32(state))
	if mod, ok := modifiers[code]; ok {
		before := [2]uint32{c.depressed, c.locked}
		switch {
		case code == 58 && pressed:
			c.locked ^= mod
		case code == 58:
		case pressed:
			c.depressed |= mod
		default:
			c.depressed &^= mod
		}
		if before != [2]uint32{c.depressed, c.locked} {
			c.send(c.keyboard, 2, u32(c.depressed), u32(0), u32(c.locked), u32(0))
		}
	}
}

var modifiers = map[uint32]uint32{42: 1, 54: 1, 58: 2, 29: 4, 97: 4, 56: 8, 125: 64, 126: 64, 100: 128}

func (c *Client) Flush() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.roundtrip()
}
