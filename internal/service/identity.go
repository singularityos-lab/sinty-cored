package service

import (
	"errors"
	"log"
	"os"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/singularityos-lab/sinty-cored/internal/pair"
	"github.com/singularityos-lab/sinty-cored/internal/vault"
)

func (s *Service) identity() pair.Identity {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.id
}

func (o *object) Backup(sender dbus.Sender, fd dbus.UnixFD) (string, *dbus.Error) {
	s := o.s
	f := os.NewFile(uintptr(fd), "backup")
	defer f.Close()
	if !s.allowed(sender) {
		return "", dbusErr("NotAllowed", errNotAllowed)
	}
	trusted, err := s.cfg.Trust.Screens()
	if err != nil {
		return "", dbusErr("Failed", err)
	}
	code, err := vault.Backup(f, vault.Contents{
		Seed:    s.identity().Priv.Seed(),
		Name:    s.cfg.Name,
		Screens: trusted,
		Created: time.Now().UTC(),
	})
	if err != nil {
		return "", dbusErr("Failed", err)
	}
	return code, nil
}

func (o *object) Restore(sender dbus.Sender, fd dbus.UnixFD, code string) *dbus.Error {
	s := o.s
	f := os.NewFile(uintptr(fd), "backup")
	defer f.Close()
	if !s.allowed(sender) {
		return dbusErr("NotAllowed", errNotAllowed)
	}
	c, err := vault.Restore(f, code)
	switch {
	case errors.Is(err, vault.ErrNotBackup):
		return dbusErr("NotABackup", err)
	case errors.Is(err, vault.ErrWrongCode):
		return dbusErr("WrongCode", err)
	case err != nil:
		return dbusErr("Failed", err)
	}
	if err := s.adopt(c); err != nil {
		return dbusErr("Failed", err)
	}
	return nil
}

func (s *Service) adopt(c vault.Contents) error {
	kept := IdentityFile
	if s.cfg.Vault != nil {
		sealed, err := s.cfg.Vault.Adopt(c.Seed)
		if err != nil {
			return err
		}
		if sealed {
			kept = IdentityTPM
		}
	}
	id := c.Identity()
	s.mu.Lock()
	s.id, s.kept = id, kept
	for _, sess := range s.sessions {
		sess.end()
	}
	s.mu.Unlock()
	known, err := s.cfg.Trust.Screens()
	if err != nil {
		known = map[string]string{}
	}
	for screen, name := range c.Screens {
		if _, ok := known[screen]; ok {
			continue
		}
		if err := s.cfg.Trust.Add(screen, name); err != nil {
			log.Printf("screen %s not restored: %v", name, err)
		}
	}
	s.set("ID", id.ID())
	s.set("Identity", kept)
	s.refreshScreens()
	return nil
}
