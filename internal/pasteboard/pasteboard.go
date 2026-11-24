package pasteboard

import (
	"log"
	"os"
	"path/filepath"
	"sync"

	"github.com/singularityos-lab/sinty-cored/internal/carry"
	"github.com/singularityos-lab/sinty-cored/internal/clip"
	"github.com/singularityos-lab/sinty-cored/internal/stream"
)

type Board interface {
	Read() ([]clip.Item, error)
	SetWith(items []clip.Item, provide func(mime string, data []byte) []byte)
}

type Link struct {
	board  Board
	send   func(kind byte, data []byte) error
	server carry.Server
	fetch  carry.Fetcher

	mu       sync.Mutex
	received []string

	OnDrag      func(items []clip.Item, provide func(mime string, data []byte) []byte)
	OnDragDone  func(dropped bool)
	OnDragLeave func()
	OnDragBack  func(items []clip.Item, provide func(mime string, data []byte) []byte)
}

func New(board Board, send func(kind byte, data []byte) error, dir string, owner func(path string) error) *Link {
	l := &Link{board: board, send: send}
	l.fetch = carry.Fetcher{
		Dir:   dir,
		Owner: owner,
		Send:  func(ask []byte) error { return send(stream.FileAsk, ask) },
	}
	return l
}

func (l *Link) Offer(items []clip.Item) error {
	if len(items) == 0 {
		return nil
	}
	l.server.Allow(clip.FileURIs(items))
	return l.send(stream.Clip, clip.Encode(items))
}

func (l *Link) ShareCurrent() error {
	items, err := l.board.Read()
	if err != nil {
		return err
	}
	return l.Offer(items)
}

func (l *Link) Receive(kind byte, data []byte) {
	switch kind {
	case stream.Clip:
		items, err := clip.Decode(data)
		if err != nil {
			return
		}
		l.apply(items)
	case stream.FileAsk:
		l.server.Serve(data, func(part []byte) error { return l.send(stream.FileData, part) })
	case stream.FileData:
		l.fetch.Deliver(data)
	case stream.Drag, stream.DragBack:
		items, err := clip.Decode(data)
		if err != nil {
			return
		}
		on := l.OnDrag
		if kind == stream.DragBack {
			on = l.OnDragBack
		}
		if on != nil {
			on(items, l.provider(items))
		}
	case stream.DragDone:
		if l.OnDragDone != nil && len(data) == 1 {
			l.OnDragDone(data[0] == 1)
		}
	case stream.DragLeave:
		if l.OnDragLeave != nil {
			l.OnDragLeave()
		}
	}
}

func (l *Link) OfferDrag(items []clip.Item) error {
	l.server.Allow(clip.FileURIs(items))
	return l.send(stream.Drag, clip.Encode(items))
}

func (l *Link) GiveBack(items []clip.Item) error {
	l.server.Allow(clip.FileURIs(items))
	return l.send(stream.DragBack, clip.Encode(items))
}

func (l *Link) DragDone(dropped bool) error {
	outcome := byte(0)
	if dropped {
		outcome = 1
	}
	return l.send(stream.DragDone, []byte{outcome})
}

func (l *Link) LeaveDrag() error { return l.send(stream.DragLeave, nil) }

func (l *Link) apply(items []clip.Item) {
	l.mu.Lock()
	old := l.received
	l.received = nil
	l.mu.Unlock()
	for _, p := range old {
		os.RemoveAll(p)
	}
	l.board.SetWith(items, l.provider(items))
}

func (l *Link) provider(items []clip.Item) func(mime string, data []byte) []byte {
	uris := clip.FileURIs(items)
	if len(uris) == 0 {
		return nil
	}
	var once sync.Mutex
	mapping := map[string]string{}
	done := false
	return func(mime string, data []byte) []byte {
		if mime != clip.URIList && mime != clip.GnomeCopied {
			return data
		}
		once.Lock()
		defer once.Unlock()
		if !done {
			for _, uri := range uris {
				local, err := l.fetch.Fetch(uri)
				if err != nil {
					log.Printf("pasteboard: %s: %v", uri, err)
					continue
				}
				mapping[uri] = carry.FileURI(local)
				l.mu.Lock()
				l.received = append(l.received, filepath.Dir(local))
				l.mu.Unlock()
			}
			done = true
		}
		return clip.Rewrite(data, mapping)
	}
}

func (l *Link) Fetch(uri string) (string, error) { return l.fetch.Fetch(uri) }

func (l *Link) Allow(uris []string) { l.server.Allow(uris) }
