package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/singularityos-lab/sinty-cored/internal/input"
	"github.com/singularityos-lab/sinty-cored/internal/share"
	"github.com/singularityos-lab/sinty-cored/internal/terminal"
	"github.com/singularityos-lab/sinty-cored/internal/vault"
	"github.com/singularityos-lab/sinty-cored/internal/wl"
)

const (
	resistDistance = 260
	resistAgain    = 10 * time.Minute
)

var sides = map[string]wl.Side{"left": wl.SideLeft, "right": wl.SideRight, "top": wl.SideTop, "bottom": wl.SideBottom}

var sideNames = map[wl.Side]string{wl.SideLeft: "left", wl.SideRight: "right", wl.SideTop: "top", wl.SideBottom: "bottom"}

var facing = map[wl.Side]wl.Side{wl.SideLeft: wl.SideRight, wl.SideRight: wl.SideLeft, wl.SideTop: wl.SideBottom, wl.SideBottom: wl.SideTop}

type command struct {
	Cmd    string `json:"cmd"`
	ID     string `json:"id"`
	Core   string `json:"core"`
	State  string `json:"state"`
	Output string `json:"output"`
	Side   string `json:"side"`
	Start  int    `json:"start"`
	End    int    `json:"end"`
	On     bool   `json:"on"`
}

type status struct {
	id   string
	gen  int
	sess *terminal.Session
	w, h int
	err  error
}

type placed struct {
	id, core, state  string
	output           string
	side             wl.Side
	start, end       int
	gen              int
	stop             chan struct{}
	edge             uint32
	sess             *terminal.Session
	w, h             int
	ptr              *share.Pointer
	armed            bool
	lastCross        time.Time
	pressing         bool
	term             *terminal.Terminal
	reportedProgress float64
}

type edges struct {
	sh      *wl.Share
	name    string
	layout  string
	out     *json.Encoder
	devices map[string]*placed
	byEdge  map[uint32]*placed
	active  *placed
	probe   uint32
	status  chan status
	gen     int
}

func serveEdges(name, layout string, in io.Reader, out io.Writer) {
	e := &edges{
		name:    name,
		layout:  layout,
		out:     json.NewEncoder(out),
		devices: map[string]*placed{},
		byEdge:  map[uint32]*placed{},
		status:  make(chan status, 16),
	}
	sh, err := wl.OpenShare(wl.SocketPath())
	if err != nil {
		e.emit(map[string]any{"event": "unavailable", "error": err.Error()})
		os.Exit(1)
	}
	e.sh = sh
	defer sh.Close()
	e.emitOutputs()

	commands := make(chan command)
	go func() {
		scan := bufio.NewScanner(in)
		scan.Buffer(make([]byte, 64<<10), 64<<10)
		for scan.Scan() {
			var c command
			if err := json.Unmarshal(scan.Bytes(), &c); err != nil {
				log.Printf("command ignored: %v", err)
				continue
			}
			commands <- c
		}
		close(commands)
	}()
	stop := make(chan struct{})
	batches := make(chan []wl.EdgeEvent)
	go func() {
		for {
			b := sh.Events(stop)
			if b == nil {
				return
			}
			batches <- b
		}
	}()
	died := make(chan error, 1)
	go func() { died <- sh.Run() }()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()

	for {
		select {
		case c, ok := <-commands:
			if !ok || c.Cmd == "quit" {
				e.shutdown()
				close(stop)
				return
			}
			e.command(c)
		case b := <-batches:
			e.handle(b)
		case st := <-e.status:
			e.connected(st)
		case <-tick.C:
			e.rearm(time.Now())
		case err := <-died:
			e.emit(map[string]any{"event": "unavailable", "error": err.Error()})
			e.shutdown()
			os.Exit(1)
		case <-sigs:
			e.shutdown()
			close(stop)
			return
		}
	}
}

func (e *edges) emit(v map[string]any) {
	if err := e.out.Encode(v); err != nil {
		log.Fatalf("events: %v", err)
	}
}

