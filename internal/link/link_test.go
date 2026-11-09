package link

import (
	"net"
	"testing"
)

func TestChoose(t *testing.T) {
	cases := []struct {
		s    State
		want Route
	}{
		{State{Cable: true, LAN: true, Direct: true}, Cable},
		{State{LAN: true, Direct: true}, LAN},
		{State{Direct: true}, Direct},
		{State{}, None},
	}
	for _, c := range cases {
		if got := Choose(c.s); got != c.want {
			t.Fatalf("%+v: %s, want %s", c.s, got, c.want)
		}
	}
}

func TestOnLAN(t *testing.T) {
	ip := net.ParseIP
	if OnLAN([]Iface{
		{"lo", true, []net.IP{ip("127.0.0.1")}},
		{"wlan0", false, []net.IP{ip("192.168.1.4")}},
		{"p2p-wlan0-0", true, []net.IP{ip("192.168.49.1")}},
		{"usb0", true, []net.IP{ip("10.55.0.1")}},
		{"eth0", true, []net.IP{ip("169.254.3.3")}},
	}) {
		t.Fatal("no real network, yet on LAN")
	}
	if !OnLAN([]Iface{{"wlan0", true, []net.IP{ip("192.168.1.4")}}}) {
		t.Fatal("wlan0 with an address is a LAN")
	}
}
