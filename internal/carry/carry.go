package carry

import (
	"archive/tar"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	chunk    = 1 << 20
	lastPart = 1
	failed   = 2
	waitPart = 2 * time.Minute
)

var (
	ErrRefused = errors.New("the other device did not offer that file")
	ErrStalled = errors.New("the other device stopped sending the file")
	errUnsafe  = errors.New("unsafe path in a received archive")
)

type Send func(data []byte) error

type Server struct {
	mu      sync.Mutex
	allowed map[string]bool
}

func (s *Server) Allow(uris []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.allowed == nil {
		s.allowed = map[string]bool{}
	}
	for _, u := range uris {
		s.allowed[u] = true
	}
}

func (s *Server) allows(uri string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.allowed[uri]
}

func Ask(id uint32, uri string) []byte {
	return append(binary.BigEndian.AppendUint32(nil, id), uri...)
}

func (s *Server) Serve(ask []byte, send Send) {
	if len(ask) < 4 {
		return
	}
	id := binary.BigEndian.Uint32(ask)
	uri := string(ask[4:])
	go func() {
		w := &parts{id: id, send: send}
		err := ErrRefused
		if s.allows(uri) {
			if path, ok := LocalPath(uri); ok {
				err = pack(path, w)
			}
		}
		w.finish(err)
	}()
}

type parts struct {
	id   uint32
	send Send
	buf  []byte
	err  error
}

func (p *parts) Write(b []byte) (int, error) {
	if p.err != nil {
		return 0, p.err
	}
	n := len(b)
	for len(b) > 0 {
		take := min(chunk-len(p.buf), len(b))
		p.buf = append(p.buf, b[:take]...)
		b = b[take:]
		if len(p.buf) == chunk {
			if p.err = p.flush(0); p.err != nil {
				return 0, p.err
			}
		}
	}
	return n, nil
}

func (p *parts) flush(flags byte) error {
	msg := binary.BigEndian.AppendUint32(make([]byte, 0, 5+len(p.buf)), p.id)
	msg = append(append(msg, flags), p.buf...)
	p.buf = p.buf[:0]
	return p.send(msg)
}

func (p *parts) finish(err error) {
	if p.err != nil {
		return
	}
	if err != nil {
		p.buf = append(p.buf[:0], err.Error()...)
		p.flush(lastPart | failed)
		return
	}
	p.flush(lastPart)
}

func pack(root string, w io.Writer) error {
	tw := tar.NewWriter(w)
	base := filepath.Dir(root)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() && !d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(base, path)
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = 0, 0, "", ""
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

type Fetcher struct {
	Send  Send
	Dir   string
	Owner func(path string) error

	mu      sync.Mutex
	next    uint32
	waiting map[uint32]*pending
}

type pending struct {
	file *os.File
	got  chan error
	seen chan struct{}
}

func (f *Fetcher) Fetch(uri string) (string, error) {
	if err := mkdirOwned(f.Dir, f.Owner); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(f.Dir, "item-")
	if err != nil {
		return "", err
	}
	if f.Owner != nil {
		f.Owner(dir)
	}
	tmp, err := os.CreateTemp(f.Dir, ".part-")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	p := &pending{file: tmp, got: make(chan error, 1), seen: make(chan struct{}, 1)}
	f.mu.Lock()
	if f.waiting == nil {
		f.waiting = map[uint32]*pending{}
	}
	f.next++
	id := f.next
	f.waiting[id] = p
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		delete(f.waiting, id)
		f.mu.Unlock()
		tmp.Close()
	}()
	if err := f.Send(Ask(id, uri)); err != nil {
		return "", err
	}
	timer := time.NewTimer(waitPart)
	defer timer.Stop()
	for {
		select {
		case err := <-p.got:
			if err != nil {
				return "", err
			}
			if _, err := tmp.Seek(0, io.SeekStart); err != nil {
				return "", err
			}
			top, err := unpack(tmp, dir, f.Owner)
			if err != nil {
				return "", err
			}
			if top == "" {
				return "", ErrRefused
			}
			return filepath.Join(dir, top), nil
		case <-p.seen:
			timer.Reset(waitPart)
		case <-timer.C:
			return "", ErrStalled
		}
	}
}

func (f *Fetcher) Deliver(part []byte) {
	if len(part) < 5 {
		return
	}
	id, flags, data := binary.BigEndian.Uint32(part), part[4], part[5:]
	f.mu.Lock()
	p := f.waiting[id]
	f.mu.Unlock()
	if p == nil {
		return
	}
	if flags&failed != 0 {
		p.got <- fmt.Errorf("%s", data)
		return
	}
	if _, err := p.file.Write(data); err != nil {
		p.got <- err
		return
	}
	if flags&lastPart != 0 {
		p.got <- nil
		return
	}
	select {
	case p.seen <- struct{}{}:
	default:
	}
}

func mkdirOwned(dir string, owner func(string) error) error {
	if _, err := os.Stat(dir); err == nil {
		return nil
	}
	if parent := filepath.Dir(dir); parent != dir {
		if err := mkdirOwned(parent, owner); err != nil {
			return err
		}
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	if owner != nil {
		owner(dir)
	}
	return nil
}

func unpack(r io.Reader, dir string, owner func(string) error) (string, error) {
	tr := tar.NewReader(r)
	top := ""
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return top, nil
		}
		if err != nil {
			return "", err
		}
		name := filepath.FromSlash(hdr.Name)
		if !filepath.IsLocal(name) {
			return "", errUnsafe
		}
		first := strings.SplitN(filepath.ToSlash(name), "/", 2)[0]
		if top == "" {
			top = first
		} else if first != top {
			return "", errUnsafe
		}
		target := filepath.Join(dir, name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return "", err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return "", err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_EXCL, os.FileMode(hdr.Mode).Perm()|0o600)
			if err != nil {
				return "", err
			}
			_, err = io.Copy(out, tr)
			out.Close()
			if err != nil {
				return "", err
			}
			os.Chtimes(target, hdr.ModTime, hdr.ModTime)
		default:
			continue
		}
		if owner != nil {
			owner(target)
		}
	}
}

func LocalPath(uri string) (string, bool) {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" || (u.Host != "" && u.Host != "localhost") || !filepath.IsAbs(u.Path) {
		return "", false
	}
	return filepath.Clean(u.Path), true
}

func FileURI(path string) string {
	return (&url.URL{Scheme: "file", Path: path}).String()
}