func (e *edges) emitOutputs() {
	list := []map[string]any{}
	for _, o := range e.sh.Outputs() {
		list = append(list, map[string]any{"name": o.Name, "x": o.X, "y": o.Y, "width": o.W / o.Scale, "height": o.H / o.Scale})
	}
	e.emit(map[string]any{"event": "outputs", "outputs": list})
}

func (e *edges) command(c command) {
	switch c.Cmd {
	case "place":
		side, ok := sides[c.Side]
		if c.ID == "" || c.State == "" || c.Output == "" || !ok || c.End <= c.Start {
			e.emit(map[string]any{"event": "error", "id": c.ID, "error": "a placement needs an id, a state, an output, a side and a stretch"})
			return
		}
		e.forget(c.ID)
		e.gen++
		p := &placed{id: c.ID, core: c.Core, state: c.State, output: c.Output, side: side, start: c.Start, end: c.End,
			gen: e.gen, stop: make(chan struct{})}
		store := vault.Store{Dir: p.state, TPM: vault.NewHelper(filepath.Join(p.state, "tpm"))}
		id, _, err := store.Load()
		if errors.Is(err, vault.ErrSealBroken) {
			id, _, err = store.Recover()
		}
		if err != nil {
			e.emit(map[string]any{"event": "error", "id": c.ID, "error": err.Error()})
			return
		}
		p.term = &terminal.Terminal{Identity: id, Name: e.name, Layout: e.layout, State: p.state, InputOnly: true}
		e.devices[p.id] = p
		go keepConnected(p.term, p.id, p.gen, p.core, p.stop, e.status)
	case "forget":
		e.forget(c.ID)
	case "probe":
		if c.On && e.probe == 0 {
			e.probe = e.sh.Probe()
		} else if !c.On && e.probe != 0 {
			e.sh.Drop(e.probe)
			e.probe = 0
		}
	case "outputs":
		e.emitOutputs()
	}
}

func (e *edges) forget(id string) {
	p := e.devices[id]
	if p == nil {
		return
	}
	delete(e.devices, id)
	close(p.stop)
	e.unbind(p)
	if p.sess != nil {
		p.sess.Close()
		p.sess = nil
	}
}

func (e *edges) unbind(p *placed) {
	if p.edge == 0 {
		return
	}
	if e.active == p {
		e.send(p, p.ptr.Leave())
		e.active = nil
	}
	e.sh.Drop(p.edge)
	delete(e.byEdge, p.edge)
	p.edge = 0
}

func (e *edges) connected(st status) {
	p := e.devices[st.id]
	if p == nil || p.gen != st.gen {
		if st.sess != nil {
			st.sess.Close()
		}
		return
	}
	switch {
	case st.err != nil:
		e.unbind(p)
		p.sess = nil
		e.emit(map[string]any{"event": "disconnected", "id": p.id, "error": st.err.Error()})
	case p.sess == st.sess && p.sess != nil:
		p.w, p.h = st.w, st.h
		if p.ptr != nil && p.ptr.Active() {
			p.ptr.Resize(float64(p.w), float64(p.h))
		}
	default:
		p.sess, p.w, p.h = st.sess, st.w, st.h
		p.ptr = &share.Pointer{Side: p.side, Length: float64(p.end - p.start)}
		p.edge = e.sh.Edge(p.output, p.side, p.start, p.end)
		e.byEdge[p.edge] = p
		e.sh.SetResistance(p.edge, resistDistance)
		p.armed = true
		e.emit(map[string]any{"event": "connected", "id": p.id, "width": p.w, "height": p.h})
	}
}

func (e *edges) rearm(now time.Time) {
	for _, p := range e.devices {
		if p.edge != 0 && !p.armed && e.active != p && now.Sub(p.lastCross) > resistAgain {
			e.sh.SetResistance(p.edge, resistDistance)
			p.armed = true
		}
	}
}

func (e *edges) send(p *placed, events []input.Event) {
	if p.sess == nil || len(events) == 0 {
		return
	}
	if err := p.sess.Send(events); err != nil {
		log.Printf("%s: %v", p.id, err)
	}
}

