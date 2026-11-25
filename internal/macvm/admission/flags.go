// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package admission

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// BSD file flags (chflags(2)), as darwin's st_flags carries them. Elsewhere statFlags reads none.
const (
	flagUserImmutable   = 0x2     // UF_IMMUTABLE, uchg
	flagSystemImmutable = 0x20000 // SF_IMMUTABLE, schg
)

// errNotUserImmutable: the entry's refused link is not the user immutable flag's doing; the link's own
// error stands.
var errNotUserImmutable = errors.New("admission: not a user immutable entry")

// clearUserImmutable clears uchg on dir's entry n, whose hard link was refused, when that flag is what
// refuses it. It opens the name without following a link and decides on the opened file: a regular
// file with exactly one name and no system immutable flag. A login can set uchg on its own entry in
// this world-writable directory, which would stop every setup; a file with another name may be one the
// login does not own, and schg is an admin's. Each of those stops setup with the one command that
// clears it.
func (s *setup) clearUserImmutable(dir *os.File, n string) error {
	path := filepath.Join(dir.Name(), n)
	fd, err := unix.Openat(int(dir.Fd()), n, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return errNotUserImmutable
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil || uint32(st.Mode)&unix.S_IFMT != unix.S_IFREG {
		return errNotUserImmutable
	}
	f := s.c.flagsOf(st)
	switch {
	case f&flagSystemImmutable != 0:
		return fmt.Errorf("admission: %s carries the system immutable flag (schg), which setup does not clear; clear it as root with: chflags noschg %s, then reinstall the Wardyn runner package", path, shellQuote(path))
	case f&flagUserImmutable == 0:
		return errNotUserImmutable
	case st.Nlink != 1:
		return fmt.Errorf("admission: %s carries the user immutable flag (uchg) and has another name, so setup does not clear it. Check whose file it is (ls -lOi %s); if it is a stray link, clear it as root with: chflags nouchg %s, then reinstall the Wardyn runner package", path, shellQuote(path), shellQuote(path))
	}
	if err := s.c.fchflags(fd, f&^flagUserImmutable); err != nil {
		return fmt.Errorf("admission: clearing the user immutable flag (uchg) on %s failed (%v); clear it as root with: chflags nouchg %s, then reinstall the Wardyn runner package", path, err, shellQuote(path))
	}
	s.c.Logf("%s carried the user immutable flag (uchg); setup cleared it", path)
	return nil
}

// shellQuote quotes s as one POSIX shell word.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
