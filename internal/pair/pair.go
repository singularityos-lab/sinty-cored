package pair

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/singularityos-lab/sinty-cored/internal/wire"
)

const (
	msgHello byte = iota + 1
	msgCore
	msgReveal
	msgConfirm
	msgReject
	msgScreenConfirm
)

var (
	ErrWrongCode = errors.New("the code does not match")
	ErrCommit    = errors.New("the screen changed its key during pairing")
	ErrPeer      = errors.New("the other side could not prove its identity")
	ErrRejected  = errors.New("pairing was refused on the Core")
)

type Identity struct {
	Priv ed25519.PrivateKey
}

func (i Identity) Public() ed25519.PublicKey { return i.Priv.Public().(ed25519.PublicKey) }

func (i Identity) ID() string { return ID(i.Public()) }

func ID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:16])
}

func LoadIdentity(path string) (Identity, error) {
	seed, err := os.ReadFile(path)
	if err == nil && len(seed) == ed25519.SeedSize {
		return Identity{ed25519.NewKeyFromSeed(seed)}, nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Identity{}, err
	}
	seed = make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return Identity{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return Identity{}, err
	}
	tmp := path + ".new"
	if err := os.WriteFile(tmp, seed, 0o600); err != nil {
		return Identity{}, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return Identity{}, err
	}
	return Identity{ed25519.NewKeyFromSeed(seed)}, nil
}

type Peer struct {
	ID     string
	Name   string
	Public ed25519.PublicKey
}

type hello struct {
	Name     string
	Identity []byte
	Commit   []byte
}

type coreHello struct {
	Name      string
	Identity  []byte
	Ephemeral []byte
	Nonce     []byte
}

type reveal struct {
	Ephemeral []byte
	Nonce     []byte
}

type confirm struct {
	Signature []byte
	MAC       []byte
}

func commitment(eph, nonce []byte) []byte {
	h := sha256.New()
	h.Write([]byte("sinty-core commit v1"))
	h.Write(eph)
	h.Write(nonce)
	return h.Sum(nil)
}

func transcript(screenID, coreID, screenEph, coreEph, screenNonce, coreNonce []byte) []byte {
	h := sha256.New()
	h.Write([]byte("sinty-core pair v1"))
	for _, part := range [][]byte{screenID, coreID, screenEph, coreEph, screenNonce, coreNonce} {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(part)))
		h.Write(n[:])
		h.Write(part)
	}
	return h.Sum(nil)
}

type secrets struct {
	code    string
	confirm []byte
}

func derive(shared, t []byte) (secrets, error) {
	raw, err := hkdf.Key(sha256.New, shared, t, "sinty-core code", 4)
	if err != nil {
		return secrets{}, err
	}
	key, err := hkdf.Key(sha256.New, shared, t, "sinty-core confirm", 32)
	if err != nil {
		return secrets{}, err
	}
	return secrets{fmt.Sprintf("%06d", binary.BigEndian.Uint32(raw)%1000000), key}, nil
}

func mac(key []byte, role string, t []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(role))
	m.Write(t)
	return m.Sum(nil)
}

func signed(role string, t []byte) []byte {
	return append([]byte("sinty-core "+role+" "), t...)
}

func nonce() ([]byte, error) {
	n := make([]byte, 16)
	_, err := rand.Read(n)
	return n, err
}

