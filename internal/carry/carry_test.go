package carry

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func pairUp(t *testing.T) (*Server, *Fetcher) {
	t.Helper()
	s := &Server{}
	f := &Fetcher{Dir: filepath.Join(t.TempDir(), "received")}
	f.Send = func(ask []byte) error {
		s.Serve(ask, func(part []byte) error {
			f.Deliver(part)
			return nil
		})
		return nil
	}
	return s, f
}

func TestFetchAFile(t *testing.T) {
	src := t.TempDir()
	path := filepath.Join(src, "report final.pdf")
	body := strings.Repeat("x", 3*chunk+17)
	if err := os.WriteFile(path, []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
	s, f := pairUp(t)
	s.Allow([]string{FileURI(path)})
	got, err := f.Fetch(FileURI(path))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(got) != "report final.pdf" {
		t.Fatalf("fetched as %s", got)
	}
	data, err := os.ReadFile(got)
	if err != nil || string(data) != body {
		t.Fatalf("content differs: %v", err)
	}
}

func TestFetchAFolder(t *testing.T) {
	src := t.TempDir()
	root := filepath.Join(src, "Photos")
	os.MkdirAll(filepath.Join(root, "trip"), 0o755)
	os.WriteFile(filepath.Join(root, "a.jpg"), []byte("a"), 0o644)
	os.WriteFile(filepath.Join(root, "trip", "b.jpg"), []byte("b"), 0o644)
	os.Symlink("/etc/passwd", filepath.Join(root, "link"))
	s, f := pairUp(t)
	s.Allow([]string{FileURI(root)})
	got, err := f.Fetch(FileURI(root))
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(got, "trip", "b.jpg")); string(b) != "b" {
		t.Fatal("nested file missing")
	}
	if _, err := os.Lstat(filepath.Join(got, "link")); err == nil {
		t.Fatal("a symlink was carried")
	}
}

func TestRefusesWhatWasNotOffered(t *testing.T) {
	src := t.TempDir()
	path := filepath.Join(src, "secret")
	os.WriteFile(path, []byte("no"), 0o600)
	_, f := pairUp(t)
	if _, err := f.Fetch(FileURI(path)); err == nil {
		t.Fatal("a file that was never offered was sent")
	}
}

func TestUnpackRejectsEscapes(t *testing.T) {
	for _, name := range []string{"../evil", "/etc/evil", "a/../../evil"} {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: 1})
		tw.Write([]byte("x"))
		tw.Close()
		dir := t.TempDir()
		if _, err := unpack(&buf, filepath.Join(dir, "in"), nil); err == nil {
			t.Fatalf("%s was unpacked", name)
		}
		if _, err := os.Stat(filepath.Join(dir, "evil")); err == nil {
			t.Fatalf("%s escaped", name)
		}
	}
}

func TestLocalPath(t *testing.T) {
	cases := map[string]string{
		"file:///home/me/a%20b.txt":      "/home/me/a b.txt",
		"file://localhost/home/me/c.txt": "/home/me/c.txt",
	}
	for in, want := range cases {
		if got, ok := LocalPath(in); !ok || got != want {
			t.Fatalf("%s: %q %v", in, got, ok)
		}
	}
	for _, in := range []string{"https://example.com/a", "file://other/home/a", "file:relative"} {
		if _, ok := LocalPath(in); ok {
			t.Fatalf("%s accepted", in)
		}
	}
	if FileURI("/home/me/a b.txt") != "file:///home/me/a%20b.txt" {
		t.Fatal(FileURI("/home/me/a b.txt"))
	}
}
