// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// THE DEPLOYMENT'S HOST-ROOT CEILING over a host_path drive, and the one gate
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
// EMPTY ROOTS REFUSE EVERY host_path DRIVE, and the refusal is inside the
// closure rather than a caller's `if`, so no call site can acquire the
// fail-open version by forgetting the guard.
func (s *Server) userDriveHostRootCheck() types.UserDriveHostRootCheck {
	return runner.UserDriveHostRootCheck(s.cfg.UserDriveHostRoots)
}

// userDriveHostRootsUsable reports whether ANY configured root could actually
// hold a host_path drive — the honest form of "is host_path available here".
//
// IT ASKS THE WRITE BOUNDARY'S OWN CHECK, once per configured root, rather than
// re-deriving what makes a root dead. That is the whole point: the three dead
// shapes (a root of "/", a root under a denied bind prefix, a root that does not
// resolve on this host) are UserDriveHostRootCheck's rules, and a second copy
// here would be a second place for them to drift — the same argument the ceiling
// hook itself makes about living in one place. A root is usable exactly when the
// deployment would accept a drive rooted AT it, which is the ordinary shape (the
// ceiling names the share's mount point and so does the drive).
//
// EMPTY ROOTS ANSWER false, unchanged: the loop does not run.
//
// The cost is one ValidateMountSource plus one EvalSymlinks per configured root
// — a handful of stat calls on the admin screen's own read, bounded by the
// operator's env var and not by anything in the database.
func (s *Server) userDriveHostRootsUsable() bool {
	check := s.userDriveHostRootCheck()
	for _, root := range s.cfg.UserDriveHostRoots {
		if check(root) == nil {
			return true
		}
	}
	return false
}

// driveHostRootNesting is the FOURTH gate, and the only one that has to look at
// the other ROWS: a host_path drive whose host_root sits inside — or contains —
// another host_path drive's host_root is refused, 422, naming the other drive.
//
// THE HOLE IT CLOSES IS A MEMBER'S, NOT AN ADMIN'S TYPO. Drive A is rooted at
// /srv/shares and gives alice a WRITABLE home at /srv/shares/alice. Drive B is
// then rooted at /srv/shares/alice/team. Nothing above notices: B's root is
// inside the deployment's ceiling, exists, is not a credential directory and is
// an ordinary path. But its whole tree is a directory alice can write from
// INSIDE a run — so she can replace `team`, or any segment under it, with a
// link, and B's members are bound wherever she points them. The driver's
// resolved-real-path checks bound where that can aim (the ceiling, and now the
// member's own home name) but they cannot make the layout supportable: two
// drives sharing a tree means one drive's members author the other drive's
// storage.
//
// STRICT nesting only. Two drives on the SAME root are left alone: that is the
// ordinary "one share, two allocations with different home templates" shape, and
// neither drive's members can move the other's root, because the root is not
// inside anybody's home. Equal roots are a naming question; nested roots are a
// containment one.
//
// ON THE STORED STRINGS **AND ON THE RESOLVED PATHS**, and it has to be both.
// The lexical half is what the rows say; the resolved half is what the
// filesystem says, and no other check compares TWO DRIVES' roots. This gate
// used to delegate the symlink half to UserDriveHostRootCheck — but that check
// resolves ONE root against the deployment's env ceiling and has no second
// drive in scope, so it cannot see nesting at all. The gap was reachable with
// the deployment's own ceiling honoured throughout: drive A rooted at
// /srv/shares, drive B rooted at /mnt/teamshare where /mnt/teamshare is a
// SYMLINK to /srv/shares/alice/team. Both roots are inside the roots, both
// resolve, neither is a credential directory — and the literal nested path is
// refused while the link to it is accepted, which is the same containment loss
// with an extra hop.
//
// A RESOLVE FAILURE ON THE OTHER ROW FALLS BACK TO THE LEXICAL ANSWER, not to a
// 500 and not to a refusal. This drive's own root has ALREADY resolved (gate 3,
// UserDriveHostRootCheck, which fails closed on exactly that), so the only path
// that can fail here is a STORED row whose share is gone — and a drive that
// cannot resolve binds nothing, so there is no tree left for it to share.
// Turning that into a refusal would let one dead row block every new drive an
// admin tries to author.
//
// ONE STORE READ, on the drive-write path only — a handful of calls in a
// deployment's lifetime, and the same list the console already loads on every
// visit to the screen.
//
// AND IT IS A READ FOLLOWED BY AN UNCONDITIONAL WRITE, exactly as
// driveRehomeGuard is, which that gate says out loud and this one did not. Two
// concurrent creates — /srv/shares and /srv/shares/alice/team — can both list
// before either writes, and both are then stored: the pair this gate exists to
// refuse, accepted 201/201. It is application-level for the same reason the
// re-home guard is (the Store interface exposes finished operations rather
// than a tx handle, to PG and to every test double alike), the database-level
// form is 0.7.1, and what it does close is the case that actually happens —
// one admin authoring one drive at a time. Stated here because a residual an
// operator cannot read is a residual nobody can plan around, and because the
// sibling gate stating its own made this one's silence read as absence.
func (s *Server) driveHostRootNesting(r *http.Request, d types.UserDrive) (int, string) {
	drives, err := s.cfg.Store.ListUserDrives(r.Context())
	if err != nil {
		// 500, never "no other drives": a list that failed cannot say the tree is
		// clear, and treating it as clear is how the check silently stops biting
		// on exactly the deployment whose database is unhappy.
		return http.StatusInternalServerError, "list user drives: " + err.Error()
	}
	mineReal := driveRootReal(d.HostRoot)
	for _, other := range drives {
		// A row is not its own ancestor: a PUT that re-saves a drive unchanged
		// must not start refusing itself.
		if other.ID == d.ID || other.Backend != types.DriveBackendHostPath || other.HostRoot == "" {
			continue
		}
		otherReal := driveRootReal(other.HostRoot)
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
// EITHER answer refuses, which is the fail-closed direction: a link that lands
// inside the other tree nests just as surely as a literal path does, and a link
// that lands OUT of it does not un-nest a literal one (the link is host-side
// state a member with a run in the outer drive can replace).
//
// STRICT on both, for driveHostRootNesting's stated reason: two drives on the
// SAME root are the ordinary "one share, two allocations" shape, and equal roots
// are a naming question rather than a containment one — including the case where
// two different strings resolve to one directory, which is that same shape
// spelled with a link.
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
