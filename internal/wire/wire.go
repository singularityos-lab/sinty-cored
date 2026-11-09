package wire

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
)

const MaxMessage = 32 << 20

var ErrTooLarge = errors.New("message too large")

func Write(w io.Writer, kind byte, payload []byte) error {
	if len(payload)+1 > MaxMessage {
		return ErrTooLarge
	}
	buf := make([]byte, 5+len(payload))
	binary.BigEndian.PutUint32(buf, uint32(len(payload)+1))
	buf[4] = kind
	copy(buf[5:], payload)
	_, err := w.Write(buf)
	return err
}

func Read(r io.Reader) (byte, []byte, error) {
	var head [4]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(head[:])
	if n == 0 || n > MaxMessage {
		return 0, nil, ErrTooLarge
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, nil, err
	}
	return buf[0], buf[1:], nil
}

func WriteJSON(w io.Writer, kind byte, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return Write(w, kind, data)
}

func ReadJSON(r io.Reader, want byte, v any) error {
	kind, data, err := Read(r)
	if err != nil {
		return err
	}
	if kind != want {
		return errors.New("unexpected message")
	}
	return json.Unmarshal(data, v)
}
