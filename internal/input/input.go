package input

import (
	"encoding/binary"
	"errors"
)

const (
	Motion uint8 = iota + 1
	Button
	Wheel
	Key
)

const size = 16

var ErrBadEvent = errors.New("damaged input event")

type Event struct {
	Kind  uint8
	Code  uint32
	Value int32
	X, Y  uint16
}

func Encode(events []Event) []byte {
	out := make([]byte, len(events)*size)
	for i, e := range events {
		b := out[i*size:]
		b[0] = e.Kind
		binary.BigEndian.PutUint32(b[4:], e.Code)
		binary.BigEndian.PutUint32(b[8:], uint32(e.Value))
		binary.BigEndian.PutUint16(b[12:], e.X)
		binary.BigEndian.PutUint16(b[14:], e.Y)
	}
	return out
}

func Decode(data []byte) ([]Event, error) {
	if len(data)%size != 0 {
		return nil, ErrBadEvent
	}
	out := make([]Event, 0, len(data)/size)
	for i := 0; i < len(data); i += size {
		b := data[i:]
		e := Event{b[0], binary.BigEndian.Uint32(b[4:]), int32(binary.BigEndian.Uint32(b[8:])), binary.BigEndian.Uint16(b[12:]), binary.BigEndian.Uint16(b[14:])}
		if e.Kind < Motion || e.Kind > Key {
			return nil, ErrBadEvent
		}
		out = append(out, e)
	}
	return out, nil
}
