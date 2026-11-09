package ble

import (
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/singularityos-lab/sinty-cored/internal/bustest"
)

type fakeBlueZ struct {
	apps    chan dbus.ObjectPath
	adverts chan dbus.ObjectPath
	sender  string
}

func (f *fakeBlueZ) RegisterApplication(sender dbus.Sender, app dbus.ObjectPath, _ map[string]dbus.Variant) *dbus.Error {
	f.sender = string(sender)
	f.apps <- app
	return nil
}

func (f *fakeBlueZ) UnregisterApplication(app dbus.ObjectPath) *dbus.Error {
	f.apps <- "-" + app
	return nil
}

func (f *fakeBlueZ) RegisterAdvertisement(adv dbus.ObjectPath, _ map[string]dbus.Variant) *dbus.Error {
	f.adverts <- adv
	return nil
}

func (f *fakeBlueZ) UnregisterAdvertisement(adv dbus.ObjectPath) *dbus.Error {
	f.adverts <- "-" + adv
	return nil
}

type objects struct{}

func (objects) GetManagedObjects() (map[dbus.ObjectPath]map[string]map[string]dbus.Variant, *dbus.Error) {
	return map[dbus.ObjectPath]map[string]map[string]dbus.Variant{
		"/org/bluez/hci0": {
			"org.bluez.Adapter1":              {},
			"org.bluez.GattManager1":          {},
			"org.bluez.LEAdvertisingManager1": {},
		},
	}, nil
}

func TestNoAdapter(t *testing.T) {
	addr := bustest.Start(t)
	p := New(bustest.Connect(t, addr), "Sinty Core", nil)
	if err := p.Start(); err != ErrNoAdapter {
		t.Fatal(err)
	}
}

func TestPeripheralOverAFakeBlueZ(t *testing.T) {
	addr := bustest.Start(t)
	bz := bustest.Connect(t, addr)
	f := &fakeBlueZ{apps: make(chan dbus.ObjectPath, 4), adverts: make(chan dbus.ObjectPath, 4)}
	bz.Export(f, "/org/bluez/hci0", "org.bluez.GattManager1")
	bz.Export(f, "/org/bluez/hci0", "org.bluez.LEAdvertisingManager1")
	bz.Export(objects{}, "/", omIface)
	bz.RequestName(bluez, dbus.NameFlagDoNotQueue)

	writes := make(chan string, 2)
	core := bustest.Connect(t, addr)
	p := New(core, "Sinty Core", func(device string, value []byte) { writes <- device + " " + string(value) })
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	if app := <-f.apps; app != root {
		t.Fatalf("application %s", app)
	}
	if adv := <-f.adverts; adv != advert {
		t.Fatalf("advertisement %s", adv)
	}

	var tree map[dbus.ObjectPath]map[string]map[string]dbus.Variant
	if err := bz.Object(f.sender, root).Call(omIface+".GetManagedObjects", 0).Store(&tree); err != nil {
		t.Fatal(err)
	}
	if tree[service][gattService]["UUID"].Value() != ServiceUUID || tree[presence][gattChar]["UUID"].Value() != PresenceUUID || tree[control][gattChar]["Service"].Value() != service {
		t.Fatalf("tree %v", tree)
	}
	var name dbus.Variant
	if err := bz.Object(f.sender, advert).Call(propsIface+".Get", 0, advIface, "LocalName").Store(&name); err != nil || name.Value() != "Sinty Core" {
		t.Fatalf("advertised name %v %v", name, err)
	}

	bz.AddMatchSignal(dbus.WithMatchObjectPath(presence), dbus.WithMatchInterface(propsIface))
	sig := make(chan *dbus.Signal, 4)
	bz.Signal(sig)
	ch := bz.Object(f.sender, presence)
	if err := ch.Call(gattChar+".StartNotify", 0).Err; err != nil {
		t.Fatal(err)
	}
	if err := p.SetPresence([]byte("unlocked")); err != nil {
		t.Fatal(err)
	}
	select {
	case s := <-sig:
		changed := s.Body[1].(map[string]dbus.Variant)
		if string(changed["Value"].Value().([]byte)) != "unlocked" {
			t.Fatalf("notified %v", changed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no notification")
	}
	var read []byte
	if err := ch.Call(gattChar+".ReadValue", 0, map[string]dbus.Variant{}).Store(&read); err != nil || string(read) != "unlocked" {
		t.Fatalf("read %q %v", read, err)
	}

	opts := map[string]dbus.Variant{"device": dbus.MakeVariant(dbus.ObjectPath("/org/bluez/hci0/dev_AA"))}
	if err := bz.Object(f.sender, control).Call(gattChar+".WriteValue", 0, []byte("lock"), opts).Err; err != nil {
		t.Fatal(err)
	}
	if w := <-writes; w != "/org/bluez/hci0/dev_AA lock" {
		t.Fatalf("control %q", w)
	}

	p.Stop()
	if a, b := <-f.adverts, <-f.apps; a != "-"+advert || b != "-"+root {
		t.Fatalf("stop %s %s", a, b)
	}
}
