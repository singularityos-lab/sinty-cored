package v4l2

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	vidiocQuerycap       = 0x80685600
	vidiocEnumFmt        = 0xc0405602
	vidiocGFmt           = 0xc0d05604
	vidiocSFmt           = 0xc0d05605
	vidiocReqbufs        = 0xc0145608
	vidiocQuerybuf       = 0xc0585609
	vidiocQbuf           = 0xc058560f
	vidiocDqbuf          = 0xc0585611
	vidiocStreamon       = 0x40045612
	vidiocStreamoff      = 0x40045613
	vidiocDqevent        = 0x80885659
	vidiocSubscribeEvent = 0x4020565a
	vidiocSSelection     = 0xc040565f

	capM2MMplane = 0x00004000
	capM2M       = 0x00008000
	capStreaming = 0x04000000
	capDevice    = 0x80000000

	typeCapture       = 1
	typeOutput        = 2
	typeCaptureMplane = 9
	typeOutputMplane  = 10

	memoryMmap = 1
	fieldNone  = 1

	eventSourceChange = 5

	formatSize = 208
	bufferSize = 88
	planeSize  = 64
	maxPlanes  = 8
)

var (
	ErrNoDevice = errors.New("no hardware codec for this format")
	ErrFormat   = errors.New("the codec refused the picture format")
	ErrTooBig   = errors.New("the coded frame does not fit the codec buffer")
)

var (
	H264  = Fourcc("H264")
	HEVC  = Fourcc("HEVC")
	VP8   = Fourcc("VP80")
	FWHT  = Fourcc("FWHT")
	XR24  = Fourcc("XR24")
	NV12  = Fourcc("NV12")
	NV12M = Fourcc("NM12")
)

var Coded = []uint32{H264, HEVC, VP8, FWHT}

var decodable = []uint32{NV12, NV12M, XR24}

func Fourcc(s string) uint32 {
	return uint32(s[0]) | uint32(s[1])<<8 | uint32(s[2])<<16 | uint32(s[3])<<24
}

func Name(f uint32) string {
	return string([]byte{byte(f), byte(f >> 8), byte(f >> 16), byte(f >> 24)})
}

type Codec struct {
	Path   string
	Card   string
	Format uint32
	raw    []uint32
	mplane bool
	proxy  string
}

func ioctl(fd int, req uintptr, arg unsafe.Pointer) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), req, uintptr(arg))
	if errno != 0 {
		return errno
	}
	return nil
}

