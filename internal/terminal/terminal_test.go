package terminal

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/singularityos-lab/sinty-cored/internal/channel"
	"github.com/singularityos-lab/sinty-cored/internal/input"
	"github.com/singularityos-lab/sinty-cored/internal/pair"
	"github.com/singularityos-lab/sinty-cored/internal/stream"
	"github.com/singularityos-lab/sinty-cored/internal/v4l2"
	"github.com/singularityos-lab/sinty-cored/internal/wl"
)

func identity(t *testing.T) pair.Identity {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pair.Identity{Priv: priv}
}

type screen struct {
	mu    sync.Mutex
	texts []string
	last  []byte
	draws int
}

func (s *screen) Clear() {}
func (s *screen) Text(t string, _, _, _ int, _ uint32) {
	s.mu.Lock()
	s.texts = append(s.texts, t)
	s.mu.Unlock()
}
func (s *screen) Draw(pix []byte, w, h int) {
	s.mu.Lock()
	s.last = append([]byte(nil), pix...)
	s.draws++
	s.mu.Unlock()
}

type source struct {
	frames []*wl.Frame
	i      int
}

func (s *source) Capture(cursor, onDamage bool, into []byte) (*wl.Frame, error) {
	if s.i >= len(s.frames) {
		time.Sleep(10 * time.Millisecond)
		return nil, nil
	}
	f := s.frames[s.i]
	s.i++
	return f, nil
}

type sink struct {
	mu     sync.Mutex
	events []string
}

func (k *sink) add(s string) { k.mu.Lock(); k.events = append(k.events, s); k.mu.Unlock() }
func (k *sink) MoveTo(x, y, w, h uint32) {
	if x == 65535 && y == 0 {
		k.add("move-top-right")
	}
}
func (k *sink) Button(code uint32, p bool)    { k.add("button") }
func (k *sink) Scroll(v bool, steps int32)    {}
func (k *sink) Key(code uint32, pressed bool) { k.add("key") }
func (k *sink) Flush() error                  { return nil }

