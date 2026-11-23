package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/singularityos-lab/sinty-cored/internal/wl"
)

func TestDraggedWindowsAreReportedForTheirPlace(t *testing.T) {
	var out bytes.Buffer
	p := &placed{id: "studio", output: "eDP-1", side: wl.SideRight, start: 0, end: 900, edge: 7}
	e := &edges{out: json.NewEncoder(&out), devices: map[string]*placed{"studio": p}, byEdge: map[uint32]*placed{7: p}}
	e.handle([]wl.EdgeEvent{
		{Edge: 7, Kind: wl.WindowEntered, AppID: "dev.sinty.edit", PID: 42, X: 450, Y: 0.5},
		{Edge: 9, Kind: wl.WindowDropped},
		{Edge: 7, Kind: wl.WindowLeft},
		{Edge: 7, Kind: wl.WindowEntered, AppID: "dev.sinty.edit", PID: 42, X: 300, Y: 0.25},
		{Edge: 7, Kind: wl.WindowDropped},
	})
	var got []map[string]any
	dec := json.NewDecoder(&out)
	for dec.More() {
		var v map[string]any
		if err := dec.Decode(&v); err != nil {
			t.Fatal(err)
		}
		got = append(got, v)
	}
	want := []map[string]any{
		{"event": "window", "id": "studio", "state": "entered", "app_id": "dev.sinty.edit", "pid": 42.0, "output": "eDP-1", "side": "right", "fraction": 0.5},
		{"event": "window", "id": "studio", "state": "left"},
		{"event": "window", "id": "studio", "state": "entered", "app_id": "dev.sinty.edit", "pid": 42.0, "output": "eDP-1", "side": "right", "fraction": 0.25},
		{"event": "window", "id": "studio", "state": "dropped"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		a, _ := json.Marshal(got[i])
		b, _ := json.Marshal(want[i])
		if !bytes.Equal(a, b) {
			t.Fatalf("event %d: got %s, want %s", i, a, b)
		}
	}
}
