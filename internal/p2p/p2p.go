package p2p

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var (
	ErrUnsupported = errors.New("the Wi-Fi card cannot start a direct connection")
	ErrTimeout     = errors.New("the direct connection did not start")
)

type Group struct {
	Interface  string
	SSID       string
	Passphrase string
	Frequency  string
}

type Control struct {
	Dir string
}

func New() *Control { return &Control{Dir: "/run/wpa_supplicant"} }

func (c *Control) device() (string, error) {
	matches, _ := filepath.Glob(filepath.Join(c.Dir, "p2p-dev-*"))
	if len(matches) == 0 {
		return "", ErrUnsupported
	}
	return filepath.Base(matches[0]), nil
}

func (c *Control) Supported() bool {
	_, err := c.device()
	return err == nil
}

type conn struct {
	c     *net.UnixConn
	local string
}

func (c *Control) dial(dev string) (*conn, error) {
	local := filepath.Join(c.Dir, fmt.Sprintf(".cored-%d-%d", os.Getpid(), time.Now().UnixNano()))
	u, err := net.DialUnix("unixgram", &net.UnixAddr{Name: local, Net: "unixgram"}, &net.UnixAddr{Name: filepath.Join(c.Dir, dev), Net: "unixgram"})
	if err != nil {
		return nil, err
	}
	return &conn{u, local}, nil
}

func (k *conn) close() {
	k.c.Close()
	os.Remove(k.local)
}

func (k *conn) read(deadline time.Time) (string, error) {
	buf := make([]byte, 4096)
	k.c.SetReadDeadline(deadline)
	n, err := k.c.Read(buf)
	if err != nil {
		return "", err
	}
	return string(buf[:n]), nil
}

func (k *conn) request(cmd string) (string, error) {
	if _, err := k.c.Write([]byte(cmd)); err != nil {
		return "", err
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		r, err := k.read(deadline)
		if err != nil {
			return "", err
		}
		if !strings.HasPrefix(r, "<") {
			return strings.TrimSpace(r), nil
		}
	}
}

func field(line, key string) string {
	i := strings.Index(line, " "+key+"=")
	if i < 0 {
		return ""
	}
	rest := line[i+len(key)+2:]
	if strings.HasPrefix(rest, `"`) {
		end := strings.Index(rest[1:], `"`)
		if end < 0 {
			return ""
		}
		return rest[1 : end+1]
	}
	if sp := strings.IndexByte(rest, ' '); sp >= 0 {
		return rest[:sp]
	}
	return rest
}

func ParseStarted(line string) (Group, bool) {
	i := strings.Index(line, "P2P-GROUP-STARTED ")
	if i < 0 {
		return Group{}, false
	}
	parts := strings.Fields(line[i:])
	if len(parts) < 3 || parts[2] != "GO" {
		return Group{}, false
	}
	return Group{parts[1], field(line, "ssid"), field(line, "passphrase"), field(line, "freq")}, true
}

func (c *Control) Start(timeout time.Duration) (Group, error) {
	dev, err := c.device()
	if err != nil {
		return Group{}, err
	}
	k, err := c.dial(dev)
	if err != nil {
		return Group{}, err
	}
	defer k.close()
	if r, err := k.request("ATTACH"); err != nil || r != "OK" {
		return Group{}, ErrUnsupported
	}
	if r, err := k.request("P2P_GROUP_ADD"); err != nil || r != "OK" {
		return Group{}, ErrUnsupported
	}
	deadline := time.Now().Add(timeout)
	for {
		line, err := k.read(deadline)
		if err != nil {
			return Group{}, ErrTimeout
		}
		if g, ok := ParseStarted(line); ok {
			return g, nil
		}
		if strings.Contains(line, "P2P-GROUP-FORMATION-FAILURE") {
			return Group{}, ErrTimeout
		}
	}
}

func (c *Control) Stop(g Group) error {
	dev, err := c.device()
	if err != nil {
		return err
	}
	k, err := c.dial(dev)
	if err != nil {
		return err
	}
	defer k.close()
	if r, err := k.request("P2P_GROUP_REMOVE " + g.Interface); err != nil || r != "OK" {
		return fmt.Errorf("direct connection not stopped: %s %v", r, err)
	}
	return nil
}
