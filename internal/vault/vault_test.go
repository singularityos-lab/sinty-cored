package vault

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeTPM struct {
	name    string
	missing bool
	broken  bool
}

func (f *fakeTPM) mask() []byte {
	sum := sha256.Sum256([]byte(f.name))
	return sum[:]
}

func (f *fakeTPM) Seal(key []byte, pin string) ([]byte, []byte, error) {
	if f.missing {
		return nil, nil, ErrUnavailable
	}
	if f.broken {
		return nil, nil, errors.New("tcti init failed")
	}
	if pin != "" {
		return nil, nil, errors.New("the Core seals without a PIN")
	}
	m := f.mask()
	priv := make([]byte, len(key))
	for i := range key {
		priv[i] = key[i] ^ m[i%len(m)]
	}
	check := sha256.Sum256(append([]byte(f.name), key...))
	return check[:], priv, nil
}

func (f *fakeTPM) Unseal(pin string, pub, priv []byte) ([]byte, error) {
	if f.broken {
		return nil, errors.New("tcti init failed")
	}
	m := f.mask()
	key := make([]byte, len(priv))
	for i := range priv {
		key[i] = priv[i] ^ m[i%len(m)]
	}
	check := sha256.Sum256(append([]byte(f.name), key...))
	if !bytes.Equal(check[:], pub) {
		return nil, errors.New("not this TPM")
	}
	return key, nil
}

func TestIdentityPersists(t *testing.T) {
	s := Store{Dir: filepath.Join(t.TempDir(), "state")}
	a, sealed, err := s.Load()
	if err != nil || sealed {
		t.Fatalf("first load: %v sealed %v", err, sealed)
	}
	b, _, err := s.Load()
	if err != nil || a.ID() != b.ID() {
		t.Fatalf("identity changed: %v", err)
	}
	if fi, err := os.Stat(s.plain()); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("identity file: %v", err)
	}
}

func TestSealedInTheTPM(t *testing.T) {
	dir := t.TempDir()
	tpm := &fakeTPM{name: "chip"}
	s := Store{Dir: dir, TPM: tpm}
	a, sealed, err := s.Load()
	if err != nil || !sealed {
		t.Fatalf("load: %v sealed %v", err, sealed)
	}
	if _, err := os.Stat(s.plain()); !os.IsNotExist(err) {
		t.Fatal("the seed was left in the clear")
	}
	data, _ := os.ReadFile(s.sealed())
	if bytes.Contains(data, a.Priv.Seed()) {
		t.Fatal("the sealed file holds the seed")
	}
	b, sealed, err := s.Load()
	if err != nil || !sealed || a.ID() != b.ID() {
		t.Fatalf("reload: %v", err)
	}
}

func TestPlainIdentityMovesIntoTheTPM(t *testing.T) {
	dir := t.TempDir()
	old, _, err := Store{Dir: dir}.Load()
	if err != nil {
		t.Fatal(err)
	}
	s := Store{Dir: dir, TPM: &fakeTPM{name: "chip"}}
	moved, sealed, err := s.Load()
	if err != nil || !sealed || moved.ID() != old.ID() {
		t.Fatalf("migration: %v sealed %v", err, sealed)
	}
	if _, err := os.Stat(s.plain()); !os.IsNotExist(err) {
		t.Fatal("the plain seed survived the migration")
	}
}

func TestNoTPMKeepsTheFile(t *testing.T) {
	s := Store{Dir: t.TempDir(), TPM: &fakeTPM{missing: true}}
	_, sealed, err := s.Load()
	if err != nil || sealed {
		t.Fatalf("load: %v sealed %v", err, sealed)
	}
	if _, err := os.Stat(s.plain()); err != nil {
		t.Fatal(err)
	}
}

func TestBrokenTPMKeepsTheFile(t *testing.T) {
	s := Store{Dir: t.TempDir(), TPM: &fakeTPM{broken: true}}
	a, sealed, err := s.Load()
	if err != nil || sealed {
		t.Fatalf("load: %v sealed %v", err, sealed)
	}
	s.TPM = &fakeTPM{name: "chip"}
	b, sealed, err := s.Load()
	if err != nil || !sealed || a.ID() != b.ID() {
		t.Fatalf("later migration: %v sealed %v", err, sealed)
	}
}

func TestTPMBackAfterAFailedStart(t *testing.T) {
	dir := t.TempDir()
	chip := &fakeTPM{name: "chip"}
	first, _, err := Store{Dir: dir, TPM: chip}.Load()
	if err != nil {
		t.Fatal(err)
	}
	chip.broken = true
	s := Store{Dir: dir, TPM: chip}
	if _, _, err := s.Load(); !errors.Is(err, ErrSealBroken) {
		t.Fatalf("broken chip: %v", err)
	}
	if _, _, err := s.Recover(); err != nil || !s.Lost() {
		t.Fatalf("recover: %v", err)
	}
	if s.Reclaim() {
		t.Fatal("reclaimed while the chip is still broken")
	}
	chip.broken = false
	if !s.Reclaim() || s.Lost() {
		t.Fatal("the identity was not reclaimed")
	}
	back, sealed, err := s.Load()
	if err != nil || !sealed || back.ID() != first.ID() {
		t.Fatalf("after reclaim: %v", err)
	}
}

