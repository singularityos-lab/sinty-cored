package clip

import (
	"encoding/binary"
	"errors"
	"slices"
	"strings"
)

const (
	Marker   = "application/x-sinty-nearby"
	Text     = "text/plain;charset=utf-8"
	MaxItem  = 16 << 20
	MaxTotal = 24 << 20
)

var TextAliases = []string{Text, "text/plain", "UTF8_STRING", "STRING", "TEXT"}

var kept = []string{"text/html", "text/uri-list", "x-special/gnome-copied-files", "image/png"}

var ErrDamaged = errors.New("damaged clipboard message")

type Item struct {
	Mime string
	Data []byte
}

type Want struct {
	Read string
	As   string
}

func Choose(offered []string) []Want {
	if slices.Contains(offered, Marker) {
		return nil
	}
	var out []Want
	for _, m := range TextAliases {
		if slices.Contains(offered, m) {
			out = append(out, Want{m, Text})
			break
		}
	}
	image := false
	for _, m := range kept {
		if slices.Contains(offered, m) {
			out = append(out, Want{m, m})
			image = image || strings.HasPrefix(m, "image/")
		}
	}
	if !image {
		for _, m := range offered {
			if strings.HasPrefix(m, "image/") {
				out = append(out, Want{m, m})
				break
			}
		}
	}
	return out
}

func Offers(items []Item) []string {
	var out []string
	for _, it := range items {
		if it.Mime == Text {
			out = append(out, TextAliases...)
			continue
		}
		out = append(out, it.Mime)
	}
	return append(out, Marker)
}

func Find(items []Item, mime string) ([]byte, bool) {
	if slices.Contains(TextAliases, mime) {
		mime = Text
	}
	for _, it := range items {
		if it.Mime == mime {
			return it.Data, true
		}
	}
	return nil, false
}

func Encode(items []Item) []byte {
	size := 2
	for _, it := range items {
		size += 2 + len(it.Mime) + 4 + len(it.Data)
	}
	out := make([]byte, 0, size)
	out = binary.BigEndian.AppendUint16(out, uint16(len(items)))
	for _, it := range items {
		out = binary.BigEndian.AppendUint16(out, uint16(len(it.Mime)))
		out = append(out, it.Mime...)
		out = binary.BigEndian.AppendUint32(out, uint32(len(it.Data)))
		out = append(out, it.Data...)
	}
	return out
}

func Decode(b []byte) ([]Item, error) {
	if len(b) < 2 {
		return nil, ErrDamaged
	}
	n := int(binary.BigEndian.Uint16(b))
	b = b[2:]
	items := make([]Item, 0, n)
	for range n {
		if len(b) < 2 {
			return nil, ErrDamaged
		}
		ml := int(binary.BigEndian.Uint16(b))
		b = b[2:]
		if ml == 0 || len(b) < ml+4 {
			return nil, ErrDamaged
		}
		mime := string(b[:ml])
		b = b[ml:]
		dl := int(binary.BigEndian.Uint32(b))
		b = b[4:]
		if dl > MaxItem || len(b) < dl {
			return nil, ErrDamaged
		}
		items = append(items, Item{mime, slices.Clone(b[:dl])})
		b = b[dl:]
	}
	if len(b) != 0 {
		return nil, ErrDamaged
	}
	return items, nil
}

const (
	URIList     = "text/uri-list"
	GnomeCopied = "x-special/gnome-copied-files"
)

func FileURIs(items []Item) []string {
	var out []string
	for _, it := range items {
		if it.Mime != URIList && it.Mime != GnomeCopied {
			continue
		}
		for _, line := range strings.Split(string(it.Data), "\n") {
			line = strings.TrimRight(line, "\r")
			if strings.HasPrefix(line, "file:") && !slices.Contains(out, line) {
				out = append(out, line)
			}
		}
	}
	return out
}

func Rewrite(data []byte, mapping map[string]string) []byte {
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		trimmed := strings.TrimRight(line, "\r")
		if to, ok := mapping[trimmed]; ok {
			lines[i] = to + line[len(trimmed):]
		}
	}
	return []byte(strings.Join(lines, "\n"))
}
