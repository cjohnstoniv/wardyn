// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The deployment's host-root ceiling over a host_path drive, and the one gate
// that has to look at the OTHER drive rows.
//
// Split out of user_drives.go because these two are the only parts of that file
// that reason about the HOST FILESYSTEM rather than about a row: the operator's
// WARDYN_USER_DRIVE_HOST_ROOTS ceiling, whether any configured root can hold a
// drive at all, and whether two drives' roots share a tree once symlinks are
// resolved. The CRUD handlers, their request shapes and their audit rows stay
// next to each other in user_drives.go.
package api

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// userDriveHostRootCheck is the deployment's ceiling over an admin-authored
// host_path, built from the boot-parsed roots. It returns the hook type
// internal/types names but cannot implement, which is the whole point of the
// hook: internal/types must not read the environment, and a ceiling that lived
// in two places would be a ceiling one of them could forget.
//
// Empty roots refuse every host_path drive, and the refusal is inside the
// closure rather than a caller's `if`, so no call site can acquire the
// fail-open version by forgetting the guard.
func (s *Server) userDriveHostRootCheck() types.UserDriveHostRootCheck {
	return runner.UserDriveHostRootCheck(s.cfg.UserDriveHostRoots)
}

// driveHostRootNesting is the FOURTH gate, and the only one that looks at other
// ROWS: a host_path drive whose host_root sits inside, or contains, another
// host_path drive's host_root is refused, 422, naming the other drive. Nested
// drives mean one drive's members author the other's storage (alice's writable
// home can hold another drive's root, and she can swap a segment for a link).
// Strict nesting only: equal roots are the ordinary "one share, two allocations"
// shape. Checked on the stored strings AND the resolved paths, because
// UserDriveHostRootCheck sees one root, and a SYMLINKED root nests just the same.
// A resolve failure on the OTHER row falls back to the lexical answer: this
// drive's root already resolved (gate 3), and a dead stored row must not block
// every new drive. A read then an unconditional write, like driveRehomeGuard:
// two concurrent creates can both pass; the database-level form is deferred.
func (s *Server) driveHostRootNesting(r *http.Request, d types.UserDrive) (int, string) {
	drives, err := s.cfg.Store.ListUserDrives(r.Context())
	if err != nil {
		// 500, never "no other drives": a list that failed cannot say the tree is
		// clear, and treating it as clear is how the check silently stops biting
		// on exactly the deployment whose database is unhappy.
		return http.StatusInternalServerError, "list user drives: " + err.Error()
	}
	// One bound for the whole gate, for userDriveHostRootsUsableWithin's reason:
	// this loop resolves EVERY stored host_path root, so without a deadline on
	// the loop's own context the first request after a mount hangs pays
	// driveShareProbeTimeout per distinct dead root. An expired context
	// makes the rest answer "" and fall back to the lexical comparison, which is
	// the same fall-back an unresolvable root already takes.
	ctx, cancel := context.WithTimeout(r.Context(), driveShareProbeTimeout)
	defer cancel()
	mineReal := s.driveRootRealWithin(ctx, d.HostRoot)
	for _, other := range drives {
		// A row is not its own ancestor: a PUT that re-saves a drive unchanged
		// must not start refusing itself.
		if other.ID == d.ID || other.Backend != types.DriveBackendHostPath || other.HostRoot == "" {
			continue
		}
		otherReal := s.driveRootRealWithin(ctx, other.HostRoot)
		switch {
		case driveRootInside(d.HostRoot, mineReal, other.HostRoot, otherReal):
			return http.StatusUnprocessableEntity, fmt.Sprintf(
				"invalid drive: host_root %q%s is inside drive %q's host_root %q%s — that tree holds directories the other drive's "+
					"members can write from inside a run, so they could redirect this one; give the two drives separate trees",
				d.HostRoot, driveRootResolvesTo(d.HostRoot, mineReal), other.Name, other.HostRoot,
				driveRootResolvesTo(other.HostRoot, otherReal))
		case driveRootInside(other.HostRoot, otherReal, d.HostRoot, mineReal):
			return http.StatusUnprocessableEntity, fmt.Sprintf(
				"invalid drive: host_root %q%s contains drive %q's host_root %q%s — this drive's members could redirect that one "+
					"from inside a run; give the two drives separate trees",
				d.HostRoot, driveRootResolvesTo(d.HostRoot, mineReal), other.Name, other.HostRoot,
				driveRootResolvesTo(other.HostRoot, otherReal))
		}
	}
	return 0, ""
}

// driveRootReal is a stored host_root's symlink-resolved form, or "" when it
// does not resolve on this host. "" is the fall-back-to-lexical signal
// driveRootInside reads — see driveHostRootNesting for why a stale row must not
// be able to refuse a new drive.
func driveRootReal(root string) string {
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		return ""
	}
	return real
}

// driveRootInside reports whether inner is STRICTLY nested inside outer, asking
// the question on the stored strings and again on the resolved paths.
//
// Either answer refuses, which is the fail-closed direction: a link that lands
// inside the other tree nests just as surely as a literal path does, and a link
// that lands OUT of it does not un-nest a literal one (the link is host-side
// state a member with a run in the outer drive can replace). Strict on both,
// for driveHostRootNesting's reason: equal roots, including two strings that
// resolve to one directory, are a naming question, not a containment one.
func driveRootInside(inner, innerReal, outer, outerReal string) bool {
	if strings.HasPrefix(inner, outer+"/") {
		return true
	}
	return innerReal != "" && outerReal != "" && strings.HasPrefix(innerReal, outerReal+"/")
}

// driveRootResolvesTo renders the " (resolves to …)" half of the refusal, and
// renders NOTHING when the path is its own real form — an admin reading about
// two paths they typed must not have to skip past two restatements of them, and
// the resolved path is exactly what they cannot see from the form.
func driveRootResolvesTo(root, real string) string {
	if real == "" || real == root {
		return ""
	}
	return fmt.Sprintf(" (resolves to %q)", real)
}
