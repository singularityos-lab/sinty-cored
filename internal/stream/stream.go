package stream

import (
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
)

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

func Serve(c *channel.Conn, src Source, sink Sink, stop <-chan struct{}) error {
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
