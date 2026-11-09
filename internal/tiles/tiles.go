package tiles

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"image"
	"io"
)

const (
	Size    = 64
	magic   = 'T'
	version = 1
	full    = 1
)

var ErrBadFrame = errors.New("damaged video frame")

type Frame struct {
	W, H, Stride int
	Pix          []byte
}

type Encoder struct {
	prev  []byte
	w, h  int
	fresh bool
}

func (e *Encoder) Reset() { e.fresh = false }

func (e *Encoder) changed(f Frame, x0, y0, x1, y1 int) bool {
	for y := y0; y < y1; y++ {
		row := f.Pix[y*f.Stride+x0*4 : y*f.Stride+x1*4]
		old := e.prev[y*f.W*4+x0*4 : y*f.W*4+x1*4]
		if !bytes.Equal(row, old) {
			return true
		}
	}
	return false
}

func (e *Encoder) Encode(f Frame) ([]byte, int, error) {
	if f.W <= 0 || f.H <= 0 || f.W > 0xffff || f.H > 0xffff || f.Stride < f.W*4 || len(f.Pix) < f.Stride*(f.H-1)+f.W*4 {
		return nil, 0, ErrBadFrame
	}
	isFull := !e.fresh || e.w != f.W || e.h != f.H
	if isFull {
		e.prev = make([]byte, f.W*f.H*4)
		e.w, e.h, e.fresh = f.W, f.H, true
	}
	var coords []uint16
	for ty := 0; ty*Size < f.H; ty++ {
		for tx := 0; tx*Size < f.W; tx++ {
			x0, y0 := tx*Size, ty*Size
			x1, y1 := min(x0+Size, f.W), min(y0+Size, f.H)
			if isFull || e.changed(f, x0, y0, x1, y1) {
				coords = append(coords, uint16(tx), uint16(ty))
			}
		}
	}
	if len(coords) == 0 {
		return nil, 0, nil
	}
	var out bytes.Buffer
	head := []byte{magic, version, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	binary.BigEndian.PutUint16(head[2:], uint16(f.W))
	binary.BigEndian.PutUint16(head[4:], uint16(f.H))
	if isFull {
		head[6] = full
	}
	binary.BigEndian.PutUint32(head[7:], uint32(len(coords)/2))
	out.Write(head)
	for _, c := range coords {
		binary.Write(&out, binary.BigEndian, c)
	}
	zw, _ := flate.NewWriter(&out, flate.BestSpeed)
	for i := 0; i < len(coords); i += 2 {
		x0, y0 := int(coords[i])*Size, int(coords[i+1])*Size
		x1, y1 := min(x0+Size, f.W), min(y0+Size, f.H)
		for y := y0; y < y1; y++ {
			row := f.Pix[y*f.Stride+x0*4 : y*f.Stride+x1*4]
			zw.Write(row)
			copy(e.prev[y*f.W*4+x0*4:], row)
		}
	}
	if err := zw.Close(); err != nil {
		return nil, 0, err
	}
	return out.Bytes(), len(coords) / 2, nil
}

type Decoder struct {
	W, H int
	Pix  []byte
}

func (d *Decoder) Decode(msg []byte) ([]image.Rectangle, error) {
	if len(msg) < 11 || msg[0] != magic || msg[1] != version {
		return nil, ErrBadFrame
	}
	w, h := int(binary.BigEndian.Uint16(msg[2:])), int(binary.BigEndian.Uint16(msg[4:]))
	n := int(binary.BigEndian.Uint32(msg[7:]))
	if w == 0 || h == 0 || n == 0 || 11+n*4 > len(msg) {
		return nil, ErrBadFrame
	}
	tilesX, tilesY := (w+Size-1)/Size, (h+Size-1)/Size
	if n > tilesX*tilesY {
		return nil, ErrBadFrame
	}
	if msg[6]&full != 0 || d.W != w || d.H != h {
		if msg[6]&full == 0 {
			return nil, ErrBadFrame
		}
		d.W, d.H, d.Pix = w, h, make([]byte, w*h*4)
	}
	coords := msg[11 : 11+n*4]
	zr := flate.NewReader(bytes.NewReader(msg[11+n*4:]))
	defer zr.Close()
	rects := make([]image.Rectangle, 0, n)
	for i := 0; i < n; i++ {
		tx, ty := int(binary.BigEndian.Uint16(coords[i*4:])), int(binary.BigEndian.Uint16(coords[i*4+2:]))
		if tx >= tilesX || ty >= tilesY {
			return nil, ErrBadFrame
		}
		x0, y0 := tx*Size, ty*Size
		x1, y1 := min(x0+Size, w), min(y0+Size, h)
		for y := y0; y < y1; y++ {
			if _, err := io.ReadFull(zr, d.Pix[y*w*4+x0*4:y*w*4+x1*4]); err != nil {
				return nil, ErrBadFrame
			}
		}
		rects = append(rects, image.Rect(x0, y0, x1, y1))
	}
	return rects, nil
}
