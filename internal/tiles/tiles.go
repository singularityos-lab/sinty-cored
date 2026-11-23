package tiles

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"image"
	"io"
	"sync"
)

const (
	Size    = 64
	magic   = 'T'
	version = 1
	split   = 2
	full    = 1

	partTiles = 24
	maxParts  = 32
)

var ErrBadFrame = errors.New("damaged video frame")

type Frame struct {
	W, H, Stride int
	Pix          []byte
}

type Encoder struct {
	Parts int

	prev    []byte
	w, h    int
	fresh   bool
	writers []*flate.Writer
	outs    []bytes.Buffer
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
	n := len(coords) / 2
	parts := min(e.Parts, n/partTiles, maxParts)
	head := []byte{magic, version, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	if parts > 1 {
		head[1] = split
	}
	binary.BigEndian.PutUint16(head[2:], uint16(f.W))
	binary.BigEndian.PutUint16(head[4:], uint16(f.H))
	if isFull {
		head[6] = full
	}
	binary.BigEndian.PutUint32(head[7:], uint32(n))
	var out bytes.Buffer
	out.Write(head)
	for _, c := range coords {
		binary.Write(&out, binary.BigEndian, c)
	}
	if parts <= 1 {
		if err := e.compress(&out, 0, f, coords); err != nil {
			return nil, 0, err
		}
		return out.Bytes(), n, nil
	}
	for len(e.outs) < parts {
		e.outs = append(e.outs, bytes.Buffer{})
	}
	errs := make([]error, parts)
	var wg sync.WaitGroup
	for p := 0; p < parts; p++ {
		e.outs[p].Reset()
		if len(e.writers) <= p {
			zw, _ := flate.NewWriter(nil, flate.BestSpeed)
			e.writers = append(e.writers, zw)
		}
	}
	for p := 0; p < parts; p++ {
		lo, hi := p*n/parts, (p+1)*n/parts
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[p] = e.compress(&e.outs[p], p, f, coords[lo*2:hi*2])
		}()
	}
	wg.Wait()
	out.WriteByte(byte(parts))
	for p := 0; p < parts; p++ {
		if errs[p] != nil {
			return nil, 0, errs[p]
		}
		binary.Write(&out, binary.BigEndian, uint32(e.outs[p].Len()))
	}
	for p := 0; p < parts; p++ {
		out.Write(e.outs[p].Bytes())
	}
	return out.Bytes(), n, nil
}

func (e *Encoder) compress(out *bytes.Buffer, slot int, f Frame, coords []uint16) error {
	for len(e.writers) <= slot {
		zw, _ := flate.NewWriter(nil, flate.BestSpeed)
		e.writers = append(e.writers, zw)
	}
	zw := e.writers[slot]
	zw.Reset(out)
	for i := 0; i < len(coords); i += 2 {
		x0, y0 := int(coords[i])*Size, int(coords[i+1])*Size
		x1, y1 := min(x0+Size, f.W), min(y0+Size, f.H)
		for y := y0; y < y1; y++ {
			row := f.Pix[y*f.Stride+x0*4 : y*f.Stride+x1*4]
			zw.Write(row)
			copy(e.prev[y*f.W*4+x0*4:], row)
		}
	}
	return zw.Close()
}

type Decoder struct {
	W, H int
	Pix  []byte
}

func (d *Decoder) Decode(msg []byte) ([]image.Rectangle, error) {
	if len(msg) < 11 || msg[0] != magic || (msg[1] != version && msg[1] != split) {
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
	coords := msg[11 : 11+n*4]
	rects := make([]image.Rectangle, n)
	for i := range n {
		tx, ty := int(binary.BigEndian.Uint16(coords[i*4:])), int(binary.BigEndian.Uint16(coords[i*4+2:]))
		if tx >= tilesX || ty >= tilesY {
			return nil, ErrBadFrame
		}
		x0, y0 := tx*Size, ty*Size
		rects[i] = image.Rect(x0, y0, min(x0+Size, w), min(y0+Size, h))
	}
	body := msg[11+n*4:]
	var streams [][]byte
	if msg[1] == split {
		if len(body) < 1 {
			return nil, ErrBadFrame
		}
		parts := int(body[0])
		if parts < 2 || parts > maxParts || parts > n || len(body) < 1+parts*4 {
			return nil, ErrBadFrame
		}
		data := body[1+parts*4:]
		for p := range parts {
			size := int(binary.BigEndian.Uint32(body[1+p*4:]))
			if size > len(data) {
				return nil, ErrBadFrame
			}
			streams = append(streams, data[:size])
			data = data[size:]
		}
	} else {
		streams = [][]byte{body}
	}
	if msg[6]&full != 0 || d.W != w || d.H != h {
		if msg[6]&full == 0 {
			return nil, ErrBadFrame
		}
		d.W, d.H, d.Pix = w, h, make([]byte, w*h*4)
	}
	parts := len(streams)
	errs := make([]error, parts)
	var wg sync.WaitGroup
	for p := range parts {
		lo, hi := p*n/parts, (p+1)*n/parts
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[p] = d.inflate(streams[p], rects[lo:hi])
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return rects, nil
}

func (d *Decoder) inflate(data []byte, rects []image.Rectangle) error {
	zr := flate.NewReader(bytes.NewReader(data))
	defer zr.Close()
	for _, r := range rects {
		for y := r.Min.Y; y < r.Max.Y; y++ {
			if _, err := io.ReadFull(zr, d.Pix[y*d.W*4+r.Min.X*4:y*d.W*4+r.Max.X*4]); err != nil {
				return ErrBadFrame
			}
		}
	}
	return nil
}