func (e *edges) approach(p *placed, fraction, progress float64) {
	if progress > 0 && progress < 1 && progress-p.reportedProgress < 0.02 && progress > p.reportedProgress {
		return
	}
	p.reportedProgress = progress
	e.send(p, []input.Event{{Kind: input.Approach, Code: uint32(facing[p.side]),
		Value: int32(progress * 1000), X: uint16(min(max(fraction, 0), 1) * 65535)}})
}

func (e *edges) handle(batch []wl.EdgeEvent) {
	for _, ev := range batch {
		if ev.Kind == wl.ProbePushed {
			if ev.Edge == e.probe {
				e.emit(map[string]any{"event": "pushed", "output": ev.Output, "side": sideNames[wl.Side(ev.Code)],
					"position": ev.X, "length": ev.Y})
			}
			continue
		}
		p := e.byEdge[ev.Edge]
		if p == nil {
			continue
		}
		switch ev.Kind {
		case wl.EdgePressing:
			p.pressing = ev.Progress > 0
			e.approach(p, ev.Y, ev.Progress)
		case wl.EdgeEntered:
			if p.sess == nil || p.w <= 0 || p.h <= 0 {
				e.sh.Release(ev.Edge, ev.X)
				continue
			}
			if p.armed || p.pressing {
				e.approach(p, ev.Y, 1)
			}
			p.armed, p.pressing = false, false
			p.lastCross = time.Now()
			p.ptr.W, p.ptr.H = float64(p.w), float64(p.h)
			e.active = p
			e.send(p, p.ptr.Enter(ev.Y))
			e.emit(map[string]any{"event": "entered", "id": p.id})
		case wl.EdgeMotion:
			events, back, position := p.ptr.Move(ev.X, ev.Y)
			e.send(p, events)
			if back {
				e.sh.Release(ev.Edge, position)
			}
		case wl.EdgeButton:
			e.send(p, p.ptr.Button(ev.Code, ev.Pressed))
		case wl.EdgeKey:
			e.send(p, p.ptr.Key(ev.Code, ev.Pressed))
		case wl.EdgeAxis:
			e.send(p, p.ptr.Axis(ev.Code, ev.X, ev.Discrete))
		case wl.EdgeLeft:
			e.send(p, p.ptr.Leave())
			p.lastCross = time.Now()
			if e.active == p {
				e.active = nil
			}
			e.emit(map[string]any{"event": "left", "id": p.id, "reason": leftReasons[ev.Code]})
		}
	}
}

var leftReasons = map[uint32]string{wl.LeftReleased: "released", wl.LeftEscaped: "escaped", wl.LeftCancelled: "cancelled"}

func (e *edges) shutdown() {
	for id := range e.devices {
		e.forget(id)
	}
	if e.probe != 0 {
		e.sh.Drop(e.probe)
		e.probe = 0
	}
}

func keepConnected(term *terminal.Terminal, id string, gen int, core string, stop <-chan struct{}, out chan<- status) {
	for {
		err := connectEdge(term, id, gen, core, stop, out)
		select {
		case <-stop:
			return
		default:
		}
		if err == nil {
			err = io.EOF
		}
		select {
		case out <- status{id: id, gen: gen, err: err}:
		case <-stop:
			return
		}
		select {
		case <-stop:
			return
		case <-time.After(3 * time.Second):
		}
	}
}

func connectEdge(term *terminal.Terminal, id string, gen int, core string, stop <-chan struct{}, out chan<- status) error {
	paired, err := term.Core()
	if err != nil {
		return err
	}
	addr, err := locate(core, paired.ID)
	if err != nil {
		return err
	}
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return err
	}
	if tc, ok := c.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}
	sess, err := term.Open(c)
	if err != nil {
		c.Close()
		return err
	}
	defer sess.Close()
	w, h, err := sess.Size()
	if err != nil {
		return err
	}
	select {
	case out <- status{id: id, gen: gen, sess: sess, w: w, h: h}:
	case <-stop:
		return nil
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-stop:
			sess.Close()
		case <-done:
		}
	}()
	defer close(done)
	return sess.Wait(func(w, h int) {
		select {
		case out <- status{id: id, gen: gen, sess: sess, w: w, h: h}:
		case <-stop:
		}
	})
}
