package channel

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"sync"

	"github.com/singularityos-lab/sinty-cored/internal/pair"
	"github.com/singularityos-lab/sinty-cored/internal/wire"
)

const (
	msgOpen byte = iota + 10
	msgAccept
	msgProof
	msgRefused
	msgData
)

var (
	ErrUntrusted = errors.New("this screen is not paired with the Core")
	ErrIdentity  = errors.New("the other side could not prove its identity")
	ErrDamaged   = errors.New("a message was changed, replayed or dropped")
)

type Conn struct {
	rw    io.ReadWriteCloser
	peer  pair.Peer
	send  cipher.AEAD
	recv  cipher.AEAD
	sendN uint64
	recvN uint64
	wmu   sync.Mutex
	rmu   sync.Mutex
}

type open struct {
	Identity  []byte
	Ephemeral []byte
	Nonce     []byte
	Layout    string
}

type accept struct {
	Identity  []byte
	Ephemeral []byte
	Nonce     []byte
	Signature []byte
}

type proof struct {
	Signature []byte
}

func transcript(screenID, coreID, screenEph, coreEph, screenNonce, coreNonce []byte) []byte {
	h := sha256.New()
	h.Write([]byte("sinty-core session v1"))
	for _, part := range [][]byte{screenID, coreID, screenEph, coreEph, screenNonce, coreNonce} {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(part)))
		h.Write(n[:])
		h.Write(part)
	}
	return h.Sum(nil)
}

func aead(shared, t []byte, label string) (cipher.AEAD, error) {
	key, err := hkdf.Key(sha256.New, shared, t, label, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func keys(c *Conn, shared, t []byte, screen bool) error {
	toCore, err := aead(shared, t, "sinty-core screen to core")
	if err != nil {
		return err
	}
	toScreen, err := aead(shared, t, "sinty-core core to screen")
	if err != nil {
		return err
	}
	if screen {
		c.send, c.recv = toCore, toScreen
	} else {
		c.send, c.recv = toScreen, toCore
	}
	return nil
}

func sig(role string, t []byte) []byte {
	return append([]byte("sinty-core session "+role+" "), t...)
}

func Screen(rw io.ReadWriteCloser, me pair.Identity, core ed25519.PublicKey, layout string) (*Conn, error) {
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	ns := make([]byte, 16)
	if _, err := rand.Read(ns); err != nil {
		return nil, err
	}
	ephPub := eph.PublicKey().Bytes()
	if err := wire.WriteJSON(rw, msgOpen, open{me.Public(), ephPub, ns, layout}); err != nil {
		return nil, err
	}
	kind, data, err := wire.Read(rw)
	if err != nil {
		return nil, err
	}
	if kind == msgRefused {
		return nil, ErrUntrusted
	}
	var a accept
	if kind != msgAccept || json.Unmarshal(data, &a) != nil || len(a.Nonce) != 16 {
		return nil, ErrIdentity
	}
	if !ed25519.PublicKey(a.Identity).Equal(core) {
		return nil, ErrIdentity
	}
	t := transcript(me.Public(), a.Identity, ephPub, a.Ephemeral, ns, a.Nonce)
	if !ed25519.Verify(core, sig("core", t), a.Signature) {
		return nil, ErrIdentity
	}
	corePub, err := ecdh.X25519().NewPublicKey(a.Ephemeral)
	if err != nil {
		return nil, ErrIdentity
	}
	shared, err := eph.ECDH(corePub)
	if err != nil {
		return nil, ErrIdentity
	}
	if err := wire.WriteJSON(rw, msgProof, proof{ed25519.Sign(me.Priv, sig("screen", t))}); err != nil {
		return nil, err
	}
	c := &Conn{rw: rw, peer: pair.Peer{ID: pair.ID(core), Public: core}}
	return c, keys(c, shared, t, true)
}

type Request struct {
	Peer   pair.Peer
	Layout string
}

func Core(rw io.ReadWriteCloser, me pair.Identity, trusted func(id string) (string, bool)) (*Conn, Request, error) {
	var o open
	if err := wire.ReadJSON(rw, msgOpen, &o); err != nil {
		return nil, Request{}, err
	}
	if len(o.Identity) != ed25519.PublicKeySize || len(o.Nonce) != 16 {
		return nil, Request{}, ErrIdentity
	}
	id := pair.ID(o.Identity)
	name, ok := trusted(id)
	if !ok {
		wire.Write(rw, msgRefused, nil)
		return nil, Request{}, ErrUntrusted
	}
	screenPub, err := ecdh.X25519().NewPublicKey(o.Ephemeral)
	if err != nil {
		return nil, Request{}, ErrIdentity
	}
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, Request{}, err
	}
	nc := make([]byte, 16)
	if _, err := rand.Read(nc); err != nil {
		return nil, Request{}, err
	}
	ephPub := eph.PublicKey().Bytes()
	t := transcript(o.Identity, me.Public(), o.Ephemeral, ephPub, o.Nonce, nc)
	if err := wire.WriteJSON(rw, msgAccept, accept{me.Public(), ephPub, nc, ed25519.Sign(me.Priv, sig("core", t))}); err != nil {
		return nil, Request{}, err
	}
	var p proof
	if err := wire.ReadJSON(rw, msgProof, &p); err != nil {
		return nil, Request{}, err
	}
	if !ed25519.Verify(o.Identity, sig("screen", t), p.Signature) {
		return nil, Request{}, ErrIdentity
	}
	shared, err := eph.ECDH(screenPub)
	if err != nil {
		return nil, Request{}, ErrIdentity
	}
	peer := pair.Peer{ID: id, Name: name, Public: ed25519.PublicKey(o.Identity)}
	c := &Conn{rw: rw, peer: peer}
	return c, Request{peer, o.Layout}, keys(c, shared, t, false)
}

func nonceFor(n uint64) []byte {
	b := make([]byte, 12)
	binary.BigEndian.PutUint64(b[4:], n)
	return b
}

func (c *Conn) Peer() pair.Peer { return c.peer }

func (c *Conn) Send(kind byte, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	plain := make([]byte, 1+len(payload))
	plain[0] = kind
	copy(plain[1:], payload)
	sealed := c.send.Seal(nil, nonceFor(c.sendN), plain, nil)
	c.sendN++
	return wire.Write(c.rw, msgData, sealed)
}

func (c *Conn) Recv() (byte, []byte, error) {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	kind, data, err := wire.Read(c.rw)
	if err != nil {
		return 0, nil, err
	}
	if kind != msgData {
		return 0, nil, ErrDamaged
	}
	plain, err := c.recv.Open(nil, nonceFor(c.recvN), data, nil)
	if err != nil || len(plain) == 0 {
		return 0, nil, ErrDamaged
	}
	c.recvN++
	return plain[0], plain[1:], nil
}

func (c *Conn) Close() error { return c.rw.Close() }