func TestClearedTPM(t *testing.T) {
	dir := t.TempDir()
	first, _, err := Store{Dir: dir, TPM: &fakeTPM{name: "chip"}}.Load()
	if err != nil {
		t.Fatal(err)
	}
	s := Store{Dir: dir, TPM: &fakeTPM{name: "cleared"}}
	if _, _, err := s.Load(); !errors.Is(err, ErrSealBroken) {
		t.Fatalf("a cleared TPM opened the identity: %v", err)
	}
	fresh, sealed, err := s.Recover()
	if err != nil || !sealed || fresh.ID() == first.ID() || !s.Lost() {
		t.Fatalf("recover: %v sealed %v lost %v", err, sealed, s.Lost())
	}
	if _, _, err := s.Load(); err != nil || !s.Lost() {
		t.Fatalf("the loss was forgotten after a restart: %v", err)
	}
	if _, err := s.Adopt(first.Priv.Seed()); err != nil || s.Lost() {
		t.Fatalf("adopt: %v", err)
	}
	back, _, err := s.Load()
	if err != nil || back.ID() != first.ID() {
		t.Fatalf("restored identity: %v", err)
	}
}

func TestBackupRoundTrip(t *testing.T) {
	id, _, err := Store{Dir: t.TempDir()}.Load()
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	code, err := Backup(&buf, Contents{Seed: id.Priv.Seed(), Name: "Salotto", Screens: map[string]string{"aa": "TV"}})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(buf.Bytes(), id.Priv.Seed()) || bytes.Contains(buf.Bytes(), []byte("Salotto")) {
		t.Fatal("the backup is not encrypted")
	}
	typed := strings.ToLower(strings.ReplaceAll(code, "-", " "))
	c, err := Restore(bytes.NewReader(buf.Bytes()), typed)
	if err != nil {
		t.Fatal(err)
	}
	if c.Identity().ID() != id.ID() || c.Screens["aa"] != "TV" || c.Name != "Salotto" {
		t.Fatalf("restored %+v", c)
	}
	wrong := []byte(code)
	if wrong[0] == '0' {
		wrong[0] = '1'
	} else {
		wrong[0] = '0'
	}
	if _, err := Restore(bytes.NewReader(buf.Bytes()), string(wrong)); !errors.Is(err, ErrWrongCode) {
		t.Fatalf("wrong code: %v", err)
	}
	damaged := append([]byte{}, buf.Bytes()...)
	damaged[len(damaged)-1] ^= 1
	if _, err := Restore(bytes.NewReader(damaged), code); !errors.Is(err, ErrWrongCode) {
		t.Fatalf("damaged backup: %v", err)
	}
	if _, err := Restore(strings.NewReader("hello"), code); !errors.Is(err, ErrNotBackup) {
		t.Fatalf("not a backup: %v", err)
	}
}

func TestRecoveryCodeShape(t *testing.T) {
	code, err := RecoveryCode()
	if err != nil {
		t.Fatal(err)
	}
	if len(code) != 29 || strings.Count(code, "-") != 4 || NormalizeCode(code) != code {
		t.Fatalf("code %q", code)
	}
}

func TestHelperSealsTheCoreIdentity(t *testing.T) {
	path := os.Getenv("SINTY_TPM_HELPER")
	if path == "" {
		t.Skip("SINTY_TPM_HELPER is not set")
	}
	dir := t.TempDir()
	h := &Helper{Path: path, WorkDir: filepath.Join(dir, "tpm")}
	t.Setenv("SINTYKEY_SEAL_PCRS", "7")
	s := Store{Dir: dir, TPM: h}
	a, sealed, err := s.Load()
	if err != nil || !sealed {
		t.Fatalf("seal: %v sealed %v", err, sealed)
	}
	b, sealed, err := s.Load()
	if err != nil || !sealed || a.ID() != b.ID() {
		t.Fatalf("unseal: %v", err)
	}
	if _, err := os.Stat(s.plain()); !os.IsNotExist(err) {
		t.Fatal("seed left in the clear")
	}
	if entries, _ := os.ReadDir(h.WorkDir); len(entries) != 0 {
		t.Fatalf("scratch files left: %d", len(entries))
	}
	missing := Store{Dir: t.TempDir(), TPM: &Helper{Path: filepath.Join(dir, "none"), WorkDir: filepath.Join(dir, "tpm")}}
	if _, sealed, err := missing.Load(); err != nil || sealed {
		t.Fatalf("without the helper: %v sealed %v", err, sealed)
	}
}
