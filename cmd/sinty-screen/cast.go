package main

import (
	"bufio"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/singularityos-lab/sinty-cored/internal/channel"
	"github.com/singularityos-lab/sinty-cored/internal/input"
	"github.com/singularityos-lab/sinty-cored/internal/pair"
	"github.com/singularityos-lab/sinty-cored/internal/stream"
	"github.com/singularityos-lab/sinty-cored/internal/terminal"
	"github.com/singularityos-lab/sinty-cored/internal/wl"
)

type castCommand struct {
	Cmd    string `json:"cmd"`
	Screen string `json:"screen"`
}

func ephemeral() (pair.Identity, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	return pair.Identity{Priv: priv}, err
}

func emitTo(out *json.Encoder, v map[string]any) {
	if err := out.Encode(v); err != nil {
		os.Exit(1)
	}
}

func ended(out *json.Encoder, err error) int {
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		emitTo(out, map[string]any{"event": "ended"})
		return 0
	}
	emitTo(out, map[string]any{"event": "ended", "error": err.Error()})
	return 1
}

func serveCast(app string, pid int, listen string, in io.Reader, w io.Writer) int {
	out := json.NewEncoder(w)
	if app == "" {
		return ended(out, errors.New("no app to cast"))
	}
	me, err := ephemeral()
	if err != nil {
		return ended(out, err)
	}
	cast, err := wl.OpenCast(wl.SocketPath(), app, pid)
	if err != nil {
		return ended(out, err)
	}
	defer cast.Close()
	l, err := net.Listen("tcp", listen)
	if err != nil {
		return ended(out, err)
	}
	defer l.Close()
	g := cast.Geometry()
	emitTo(out, map[string]any{
		"event":  "ready",
		"port":   l.Addr().(*net.TCPAddr).Port,
		"key":    base64.StdEncoding.EncodeToString(me.Public()),
		"width":  g.W,
		"height": g.H,
	})

	trusted := make(chan string, 1)
	stop := make(chan struct{})
	var once sync.Once
	quit := func() { once.Do(func() { close(stop) }) }
	go func() {
		defer quit()
		scan := bufio.NewScanner(in)
		for scan.Scan() {
			var c castCommand
			if json.Unmarshal(scan.Bytes(), &c) != nil {
				continue
			}
			switch c.Cmd {
			case "trust":
				key, err := base64.StdEncoding.DecodeString(c.Screen)
				if err == nil && len(key) == ed25519.PublicKeySize {
					select {
					case trusted <- pair.ID(key):
					default:
					}
				}
			case "stop":
				return
			}
		}
	}()
	go func() {
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
		<-sigs
		quit()
	}()
	var screen string
	select {
	case screen = <-trusted:
	case <-stop:
		return ended(out, nil)
	}
	go func() {
		<-stop
		l.Close()
	}()
	for {
		conn, err := l.Accept()
		if err != nil {
			return ended(out, err)
		}
		conn.SetDeadline(time.Now().Add(10 * time.Second))
		c, _, err := channel.Core(conn, me, func(id string) (string, bool) { return "", id == screen })
		if err != nil {
			conn.Close()
			continue
		}
		conn.SetDeadline(time.Time{})
		l.Close()
		emitTo(out, map[string]any{"event": "showing"})
		err = stream.Serve(c, cast, cast, nil, stream.Options{InFlight: 2}, stop)
		c.Close()
		if errors.Is(err, wl.ErrWindowGone) {
			emitTo(out, map[string]any{"event": "ended", "reason": "closed"})
			return 0
		}
		return ended(out, err)
	}
}

func showCast(addr, key, title, app, size, layout string, w io.Writer) int {
	out := json.NewEncoder(w)
	public, err := base64.StdEncoding.DecodeString(key)
	if err != nil || len(public) != ed25519.PublicKeySize || addr == "" {
		return ended(out, errors.New("the casting device is not named"))
	}
	var width, height int
	if _, err := fmt.Sscanf(size, "%dx%d", &width, &height); err != nil || width <= 0 || height <= 0 {
		width, height = 960, 600
	}
	me, err := ephemeral()
	if err != nil {
		return ended(out, err)
	}
	emitTo(out, map[string]any{"event": "identity", "key": base64.StdEncoding.EncodeToString(me.Public())})
	peer := pair.Peer{ID: pair.ID(public), Name: title, Public: public}
	var sess *terminal.Session
	var conn net.Conn
	term := &terminal.Terminal{Identity: me, Layout: layout, Pinned: &peer, W: width, H: height}
	deadline := time.Now().Add(20 * time.Second)
	for {
		conn, err = net.DialTimeout("tcp", addr, 5*time.Second)
		if err == nil {
			conn.SetDeadline(time.Now().Add(10 * time.Second))
			sess, err = term.Open(conn)
			if err == nil {
				conn.SetDeadline(time.Time{})
				break
			}
			conn.Close()
		}
		if time.Now().After(deadline) {
			return ended(out, err)
		}
		time.Sleep(300 * time.Millisecond)
	}
	defer sess.Close()
	win, err := wl.OpenWindow(wl.SocketPath(), title, app, width, height)
	if err != nil {
		return ended(out, err)
	}
	defer win.Close()
	term.Display = win
	win.OnInput = func(events []input.Event) { sess.Send(events) }
	done := make(chan error, 2)
	go func() { done <- win.Run() }()
	go func() { done <- term.Show(sess) }()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	select {
	case <-win.Closed():
		emitTo(out, map[string]any{"event": "ended", "reason": "closed"})
		return 0
	case <-sigs:
		return ended(out, nil)
	case err := <-done:
		return ended(out, err)
	}
}
