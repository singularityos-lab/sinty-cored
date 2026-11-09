package vault

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/singularityos-lab/sinty-cored/internal/pair"
)

const (
	magic     = "SINTYCOREBACKUP1\n"
	saltSize  = 16
	maxBackup = 1 << 20
)

var (
	ErrUnavailable = errors.New("no TPM")
	ErrSealBroken  = errors.New("the TPM no longer opens the Core identity")
	ErrNotBackup   = errors.New("not a Sinty Core backup")
	ErrWrongCode   = errors.New("wrong recovery code")
)

type TPM interface {
	Seal(key []byte, pin string) (pub, priv []byte, err error)
	Unseal(pin string, pub, priv []byte) ([]byte, error)
}

type Helper struct {
	Path    string
	WorkDir string
}

func NewHelper(workDir string) *Helper {
	return &Helper{Path: "/usr/libexec/sintykey-tpm", WorkDir: workDir}
}

func (h *Helper) run(args ...string) error {
	if _, err := os.Stat(h.Path); err != nil {
		return ErrUnavailable
	}
	cmd := exec.Command(h.Path, args...)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "SINTYKEY_SEAL_PCRS=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "LC_ALL=C")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %v: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (h *Helper) scratch() (string, func(), error) {
	if err := os.MkdirAll(h.WorkDir, 0o700); err != nil {
		return "", nil, err
	}
	dir, err := os.MkdirTemp(h.WorkDir, "op-")
	if err != nil {
		return "", nil, err
	}
	return dir, func() { wipeDir(dir) }, nil
}

func (h *Helper) Seal(key []byte, pin string) ([]byte, []byte, error) {
	dir, done, err := h.scratch()
	if err != nil {
		return nil, nil, err
	}
	defer done()
	kf, pf := filepath.Join(dir, "key"), filepath.Join(dir, "pin")
	pub, priv := filepath.Join(dir, "pub"), filepath.Join(dir, "priv")
	if err := os.WriteFile(kf, key, 0o600); err != nil {
		return nil, nil, err
	}
	if err := os.WriteFile(pf, []byte(pin), 0o600); err != nil {
		return nil, nil, err
	}
	if err := h.run("seal", kf, pf, pub, priv); err != nil {
		return nil, nil, err
	}
	a, err := os.ReadFile(pub)
	if err != nil {
		return nil, nil, err
	}
	b, err := os.ReadFile(priv)
	return a, b, err
}

func (h *Helper) Unseal(pin string, pub, priv []byte) ([]byte, error) {
	dir, done, err := h.scratch()
	if err != nil {
		return nil, err
	}
	defer done()
	pf, pubf, privf, out := filepath.Join(dir, "pin"), filepath.Join(dir, "pub"), filepath.Join(dir, "priv"), filepath.Join(dir, "out")
	for name, data := range map[string][]byte{pf: []byte(pin), pubf: pub, privf: priv} {
		if err := os.WriteFile(name, data, 0o600); err != nil {
			return nil, err
		}
	}
	if err := h.run("unseal", pf, pubf, privf, out); err != nil {
		return nil, err
	}
	return os.ReadFile(out)
}

func wipeDir(dir string) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		wipe(filepath.Join(dir, e.Name()))
	}
	os.RemoveAll(dir)
}

func wipe(path string) {
	if fi, err := os.Stat(path); err == nil && fi.Mode().IsRegular() {
		os.WriteFile(path, make([]byte, fi.Size()), 0o600)
	}
	os.Remove(path)
}

type sealed struct {
	Pub  []byte `json:"pub"`
	Priv []byte `json:"priv"`
}

type Store struct {
	Dir string
	TPM TPM
}

func (s Store) plain() string  { return filepath.Join(s.Dir, "identity") }
func (s Store) sealed() string { return filepath.Join(s.Dir, "identity.sealed") }
func (s Store) lost() string   { return filepath.Join(s.Dir, "identity.sealed.lost") }

func (s Store) Lost() bool {
	_, err := os.Stat(s.lost())
	return err == nil
}

func (s Store) unseal(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if s.TPM == nil {
		return nil, ErrSealBroken
	}
	var blob sealed
	if err := json.Unmarshal(data, &blob); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSealBroken, err)
	}
	seed, err := s.TPM.Unseal("", blob.Pub, blob.Priv)
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("%w: %v", ErrSealBroken, err)
	}
	return seed, nil
}

func (s Store) Reclaim() bool {
	if !s.Lost() {
		return false
	}
	if _, err := s.unseal(s.lost()); err != nil {
		return false
	}
	if err := os.Rename(s.lost(), s.sealed()); err != nil {
		return false
	}
	return true
}

func (s Store) Recover() (pair.Identity, bool, error) {
	if err := os.Rename(s.sealed(), s.lost()); err != nil {
		return pair.Identity{}, false, err
	}
	return s.Load()
}

