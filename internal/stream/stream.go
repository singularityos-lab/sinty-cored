package stream

import (
	"encoding/binary"
	"sync"

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
)

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

func Serve(c *channel.Conn, src Source, sink Sink, hw Hardware, stop <-chan struct{}) error {
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
			if kind != Input || sink == nil {
				continue
			}
			events, err := input.Decode(data)
			if err != nil {
				recvErr = err
				return
			}
			Apply(sink, events)
		}
	}()
	var enc tiles.Encoder
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
		f, err := src.Capture(true, !first, buf)
		if err != nil {
			return err
		}
		if f == nil {
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
				if len(data) > 0 {
					if err := c.Send(Coded, append(CodedHeader(coder.Format(), f.W, f.H), data...)); err != nil {
						return err
					}
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
			continue
		}
		if err := c.Send(Video, msg); err != nil {
			return err
		}
	}
}
