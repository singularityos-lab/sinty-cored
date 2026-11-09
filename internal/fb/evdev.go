package fb

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"

	"github.com/singularityos-lab/sinty-cored/internal/input"
)

const (
	evSyn = 0
	evKey = 1
	evRel = 2
	evAbs = 3

	relX      = 0
	relY      = 1
	relHWheel = 6
	relWheel  = 8
	absX      = 0
	absY      = 1
	btnLeft   = 0x110
	btnTouch  = 0x14a
	btnMax    = 0x15f
)

type absRange struct{ min, max int32 }

type Pointer struct {
	X, Y    float64
	W, H    int
	absX    map[int]absRange
	absY    map[int]absRange
	pending []input.Event
	fit     Fit
	moved   bool
}

func NewPointer(w, h int) *Pointer {
	return &Pointer{X: float64(w) / 2, Y: float64(h) / 2, W: w, H: h, absX: map[int]absRange{}, absY: map[int]absRange{}}
}

func (p *Pointer) SetFit(f Fit) { p.fit = f }

func (p *Pointer) normalized() (uint16, uint16) {
	f := p.fit
	if f.W == 0 {
		f = Fit{0, 0, p.W, p.H}
	}
	nx := (p.X - float64(f.X)) / float64(max(f.W-1, 1))
	ny := (p.Y - float64(f.Y)) / float64(max(f.H-1, 1))
	nx, ny = min(max(nx, 0), 1), min(max(ny, 0), 1)
	return uint16(nx * 65535), uint16(ny * 65535)
}

func (p *Pointer) Feed(dev int, typ, code uint16, value int32) []input.Event {
	switch typ {
	case evRel:
		switch code {
		case relX:
			p.X = min(max(p.X+float64(value), 0), float64(p.W-1))
			p.moved = true
		case relY:
			p.Y = min(max(p.Y+float64(value), 0), float64(p.H-1))
			p.moved = true
		case relWheel:
			p.pending = append(p.pending, input.Event{Kind: input.Wheel, Code: 0, Value: -value})
		case relHWheel:
			p.pending = append(p.pending, input.Event{Kind: input.Wheel, Code: 1, Value: value})
		}
	case evAbs:
		if r, ok := p.absX[dev]; ok && code == absX && r.max > r.min {
			p.X = float64(value-r.min) * float64(p.W-1) / float64(r.max-r.min)
			p.moved = true
		}
		if r, ok := p.absY[dev]; ok && code == absY && r.max > r.min {
			p.Y = float64(value-r.min) * float64(p.H-1) / float64(r.max-r.min)
			p.moved = true
		}
	case evKey:
		if value == 2 {
			break
		}
		switch {
		case code == btnTouch:
			p.pending = append(p.pending, input.Event{Kind: input.Button, Code: btnLeft, Value: value})
		case code >= btnLeft && code <= btnMax:
			p.pending = append(p.pending, input.Event{Kind: input.Button, Code: uint32(code), Value: value})
		default:
			p.pending = append(p.pending, input.Event{Kind: input.Key, Code: uint32(code), Value: value})
		}
	case evSyn:
		var out []input.Event
		if p.moved {
			x, y := p.normalized()
			out = append(out, input.Event{Kind: input.Motion, X: x, Y: y})
			p.moved = false
		}
		out = append(out, p.pending...)
		p.pending = p.pending[:0]
		return out
	}
	return nil
}

type Device struct {
	f  *os.File
	id int
}

func ioc(dir, typ, nr, size uintptr) uintptr {
	return dir<<30 | size<<16 | typ<<8 | nr
}

func OpenInputs(dir string, p *Pointer) []*Device {
	paths, _ := filepath.Glob(filepath.Join(dir, "event*"))
	var out []*Device
	for i, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		var info [6]int32
		if ioctlPtr(f, ioc(2, 'E', 0x40+absX, 24), unsafe.Pointer(&info)) == nil && info[2] > info[1] {
			p.absX[i] = absRange{info[1], info[2]}
		}
		if ioctlPtr(f, ioc(2, 'E', 0x40+absY, 24), unsafe.Pointer(&info)) == nil && info[2] > info[1] {
			p.absY[i] = absRange{info[1], info[2]}
		}
		syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), ioc(1, 'E', 0x90, 4), 1)
		out = append(out, &Device{f, i})
	}
	return out
}

func ioctlPtr(f *os.File, req uintptr, arg unsafe.Pointer) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), req, uintptr(arg))
	if errno != 0 {
		return errno
	}
	return nil
}

func (d *Device) Run(feed func(dev int, typ, code uint16, value int32)) error {
	buf := make([]byte, 24*64)
	for {
		n, err := d.f.Read(buf)
		if err != nil {
			return err
		}
		for i := 0; i+24 <= n; i += 24 {
			ev := buf[i:]
			feed(d.id, binary.LittleEndian.Uint16(ev[16:]), binary.LittleEndian.Uint16(ev[18:]), int32(binary.LittleEndian.Uint32(ev[20:])))
		}
	}
}

func (d *Device) Close() error { return d.f.Close() }
