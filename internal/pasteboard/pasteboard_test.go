package pasteboard

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/singularityos-lab/sinty-cored/internal/carry"
	"github.com/singularityos-lab/sinty-cored/internal/clip"
)

type board struct {
	mu      sync.Mutex
	items   []clip.Item
	provide func(mime string, data []byte) []byte
}

func (b *board) Read() ([]clip.Item, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.items, nil
}

func (b *board) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.items)
}

func (b *board) SetWith(items []clip.Item, provide func(mime string, data []byte) []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.items, b.provide = items, provide
}

func (b *board) paste(mime string) string {
	b.mu.Lock()
	data, _ := clip.Find(b.items, mime)
	provide := b.provide
	b.mu.Unlock()
	if provide != nil {
		data = provide(mime, data)
	}
	return string(data)
}

func linked(t *testing.T) (*Link, *board, *Link, *board) {
	t.Helper()
	ab, bb := &board{}, &board{}
	var a, b *Link
	a = New(ab, func(kind byte, data []byte) error { go b.Receive(kind, data); return nil }, filepath.Join(t.TempDir(), "a"), nil)
	b = New(bb, func(kind byte, data []byte) error { go a.Receive(kind, data); return nil }, filepath.Join(t.TempDir(), "b"), nil)
	return a, ab, b, bb
}

func TestTextCrosses(t *testing.T) {
	a, ab, _, bb := linked(t)
	ab.items = []clip.Item{{Mime: clip.Text, Data: []byte("hello")}}
	if err := a.ShareCurrent(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return bb.count() == 1 })
	if got := bb.paste("UTF8_STRING"); got != "hello" {
		t.Fatalf("pasted %q", got)
	}
}

func TestCopiedFileIsFetchedOnPaste(t *testing.T) {
	a, ab, _, bb := linked(t)
	src := filepath.Join(t.TempDir(), "notes.txt")
	os.WriteFile(src, []byte("carried"), 0o644)
	uri := carry.FileURI(src)
	ab.items = []clip.Item{{Mime: clip.GnomeCopied, Data: []byte("copy\n" + uri)}, {Mime: clip.URIList, Data: []byte(uri + "\r\n")}}
	if err := a.ShareCurrent(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return bb.count() == 2 })
	list := bb.paste(clip.URIList)
	local, ok := carry.LocalPath(strings.TrimSpace(list))
	if !ok || local == src {
		t.Fatalf("pasted %q", list)
	}
	if data, err := os.ReadFile(local); err != nil || string(data) != "carried" {
		t.Fatalf("fetched %q %v", data, err)
	}
	if copied := bb.paste(clip.GnomeCopied); copied != "copy\n"+carry.FileURI(local) {
		t.Fatalf("pasted %q", copied)
	}
}

func TestOnlyOfferedFilesAreServed(t *testing.T) {
	_, _, b, _ := linked(t)
	secret := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(secret, []byte("no"), 0o600)
	if _, err := b.Fetch(carry.FileURI(secret)); err == nil {
		t.Fatal("a file that was never offered crossed")
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for range 200 {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out")
}