func TestPairThenStreamAndControl(t *testing.T) {
	core := identity(t)
	disp := &screen{}
	term := &Terminal{Identity: identity(t), Name: "Kitchen", Layout: "us", State: t.TempDir(), Display: disp, W: 800, H: 480}

	a, b := net.Pipe()
	go term.Pair(a)
	var code string
	_, err := pair.Core(b, core, "My Core", func(name string) (string, error) {
		for {
			disp.mu.Lock()
			for _, s := range disp.texts {
				if len(s) == 7 && s[3] == ' ' {
					code = s[:3] + s[4:]
				}
			}
			disp.mu.Unlock()
			if code != "" {
				return code, nil
			}
			time.Sleep(5 * time.Millisecond)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if p, err := term.Core(); err != nil || p.Name != "My Core" {
		t.Fatalf("paired core: %+v %v", p, err)
	}

	f1 := &wl.Frame{W: 100, H: 70, Stride: 400, Pix: bytes.Repeat([]byte{1, 2, 3, 255}, 100*70)}
	f2 := &wl.Frame{W: 100, H: 70, Stride: 400, Pix: append([]byte(nil), f1.Pix...)}
	f2.Pix[69*400+99*4] = 9
	src := &source{frames: []*wl.Frame{f1, f2}}
	k := &sink{}
	c, d := net.Pipe()
	stop := make(chan struct{})
	served := make(chan error, 1)
	go func() {
		cc, _, err := channel.Core(d, core, func(id string) (string, bool) { return "Kitchen", id == term.Identity.ID() })
		if err != nil {
			served <- err
			return
		}
		served <- stream.Serve(cc, src, k, nil, stream.Options{}, stop)
	}()
	sess, err := term.Open(c)
	if err != nil {
		t.Fatal(err)
	}
	shown := make(chan error, 1)
	go func() { shown <- term.Show(sess) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		disp.mu.Lock()
		ok := disp.draws >= 2 && bytes.Equal(disp.last, f2.Pix)
		disp.mu.Unlock()
		if ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("frames did not arrive intact")
		}
		time.Sleep(10 * time.Millisecond)
	}
	sess.Send([]input.Event{{Kind: input.Motion, X: 65535, Y: 0}, {Kind: input.Button, Code: 0x110, Value: 1}, {Kind: input.Key, Code: 30, Value: 1}})
	for time.Now().Before(deadline) {
		k.mu.Lock()
		n := len(k.events)
		k.mu.Unlock()
		if n == 3 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(k.events) != 3 || k.events[0] != "move-top-right" {
		t.Fatalf("input on the Core: %v", k.events)
	}
	close(stop)
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	<-shown

	e, f := net.Pipe()
	go channel.Core(f, core, func(string) (string, bool) { return "", false })
	if _, err := term.Open(e); !errors.Is(err, channel.ErrUntrusted) {
		t.Fatalf("revoked screen: %v", err)
	}
}

const testFormat = 0x54534554

type fakeCoder struct {
	mu     sync.Mutex
	calls  int
	failAt int
	w, h   int
}

func (c *fakeCoder) Encode(pix []byte, stride int) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.calls == c.failAt {
		return nil, errors.New("codec gone")
	}
	out := make([]byte, 0, c.w*c.h*4)
	for y := 0; y < c.h; y++ {
		out = append(out, pix[y*stride:y*stride+c.w*4]...)
	}
	return out, nil
}
func (c *fakeCoder) Format() uint32 { return testFormat }
func (c *fakeCoder) Close() error   { return nil }

type fakeDecoder struct{ decoded *int }

func (d fakeDecoder) Decode(data []byte) ([]byte, error) {
	*d.decoded++
	return data, nil
}
func (d fakeDecoder) Close() error { return nil }

func codecSession(t *testing.T, failAt int) (*fakeCoder, int, *screen) {
	core := identity(t)
	disp := &screen{}
	decoded := 0
	term := &Terminal{Identity: identity(t), Name: "TV", Layout: "us", State: t.TempDir(), Display: disp, W: 800, H: 480,
		Codecs: []string{"TEST"},
		Decoders: func(format uint32, w, h int) (Decoder, error) {
			if format != testFormat {
				return nil, errors.New("unknown format")
			}
			return fakeDecoder{&decoded}, nil
		}}
	data, _ := json.Marshal(paired{core.ID(), "Core", core.Public()})
	os.WriteFile(term.corePath(), data, 0o600)
	f1 := &wl.Frame{W: 64, H: 32, Stride: 300, Pix: bytes.Repeat([]byte{5, 6, 7, 255}, 300*32/4)}
	f2 := &wl.Frame{W: 64, H: 32, Stride: 300, Pix: append([]byte(nil), f1.Pix...)}
	f2.Pix[31*300+63*4] = 77
	src := &source{frames: []*wl.Frame{f1, f2}}
	coder := &fakeCoder{failAt: failAt, w: 64, h: 32}
	c, d := net.Pipe()
	stop := make(chan struct{})
	served := make(chan error, 1)
	go func() {
		cc, req, err := channel.Core(d, core, func(id string) (string, bool) { return "TV", true })
		if err != nil {
			served <- err
			return
		}
		var hw stream.Hardware
		if slices.Contains(req.Codecs, "TEST") {
			hw = func(w, h int) (stream.Coder, error) { return coder, nil }
		}
		served <- stream.Serve(cc, src, &sink{}, hw, stream.Options{}, stop)
	}()
	sess, err := term.Open(c)
	if err != nil {
		t.Fatal(err)
	}
	go term.Show(sess)
	want := make([]byte, 0, 64*32*4)
	for y := 0; y < 32; y++ {
		want = append(want, f2.Pix[y*300:y*300+64*4]...)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		disp.mu.Lock()
		ok := bytes.Equal(disp.last, want)
		disp.mu.Unlock()
		if ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the second frame never reached the screen")
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(stop)
	<-served
	sess.Close()
	return coder, decoded, disp
}

func TestHardwareCodecPath(t *testing.T) {
	coder, decoded, _ := codecSession(t, 0)
	if coder.calls != 2 || decoded != 2 {
		t.Fatalf("encoded %d decoded %d", coder.calls, decoded)
	}
}

func TestHardwareCodecFallsBackToTiles(t *testing.T) {
	coder, decoded, _ := codecSession(t, 2)
	if coder.calls != 2 || decoded != 1 {
		t.Fatalf("encoded %d decoded %d", coder.calls, decoded)
	}
}

func TestSessionThroughVideoCodec(t *testing.T) {
	dir := os.Getenv("SINTY_V4L2_DIR")
	if dir == "" {
		dir = "/dev"
	}
	encs, decs := v4l2.Find(dir, true), v4l2.Find(dir, false)
	if len(encs) == 0 || len(decs) == 0 {
		t.Skip("no video codec devices")
	}
	core := identity(t)
	disp := &screen{}
	var names []string
	for _, d := range decs {
		names = append(names, v4l2.Name(d.Format))
	}
	term := &Terminal{Identity: identity(t), Name: "TV", Layout: "us", State: t.TempDir(), Display: disp, W: 1280, H: 720, Codecs: names,
		Decoders: func(format uint32, w, h int) (Decoder, error) {
			for _, d := range decs {
				if d.Format == format {
					return v4l2.NewDecoder(d, w, h)
				}
			}
			return nil, v4l2.ErrNoDevice
		}}
	data, _ := json.Marshal(paired{core.ID(), "Core", core.Public()})
	os.WriteFile(term.corePath(), data, 0o600)
	w, h := 640, 360
	var frames []*wl.Frame
	for i := 0; i < 4; i++ {
		pix := make([]byte, w*h*4)
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				o := (y*w + x) * 4
				pix[o], pix[o+1], pix[o+2], pix[o+3] = byte(x*255/w), byte(y*255/h), byte(64+i*40), 255
			}
		}
		frames = append(frames, &wl.Frame{W: w, H: h, Stride: w * 4, Pix: pix})
	}
	src := &source{frames: frames}
	c, d := net.Pipe()
	stop := make(chan struct{})
	served := make(chan error, 1)
	coded := 0
	go func() {
		cc, req, err := channel.Core(d, core, func(string) (string, bool) { return "TV", true })
		if err != nil {
			served <- err
			return
		}
		var hw stream.Hardware
		for _, e := range encs {
			if slices.Contains(req.Codecs, v4l2.Name(e.Format)) {
				e := e
				hw = func(w, h int) (stream.Coder, error) { coded++; return v4l2.NewEncoder(e, w, h) }
				break
			}
		}
		served <- stream.Serve(cc, src, &sink{}, hw, stream.Options{}, stop)
	}()
	sess, err := term.Open(c)
	if err != nil {
		t.Fatal(err)
	}
	go term.Show(sess)
	last := frames[3].Pix
	deadline := time.Now().Add(10 * time.Second)
	for {
		disp.mu.Lock()
		got := disp.last
		draws := disp.draws
		disp.mu.Unlock()
		if draws >= 3 && len(got) == len(last) {
			var diff int
			for i := 0; i < len(got); i += 4 {
				for k := 0; k < 3; k++ {
					v := int(got[i+k]) - int(last[i+k])
					if v < 0 {
						v = -v
					}
					diff += v
				}
			}
			if avg := float64(diff) / float64(w*h*3); avg < 6 {
				t.Logf("%d frames drawn through %v, mean error %.2f", draws, names, avg)
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the last frame never arrived through the codec (draws %d)", draws)
		}
		time.Sleep(20 * time.Millisecond)
	}
	close(stop)
	<-served
	sess.Close()
	if coded != 1 {
		t.Fatalf("hardware encoder opened %d times", coded)
	}
}

func TestCoreWaitsUntilFramesAreShown(t *testing.T) {
	core, me := identity(t), identity(t)
	var frames []*wl.Frame
	for i := range 4 {
		frames = append(frames, &wl.Frame{W: 64, H: 32, Stride: 256, Pix: bytes.Repeat([]byte{byte(i), 6, 7, 255}, 64*32)})
	}
	src := &source{frames: frames}
	c, d := net.Pipe()
	stop := make(chan struct{})
	served := make(chan error, 1)
	go func() {
		cc, _, err := channel.Core(d, core, func(id string) (string, bool) { return "TV", true })
		if err != nil {
			served <- err
			return
		}
		served <- stream.Serve(cc, src, &sink{}, nil, stream.Options{InFlight: 1}, stop)
	}()
	sc, err := channel.Screen(c, me, core.Public(), "us", []string{stream.FeatureShown})
	if err != nil {
		t.Fatal(err)
	}
	frame := func() {
		t.Helper()
		if kind, _, err := sc.Recv(); err != nil || kind != stream.Video {
			t.Fatalf("frame: kind %d %v", kind, err)
		}
	}
	frame()
	got := make(chan struct{})
	go func() {
		sc.Recv()
		close(got)
	}()
	select {
	case <-got:
		t.Fatal("a second frame was sent before the first was shown")
	case <-time.After(200 * time.Millisecond):
	}
	if err := sc.Send(stream.Shown, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("the next frame never came after the first was shown")
	}
	close(stop)
	c.Close()
	<-served
}

type sizedSource struct {
	source
	mu   sync.Mutex
	w, h int
}

func (s *sizedSource) Size() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w, s.h
}

func (s *sizedSource) resize(w, h int) {
	s.mu.Lock()
	s.w, s.h = w, h
	s.mu.Unlock()
}

func TestInputOnlySessionSendsSizeAndApproaches(t *testing.T) {
	core, me := identity(t), identity(t)
	src := &sizedSource{w: 1920, h: 1080}
	k := &sink{}
	approached := make(chan [3]float64, 4)
	c, d := net.Pipe()
	stop := make(chan struct{})
	served := make(chan error, 1)
	go func() {
		cc, req, err := channel.Core(d, core, func(id string) (string, bool) { return "Laptop", true })
		if err != nil {
			served <- err
			return
		}
		opts := stream.Options{InputOnly: slices.Contains(req.Codecs, stream.FeatureInput)}
		opts.OnApproach = func(side uint32, position, progress float64) {
			approached <- [3]float64{float64(side), position, progress}
		}
		served <- stream.Serve(cc, src, k, nil, opts, stop)
	}()
	term := &Terminal{Identity: me, State: t.TempDir(), InputOnly: true}
	if err := term.Adopt(pair.Peer{ID: core.ID(), Name: "Desk", Public: core.Public()}); err != nil {
		t.Fatal(err)
	}
	sess, err := term.Open(c)
	if err != nil {
		t.Fatal(err)
	}
	w, h, err := sess.Size()
	if err != nil || w != 1920 || h != 1080 {
		t.Fatalf("size %dx%d %v", w, h, err)
	}
	if err := sess.Send([]input.Event{{Kind: input.Approach, Code: 0, Value: 500, X: 65535}, {Kind: input.Motion, X: 65535, Y: 0}}); err != nil {
		t.Fatal(err)
	}
	select {
	case a := <-approached:
		if a != [3]float64{0, 1, 0.5} {
			t.Fatalf("approach %v", a)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the approach never reached the Core")
	}
	resized := make(chan [2]int, 1)
	go sess.Wait(func(w, h int) { resized <- [2]int{w, h} }, nil)
	src.resize(1280, 720)
	select {
	case r := <-resized:
		if r != [2]int{1280, 720} {
			t.Fatalf("resized to %v", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the new size never reached the screen")
	}
	k.mu.Lock()
	moved := slices.Contains(k.events, "move-top-right")
	k.mu.Unlock()
	if !moved {
		t.Fatalf("the motion after the approach was lost: %v", k.events)
	}
	close(stop)
	c.Close()
	<-served
}

func TestInputOnlySessionCarriesTheClipboard(t *testing.T) {
	core, me := identity(t), identity(t)
	c, d := net.Pipe()
	stop := make(chan struct{})
	served := make(chan error, 1)
	atCore := make(chan []byte, 1)
	coreConn := make(chan *channel.Conn, 1)
	go func() {
		cc, _, err := channel.Core(d, core, func(id string) (string, bool) { return "Laptop", true })
		if err != nil {
			served <- err
			return
		}
		coreConn <- cc
		opts := stream.Options{InputOnly: true, OnMessage: func(kind byte, data []byte) { atCore <- data }}
		served <- stream.Serve(cc, &sizedSource{w: 800, h: 600}, &sink{}, nil, opts, stop)
	}()
	term := &Terminal{Identity: me, State: t.TempDir(), InputOnly: true}
	if err := term.Adopt(pair.Peer{ID: core.ID(), Name: "Desk", Public: core.Public()}); err != nil {
		t.Fatal(err)
	}
	sess, err := term.Open(c)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := sess.Size(); err != nil {
		t.Fatal(err)
	}
	atScreen := make(chan []byte, 1)
	go sess.Wait(nil, func(kind byte, data []byte) { atScreen <- data })
	if err := sess.SendMessage(stream.Clip, []byte("from the laptop")); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-atCore:
		if string(got) != "from the laptop" {
			t.Fatalf("core got %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the clipboard never reached the Core")
	}
	if err := (<-coreConn).Send(stream.Clip, []byte("from the core")); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-atScreen:
		if string(got) != "from the core" {
			t.Fatalf("screen got %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the clipboard never came back")
	}
	close(stop)
	c.Close()
	<-served
}
