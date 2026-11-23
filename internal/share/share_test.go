package share

import (
	"testing"

	"github.com/singularityos-lab/sinty-cored/internal/input"
	"github.com/singularityos-lab/sinty-cored/internal/wl"
)

func TestCrossingRightEntersOnTheLeftOfTheOtherDesktop(t *testing.T) {
	p := Pointer{Side: wl.SideRight, W: 1921, H: 1081, Length: 720}
	ev := p.Enter(0.5)
	if len(ev) != 1 || ev[0].Kind != input.Motion || ev[0].X != 0 || ev[0].Y != 32768 {
		t.Fatalf("enter: %+v", ev)
	}
	ev, back, _ := p.Move(960, 0)
	if back || ev[0].X != 32768 {
		t.Fatalf("move: %+v back %v", ev, back)
	}
	ev, back, _ = p.Move(5000, -5000)
	if back || ev[0].X != 65535 || ev[0].Y != 0 {
		t.Fatalf("clamped: %+v back %v", ev, back)
	}
}

func TestComingBackReleasesHeldInputAndKeepsThePlaceAlongTheEdge(t *testing.T) {
	p := Pointer{Side: wl.SideRight, W: 1001, H: 501, Length: 1000}
	p.Enter(0)
	p.Move(0, 125)
	p.Key(42, true)
	p.Button(272, true)
	if p.Key(30, false) != nil {
		t.Fatal("a release for a key pressed before crossing was sent")
	}
	ev, back, pos := p.Move(-1, 0)
	if !back || pos != 250 || p.Active() {
		t.Fatalf("back %v pos %v active %v", back, pos, p.Active())
	}
	if len(ev) != 2 {
		t.Fatalf("held input not released: %+v", ev)
	}
	for _, e := range ev {
		if e.Value != 0 {
			t.Fatalf("not a release: %+v", e)
		}
	}
}

func TestEverySideComesBackTheWayItCame(t *testing.T) {
	cases := []struct {
		side   wl.Side
		dx, dy float64
	}{{wl.SideRight, -1, 0}, {wl.SideLeft, 1, 0}, {wl.SideTop, 0, 1}, {wl.SideBottom, 0, -1}}
	for _, c := range cases {
		p := Pointer{Side: c.side, W: 100, H: 100, Length: 100}
		p.Enter(0.5)
		if _, back, _ := p.Move(-c.dx, -c.dy); back {
			t.Fatalf("side %d came back when moving away", c.side)
		}
		p.Move(c.dx, c.dy)
		if _, back, _ := p.Move(c.dx*2, c.dy*2); !back {
			t.Fatalf("side %d never came back", c.side)
		}
	}
}

func TestSmoothScrollingBecomesWheelSteps(t *testing.T) {
	p := Pointer{Side: wl.SideRight, W: 10, H: 10, Length: 10}
	p.Enter(0)
	if ev := p.Axis(0, 10, 0); ev != nil {
		t.Fatalf("partial step sent: %+v", ev)
	}
	ev := p.Axis(0, 10, 0)
	if len(ev) != 1 || ev[0].Kind != input.Wheel || ev[0].Code != 0 || ev[0].Value != 1 {
		t.Fatalf("step: %+v", ev)
	}
	if ev := p.Axis(1, 0, -2); ev[0].Value != -2 || ev[0].Code != 1 {
		t.Fatalf("wheel clicks: %+v", ev)
	}
}