func (s Store) Adopt(seed []byte) (bool, error) {
	sealed, err := s.Save(seed)
	if err == nil {
		os.Remove(s.lost())
	}
	return sealed, err
}

func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".new"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s Store) Load() (pair.Identity, bool, error) {
	seed, err := s.unseal(s.sealed())
	if err == nil {
		return pair.Identity{Priv: ed25519.NewKeyFromSeed(seed)}, true, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return pair.Identity{}, false, err
	}
	seed, err = os.ReadFile(s.plain())
	switch {
	case err == nil && len(seed) == ed25519.SeedSize:
	case err == nil || errors.Is(err, os.ErrNotExist):
		seed = make([]byte, ed25519.SeedSize)
		if _, err := rand.Read(seed); err != nil {
			return pair.Identity{}, false, err
		}
	default:
		return pair.Identity{}, false, err
	}
	sealedOK, err := s.Save(seed)
	if err != nil {
		return pair.Identity{}, false, err
	}
	return pair.Identity{Priv: ed25519.NewKeyFromSeed(seed)}, sealedOK, nil
}

func (s Store) Save(seed []byte) (bool, error) {
	if len(seed) != ed25519.SeedSize {
		return false, errors.New("an identity seed is 32 bytes")
	}
	if s.TPM != nil {
		pub, priv, err := s.TPM.Seal(seed, "")
		if err == nil {
			data, _ := json.Marshal(sealed{pub, priv})
			if err := writeAtomic(s.sealed(), data); err != nil {
				return false, err
			}
			wipe(s.plain())
			return true, nil
		}
	}
	if err := writeAtomic(s.plain(), seed); err != nil {
		return false, err
	}
	os.Remove(s.sealed())
	return false, nil
}

type Contents struct {
	Seed    []byte            `json:"seed"`
	Name    string            `json:"name"`
	Screens map[string]string `json:"screens"`
	Created time.Time         `json:"created"`
}

func (c Contents) Identity() pair.Identity {
	return pair.Identity{Priv: ed25519.NewKeyFromSeed(c.Seed)}
}

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

func RecoveryCode() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	var out []byte
	var acc uint32
	bits, emitted := 0, 0
	for _, b := range raw {
		acc = acc<<8 | uint32(b)
		bits += 8
		for bits >= 5 {
			bits -= 5
			if emitted != 0 && emitted%5 == 0 {
				out = append(out, '-')
			}
			out = append(out, crockford[(acc>>bits)&0x1f])
			emitted++
		}
	}
	return string(out), nil
}

func NormalizeCode(code string) string {
	var out []byte
	n := 0
	for _, r := range code {
		switch {
		case r >= 'a' && r <= 'z':
			r -= 'a' - 'A'
		case r == ' ' || r == '-':
			continue
		}
		switch r {
		case 'O':
			r = '0'
		case 'I', 'L':
			r = '1'
		}
		if n != 0 && n%5 == 0 {
			out = append(out, '-')
		}
		out = append(out, byte(r))
		n++
	}
	return string(out)
}

func aead(code string, salt []byte) (cipher.AEAD, error) {
	key, err := hkdf.Key(sha256.New, []byte(NormalizeCode(code)), salt, "sinty-core backup v1", 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func Backup(w io.Writer, c Contents) (string, error) {
	if len(c.Seed) != ed25519.SeedSize {
		return "", errors.New("an identity seed is 32 bytes")
	}
	code, err := RecoveryCode()
	if err != nil {
		return "", err
	}
	plain, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	head := make([]byte, saltSize+12)
	if _, err := rand.Read(head); err != nil {
		return "", err
	}
	g, err := aead(code, head[:saltSize])
	if err != nil {
		return "", err
	}
	out := append([]byte(magic), head...)
	out = g.Seal(out, head[saltSize:], plain, []byte(magic))
	if _, err := w.Write(out); err != nil {
		return "", err
	}
	return code, nil
}

func Restore(r io.Reader, code string) (Contents, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxBackup))
	if err != nil {
		return Contents{}, err
	}
	if !bytes.HasPrefix(data, []byte(magic)) || len(data) < len(magic)+saltSize+12+16 {
		return Contents{}, ErrNotBackup
	}
	data = data[len(magic):]
	g, err := aead(code, data[:saltSize])
	if err != nil {
		return Contents{}, err
	}
	plain, err := g.Open(nil, data[saltSize:saltSize+12], data[saltSize+12:], []byte(magic))
	if err != nil {
		return Contents{}, ErrWrongCode
	}
	var c Contents
	if err := json.Unmarshal(plain, &c); err != nil || len(c.Seed) != ed25519.SeedSize {
		return Contents{}, ErrNotBackup
	}
	return c, nil
}
