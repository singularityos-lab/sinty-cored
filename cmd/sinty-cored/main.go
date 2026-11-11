package main

import (
	"errors"
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
	"github.com/singularityos-lab/sinty-cored/internal/service"
	"github.com/singularityos-lab/sinty-cored/internal/v4l2"
	"github.com/singularityos-lab/sinty-cored/internal/vault"
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
	codecDir := flag.String("codecs", "/dev", "directory with the video codec devices, none when empty")
	codecHelper := flag.String("codec-helper", v4l2.DefaultHelper, "helper that reaches codecs behind a vendor V4L2 library, none when empty")
	flag.Parse()
	log.SetFlags(0)
	log.SetPrefix("sinty-cored: ")
	if *name == "" {
		*name = hostname()
	}
	if os.Getenv("DBUS_SYSTEM_BUS_ADDRESS") == "" {
		os.Setenv("DBUS_SYSTEM_BUS_ADDRESS", "unix:path=/run/dbus/system_bus_socket")
	}
	store := vault.Store{Dir: *state, TPM: vault.NewHelper(filepath.Join(*state, "tpm"))}
	if store.Reclaim() {
		log.Printf("identity: the TPM opened the Core identity again")
	}
	id, sealed, err := store.Load()
	for i := 0; i < 5 && errors.Is(err, vault.ErrSealBroken); i++ {
		time.Sleep(2 * time.Second)
		id, sealed, err = store.Load()
	}
	if errors.Is(err, vault.ErrSealBroken) {
		log.Printf("identity: %v, starting with a new one until it is restored from a backup", err)
		id, sealed, err = store.Recover()
	}
	if err != nil {
		log.Fatalf("identity: %v", err)
	}
	kept := service.IdentityFile
	switch {
	case store.Lost():
		kept = service.IdentityReplaced
	case sealed:
		kept = service.IdentityTPM
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
		Kept:     kept,
		Vault:    store,
		Name:     *name,
		Trust:    service.KeyTrust{Conn: conn},
		Power:    service.Powerd{Conn: conn},
		Desktop:  desktop,
		Direct:   p2p.New(),
	}
	if *codecDir != "" {
		if v := service.NewV4L2(*codecDir, *codecHelper); v.Available() {
			cfg.Codecs = v
		}
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
