package tiles

import (
	"bytes"
	"math/rand/v2"
	"testing"
)

func frame(w, h, pad int, seed uint64) Frame {
	r := rand.New(rand.NewPCG(seed, 1))
	f := Frame{W: w, H: h, Stride: w*4 + pad, Pix: make([]byte, (w*4+pad)*h)}
	for i := range f.Pix {
		f.Pix[i] = byte(r.IntN(4))
	}
	return f
}

func same(t *testing.T, f Frame, d *Decoder) {
	t.Helper()
	for y := 0; y < f.H; y++ {
		if !bytes.Equal(f.Pix[y*f.Stride:y*f.Stride+f.W*4], d.Pix[y*f.W*4:(y+1)*f.W*4]) {
			t.Fatalf("row %d differs", y)
		}
	}
}

func TestOnlyChangedTilesTravel(t *testing.T) {
	var e Encoder
	var d Decoder
	f := frame(300, 130, 8, 1)
	msg, n, err := e.Encode(f)
	if err != nil || n != 5*3 {
		t.Fatalf("first frame: %d tiles %v", n, err)
	}
	if _, err := d.Decode(msg); err != nil {
		t.Fatal(err)
	}
	same(t, f, &d)
	if msg, n, _ := e.Encode(f); msg != nil || n != 0 {
		t.Fatal("an unchanged frame was sent")
	}
	f.Pix[70*f.Stride+290*4] ^= 0xff
	msg, n, err = e.Encode(f)
	if err != nil || n != 1 {
		t.Fatalf("one pixel: %d tiles %v", n, err)
	}
	rects, err := d.Decode(msg)
	if err != nil || len(rects) != 1 || rects[0].Min.X != 256 || rects[0].Max.X != 300 || rects[0].Min.Y != 64 {
		t.Fatalf("rects %v %v", rects, err)
	}
	same(t, f, &d)
}

func TestNewScreenSizeStartsOver(t *testing.T) {
	var e Encoder
	var d Decoder
	for _, f := range []Frame{frame(128, 64, 0, 2), frame(64, 128, 0, 3)} {
		msg, _, err := e.Encode(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.Decode(msg); err != nil {
			t.Fatal(err)
		}
		same(t, f, &d)
	}
}

func TestDamagedFramesAreRefused(t *testing.T) {
	var e Encoder
	msg, _, _ := e.Encode(frame(128, 128, 0, 4))
	for _, bad := range [][]byte{
		msg[:20],
		append([]byte{'T', 1, 0, 128, 0, 128, 1, 0, 0, 0, 9, 0, 9, 0, 9}, msg[15:]...),
		append([]byte{'X'}, msg[1:]...),
	} {
		var d Decoder
		if _, err := d.Decode(bad); err == nil {
			t.Fatal("damaged frame accepted")
		}
	}
	var d Decoder
	delta := append([]byte(nil), msg...)
	delta[6] = 0
	if _, err := d.Decode(delta); err == nil {
		t.Fatal("a partial frame was applied to an empty screen")
	}
}

func BenchmarkDesktopFrame(b *testing.B) {
	var e Encoder
	f := frame(1920, 1080, 0, 5)
	e.Encode(f)
	for i := 0; i < b.N; i++ {
		f.Pix[(i%1080)*f.Stride+(i%1920)*4]++
		e.Encode(f)
	}
}
