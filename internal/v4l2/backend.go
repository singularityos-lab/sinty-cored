package v4l2

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"os/exec"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

type backend interface {
	ioctl(req uintptr, arg, planes []byte) error
	mmap(offset, length uint32) ([]byte, error)
	munmap(mem []byte, offset uint32)
	poll(events int16, ms int) int16
	close() error
}

func (c Codec) open() (backend, error) {
	if c.proxy != "" {
		return startProxy(c.proxy, c.Path)
	}
	fd, err := unix.Open(c.Path, unix.O_RDWR|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return native{fd: fd}, nil
}

type native struct{ fd int }

func (n native) ioctl(req uintptr, arg, planes []byte) error {
	err := ioctl(n.fd, req, unsafe.Pointer(&arg[0]))
	runtime.KeepAlive(planes)
	return err
}

func (n native) mmap(offset, length uint32) ([]byte, error) {
	return unix.Mmap(n.fd, int64(offset), int(length), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
}

func (n native) munmap(mem []byte, offset uint32) { unix.Munmap(mem) }

func (n native) poll(events int16, ms int) int16 {
	fds := []unix.PollFd{{Fd: int32(n.fd), Events: events}}
	if c, err := unix.Poll(fds, ms); err != nil || c == 0 {
		return 0
	}
	return fds[0].Revents
}

func (n native) close() error { return unix.Close(n.fd) }

const (
	opOpen  = 1
	opIoctl = 2
	opMmap  = 3
	opWrite = 4
	opRead  = 5
	opPoll  = 6
	opClose = 7
	opUnmap = 8
)

var ErrProxy = errors.New("the codec helper stopped")

type slot struct {
	typ, index, plane uint32
}

type proxy struct {
	cmd     *exec.Cmd
	in      io.WriteCloser
	out     *bufio.Reader
	buf     []byte
	shadows map[uint32][]byte
	offsets map[slot]uint32
	dead    bool
}

func startProxy(helper, path string) (*proxy, error) {
	cmd := exec.Command(helper)
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &proxy{cmd: cmd, in: in, out: bufio.NewReaderSize(out, 1<<16), shadows: map[uint32][]byte{}, offsets: map[slot]uint32{}}
	p.put32(opOpen)
	p.put32(uint32(len(path)))
	p.buf = append(p.buf, path...)
	if err := p.call(); err != nil {
		p.close()
		return nil, err
	}
	return p, nil
}

func (p *proxy) put32(v uint32) { p.buf = binary.LittleEndian.AppendUint32(p.buf, v) }

func (p *proxy) flush() error {
	if p.dead {
		return ErrProxy
	}
	_, err := p.in.Write(p.buf)
	p.buf = p.buf[:0]
	if err != nil {
		p.dead = true
		return ErrProxy
	}
	return nil
}

func (p *proxy) get32() (uint32, error) {
	var b [4]byte
	if _, err := io.ReadFull(p.out, b[:]); err != nil {
		p.dead = true
		return 0, ErrProxy
	}
	return binary.LittleEndian.Uint32(b[:]), nil
}

func (p *proxy) status() error {
	v, err := p.get32()
	if err != nil {
		return err
	}
	if v != 0 {
		return syscall.Errno(v)
	}
	return nil
}

func (p *proxy) call() error {
	if err := p.flush(); err != nil {
		return err
	}
	return p.status()
}

func (p *proxy) ioctl(req uintptr, arg, planes []byte) error {
	typ := uint32(0)
	if len(arg) >= 8 && (req == vidiocQbuf || req == vidiocDqbuf || req == vidiocQuerybuf) {
		typ = binary.LittleEndian.Uint32(arg[4:])
	}
	if req == vidiocQbuf && output(typ) {
		if err := p.push(arg, planes, typ); err != nil {
			return err
		}
	}
	nplanes := 0
	if len(planes) > 0 && multi(typ) {
		nplanes = min(int(binary.LittleEndian.Uint32(arg[72:])), len(planes)/planeSize)
	}
	sent := planes[:nplanes*planeSize]
	p.put32(opIoctl)
	p.buf = binary.LittleEndian.AppendUint64(p.buf, uint64(req))
	p.put32(uint32(len(arg)))
	p.buf = append(p.buf, arg...)
	p.put32(uint32(len(sent)))
	p.buf = append(p.buf, sent...)
	if err := p.flush(); err != nil {
		return err
	}
	res := p.status()
	if errors.Is(res, ErrProxy) {
		return res
	}
	if _, err := io.ReadFull(p.out, arg); err != nil {
		p.dead = true
		return ErrProxy
	}
	if _, err := io.ReadFull(p.out, sent); err != nil {
		p.dead = true
		return ErrProxy
	}
	if res != nil {
		return res
	}
	switch {
	case req == vidiocQuerybuf:
		index := binary.LittleEndian.Uint32(arg[0:])
		for i := 0; i < max(nplanes, 1); i++ {
			_, offset := bufPlane(arg, sent, i, multi(typ))
			p.offsets[slot{typ, index, uint32(i)}] = offset
		}
	case req == vidiocDqbuf && !output(typ):
		return p.pull(arg, sent, typ, nplanes)
	}
	return nil
}

func output(typ uint32) bool { return typ == typeOutput || typ == typeOutputMplane }

func multi(typ uint32) bool { return typ == typeCaptureMplane || typ == typeOutputMplane }

func (p *proxy) used(arg, planes []byte, typ uint32, i int) uint32 {
	if multi(typ) {
		return binary.LittleEndian.Uint32(planes[i*planeSize:])
	}
	return binary.LittleEndian.Uint32(arg[8:])
}

func (p *proxy) push(arg, planes []byte, typ uint32) error {
	index := binary.LittleEndian.Uint32(arg[0:])
	n := 1
	if multi(typ) {
		n = min(int(binary.LittleEndian.Uint32(arg[72:])), len(planes)/planeSize)
	}
	for i := 0; i < n; i++ {
		offset, ok := p.offsets[slot{typ, index, uint32(i)}]
		shadow := p.shadows[offset]
		if !ok || shadow == nil {
			continue
		}
		size := min(int(p.used(arg, planes, typ, i)), len(shadow))
		p.put32(opWrite)
		p.put32(offset)
		p.put32(uint32(size))
		p.buf = append(p.buf, shadow[:size]...)
		if err := p.call(); err != nil {
			return err
		}
	}
	return nil
}

func (p *proxy) pull(arg, planes []byte, typ uint32, nplanes int) error {
	index := binary.LittleEndian.Uint32(arg[0:])
	for i := 0; i < max(nplanes, 1); i++ {
		offset, ok := p.offsets[slot{typ, index, uint32(i)}]
		shadow := p.shadows[offset]
		if !ok || shadow == nil {
			continue
		}
		size := min(int(p.used(arg, planes, typ, i)), len(shadow))
		p.put32(opRead)
		p.put32(offset)
		p.put32(uint32(size))
		if err := p.call(); err != nil {
			return err
		}
		if _, err := io.ReadFull(p.out, shadow[:size]); err != nil {
			p.dead = true
			return ErrProxy
		}
	}
	return nil
}

func (p *proxy) mmap(offset, length uint32) ([]byte, error) {
	p.put32(opMmap)
	p.put32(offset)
	p.put32(length)
	if err := p.call(); err != nil {
		return nil, err
	}
	mem := make([]byte, length)
	p.shadows[offset] = mem
	return mem, nil
}

func (p *proxy) munmap(mem []byte, offset uint32) {
	delete(p.shadows, offset)
	p.put32(opUnmap)
	p.put32(offset)
	p.call()
}

func (p *proxy) poll(events int16, ms int) int16 {
	p.put32(opPoll)
	p.put32(uint32(int32(events)))
	p.put32(uint32(int32(ms)))
	if p.flush() != nil {
		return 0
	}
	v, err := p.get32()
	if err != nil {
		return 0
	}
	return int16(v)
}

func (p *proxy) close() error {
	if !p.dead {
		p.put32(opClose)
		p.call()
	}
	p.in.Close()
	return p.cmd.Wait()
}
