// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
)

// A macOS VM's folder share records ONE host path, the helper shares that path, and every rule below
// decides about that same path. The container bind validates its source and binds the source, each
// rule resolving it on its own; a share instead takes the path the caller resolved, and refuses it
// unless each rule that reads the filesystem resolved it to itself.

// MacProtectedPaths are the host paths a macOS VM share may neither be, sit under, nor contain: the
// system's secrets (resolved spellings, as EvalSymlinks gives /etc as /private/etc), the runner
// login's Library, and runnerDirs: the runner's macOS VM root, ledger and socket directories, which
// hold every run's disk, machine identity and control socket.
func MacProtectedPaths(home string, runnerDirs ...string) []string {
	return append([]string{"/private/etc", "/private/var/db", "/private/var/run", "/System", "/Library/Keychains",
		filepath.Join(home, "Library")}, runnerDirs...)
}

// CheckMacShareSource runs the container's source rules, and the macOS protected-path rule, on real,
// which the caller resolved and will record. A member-authored share is also held to the member's
// roots, on a runner to its owner's own roots (ownRoots, --drive-root; nil on wardynd's host), and,
// unless readOnly, to the writable ceiling, where an absent ceiling refuses. Each rule's own resolution
// must be real itself, and so must a last one: a path swapped while the rules ran is refused, never
// recorded.
func CheckMacShareSource(real string, member, readOnly bool, roots, ownRoots []string, writable *UserWritableCeiling, protected []string) error {
	if err := ValidateMountSourceLexical(real); err != nil {
		return err
	}
	// The deny-list's filesystem half, on the path it resolved.
	resolved, err := filepath.EvalSymlinks(real)
	if err != nil {
		return fmt.Errorf("mount source %q could not be resolved: %w", real, err)
	}
	if resolved != real {
		return errMacShareMoved(real)
	}
	if err := deniedSource(resolved); err != nil {
		return err
	}
	macShareStep("member")
	if member {
		checked, err := validateUserSource(real, roots)
		if err != nil {
			return err
		}
		if checked != real {
			return errMacShareMoved(real)
		}
		if err := withinOwnRoots(real, checked, ownRoots); err != nil {
			return err
		}
		if !readOnly {
			// No ceiling never means "anything goes": an empty one refuses every writable share.
			if writable == nil {
				writable = &UserWritableCeiling{}
			}
			if err := writable.allows(real, checked); err != nil {
				return err
			}
		}
	}
	macShareStep("protected")
	if err := macProtectedOverlap(real, protected); err != nil {
		return err
	}
	if final, err := filepath.EvalSymlinks(real); err != nil || final != real {
		return errMacShareMoved(real)
	}
	return nil
}

// macShareStep runs before the named rule of CheckMacShareSource; tests swap a path there.
var macShareStep = func(string) {}

func errMacShareMoved(real string) error {
	return fmt.Errorf("mount source %q changed while it was being checked; denied", real)
}

// macProtectedOverlap refuses real when it is, sits under, or contains a protected path. Each
// protected path is resolved now, through its longest existing prefix (resolveEntry), so one not made
// yet behind a linked parent still compares as where it will be. The comparison is name by name and
// case-folded: a Mac's volume is case-insensitive, and EvalSymlinks keeps the spelling it was given.
func macProtectedOverlap(real string, protected []string) error {
	for _, p := range protected {
		if p == "" {
			continue
		}
		canon := resolveEntry(p)
		switch {
		case foldWithin(canon, real):
			return fmt.Errorf("mount source %q is under denied host path %q", real, canon)
		case foldWithin(real, canon):
			return fmt.Errorf("mount source %q may not be shared into a macOS VM: it contains the protected path %q", real, canon)
		}
	}
	return nil
}

// MacVMDirError is CheckBindOutsideMacVMDirs' refusal. Its sentence names no path: a member reads it as the
// run's failure hint, and a path would tell them that a VM directory exists and where it is linked. Src and
// Dir are for the caller's log.
type MacVMDirError struct {
	Src, Dir string // the bind's resolved source, and the protected directory's resolved location
	Inside   bool   // Src is Dir or inside it; false: Src contains Dir
}

func (e *MacVMDirError) Error() string {
	if e.Inside {
		return "this folder is, or is inside, a directory the macOS VM runner keeps for its own use, which a container may not mount"
	}
	return "this folder contains a directory the macOS VM runner keeps for its own use, which a container may not mount"
}

// CheckBindOutsideMacVMDirs refuses a container bind of src when src is, sits under, or contains any of dirs,
// the runner's macOS VM directories: the VM root, which holds every run's disk, the guest agent's signing key
// and the Mac's inspection CA key, and the admission ledger and socket directories, which the VM shares keep
// out too. Both sides are resolved through their symlinks (resolveEntry), so a directory linked to another
// volume is held by where it is, not by what it is called, and a parent of it, such as the home, is refused
// as well. A directory that does not exist on this machine refuses nothing. The error is a *MacVMDirError.
func CheckBindOutsideMacVMDirs(src string, dirs ...string) error {
	real := resolveEntry(src)
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		canon, err := filepath.EvalSymlinks(dir)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			canon = resolveEntry(dir)
		}
		switch {
		case foldWithin(canon, real):
			return &MacVMDirError{Src: real, Dir: canon, Inside: true}
		case foldWithin(real, canon):
			return &MacVMDirError{Src: real, Dir: canon}
		}
	}
	return nil
}

// foldWithin reports whether p is dir or inside it, comparing whole names case-folded.
func foldWithin(dir, p string) bool {
	d, q := strings.Split(strings.Trim(dir, "/"), "/"), strings.Split(strings.Trim(p, "/"), "/")
	if dir == "/" {
		d = nil
	}
	if len(q) < len(d) {
		return false
	}
	for i := range d {
		if !strings.EqualFold(d[i], q[i]) {
			return false
		}
	}
	return true
}
