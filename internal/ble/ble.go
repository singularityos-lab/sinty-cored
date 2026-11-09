package ble

import (
	"errors"
	"sync"

	"github.com/godbus/dbus/v5"
)

const (
	ServiceUUID  = "7d5c0001-5e1f-4c1b-9a43-53696e747901"
	PresenceUUID = "7d5c0002-5e1f-4c1b-9a43-53696e747901"
	ControlUUID  = "7d5c0003-5e1f-4c1b-9a43-53696e747901"

	bluez       = "org.bluez"
	gattService = "org.bluez.GattService1"
	gattChar    = "org.bluez.GattCharacteristic1"
	advIface    = "org.bluez.LEAdvertisement1"
	propsIface  = "org.freedesktop.DBus.Properties"
	omIface     = "org.freedesktop.DBus.ObjectManager"

	root     = dbus.ObjectPath("/dev/sinty/Core1/gatt")
	service  = root + "/service0"
	presence = service + "/char0"
	control  = service + "/char1"
	advert   = dbus.ObjectPath("/dev/sinty/Core1/advertisement0")
)

var ErrNoAdapter = errors.New("no Bluetooth adapter is available")

type Peripheral struct {
	conn      *dbus.Conn
	adapter   dbus.ObjectPath
	name      string
	onControl func(device string, value []byte)

	mu       sync.Mutex
	value    []byte
	notifies bool
}

func New(conn *dbus.Conn, name string, onControl func(device string, value []byte)) *Peripheral {
	return &Peripheral{conn: conn, name: name, onControl: onControl}
}

func (p *Peripheral) props(path dbus.ObjectPath) map[string]map[string]dbus.Variant {
	switch path {
	case service:
		return map[string]map[string]dbus.Variant{gattService: {
			"UUID":    dbus.MakeVariant(ServiceUUID),
			"Primary": dbus.MakeVariant(true),
		}}
	case presence:
		p.mu.Lock()
		v := append([]byte{}, p.value...)
		p.mu.Unlock()
		return map[string]map[string]dbus.Variant{gattChar: {
			"UUID":    dbus.MakeVariant(PresenceUUID),
			"Service": dbus.MakeVariant(service),
			"Flags":   dbus.MakeVariant([]string{"encrypt-read", "notify"}),
			"Value":   dbus.MakeVariant(v),
		}}
	case control:
		return map[string]map[string]dbus.Variant{gattChar: {
			"UUID":    dbus.MakeVariant(ControlUUID),
			"Service": dbus.MakeVariant(service),
			"Flags":   dbus.MakeVariant([]string{"encrypt-authenticated-write"}),
		}}
	case advert:
		return map[string]map[string]dbus.Variant{advIface: {
			"Type":         dbus.MakeVariant("peripheral"),
			"ServiceUUIDs": dbus.MakeVariant([]string{ServiceUUID}),
			"LocalName":    dbus.MakeVariant(p.name),
		}}
	}
	return nil
}

type manager struct{ p *Peripheral }

func (m manager) GetManagedObjects() (map[dbus.ObjectPath]map[string]map[string]dbus.Variant, *dbus.Error) {
	out := map[dbus.ObjectPath]map[string]map[string]dbus.Variant{}
	for _, path := range []dbus.ObjectPath{service, presence, control} {
		out[path] = m.p.props(path)
	}
	return out, nil
}

type properties struct {
	p    *Peripheral
	path dbus.ObjectPath
}

func (o properties) Get(iface, name string) (dbus.Variant, *dbus.Error) {
	v, ok := o.p.props(o.path)[iface][name]
	if !ok {
		return dbus.Variant{}, dbus.MakeFailedError(errors.New("no such property"))
	}
	return v, nil
}

func (o properties) GetAll(iface string) (map[string]dbus.Variant, *dbus.Error) {
	return o.p.props(o.path)[iface], nil
}

func (o properties) Set(string, string, dbus.Variant) *dbus.Error {
	return dbus.MakeFailedError(errors.New("read only"))
}

