package wl

import (
	"encoding/binary"
	"errors"
	"sync"
	"time"
)

var ErrNoShare = errors.New("the desktop does not hand its input to other devices")

type Side uint32

const (
	SideLeft Side = iota
	SideRight
	SideTop
	SideBottom
)

type EdgeKind int

const (
	EdgeEntered EdgeKind = iota + 1
	EdgeMotion
	EdgeButton
	EdgeAxis
	EdgeKey
	EdgeLeft
	EdgePressing
	ProbePushed
	WindowEntered
	WindowLeft
	WindowDropped
)

const (
	edgePressing uint16 = iota
	edgeEntered
	edgeMotion
	edgeButton
	edgeAxis
	edgeKey
	edgeLeft
	edgeWindowEntered
	edgeWindowLeft
	edgeWindowDropped
)

const (
	LeftReleased uint32 = iota
	LeftEscaped
	LeftCancelled
)

type EdgeEvent struct {
	Edge     uint32
	Kind     EdgeKind
	X, Y     float64
	Code     uint32
	Pressed  bool
	Discrete int32
	Progress float64
	Output   string
	AppID    string
	PID      int32
}

type Output struct {
	Name  string
	X, Y  int
	W, H  int
	Scale int
}

type Share struct {
	c       *Client
	mu      sync.Mutex
	manager uint32
	outputs []*Output

	qmu   sync.Mutex
	queue []EdgeEvent
	wake  chan struct{}
}

func OpenShare(socket string) (*Share, error) {
	c, err := dial(socket)
	if err != nil {
		return nil, err
	}
	s := &Share{c: c, wake: make(chan struct{}, 1)}
	c.guard = &s.mu
	s.manager = c.bind("zsingularity_input_share_manager_v1", 2)
	if s.manager == 0 {
		c.conn.Close()
		return nil, ErrNoShare
	}
	for _, g := range c.outputs {
		o := &Output{Scale: 1}
		s.outputs = append(s.outputs, o)
		id := c.newID(func(op uint16, args []byte) {
			switch {
			case op == 0 && len(args) >= 8:
				o.X = int(int32(binary.LittleEndian.Uint32(args)))
				o.Y = int(int32(binary.LittleEndian.Uint32(args[4:])))
			case op == 1 && len(args) >= 12 && binary.LittleEndian.Uint32(args)&1 != 0:
				o.W = int(int32(binary.LittleEndian.Uint32(args[4:])))
				o.H = int(int32(binary.LittleEndian.Uint32(args[8:])))
			case op == 3 && len(args) >= 4:
				o.Scale = max(int(int32(binary.LittleEndian.Uint32(args))), 1)
			case op == 4:
				o.Name = str(args)
			}
		})
		c.send(c.registry, 0, u32(g.name), sarg("wl_output"), u32(min(g.version, 4)), u32(id))
	}
	for range 2 {
		if err := c.roundtrip(); err != nil {
			c.conn.Close()
			return nil, err
		}
	}
	return s, nil
}

func (s *Share) Outputs() []Output {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Output, 0, len(s.outputs))
	for _, o := range s.outputs {
		out = append(out, *o)
	}
	return out
}

func (s *Share) Edge(output string, side Side, start, end int) uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var id uint32
	id = s.c.newID(func(op uint16, args []byte) {
		e := EdgeEvent{Edge: id}
		switch {
		case op == edgePressing && len(args) >= 12:
			e.Kind, e.X, e.Y, e.Progress = EdgePressing, fixedArg(args), fixedArg(args[4:]), fixedArg(args[8:])
		case op == edgeEntered && len(args) >= 8:
			e.Kind, e.X, e.Y = EdgeEntered, fixedArg(args), fixedArg(args[4:])
		case op == edgeMotion && len(args) >= 8:
			e.Kind, e.X, e.Y = EdgeMotion, fixedArg(args), fixedArg(args[4:])
		case op == edgeButton && len(args) >= 8:
			e.Kind, e.Code, e.Pressed = EdgeButton, binary.LittleEndian.Uint32(args), binary.LittleEndian.Uint32(args[4:]) != 0
		case op == edgeAxis && len(args) >= 12:
			e.Kind, e.Code, e.X = EdgeAxis, binary.LittleEndian.Uint32(args), fixedArg(args[4:])
			e.Discrete = int32(binary.LittleEndian.Uint32(args[8:]))
		case op == edgeKey && len(args) >= 8:
			e.Kind, e.Code, e.Pressed = EdgeKey, binary.LittleEndian.Uint32(args), binary.LittleEndian.Uint32(args[4:]) != 0
		case op == edgeLeft && len(args) >= 4:
			e.Kind, e.Code = EdgeLeft, binary.LittleEndian.Uint32(args)
		case op == edgeWindowEntered:
			app, rest := strArg(args)
			if len(rest) < 12 {
				return
			}
			e.Kind, e.AppID, e.PID = WindowEntered, app, int32(binary.LittleEndian.Uint32(rest))
			e.X, e.Y = fixedArg(rest[4:]), fixedArg(rest[8:])
		case op == edgeWindowLeft:
			e.Kind = WindowLeft
		case op == edgeWindowDropped:
			e.Kind = WindowDropped
		default:
			return
		}
		s.push(e)
	})
	s.c.send(s.manager, 1, u32(id), sarg(output), u32(uint32(side)), i32(int32(start)), i32(int32(end)))
	return id
}

func (s *Share) Release(edge uint32, position float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.c.send(edge, 1, fixed(position))
}

func (s *Share) SetResistance(edge uint32, distance uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.c.send(edge, 2, u32(distance))
}

func (s *Share) Drop(edge uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.c.send(edge, 0)
	delete(s.c.handlers, edge)
}

func (s *Share) Probe() uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var id uint32
	id = s.c.newID(func(op uint16, args []byte) {
		if op != 0 {
			return
		}
		name, rest := strArg(args)
		if len(rest) < 12 {
			return
		}
		s.push(EdgeEvent{Edge: id, Kind: ProbePushed, Output: name, Code: binary.LittleEndian.Uint32(rest),
			X: fixedArg(rest[4:]), Y: fixedArg(rest[8:])})
	})
	s.c.send(s.manager, 2, u32(id))
	return id
}

func (s *Share) push(e EdgeEvent) {
	s.qmu.Lock()
	s.queue = append(s.queue, e)
	s.qmu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Share) Events(stop <-chan struct{}) []EdgeEvent {
	for {
		s.qmu.Lock()
		if len(s.queue) > 0 {
			out := s.queue
			s.queue = nil
			s.qmu.Unlock()
			return out
		}
		s.qmu.Unlock()
		select {
		case <-s.wake:
		case <-stop:
			return nil
		}
	}
}

func (s *Share) Run() error {
	for {
		if err := s.c.dispatch(time.Time{}); err != nil {
			return err
		}
	}
}

func (s *Share) Close() error { return s.c.conn.Close() }
