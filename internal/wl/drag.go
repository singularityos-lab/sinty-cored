package wl

import (
	"errors"
	"io"
	"os"
	"time"

	"github.com/singularityos-lab/sinty-cored/internal/clip"
)

var ErrNoDrag = errors.New("the desktop cannot carry drags to other devices")

const syncTimeout = 2 * time.Second

func (s *Share) CanDrag() bool { return s.version >= 4 }

func (s *Share) Sync() error {
	done := make(chan struct{})
	s.mu.Lock()
	s.c.send(1, 0, u32(s.c.newID(func(uint16, []byte) { close(done) })))
	s.mu.Unlock()
	select {
	case <-done:
		return nil
	case <-time.After(syncTimeout):
		return errors.New("the compositor did not answer")
	}
}

type DragOffer struct {
	s     *Share
	id    uint32
	Mimes []string
}

func (s *Share) CurrentDrag() (*DragOffer, error) {
	if !s.CanDrag() {
		return nil, ErrNoDrag
	}
	o := &DragOffer{s: s}
	done := make(chan struct{})
	s.mu.Lock()
	o.id = s.c.newID(func(op uint16, args []byte) {
		switch op {
		case 0:
			o.Mimes = append(o.Mimes, str(args))
		case 1:
			close(done)
		}
	})
	s.c.send(s.manager, 4, u32(o.id))
	s.mu.Unlock()
	select {
	case <-done:
	case <-time.After(syncTimeout):
		o.Close()
		return nil, errors.New("the compositor did not describe the drag")
	}
	if len(o.Mimes) == 0 {
		o.Close()
		return nil, nil
	}
	return o, nil
}

func (o *DragOffer) Read() []clip.Item {
	var items []clip.Item
	total := 0
	for _, w := range clip.Choose(o.Mimes) {
		data := o.receive(w.Read)
		if data == nil || total+len(data) > clip.MaxTotal {
			continue
		}
		total += len(data)
		items = append(items, clip.Item{Mime: w.As, Data: data})
	}
	return items
}

func (o *DragOffer) receive(mime string) []byte {
	r, w, err := os.Pipe()
	if err != nil {
		return nil
	}
	defer r.Close()
	o.s.mu.Lock()
	o.s.c.sendFD(o.id, 1, int(w.Fd()), sarg(mime))
	o.s.mu.Unlock()
	w.Close()
	r.SetReadDeadline(time.Now().Add(readTimeout))
	data, err := io.ReadAll(io.LimitReader(r, clip.MaxItem+1))
	if err != nil || len(data) > clip.MaxItem {
		return nil
	}
	return data
}

func (o *DragOffer) Finish(dropped bool) {
	outcome := uint32(0)
	if dropped {
		outcome = 1
	}
	o.s.mu.Lock()
	o.s.c.send(o.id, 2, u32(outcome))
	o.s.mu.Unlock()
	o.s.Sync()
}

func (o *DragOffer) Close() {
	o.s.mu.Lock()
	defer o.s.mu.Unlock()
	if _, ok := o.s.c.handlers[o.id]; !ok {
		return
	}
	o.s.c.send(o.id, 0)
	delete(o.s.c.handlers, o.id)
}

type Drag struct {
	s       *Share
	id      uint32
	outcome chan bool
}

func (s *Share) StartDrag(items []clip.Item, provide func(mime string, data []byte) []byte) (*Drag, error) {
	if !s.CanDrag() {
		return nil, ErrNoDrag
	}
	d := &Drag{s: s, outcome: make(chan bool, 1)}
	dropped := false
	ended := false
	end := func(ok bool) {
		if ended {
			return
		}
		ended = true
		d.outcome <- ok
	}
	s.mu.Lock()
	d.id = s.c.newID(func(op uint16, args []byte) {
		switch op {
		case 0:
			fd := s.c.takeFD()
			if fd < 0 {
				return
			}
			f := os.NewFile(uintptr(fd), "drag")
			mime := str(args)
			data, _ := clip.Find(items, mime)
			go func() {
				if provide != nil {
					data = provide(mime, data)
				}
				f.Write(data)
				f.Close()
			}()
		case 1:
			dropped = true
		case 2:
			end(dropped)
		case 3:
			end(false)
		}
	})
	s.c.send(s.manager, 5, u32(d.id))
	for _, m := range clip.Offers(items) {
		s.c.send(d.id, 1, sarg(m))
	}
	s.c.send(d.id, 2)
	s.mu.Unlock()
	return d, s.Sync()
}

func (d *Drag) Outcome() <-chan bool { return d.outcome }

func (d *Drag) Cancel() {
	d.s.mu.Lock()
	if _, ok := d.s.c.handlers[d.id]; ok {
		d.s.c.send(d.id, 3)
	}
	d.s.mu.Unlock()
	d.s.Sync()
}

func (d *Drag) Close() {
	d.s.mu.Lock()
	defer d.s.mu.Unlock()
	if _, ok := d.s.c.handlers[d.id]; !ok {
		return
	}
	d.s.c.send(d.id, 0)
	delete(d.s.c.handlers, d.id)
}
