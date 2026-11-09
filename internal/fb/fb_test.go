package fb

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/singularityos-lab/sinty-cored/internal/input"
)

func TestDrawFitsAndLetterboxes(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "fb"))
	if err != nil {
		t.Fatal(err)
	}
	d, err := OpenFile(f, 400, 400)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	pix := make([]byte, 200*100*4)
	for i := range pix {
		pix[i] = 0x80
	}
	d.Draw(pix, 200, 100)
	if fit := d.FitFor(200, 100); fit != (Fit{0, 100, 400, 200}) {
		t.Fatalf("fit %+v", fit)
	}
	if d.Pixels()[50*d.Stride] != 0 || d.Pixels()[150*d.Stride] != 0x80 || d.Pixels()[350*d.Stride] != 0 {
		t.Fatal("letterbox not respected")
	}
	d.Text("CODE 123", 200, 200, 2, 0xffffffff)
}

func TestPointerFromAMouseAndATouchscreen(t *testing.T) {
	p := NewPointer(1000, 500)
	p.SetFit(Fit{0, 0, 1000, 500})
	p.Feed(0, evRel, relX, 10000)
	p.Feed(0, evRel, relY, -10000)
	p.Feed(0, evKey, btnLeft, 1)
	out := p.Feed(0, evSyn, 0, 0)
	if len(out) != 2 || out[0].Kind != input.Motion || out[0].X != 65535 || out[0].Y != 0 || out[1].Kind != input.Button {
		t.Fatalf("mouse: %+v", out)
	}
	p.absX[1], p.absY[1] = absRange{0, 4095}, absRange{0, 4095}
	p.Feed(1, evAbs, absX, 0)
	p.Feed(1, evAbs, absY, 4095)
	p.Feed(1, evKey, btnTouch, 1)
	p.Feed(1, evKey, 30, 2)
	out = p.Feed(1, evSyn, 0, 0)
	if len(out) != 2 || out[0].X != 0 || out[0].Y != 65535 || out[1].Code != btnLeft {
		t.Fatalf("touch: %+v", out)
	}
	p.Feed(0, evRel, relWheel, 1)
	if out = p.Feed(0, evSyn, 0, 0); len(out) != 1 || out[0].Kind != input.Wheel || out[0].Value != -1 {
		t.Fatalf("wheel: %+v", out)
	}
}
