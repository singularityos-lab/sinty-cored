package clip

import (
	"bytes"
	"slices"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	in := []Item{{Text, []byte("hello")}, {"image/png", []byte{0, 1, 2}}, {"text/html", nil}}
	out, err := Decode(Encode(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != len(in) {
		t.Fatalf("got %d items", len(out))
	}
	for i := range in {
		if out[i].Mime != in[i].Mime || !bytes.Equal(out[i].Data, in[i].Data) {
			t.Fatalf("item %d: %+v", i, out[i])
		}
	}
}

func TestDecodeRejectsDamage(t *testing.T) {
	good := Encode([]Item{{Text, []byte("hello")}})
	for i := 0; i < len(good); i++ {
		if _, err := Decode(good[:i]); err == nil {
			t.Fatalf("truncated at %d decoded", i)
		}
	}
	if _, err := Decode(append(good, 0)); err == nil {
		t.Fatal("trailing byte decoded")
	}
}

func TestChooseReadsTextOnce(t *testing.T) {
	got := Choose([]string{"TEXT", "UTF8_STRING", "text/plain", "text/html", "image/jpeg", "application/x-gtk"})
	want := []Want{{"text/plain", Text}, {"text/html", "text/html"}, {"image/jpeg", "image/jpeg"}}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v", got)
	}
}

func TestChoosePrefersPNG(t *testing.T) {
	got := Choose([]string{"image/jpeg", "image/png"})
	if !slices.Equal(got, []Want{{"image/png", "image/png"}}) {
		t.Fatalf("got %v", got)
	}
}

func TestOwnSelectionIsSkipped(t *testing.T) {
	if Choose(Offers([]Item{{Text, []byte("x")}})) != nil {
		t.Fatal("a selection set from another device was read again")
	}
}

func TestFindAnswersTextAliases(t *testing.T) {
	items := []Item{{Text, []byte("x")}}
	for _, m := range TextAliases {
		if d, ok := Find(items, m); !ok || string(d) != "x" {
			t.Fatalf("%s not served", m)
		}
	}
	if _, ok := Find(items, "image/png"); ok {
		t.Fatal("missing type served")
	}
}

func TestFileURIsAndRewrite(t *testing.T) {
	items := []Item{
		{URIList, []byte("file:///home/a/x.pdf\r\nhttps://example.com/\r\nfile:///home/a/Photos\r\n")},
		{GnomeCopied, []byte("copy\nfile:///home/a/x.pdf\nfile:///home/a/Photos")},
	}
	got := FileURIs(items)
	if !slices.Equal(got, []string{"file:///home/a/x.pdf", "file:///home/a/Photos"}) {
		t.Fatalf("got %v", got)
	}
	m := map[string]string{"file:///home/a/x.pdf": "file:///home/b/in/x.pdf"}
	if out := string(Rewrite(items[0].Data, m)); out != "file:///home/b/in/x.pdf\r\nhttps://example.com/\r\nfile:///home/a/Photos\r\n" {
		t.Fatalf("rewrote %q", out)
	}
	if out := string(Rewrite(items[1].Data, m)); out != "copy\nfile:///home/b/in/x.pdf\nfile:///home/a/Photos" {
		t.Fatalf("rewrote %q", out)
	}
}