type presenceChar struct{ p *Peripheral }

func (c presenceChar) ReadValue(map[string]dbus.Variant) ([]byte, *dbus.Error) {
	c.p.mu.Lock()
	defer c.p.mu.Unlock()
	return append([]byte{}, c.p.value...), nil
}

func (c presenceChar) StartNotify() *dbus.Error {
	c.p.mu.Lock()
	c.p.notifies = true
	c.p.mu.Unlock()
	return nil
}

func (c presenceChar) StopNotify() *dbus.Error {
	c.p.mu.Lock()
	c.p.notifies = false
	c.p.mu.Unlock()
	return nil
}

type controlChar struct{ p *Peripheral }

func (c controlChar) WriteValue(value []byte, options map[string]dbus.Variant) *dbus.Error {
	device := ""
	if v, ok := options["device"]; ok {
		if path, ok := v.Value().(dbus.ObjectPath); ok {
			device = string(path)
		}
	}
	if c.p.onControl != nil {
		c.p.onControl(device, append([]byte{}, value...))
	}
	return nil
}

type advertisement struct{}

func (advertisement) Release() *dbus.Error { return nil }

func findAdapter(conn *dbus.Conn) (dbus.ObjectPath, error) {
	var objects map[dbus.ObjectPath]map[string]map[string]dbus.Variant
	if err := conn.Object(bluez, "/").Call(omIface+".GetManagedObjects", 0).Store(&objects); err != nil {
		return "", ErrNoAdapter
	}
	best := dbus.ObjectPath("")
	for path, ifaces := range objects {
		if _, ok := ifaces["org.bluez.GattManager1"]; !ok {
			continue
		}
		if _, ok := ifaces["org.bluez.LEAdvertisingManager1"]; !ok {
			continue
		}
		if best == "" || path < best {
			best = path
		}
	}
	if best == "" {
		return "", ErrNoAdapter
	}
	return best, nil
}

func (p *Peripheral) Start() error {
	adapter, err := findAdapter(p.conn)
	if err != nil {
		return err
	}
	exports := []struct {
		v     any
		path  dbus.ObjectPath
		iface string
	}{
		{manager{p}, root, omIface},
		{properties{p, service}, service, propsIface},
		{properties{p, presence}, presence, propsIface},
		{presenceChar{p}, presence, gattChar},
		{properties{p, control}, control, propsIface},
		{controlChar{p}, control, gattChar},
		{properties{p, advert}, advert, propsIface},
		{advertisement{}, advert, advIface},
	}
	for _, e := range exports {
		if err := p.conn.Export(e.v, e.path, e.iface); err != nil {
			return err
		}
	}
	obj := p.conn.Object(bluez, adapter)
	if err := obj.Call("org.bluez.GattManager1.RegisterApplication", 0, root, map[string]dbus.Variant{}).Err; err != nil {
		return err
	}
	if err := obj.Call("org.bluez.LEAdvertisingManager1.RegisterAdvertisement", 0, advert, map[string]dbus.Variant{}).Err; err != nil {
		obj.Call("org.bluez.GattManager1.UnregisterApplication", 0, root)
		return err
	}
	p.adapter = adapter
	return nil
}

func (p *Peripheral) SetPresence(value []byte) error {
	p.mu.Lock()
	p.value = append([]byte{}, value...)
	notify := p.notifies
	p.mu.Unlock()
	if !notify {
		return nil
	}
	return p.conn.Emit(presence, propsIface+".PropertiesChanged", gattChar, map[string]dbus.Variant{"Value": dbus.MakeVariant(value)}, []string{})
}

func (p *Peripheral) Stop() {
	if p.adapter == "" {
		return
	}
	obj := p.conn.Object(bluez, p.adapter)
	obj.Call("org.bluez.LEAdvertisingManager1.UnregisterAdvertisement", 0, advert)
	obj.Call("org.bluez.GattManager1.UnregisterApplication", 0, root)
	p.adapter = ""
}