func cstr(b []byte) string {
	if i := slices.Index(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

func formats(be backend, typ uint32) []uint32 {
	var out []uint32
	for i := uint32(0); i < 64; i++ {
		d := make([]byte, 64)
		binary.LittleEndian.PutUint32(d[0:], i)
		binary.LittleEndian.PutUint32(d[4:], typ)
		if be.ioctl(vidiocEnumFmt, d, nil) != nil {
			break
		}
		out = append(out, binary.LittleEndian.Uint32(d[44:]))
	}
	return out
}

func probe(be backend) (card string, mplane bool, ok bool) {
	c := make([]byte, 104)
	if be.ioctl(vidiocQuerycap, c, nil) != nil {
		return "", false, false
	}
	caps := binary.LittleEndian.Uint32(c[84:])
	if caps&capDevice != 0 {
		caps = binary.LittleEndian.Uint32(c[88:])
	}
	if caps&capStreaming == 0 || caps&(capM2M|capM2MMplane) == 0 {
		return "", false, false
	}
	return cstr(c[16:48]), caps&capM2MMplane != 0, true
}

func Find(dir string, encoder bool) []Codec {
	paths, _ := filepath.Glob(filepath.Join(dir, "video*"))
	slices.SortFunc(paths, func(a, b string) int {
		if len(a) != len(b) {
			return len(a) - len(b)
		}
		return strings.Compare(a, b)
	})
	var out []Codec
	for _, p := range paths {
		out = append(out, codecsAt(Codec{Path: p}, encoder)...)
	}
	return sorted(out)
}

const DefaultHelper = "/usr/libexec/sinty-v4l2-proxy"

var ProxyNodes = []string{"/dev/v4l2-nvenc", "/dev/v4l2-nvdec"}

func FindAll(dir, helper string, encoder bool) []Codec {
	out := Find(dir, encoder)
	if helper != "" {
		out = append(out, FindProxy(helper, ProxyNodes, encoder)...)
	}
	return out
}

func FindProxy(helper string, paths []string, encoder bool) []Codec {
	if _, err := os.Stat(helper); err != nil {
		return nil
	}
	var out []Codec
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		out = append(out, codecsAt(Codec{Path: p, proxy: helper}, encoder)...)
	}
	return sorted(out)
}

func sorted(out []Codec) []Codec {
	slices.SortStableFunc(out, func(a, b Codec) int {
		return slices.Index(Coded, a.Format) - slices.Index(Coded, b.Format)
	})
	return out
}

func codecsAt(c Codec, encoder bool) []Codec {
	be, err := c.open()
	if err != nil {
		return nil
	}
	defer be.close()
	card, mplane, ok := probe(be)
	if !ok {
		return nil
	}
	capT, outT := uint32(typeCapture), uint32(typeOutput)
	if mplane {
		capT, outT = typeCaptureMplane, typeOutputMplane
	}
	codedT, rawT := outT, capT
	if encoder {
		codedT, rawT = capT, outT
	}
	coded, raw := formats(be, codedT), formats(be, rawT)
	usable := slices.Contains(raw, XR24) || slices.Contains(raw, NV12) || slices.Contains(raw, NV12M)
	if !usable {
		return nil
	}
	var out []Codec
	for _, f := range Coded {
		if slices.Contains(coded, f) && !slices.Contains(raw, f) {
			out = append(out, Codec{Path: c.Path, Card: card, Format: f, raw: raw, mplane: mplane, proxy: c.proxy})
		}
	}
	return out
}

type buffer struct {
	planes  [][]byte
	offsets []uint32
	queued  bool
}

type queue struct {
	typ     uint32
	bufs    []buffer
	nplanes int
	pix     pixFormat
	on      bool
}

type pixFormat struct {
	W, H   int
	Format uint32
	Planes int
	Stride [maxPlanes]int
	Size   [maxPlanes]int
}

type device struct {
	be     backend
	mplane bool
	out    queue
	cap    queue
}

func open(c Codec) (*device, error) {
	be, err := c.open()
	if err != nil {
		return nil, err
	}
	d := &device{be: be, mplane: c.mplane}
	d.cap.typ, d.out.typ = typeCapture, typeOutput
	if c.mplane {
		d.cap.typ, d.out.typ = typeCaptureMplane, typeOutputMplane
	}
	return d, nil
}

func (d *device) setFormat(typ uint32, p pixFormat, get bool) (pixFormat, error) {
	f := make([]byte, formatSize)
	binary.LittleEndian.PutUint32(f[0:], typ)
	req := uintptr(vidiocSFmt)
	if get {
		req = vidiocGFmt
	} else if d.mplane {
		binary.LittleEndian.PutUint32(f[8:], uint32(p.W))
		binary.LittleEndian.PutUint32(f[12:], uint32(p.H))
		binary.LittleEndian.PutUint32(f[16:], p.Format)
		binary.LittleEndian.PutUint32(f[20:], fieldNone)
		binary.LittleEndian.PutUint32(f[28:], uint32(p.Size[0]))
		f[8+180] = byte(max(p.Planes, 1))
	} else {
		binary.LittleEndian.PutUint32(f[8:], uint32(p.W))
		binary.LittleEndian.PutUint32(f[12:], uint32(p.H))
		binary.LittleEndian.PutUint32(f[16:], p.Format)
		binary.LittleEndian.PutUint32(f[20:], fieldNone)
		binary.LittleEndian.PutUint32(f[28:], uint32(p.Size[0]))
	}
	if err := d.be.ioctl(req, f, nil); err != nil {
		return pixFormat{}, err
	}
	var r pixFormat
	r.W = int(binary.LittleEndian.Uint32(f[8:]))
	r.H = int(binary.LittleEndian.Uint32(f[12:]))
	r.Format = binary.LittleEndian.Uint32(f[16:])
	if d.mplane {
		r.Planes = int(f[8+180])
		for i := 0; i < min(r.Planes, maxPlanes); i++ {
			base := 8 + 20 + i*20
			r.Size[i] = int(binary.LittleEndian.Uint32(f[base:]))
			r.Stride[i] = int(binary.LittleEndian.Uint32(f[base+4:]))
		}
	} else {
		r.Planes = 1
		r.Stride[0] = int(binary.LittleEndian.Uint32(f[24:]))
		r.Size[0] = int(binary.LittleEndian.Uint32(f[28:]))
	}
	return r, nil
}

func (d *device) crop(w, h int) {
	sel := make([]byte, 64)
	binary.LittleEndian.PutUint32(sel[0:], typeOutput)
	binary.LittleEndian.PutUint32(sel[20:], uint32(w))
	binary.LittleEndian.PutUint32(sel[24:], uint32(h))
	d.be.ioctl(vidiocSSelection, sel, nil)
}

func (d *device) request(q *queue, count int) error {
	r := make([]byte, 20)
	binary.LittleEndian.PutUint32(r[0:], uint32(count))
	binary.LittleEndian.PutUint32(r[4:], q.typ)
	binary.LittleEndian.PutUint32(r[8:], memoryMmap)
	if err := d.be.ioctl(vidiocReqbufs, r, nil); err != nil {
		return err
	}
	n := int(binary.LittleEndian.Uint32(r[0:]))
	if count > 0 && n == 0 {
		return errors.New("the codec gave no buffers")
	}
	q.bufs = make([]buffer, n)
	q.nplanes = max(q.pix.Planes, 1)
	for i := range q.bufs {
		b, planes := d.newBuf(q, i)
		if err := d.be.ioctl(vidiocQuerybuf, b, planes); err != nil {
			return err
		}
		for p := 0; p < q.nplanes; p++ {
			length, offset := bufPlane(b, planes, p, d.mplane)
			mem, err := d.be.mmap(offset, length)
			if err != nil {
				return err
			}
			q.bufs[i].planes = append(q.bufs[i].planes, mem)
			q.bufs[i].offsets = append(q.bufs[i].offsets, offset)
		}
	}
	return nil
}

func (d *device) newBuf(q *queue, index int) ([]byte, []byte) {
	b := make([]byte, bufferSize)
	binary.LittleEndian.PutUint32(b[0:], uint32(index))
	binary.LittleEndian.PutUint32(b[4:], q.typ)
	binary.LittleEndian.PutUint32(b[60:], memoryMmap)
	if !d.mplane {
		return b, nil
	}
	planes := make([]byte, planeSize*maxPlanes)
	binary.LittleEndian.PutUint64(b[64:], uint64(uintptr(unsafe.Pointer(&planes[0]))))
	binary.LittleEndian.PutUint32(b[72:], uint32(q.nplanes))
	return b, planes
}

func bufPlane(b, planes []byte, p int, mplane bool) (length, offset uint32) {
	if !mplane {
		return binary.LittleEndian.Uint32(b[72:]), binary.LittleEndian.Uint32(b[64:])
	}
	base := p * planeSize
	return binary.LittleEndian.Uint32(planes[base+4:]), binary.LittleEndian.Uint32(planes[base+8:])
}

func (d *device) queueBuf(q *queue, index int, used []int) error {
	b, planes := d.newBuf(q, index)
	if d.mplane {
		for p := 0; p < q.nplanes; p++ {
			n := 0
			if p < len(used) {
				n = used[p]
			}
			binary.LittleEndian.PutUint32(planes[p*planeSize:], uint32(n))
			binary.LittleEndian.PutUint32(planes[p*planeSize+4:], uint32(len(q.bufs[index].planes[p])))
		}
	} else if len(used) > 0 {
		binary.LittleEndian.PutUint32(b[8:], uint32(used[0]))
	}
	err := d.be.ioctl(vidiocQbuf, b, planes)
	if err != nil {
		return err
	}
	q.bufs[index].queued = true
	return nil
}

func (d *device) dequeue(q *queue) (int, []int, error) {
	b, planes := d.newBuf(q, 0)
	err := d.be.ioctl(vidiocDqbuf, b, planes)
	if err != nil {
		return -1, nil, err
	}
	index := int(binary.LittleEndian.Uint32(b[0:]))
	if index >= len(q.bufs) {
		return -1, nil, errors.New("the codec returned an unknown buffer")
	}
	q.bufs[index].queued = false
	var used []int
	if d.mplane {
		for p := 0; p < q.nplanes; p++ {
			used = append(used, int(binary.LittleEndian.Uint32(planes[p*planeSize:])))
		}
	} else {
		used = []int{int(binary.LittleEndian.Uint32(b[8:]))}
	}
	return index, used, nil
}

func (d *device) stream(q *queue, on bool) error {
	t := binary.LittleEndian.AppendUint32(nil, q.typ)
	req := uintptr(vidiocStreamoff)
	if on {
		req = vidiocStreamon
	}
	if err := d.be.ioctl(req, t, nil); err != nil {
		return err
	}
	q.on = on
	return nil
}

func (d *device) wait(events int16, ms int) int16 {
	return d.be.poll(events, ms)
}

func (d *device) free(q *queue) int {
	for i := range q.bufs {
		if !q.bufs[i].queued {
			return i
		}
	}
	for {
		if _, _, err := d.dequeue(q); err != nil {
			break
		}
	}
	for i := range q.bufs {
		if !q.bufs[i].queued {
			return i
		}
	}
	if d.wait(unix.POLLOUT, 1000)&unix.POLLOUT == 0 {
		return -1
	}
	if i, _, err := d.dequeue(q); err == nil {
		return i
	}
	return -1
}

func (d *device) close() error {
	if d.out.on {
		d.stream(&d.out, false)
	}
	if d.cap.on {
		d.stream(&d.cap, false)
	}
	for _, q := range []*queue{&d.out, &d.cap} {
		d.unmap(q)
	}
	return d.be.close()
}

func (d *device) unmap(q *queue) {
	for _, b := range q.bufs {
		for i, p := range b.planes {
			d.be.munmap(p, b.offsets[i])
		}
	}
	q.bufs = nil
}

func pickRaw(raw []uint32) uint32 {
	for _, f := range []uint32{NV12, NV12M, XR24} {
		if slices.Contains(raw, f) {
			return f
		}
	}
	return 0
}

type Encoder struct {
	d     *device
	w, h  int
	coded []byte
}

func NewEncoder(c Codec, w, h int) (*Encoder, error) {
	return newEncoder(c, w, h, pickRaw(c.raw))
}

func newEncoder(c Codec, w, h int, raw uint32) (*Encoder, error) {
	if raw == 0 {
		return nil, ErrFormat
	}
	d, err := open(c)
	if err != nil {
		return nil, err
	}
	e := &Encoder{d: d, w: w, h: h}
	if err := e.setup(c, raw); err != nil {
		d.close()
		return nil, err
	}
	return e, nil
}

func (e *Encoder) setup(c Codec, raw uint32) error {
	d := e.d
	cp, err := d.setFormat(d.cap.typ, pixFormat{W: e.w, H: e.h, Format: c.Format, Planes: 1, Size: [maxPlanes]int{e.w * e.h * 2}}, false)
	if err != nil || cp.Format != c.Format {
		return ErrFormat
	}
	planes := 1
	if raw == NV12M {
		planes = 2
	}
	op, err := d.setFormat(d.out.typ, pixFormat{W: e.w, H: e.h, Format: raw, Planes: planes}, false)
	if err != nil || op.Format != raw || op.W < e.w || op.H < e.h {
		return ErrFormat
	}
	d.cap.pix, d.out.pix = cp, op
	if op.W != e.w || op.H != e.h {
		d.crop(e.w, e.h)
	}
	if err := d.request(&d.out, 2); err != nil {
		return err
	}
	if err := d.request(&d.cap, 4); err != nil {
		return err
	}
	for i := range d.cap.bufs {
		if err := d.queueBuf(&d.cap, i, nil); err != nil {
			return err
		}
	}
	if err := d.stream(&d.out, true); err != nil {
		return err
	}
	return d.stream(&d.cap, true)
}

func (e *Encoder) Format() uint32 { return e.d.cap.pix.Format }

func (e *Encoder) Encode(pix []byte, stride int) ([]byte, error) {
	d := e.d
	i := d.free(&d.out)
	if i < 0 {
		return nil, errors.New("the codec stopped taking frames")
	}
	used := fill(d.out.pix, d.out.bufs[i].planes, pix, stride, e.w, e.h)
	if err := d.queueBuf(&d.out, i, used); err != nil {
		return nil, err
	}
	e.coded = e.coded[:0]
	deadline := 1000
	for {
		if d.wait(unix.POLLIN, deadline)&(unix.POLLIN) == 0 {
			break
		}
		got := false
		for {
			idx, n, err := d.dequeue(&d.cap)
			if err != nil {
				break
			}
			got = true
			e.coded = append(e.coded, d.cap.bufs[idx].planes[0][:n[0]]...)
			if err := d.queueBuf(&d.cap, idx, nil); err != nil {
				return nil, err
			}
		}
		if got {
			break
		}
		deadline = 100
	}
	if len(e.coded) == 0 {
		return nil, nil
	}
	return e.coded, nil
}

func (e *Encoder) Close() error { return e.d.close() }

func fill(p pixFormat, planes [][]byte, pix []byte, stride, w, h int) []int {
	switch p.Format {
	case XR24:
		dst := planes[0]
		for y := 0; y < h; y++ {
			copy(dst[y*p.Stride[0]:y*p.Stride[0]+w*4], pix[y*stride:y*stride+w*4])
		}
		return []int{p.Stride[0] * p.H}
	case NV12M:
		ToNV12(planes[0], p.Stride[0], planes[1], p.Stride[1], pix, stride, w, h)
		return []int{p.Stride[0] * p.H, p.Stride[1] * ((p.H + 1) / 2)}
	default:
		ys := p.Stride[0] * p.H
		ToNV12(planes[0][:ys], p.Stride[0], planes[0][ys:], p.Stride[0], pix, stride, w, h)
		return []int{ys + p.Stride[0]*((p.H+1)/2)}
	}
}

func ToNV12(yp []byte, ys int, uv []byte, uvs int, pix []byte, stride, w, h int) {
	for y := 0; y < h; y++ {
		row := pix[y*stride:]
		out := yp[y*ys:]
		for x := 0; x < w; x++ {
			b, g, r := int(row[x*4]), int(row[x*4+1]), int(row[x*4+2])
			out[x] = byte((66*r+129*g+25*b+128)>>8 + 16)
		}
	}
	for y := 0; y < h; y += 2 {
		out := uv[(y/2)*uvs:]
		for x := 0; x < w; x += 2 {
			var r, g, b int
			n := 0
			for dy := 0; dy < 2 && y+dy < h; dy++ {
				for dx := 0; dx < 2 && x+dx < w; dx++ {
					o := (y+dy)*stride + (x+dx)*4
					b += int(pix[o])
					g += int(pix[o+1])
					r += int(pix[o+2])
					n++
				}
			}
			r, g, b = r/n, g/n, b/n
			out[x] = byte((-38*r-74*g+112*b+128)>>8 + 128)
			out[x+1] = byte((112*r-94*g-18*b+128)>>8 + 128)
		}
	}
}

func clamp(v int) byte {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return byte(v)
}

func FromNV12(dst []byte, yp []byte, ys int, uv []byte, uvs int, w, h int) {
	for y := 0; y < h; y++ {
		row := dst[y*w*4:]
		for x := 0; x < w; x++ {
			c := int(yp[y*ys+x]) - 16
			o := (y/2)*uvs + (x/2)*2
			u, v := int(uv[o])-128, int(uv[o+1])-128
			row[x*4] = clamp((298*c + 516*u + 128) >> 8)
			row[x*4+1] = clamp((298*c - 100*u - 208*v + 128) >> 8)
			row[x*4+2] = clamp((298*c + 409*v + 128) >> 8)
			row[x*4+3] = 0xff
		}
	}
}

type Decoder struct {
	d     *device
	codec Codec
	w, h  int
	pix   []byte
	ready bool
}

func NewDecoder(c Codec, w, h int) (*Decoder, error) {
	d, err := open(c)
	if err != nil {
		return nil, err
	}
	dec := &Decoder{d: d, codec: c, w: w, h: h}
	if err := dec.setup(); err != nil {
		d.close()
		return nil, err
	}
	return dec, nil
}

func (dec *Decoder) setup() error {
	d := dec.d
	op, err := d.setFormat(d.out.typ, pixFormat{W: dec.w, H: dec.h, Format: dec.codec.Format, Planes: 1, Size: [maxPlanes]int{max(dec.w*dec.h*2, 1<<20)}}, false)
	if err != nil || op.Format != dec.codec.Format {
		return ErrFormat
	}
	d.out.pix = op
	if err := d.request(&d.out, 4); err != nil {
		return err
	}
	sub := make([]byte, 32)
	binary.LittleEndian.PutUint32(sub[0:], eventSourceChange)
	if err := d.be.ioctl(vidiocSubscribeEvent, sub, nil); err != nil {
		return err
	}
	return d.stream(&d.out, true)
}

func (dec *Decoder) sourceChanged() bool {
	d := dec.d
	changed := false
	for {
		ev := make([]byte, 136)
		if d.be.ioctl(vidiocDqevent, ev, nil) != nil {
			return changed
		}
		if binary.LittleEndian.Uint32(ev[0:]) == eventSourceChange {
			changed = true
		}
	}
}

func (dec *Decoder) startCapture() error {
	d := dec.d
	if d.cap.on {
		d.stream(&d.cap, false)
		d.unmap(&d.cap)
		d.request(&d.cap, 0)
	}
	cur, err := d.setFormat(d.cap.typ, pixFormat{}, true)
	if err != nil {
		return err
	}
	raw := cur.Format
	if !slices.Contains(decodable, raw) {
		raw = 0
		offered := formats(d.be, d.cap.typ)
		for _, f := range decodable {
			if slices.Contains(offered, f) {
				raw = f
				break
			}
		}
	}
	if raw == 0 {
		return ErrFormat
	}
	planes := 1
	if raw == NV12M {
		planes = 2
	}
	cp, err := d.setFormat(d.cap.typ, pixFormat{W: cur.W, H: cur.H, Format: raw, Planes: planes}, false)
	if err != nil || cp.Format != raw {
		return ErrFormat
	}
	d.cap.pix = cp
	if err := d.request(&d.cap, 6); err != nil {
		return err
	}
	for i := range d.cap.bufs {
		if err := d.queueBuf(&d.cap, i, nil); err != nil {
			return err
		}
	}
	if err := d.stream(&d.cap, true); err != nil {
		return err
	}
	dec.ready = true
	return nil
}

func (dec *Decoder) Decode(data []byte) ([]byte, error) {
	d := dec.d
	i := d.free(&d.out)
	if i < 0 {
		return nil, errors.New("the codec stopped taking frames")
	}
	if len(data) > len(d.out.bufs[i].planes[0]) {
		return nil, ErrTooBig
	}
	copy(d.out.bufs[i].planes[0], data)
	if err := d.queueBuf(&d.out, i, []int{len(data)}); err != nil {
		return nil, err
	}
	var frame []byte
	for wait := 0; wait < 10; wait++ {
		rev := d.wait(unix.POLLIN|unix.POLLPRI, 100)
		if rev&unix.POLLPRI != 0 && dec.sourceChanged() {
			if err := dec.startCapture(); err != nil {
				return nil, err
			}
			continue
		}
		if !dec.ready || rev&(unix.POLLIN) == 0 {
			continue
		}
		for {
			idx, used, err := d.dequeue(&d.cap)
			if err != nil {
				break
			}
			if used[0] > 0 {
				frame = dec.convert(idx)
			}
			if err := d.queueBuf(&d.cap, idx, nil); err != nil {
				return nil, err
			}
		}
		if frame != nil {
			break
		}
	}
	return frame, nil
}

func (dec *Decoder) convert(idx int) []byte {
	p := dec.d.cap.pix
	planes := dec.d.cap.bufs[idx].planes
	w, h := min(dec.w, p.W), min(dec.h, p.H)
	if len(dec.pix) != w*h*4 {
		dec.pix = make([]byte, w*h*4)
	}
	switch p.Format {
	case XR24:
		for y := 0; y < h; y++ {
			copy(dec.pix[y*w*4:(y+1)*w*4], planes[0][y*p.Stride[0]:])
		}
	case NV12M:
		FromNV12(dec.pix, planes[0], p.Stride[0], planes[1], p.Stride[1], w, h)
	default:
		ys := p.Stride[0] * p.H
		FromNV12(dec.pix, planes[0][:ys], p.Stride[0], planes[0][ys:], p.Stride[0], w, h)
	}
	return dec.pix
}

func (dec *Decoder) Size() (int, int) { return dec.w, dec.h }

func (dec *Decoder) Close() error { return dec.d.close() }
