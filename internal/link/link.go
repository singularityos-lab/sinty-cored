package link

import (
	"net"
	"strings"
)

type Route string

const (
	Cable  Route = "cable"
	LAN    Route = "network"
	Direct Route = "direct"
	None   Route = "none"
)

type State struct {
	Cable  bool
	LAN    bool
	Direct bool
}

func Choose(s State) Route {
	switch {
	case s.Cable:
		return Cable
	case s.LAN:
		return LAN
	case s.Direct:
		return Direct
	}
	return None
}

type Iface struct {
	Name  string
	Up    bool
	Addrs []net.IP
}

func OnLAN(ifaces []Iface) bool {
	for _, i := range ifaces {
		if !i.Up || i.Name == "lo" || strings.HasPrefix(i.Name, "p2p-") || strings.HasPrefix(i.Name, "usb") {
			continue
		}
		for _, a := range i.Addrs {
			if a.To4() != nil && !a.IsLoopback() && !a.IsLinkLocalUnicast() {
				return true
			}
		}
	}
	return false
}

func System() []Iface {
	list, err := net.Interfaces()
	if err != nil {
		return nil
	}
	out := make([]Iface, 0, len(list))
	for _, i := range list {
		it := Iface{Name: i.Name, Up: i.Flags&net.FlagUp != 0 && i.Flags&net.FlagRunning != 0}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok {
				it.Addrs = append(it.Addrs, n.IP)
			}
		}
		out = append(out, it)
	}
	return out
}
