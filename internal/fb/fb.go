package fb

import (
	"encoding/binary"
	"errors"
	"os"
	"syscall"
	"unsafe"
)

var ErrDepth = errors.New("the display is not 32 bits per pixel")

type Display struct {
	f      *os.File
	mem    []byte
	W, H   int
	Stride int
}

type varInfo struct {
	xres, yres, xresVirtual, yresVirtual, xoffset, yoffset, bitsPerPixel uint32
	rest                                                                 [33]uint32
}

type fixInfo struct {
	id         [16]byte
	smemStart  uint64
	smemLen    uint32
	kind       uint32
	typeAux    uint32
	visual     uint32
	xpanstep   uint16
	ypanstep   uint16
	ywrapstep  uint16
	_          uint16
	lineLength uint32
	mmioStart  uint64
	mmioLen    uint32
	accel      uint32
	caps       uint16
	reserved   [2]uint16
}

func Open(path string) (*Display, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	var v varInfo
	var x fixInfo
	if err := ioctl(f, 0x4600, unsafe.Pointer(&v)); err != nil {
		return OpenFile(f, 0, 0)
	}
	if err := ioctl(f, 0x4602, unsafe.Pointer(&x)); err != nil {
		f.Close()
		return nil, err
	}
	if v.bitsPerPixel != 32 {
		f.Close()
		return nil, ErrDepth
	}
	mem, err := syscall.Mmap(int(f.Fd()), 0, int(x.smemLen), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		f.Close()
		return nil, err
	}
	return &Display{f, mem, int(v.xres), int(v.yres), int(x.lineLength)}, nil
}

func OpenFile(f *os.File, w, h int) (*Display, error) {
	if w == 0 || h == 0 {
		f.Close()
		return nil, errors.New("not a framebuffer")
	}
	if err := f.Truncate(int64(w * h * 4)); err != nil {
		f.Close()
		return nil, err
	}
	mem, err := syscall.Mmap(int(f.Fd()), 0, w*h*4, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		f.Close()
		return nil, err
	}
	return &Display{f, mem, w, h, w * 4}, nil
}

func ioctl(f *os.File, req uintptr, arg unsafe.Pointer) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), req, uintptr(arg))
	if errno != 0 {
		return errno
	}
	return nil
}

func (d *Display) Close() error {
	syscall.Munmap(d.mem)
	return d.f.Close()
}

type Fit struct {
	X, Y, W, H int
}

func (d *Display) FitFor(w, h int) Fit {
	if w <= 0 || h <= 0 {
		return Fit{}
	}
	fw, fh := d.W, d.W*h/w
	if fh > d.H {
		fw, fh = d.H*w/h, d.H
	}
	return Fit{(d.W - fw) / 2, (d.H - fh) / 2, fw, fh}
}

func (d *Display) Clear() {
	for i := range d.mem[:d.Stride*d.H] {
		d.mem[i] = 0
	}
}

func (d *Display) Draw(pix []byte, w, h int) {
	fit := d.FitFor(w, h)
	if fit.W == 0 {
		return
	}
	if fit.W == w && fit.H == h {
		for y := 0; y < h; y++ {
			copy(d.mem[(fit.Y+y)*d.Stride+fit.X*4:], pix[y*w*4:(y+1)*w*4])
		}
		return
	}
	for y := 0; y < fit.H; y++ {
		sy := y * h / fit.H
		row := d.mem[(fit.Y+y)*d.Stride+fit.X*4:]
		for x := 0; x < fit.W; x++ {
			sx := x * w / fit.W
			copy(row[x*4:x*4+4], pix[(sy*w+sx)*4:])
		}
	}
}

func (d *Display) Fill(x0, y0, w, h int, c uint32) {
	for y := max(y0, 0); y < min(y0+h, d.H); y++ {
		for x := max(x0, 0); x < min(x0+w, d.W); x++ {
			binary.LittleEndian.PutUint32(d.mem[y*d.Stride+x*4:], c)
		}
	}
}

func (d *Display) Text(s string, cx, cy, scale int, c uint32) {
	width := len(s) * 6 * scale
	x := cx - width/2
	y := cy - 7*scale/2
	for _, r := range s {
		g := glyph(r)
		for col := 0; col < 5; col++ {
			for row := 0; row < 7; row++ {
				if g[col]>>row&1 == 1 {
					d.Fill(x+col*scale, y+row*scale, scale, scale, c)
				}
			}
		}
		x += 6 * scale
	}
}

func (d *Display) Pixels() []byte { return d.mem[:d.Stride*d.H] }
