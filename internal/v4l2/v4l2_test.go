package v4l2

import (
	"math"
	"os"
	"testing"
)

func picture(w, h, shift int) []byte {
	pix := make([]byte, w*h*4)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			o := (y*w + x) * 4
			pix[o] = byte((x + shift) * 255 / w)
			pix[o+1] = byte(y * 255 / h)
			pix[o+2] = byte(128 + 100*math.Sin(float64(x+y+shift)/40))
			pix[o+3] = 0xff
		}
	}
	return pix
}

func psnr(a, b []byte, w, h int) float64 {
	var sum float64
	for i := 0; i < w*h; i++ {
		for c := 0; c < 3; c++ {
			d := float64(a[i*4+c]) - float64(b[i*4+c])
			sum += d * d
		}
	}
	mse := sum / float64(w*h*3)
	if mse == 0 {
		return 99
	}
	return 10 * math.Log10(255*255/mse)
}

func TestNV12RoundTrip(t *testing.T) {
	w, h := 64, 48
	src := make([]byte, w*h*4)
	for i := 0; i < w*h; i++ {
		x, y := i%w, i/w
		src[i*4], src[i*4+1], src[i*4+2], src[i*4+3] = byte(x*4), byte(y*5), byte(128+x-y), 0xff
	}
	y := make([]byte, w*h)
	uv := make([]byte, w*h/2)
	ToNV12(y, w, uv, w, src, w*4, w, h)
	out := make([]byte, w*h*4)
	FromNV12(out, y, w, uv, w, w, h)
	if q := psnr(src, out, w, h); q < 35 {
		t.Fatalf("colour conversion lost too much: %.1f dB", q)
	}
}

func TestFourcc(t *testing.T) {
	if Name(Fourcc("H264")) != "H264" || Fourcc("XR24") != 0x34325258 {
		t.Fatal("fourcc packing")
	}
}

func devices(t *testing.T) (Codec, Codec) {
	dir := os.Getenv("SINTY_V4L2_DIR")
	if dir == "" {
		dir = "/dev"
	}
	encs, decs := Find(dir, true), Find(dir, false)
	for _, e := range encs {
		for _, d := range decs {
			if e.Format == d.Format {
				return e, d
			}
		}
	}
	t.Skip("no matching hardware encoder and decoder")
	return Codec{}, Codec{}
}

func roundTrip(t *testing.T, enc *Encoder, dc Codec, w, h int) {
	dec, err := NewDecoder(dc, w, h)
	if err != nil {
		t.Fatalf("decoder: %v", err)
	}
	defer dec.Close()
	decoded := 0
	for i := 0; i < 5; i++ {
		src := picture(w, h, i*7)
		coded, err := enc.Encode(src, w*4)
		if err != nil {
			t.Fatalf("encode %d: %v", i, err)
		}
		if len(coded) == 0 {
			continue
		}
		if len(coded) >= w*h*4 {
			t.Fatalf("frame %d was not compressed: %d bytes", i, len(coded))
		}
		out, err := dec.Decode(coded)
		if err != nil {
			t.Fatalf("decode %d: %v", i, err)
		}
		if out == nil {
			continue
		}
		decoded++
		if q := psnr(src, out, w, h); q < 25 {
			t.Fatalf("frame %d decoded at %.1f dB", i, q)
		}
		t.Logf("frame %d: %d bytes coded, %.1f dB", i, len(coded), psnr(src, out, w, h))
	}
	if decoded < 3 {
		t.Fatalf("only %d of 5 frames came back", decoded)
	}
}

func TestHardwareRoundTrip(t *testing.T) {
	ec, dc := devices(t)
	t.Logf("encoder %s (%s) %s, decoder %s, multi-planar %v", ec.Path, ec.Card, Name(ec.Format), dc.Path, ec.mplane)
	w, h := 640, 360
	enc, err := NewEncoder(ec, w, h)
	if err != nil {
		t.Fatalf("encoder: %v", err)
	}
	defer enc.Close()
	roundTrip(t, enc, dc, w, h)
}

func TestHardwareRoundTripNV12(t *testing.T) {
	ec, dc := devices(t)
	w, h := 320, 240
	enc, err := newEncoder(ec, w, h, NV12)
	if err != nil {
		t.Skipf("encoder takes no NV12: %v", err)
	}
	defer enc.Close()
	roundTrip(t, enc, dc, w, h)
}
