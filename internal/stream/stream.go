package stream

import (
	"encoding/binary"
	"sync"
	"time"

	"github.com/singularityos-lab/sinty-cored/internal/channel"
	"github.com/singularityos-lab/sinty-cored/internal/input"
	"github.com/singularityos-lab/sinty-cored/internal/tiles"
	"github.com/singularityos-lab/sinty-cored/internal/wl"
)

const (
	Video byte = iota + 1
	Input
	Notice
	Coded
	Shown
	Size
	Clip
	FileAsk
	FileData
)

const (
	FeatureShown = "shown"
	FeatureSplit = "split-tiles"
	FeatureInput = "input-only"
)

type Options struct {
	InFlight   int
	Parts      int
	InputOnly  bool
	OnApproach func(side uint32, position, progress float64)
	OnMessage  func(kind byte, data []byte)
}

func approaches(events []input.Event, on func(side uint32, position, progress float64)) []input.Event {
	kept := events[:0]
	for _, e := range events {
		if e.Kind != input.Approach {
			kept = append(kept, e)
			continue
		}
		if on != nil && e.Code <= 3 {
			on(e.Code, float64(e.X)/65535, min(max(float64(e.Value)/1000, 0), 1))
		}
	}
	return kept
}

type Sized interface {
	Size() (int, int)
}

func SizeMessage(w, h int) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint16(b, uint16(min(max(w, 0), 0xffff)))
	binary.BigEndian.PutUint16(b[2:], uint16(min(max(h, 0), 0xffff)))
	return b
}

func ParseSize(b []byte) (int, int, bool) {
	if len(b) != 4 {
		return 0, 0, false
	}
	w, h := int(binary.BigEndian.Uint16(b)), int(binary.BigEndian.Uint16(b[2:]))
	return w, h, w > 0 && h > 0
}

type Coder interface {
	Encode(pix []byte, stride int) ([]byte, error)
	Format() uint32
	Close() error
}

type Hardware func(w, h int) (Coder, error)

func CodedHeader(format uint32, w, h int) []byte {
	head := make([]byte, 8)
	binary.BigEndian.PutUint32(head[0:], format)
	binary.BigEndian.PutUint16(head[4:], uint16(w))
	binary.BigEndian.PutUint16(head[6:], uint16(h))
	return head
}

func ParseCoded(msg []byte) (format uint32, w, h int, data []byte, ok bool) {
	if len(msg) < 8 {
		return 0, 0, 0, nil, false
	}
	format = binary.BigEndian.Uint32(msg[0:])
	w, h = int(binary.BigEndian.Uint16(msg[4:])), int(binary.BigEndian.Uint16(msg[6:]))
	return format, w, h, msg[8:], w > 0 && h > 0
}

type Source interface {
	Capture(cursor, onDamage bool, into []byte) (*wl.Frame, error)
}

type Sink interface {
	MoveTo(x, y, w, h uint32)
	Button(code uint32, pressed bool)
	Scroll(vertical bool, steps int32)
	Key(code uint32, pressed bool)
	Flush() error
}

func Apply(sink Sink, events []input.Event) {
	for _, e := range events {
		switch e.Kind {
		case input.Motion:
			sink.MoveTo(uint32(e.X), uint32(e.Y), 65535, 65535)
		case input.Button:
			sink.Button(e.Code, e.Value != 0)
		case input.Wheel:
			sink.Scroll(e.Code == 0, e.Value)
		case input.Key:
			sink.Key(e.Code, e.Value != 0)
		}
	}
	sink.Flush()
}

func Serve(c *channel.Conn, src Source, sink Sink, hw Hardware, opts Options, stop <-chan struct{}) error {
	var credits chan struct{}
	refund := func() {
		if credits == nil {
			return
		}
		select {
		case credits <- struct{}{}:
		default:
		}
	}
	if opts.InFlight > 0 {
		credits = make(chan struct{}, opts.InFlight)
		for range opts.InFlight {
			credits <- struct{}{}
		}
	}
	var once sync.Once
	done := make(chan struct{})
	finish := func() { once.Do(func() { close(done) }) }
	var recvErr error
	go func() {
		defer finish()
		for {
			kind, data, err := c.Recv()
			if err != nil {
				recvErr = err
				return
			}
			if kind == Shown {
				refund()
				continue
			}
			if kind == Clip || kind == FileAsk || kind == FileData {
				if opts.OnMessage != nil {
					opts.OnMessage(kind, data)
				}
				continue
			}
			if kind != Input || sink == nil {
				continue
			}
			events, err := input.Decode(data)
			if err != nil {
				recvErr = err
				return
			}
			if events = approaches(events, opts.OnApproach); len(events) > 0 {
				Apply(sink, events)
			}
		}
	}()
	if opts.InputOnly {
		sized, _ := src.(Sized)
		w, h := 0, 0
		if sized != nil {
			w, h = sized.Size()
		}
		if err := c.Send(Size, SizeMessage(w, h)); err != nil {
			return err
		}
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				c.Send(Notice, []byte("stopped"))
				return nil
			case <-done:
				return recvErr
			case <-tick.C:
				if sized == nil {
					continue
				}
				if nw, nh := sized.Size(); nw != w || nh != h {
					w, h = nw, nh
					if err := c.Send(Size, SizeMessage(w, h)); err != nil {
						return err
					}
				}
			}
		}
	}
	enc := tiles.Encoder{Parts: opts.Parts}
	var coder Coder
	var cw, ch int
	defer func() {
		if coder != nil {
			coder.Close()
		}
	}()
	var buf []byte
	first := true
	for {
		select {
		case <-stop:
			c.Send(Notice, []byte("stopped"))
			return nil
		case <-done:
			return recvErr
		default:
		}
		if credits != nil {
			select {
			case <-credits:
			case <-stop:
				c.Send(Notice, []byte("stopped"))
				return nil
			case <-done:
				return recvErr
			}
		}
		f, err := src.Capture(true, !first, buf)
		if err != nil {
			return err
		}
		if f == nil {
			refund()
			continue
		}
		buf = f.Pix
		first = false
		if hw != nil && (coder == nil || cw != f.W || ch != f.H) {
			if coder != nil {
				coder.Close()
			}
			coder, cw, ch = nil, f.W, f.H
			if cd, err := hw(f.W, f.H); err == nil {
				coder = cd
			} else {
				hw = nil
			}
		}
		if coder != nil {
			data, err := coder.Encode(f.Pix, f.Stride)
			if err == nil {
				if len(data) == 0 {
					refund()
				} else if err := c.Send(Coded, append(CodedHeader(coder.Format(), f.W, f.H), data...)); err != nil {
					return err
				}
				continue
			}
			coder.Close()
			coder, hw = nil, nil
			enc.Reset()
		}
		msg, _, err := enc.Encode(tiles.Frame{W: f.W, H: f.H, Stride: f.Stride, Pix: f.Pix})
		if err != nil {
			return err
		}
		if msg == nil {
			refund()
			continue
		}
		if err := c.Send(Video, msg); err != nil {
			return err
		}
	}
}
