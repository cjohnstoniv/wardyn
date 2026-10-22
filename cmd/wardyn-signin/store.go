// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

const (
	// storeDir is the person's store: a tmpfs mounted uid=1001,mode=0700 by
	// the runner, never on disk and never in an image layer.
	storeDir = "/run/wardyn/signin"
	// personUID is the person's uid in the desktop image; the agent is 1000.
	personUID = 1001
	// tmpfsMagic is TMPFS_MAGIC from linux/magic.h.
	tmpfsMagic = 0x01021994
)

// The store's layout. Every socket the person's display uses is here, so
// nothing of the person's sits in the shared /tmp, on loopback or in the
// abstract socket namespace, except the uid-checked proxy forwarder.
var (
	credPath    = filepath.Join(storeDir, "cred")      // the sign-in lane credential, 0600
	runtimeDir  = filepath.Join(storeDir, "run")       // XDG_RUNTIME_DIR: the Wayland socket
	homeDir     = filepath.Join(storeDir, "home")      // HOME
	profileDir  = filepath.Join(storeDir, "profile")   // the person's browser profile
	tmpDir      = filepath.Join(storeDir, "tmp")       // TMPDIR
	vncSock     = filepath.Join(storeDir, "vnc.sock")  // wayvnc
	wsSock      = filepath.Join(storeDir, "ws.sock")   // websockify, what the relay dials
	ctlSock     = filepath.Join(storeDir, "ctl.sock")  // serve's browser control
	displayPath = filepath.Join(runtimeDir, "display") // the Wayland display name hold saw
	serveLog    = filepath.Join(storeDir, "serve.log") // serve's own lines; never a URL
)

// checkUID refuses to run as any uid but want.
func checkUID(want int) error {
	if got := os.Getuid(); got != want {
		return fmt.Errorf("this subcommand runs as uid %d only (running as %d)", want, got)
	}
	return nil
}

// checkAgentUID refuses the person's uid and root.
func checkAgentUID() error {
	if u := os.Getuid(); u == personUID || u == 0 {
		return fmt.Errorf("this subcommand runs as the agent's uid only (running as %d)", u)
	}
	return nil
}

// checkPerson is the guard every person-side subcommand runs first: the
// person's uid, and a store that is a real tmpfs directory the person alone owns.
func checkPerson() error {
	if err := checkUID(personUID); err != nil {
		return err
	}
	return checkStore(storeDir, personUID)
}

func checkStore(dir string, uid int) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("the sign-in store is missing: %w", err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return errors.New("the sign-in store is not a directory")
	}
	if int(st.Uid) != uid || fi.Mode().Perm() != 0o700 {
		return fmt.Errorf("the sign-in store must be owned by uid %d with mode 0700 (uid %d, mode %o)", uid, st.Uid, fi.Mode().Perm())
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(dir, &fs); err != nil {
		return fmt.Errorf("the sign-in store: %w", err)
	}
	if uint32(fs.Type) != tmpfsMagic {
		return fmt.Errorf("the sign-in store is not a tmpfs (type %#x)", fs.Type)
	}
	return nil
}

// mkdirs creates the store's private subdirectories.
func mkdirs() error {
	for _, d := range []string{runtimeDir, homeDir, profileDir, tmpDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(d, 0o700); err != nil {
			return err
		}
	}
	return nil
}
