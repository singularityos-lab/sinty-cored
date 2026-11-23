package share

import (
	"math"

	"github.com/singularityos-lab/sinty-cored/internal/input"
	"github.com/singularityos-lab/sinty-cored/internal/wl"
)

const wheelStep = 15

type Pointer struct {
	Side   wl.Side
	W, H   float64
	Length float64

	x, y    float64
	active  bool
	keys    map[uint32]bool
	buttons map[uint32]bool
	smooth  [2]float64
}

func (p *Pointer) Active() bool { return p.active }

func (p *Pointer) Resize(w, h float64) {
	if w == p.W && h == p.H {
		return
	}
	if p.W > 1 && p.H > 1 {
		p.x = p.x / (p.W - 1) * (w - 1)
		p.y = p.y / (p.H - 1) * (h - 1)
	}
	p.W, p.H = w, h
}

func (p *Pointer) Enter(fraction float64) []input.Event {
	fraction = math.Min(math.Max(fraction, 0), 1)
	switch p.Side {
	case wl.SideRight:
		p.x, p.y = 0, fraction*(p.H-1)
	case wl.SideLeft:
		p.x, p.y = p.W-1, fraction*(p.H-1)
	case wl.SideTop:
		p.x, p.y = fraction*(p.W-1), p.H-1
	case wl.SideBottom:
		p.x, p.y = fraction*(p.W-1), 0
	}
	p.active = true
	p.keys, p.buttons = map[uint32]bool{}, map[uint32]bool{}
	p.smooth = [2]float64{}
	return []input.Event{p.motion()}
}

func (p *Pointer) Move(dx, dy float64) (events []input.Event, back bool, position float64) {
	if !p.active {
		return nil, false, 0
	}
	p.x += dx
	p.y += dy
	switch p.Side {
	case wl.SideRight:
		back = p.x < 0
	case wl.SideLeft:
		back = p.x > p.W-1
	case wl.SideTop:
		back = p.y > p.H-1
	case wl.SideBottom:
		back = p.y < 0
	}
	p.x = math.Min(math.Max(p.x, 0), p.W-1)
	p.y = math.Min(math.Max(p.y, 0), p.H-1)
	if back {
		along := p.y / math.Max(p.H-1, 1)
		if p.Side == wl.SideTop || p.Side == wl.SideBottom {
			along = p.x / math.Max(p.W-1, 1)
		}
		return p.Leave(), true, along * p.Length
	}
	return []input.Event{p.motion()}, false, 0
}

func (p *Pointer) Button(code uint32, pressed bool) []input.Event {
	if !p.active || p.buttons[code] == pressed {
		return nil
	}
	p.buttons[code] = pressed
	return []input.Event{{Kind: input.Button, Code: code, Value: flag(pressed)}}
}

func (p *Pointer) Key(code uint32, pressed bool) []input.Event {
	if !p.active || (!pressed && !p.keys[code]) {
		return nil
	}
	p.keys[code] = pressed
	return []input.Event{{Kind: input.Key, Code: code, Value: flag(pressed)}}
}

func (p *Pointer) Axis(axis uint32, value float64, discrete int32) []input.Event {
	if !p.active || axis > 1 {
		return nil
	}
	steps := discrete
	if steps == 0 {
		p.smooth[axis] += value / wheelStep
		steps = int32(p.smooth[axis])
		p.smooth[axis] -= float64(steps)
	}
	if steps == 0 {
		return nil
	}
	return []input.Event{{Kind: input.Wheel, Code: axis, Value: steps}}
}

func (p *Pointer) Leave() []input.Event {
	if !p.active {
		return nil
	}
	p.active = false
	var out []input.Event
	for code, down := range p.keys {
		if down {
			out = append(out, input.Event{Kind: input.Key, Code: code})
		}
	}
	for code, down := range p.buttons {
		if down {
			out = append(out, input.Event{Kind: input.Button, Code: code})
		}
	}
	return out
}

func (p *Pointer) motion() input.Event {
	return input.Event{Kind: input.Motion, X: scale(p.x, p.W), Y: scale(p.y, p.H)}
}

func scale(v, size float64) uint16 {
	if size <= 1 {
		return 0
	}
	return uint16(math.Round(v / (size - 1) * 65535))
}

func flag(on bool) int32 {
	if on {
		return 1
	}
	return 0
}
