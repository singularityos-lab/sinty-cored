package wl

import (
	"encoding/binary"
	"errors"
	"io"
	"os"
	"sync"
	"time"

	"github.com/singularityos-lab/sinty-cored/internal/clip"
)

var ErrNoClipboard = errors.New("the desktop does not share its clipboard")

const readTimeout = 3 * time.Second

type Clipboard struct {
	c       *Client
	mu      sync.Mutex
	manager uint32
	device  uint32
	offers  map[uint32][]string
	current uint32
	primary uint32
	source  uint32
	changed chan struct{}
	closed  chan struct{}
}

func OpenClipboard(socket string) (*Clipboard, error) {
	c, err := dial(socket)
	if err != nil {
		return nil, err
	}
	iface := "ext_data_control_manager_v1"
	g, ok := c.globals[iface]
	if !ok {
		iface = "zwlr_data_control_manager_v1"
		g, ok = c.globals[iface]
	}
	if _, seat := c.globals["wl_seat"]; !ok || !seat {
		c.conn.Close()
		return nil, ErrNoClipboard
	}
	k := &Clipboard{c: c, offers: map[uint32][]string{}, changed: make(chan struct{}, 1), closed: make(chan struct{})}
	k.manager = c.bindGlobal(g, iface, 1)
	seat := c.bind("wl_seat", 1)
	k.device = c.newID(k.onDevice)
	c.send(k.manager, 1, u32(k.device), u32(seat))
	c.guard = &k.mu
	if err := c.roundtrip(); err != nil {
		c.conn.Close()
		return nil, err
	}
	select {
	case <-k.changed:
	default:
	}
	go k.run()
	return k, nil
}

func (k *Clipboard) run() {
	defer close(k.closed)
	for {
		if err := k.c.dispatch(time.Time{}); err != nil {
			return
		}
	}
}

func (k *Clipboard) Changed() <-chan struct{} { return k.changed }

func (k *Clipboard) Done() <-chan struct{} { return k.closed }

func (k *Clipboard) Close() error { return k.c.conn.Close() }

func (k *Clipboard) onDevice(op uint16, args []byte) {
	if len(args) < 4 && op != 2 {
		return
	}
	switch op {
	case 0:
		id := binary.LittleEndian.Uint32(args)
		k.offers[id] = nil
		k.c.handlers[id] = func(op uint16, a []byte) {
			if op == 0 {
				k.offers[id] = append(k.offers[id], str(a))
			}
		}
	case 1:
		id := binary.LittleEndian.Uint32(args)
		if k.current != id {
			k.drop(k.current)
		}
		k.current = id
		select {
		case k.changed <- struct{}{}:
		default:
		}
	case 3:
		id := binary.LittleEndian.Uint32(args)
		if k.primary != id {
			k.drop(k.primary)
		}
		k.primary = id
	}
}

func (k *Clipboard) drop(offer uint32) {
	if offer == 0 {
		return
	}
	k.c.send(offer, 1)
	delete(k.c.handlers, offer)
	delete(k.offers, offer)
}

func (k *Clipboard) Read() ([]clip.Item, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.current == 0 {
		return nil, nil
	}
	var items []clip.Item
	total := 0
	for _, w := range clip.Choose(k.offers[k.current]) {
		data, err := k.receive(k.current, w.Read)
		if err != nil {
			return nil, err
		}
		if data == nil || total+len(data) > clip.MaxTotal {
			continue
		}
		total += len(data)
		items = append(items, clip.Item{Mime: w.As, Data: data})
	}
	return items, k.c.err
}

func (k *Clipboard) receive(offer uint32, mime string) ([]byte, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer r.Close()
	k.c.sendFD(offer, 0, int(w.Fd()), sarg(mime))
	w.Close()
	r.SetReadDeadline(time.Now().Add(readTimeout))
	data, err := io.ReadAll(io.LimitReader(r, clip.MaxItem+1))
	if err != nil || len(data) > clip.MaxItem {
		return nil, nil
	}
	return data, nil
}

func (k *Clipboard) Set(items []clip.Item) {
	if len(items) == 0 {
		return
	}
	k.set(items, clip.Offers(items), nil)
}

func (k *Clipboard) SetWith(items []clip.Item, provide func(mime string, data []byte) []byte) {
	if len(items) == 0 {
		return
	}
	k.set(items, clip.Offers(items), provide)
}

func (k *Clipboard) set(items []clip.Item, offers []string, provide func(mime string, data []byte) []byte) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.source != 0 {
		k.c.send(k.source, 1)
		delete(k.c.handlers, k.source)
	}
	var src uint32
	src = k.c.newID(func(op uint16, a []byte) {
		switch op {
		case 0:
			fd := k.c.takeFD()
			if fd < 0 {
				return
			}
			f := os.NewFile(uintptr(fd), "clipboard")
			mime := str(a)
			data, _ := clip.Find(items, mime)
			go func() {
				if provide != nil {
					data = provide(mime, data)
				}
				f.Write(data)
				f.Close()
			}()
		case 1:
			k.c.send(src, 1)
			delete(k.c.handlers, src)
			if k.source == src {
				k.source = 0
			}
		}
	})
	k.source = src
	k.c.send(k.manager, 0, u32(src))
	for _, m := range offers {
		k.c.send(src, 0, sarg(m))
	}
	k.c.send(k.device, 0, u32(src))
}
