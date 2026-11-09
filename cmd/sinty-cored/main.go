package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/singularityos-lab/sinty-cored/internal/ble"
	"github.com/singularityos-lab/sinty-cored/internal/gadget"
	"github.com/singularityos-lab/sinty-cored/internal/p2p"
	"github.com/singularityos-lab/sinty-cored/internal/pair"
	"github.com/singularityos-lab/sinty-cored/internal/service"
)

func hostname() string {
	if data, err := os.ReadFile("/etc/hostname"); err == nil {
		if h := strings.TrimSpace(string(data)); h != "" {
			return h
		}
	}
	h, _ := os.Hostname()
	return h
}

func main() {
	state := flag.String("state", "/var/lib/sinty-cored", "identity and Core disk")
	name := flag.String("name", "", "name shown on screens, the host name when empty")
	port := flag.Int("port", service.ScreenPort, "TCP port for screens")
	noBLE := flag.Bool("no-ble", false, "do not announce the Core over Bluetooth")
	diskSize := flag.Int64("disk-size", 32<<30, "size of the Core disk in bytes")
	runtime := flag.String("runtime", "/run/user", "directory holding the runtime directory of each user")
	flag.Parse()
	log.SetFlags(0)
	log.SetPrefix("sinty-cored: ")
	if *name == "" {
		*name = hostname()
	}
	if os.Getenv("DBUS_SYSTEM_BUS_ADDRESS") == "" {
		os.Setenv("DBUS_SYSTEM_BUS_ADDRESS", "unix:path=/run/dbus/system_bus_socket")
	}
	id, err := pair.LoadIdentity(filepath.Join(*state, "identity"))
	if err != nil {
		log.Fatalf("identity: %v", err)
	}

	var conn *dbus.Conn
	for i := 0; ; i++ {
		conn, err = dbus.ConnectSystemBus()
		if err == nil {
			break
		}
		if i%10 == 0 {
			log.Printf("system bus not ready: %v", err)
		}
		time.Sleep(time.Second)
	}

	desktop := service.NewWayland()
	desktop.RunDir = *runtime
	cfg := service.Config{
		Identity: id,
		Name:     *name,
		Trust:    service.KeyTrust{Conn: conn},
		Power:    service.Powerd{Conn: conn},
		Desktop:  desktop,
		Direct:   p2p.New(),
	}
	g := gadget.New()
	if _, err := g.Port(); err == nil {
		cfg.USB = &service.GadgetUSB{G: g, Disk: filepath.Join(*state, "core-disk.img"), Size: *diskSize, Serial: id.ID()}
	}
	var periph *ble.Peripheral
	var svc *service.Service
	if !*noBLE && cfg.Power.Form() == "headless" {
		periph = ble.New(conn, *name, func(device string, value []byte) { svc.Phone(device, string(value)) })
		cfg.OnPresence = func(v []byte) { periph.SetPresence(v) }
	}
	svc = service.New(cfg)
	if periph != nil {
		if err := periph.Start(); err != nil {
			log.Printf("Bluetooth not announced: %v", err)
		}
	}
	if err := service.Export(conn, svc); err != nil {
		log.Fatal(err)
	}
	reply, err := conn.RequestName(service.Name, dbus.NameFlagDoNotQueue)
	if err != nil || reply != dbus.RequestNameReplyPrimaryOwner {
		log.Fatalf("cannot own %s: %v", service.Name, err)
	}
	go svc.Follow(conn)

	l, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		log.Fatal(err)
	}
	go func() { log.Fatal(svc.Serve(l)) }()
	pc, err := net.ListenPacket("udp", fmt.Sprintf(":%d", service.DiscoveryPort))
	if err != nil {
		log.Printf("screens must be given the address, discovery is off: %v", err)
	} else {
		go svc.Discovery(pc, *port)
	}
	log.Printf("serving %s as %q", service.Name, *name)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	<-sigs
	if periph != nil {
		periph.Stop()
	}
}
