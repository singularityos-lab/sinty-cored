package input

import (
	"reflect"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	in := []Event{{Motion, 0, 0, 32768, 100}, {Button, 0x110, 1, 0, 0}, {Wheel, 0, -1, 0, 0}, {Key, 30, 1, 0, 0}}
	out, err := Decode(Encode(in))
	if err != nil || !reflect.DeepEqual(in, out) {
		t.Fatalf("%v %v", out, err)
	}
	if _, err := Decode([]byte{9, 0, 0}); err == nil {
		t.Fatal("short event accepted")
	}
	if _, err := Decode(make([]byte, 16)); err == nil {
		t.Fatal("unknown kind accepted")
	}
}