func Screen(conn io.ReadWriter, me Identity, name string, show func(code string)) (Peer, error) {
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return Peer{}, err
	}
	ns, err := nonce()
	if err != nil {
		return Peer{}, err
	}
	ephPub := eph.PublicKey().Bytes()
	if err := wire.WriteJSON(conn, msgHello, hello{name, me.Public(), commitment(ephPub, ns)}); err != nil {
		return Peer{}, err
	}
	var ch coreHello
	if err := wire.ReadJSON(conn, msgCore, &ch); err != nil {
		return Peer{}, err
	}
	if len(ch.Identity) != ed25519.PublicKeySize || len(ch.Nonce) != 16 {
		return Peer{}, ErrPeer
	}
	corePub, err := ecdh.X25519().NewPublicKey(ch.Ephemeral)
	if err != nil {
		return Peer{}, ErrPeer
	}
	if err := wire.WriteJSON(conn, msgReveal, reveal{ephPub, ns}); err != nil {
		return Peer{}, err
	}
	shared, err := eph.ECDH(corePub)
	if err != nil {
		return Peer{}, ErrPeer
	}
	t := transcript(me.Public(), ch.Identity, ephPub, ch.Ephemeral, ns, ch.Nonce)
	sec, err := derive(shared, t)
	if err != nil {
		return Peer{}, err
	}
	show(sec.code)
	kind, data, err := wire.Read(conn)
	if err != nil {
		return Peer{}, err
	}
	if kind == msgReject {
		return Peer{}, ErrRejected
	}
	if kind != msgConfirm {
		return Peer{}, ErrPeer
	}
	var c confirm
	if err := json.Unmarshal(data, &c); err != nil {
		return Peer{}, err
	}
	if !hmac.Equal(c.MAC, mac(sec.confirm, "core", t)) || !ed25519.Verify(ch.Identity, signed("core", t), c.Signature) {
		return Peer{}, ErrPeer
	}
	reply := confirm{ed25519.Sign(me.Priv, signed("screen", t)), mac(sec.confirm, "screen", t)}
	if err := wire.WriteJSON(conn, msgScreenConfirm, reply); err != nil {
		return Peer{}, err
	}
	return Peer{ID(ch.Identity), ch.Name, ed25519.PublicKey(ch.Identity)}, nil
}

func Core(conn io.ReadWriter, me Identity, name string, ask func(screen string) (string, error)) (Peer, error) {
	var h hello
	if err := wire.ReadJSON(conn, msgHello, &h); err != nil {
		return Peer{}, err
	}
	if len(h.Identity) != ed25519.PublicKeySize || len(h.Commit) != sha256.Size {
		return Peer{}, ErrPeer
	}
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return Peer{}, err
	}
	nc, err := nonce()
	if err != nil {
		return Peer{}, err
	}
	ephPub := eph.PublicKey().Bytes()
	if err := wire.WriteJSON(conn, msgCore, coreHello{name, me.Public(), ephPub, nc}); err != nil {
		return Peer{}, err
	}
	var r reveal
	if err := wire.ReadJSON(conn, msgReveal, &r); err != nil {
		return Peer{}, err
	}
	if len(r.Nonce) != 16 || !bytes.Equal(commitment(r.Ephemeral, r.Nonce), h.Commit) {
		return Peer{}, ErrCommit
	}
	screenPub, err := ecdh.X25519().NewPublicKey(r.Ephemeral)
	if err != nil {
		return Peer{}, ErrPeer
	}
	shared, err := eph.ECDH(screenPub)
	if err != nil {
		return Peer{}, ErrPeer
	}
	t := transcript(h.Identity, me.Public(), r.Ephemeral, ephPub, r.Nonce, nc)
	sec, err := derive(shared, t)
	if err != nil {
		return Peer{}, err
	}
	typed, err := ask(h.Name)
	if err != nil || subtle.ConstantTimeCompare([]byte(typed), []byte(sec.code)) != 1 {
		wire.Write(conn, msgReject, nil)
		if err != nil {
			return Peer{}, err
		}
		return Peer{}, ErrWrongCode
	}
	if err := wire.WriteJSON(conn, msgConfirm, confirm{ed25519.Sign(me.Priv, signed("core", t)), mac(sec.confirm, "core", t)}); err != nil {
		return Peer{}, err
	}
	var c confirm
	if err := wire.ReadJSON(conn, msgScreenConfirm, &c); err != nil {
		return Peer{}, err
	}
	if !hmac.Equal(c.MAC, mac(sec.confirm, "screen", t)) || !ed25519.Verify(h.Identity, signed("screen", t), c.Signature) {
		return Peer{}, ErrPeer
	}
	return Peer{ID(h.Identity), h.Name, ed25519.PublicKey(h.Identity)}, nil
}
