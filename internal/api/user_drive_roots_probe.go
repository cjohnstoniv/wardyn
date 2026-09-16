// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// THE ADMIN DOORS' HALF OF THE SHARE BOUND.
//
// The member doors have run their filesystem questions through driveShareProbe
// since F295: a 5-second bound, a strand memory so a read never queues behind a
// syscall that is already overdue, and one WARN per strand. The ADMIN doors —
// GET /drives, the host_root ceiling on a drive write, and the nesting gate's
// symlink resolution — ran the SAME uncancellable EvalSymlinks/stat calls
// unbounded, on a server that deliberately sets no WriteTimeout. A hard-mounted
// NAS that stops answering therefore hung every /drives read and every drive
// write for as long as the mount took, leaking one kernel thread per attempt
// (B5-F1).
//
// Split out of user_drive_roots.go rather than added to it because user_drives.go
// is at the file-size ceiling and this is new logic, not an edit of the ceiling's
// own rules: the three dead-root shapes still live in exactly one place
// (runner.UserDriveHostRootCheck) and everything here only decides HOW LONG to
// wait for the filesystem to answer about them.
//
// THE KEY IS "root:"+root, which is the key the member path already uses for the
// same subject (driveShareBindFailure). That is deliberate: one hung share is one
// strand, whoever asks about it, so an admin's GET /drives and a member's run
// launch short-circuit on each other's outstanding probe instead of each
// starting their own thread.
package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// errDriveRootUnanswered is what a bounded root check returns when the probe did
// not come back — told apart from a ceiling REFUSAL because the two have
// different remedies (an admin re-authoring the drive versus an operator's
// mount) and different statuses, and because "we could not tell" must never
// render as "we checked and it is fine".
var errDriveRootUnanswered = errors.New("the host root did not answer in time")

// DRAFT (M2 canon pending). The one new admin-facing sentence this bound needs.
const (
	// driveRootUnansweredMsg is the 503 a drive WRITE gets when the deployment's
	// host-root ceiling cannot be evaluated because the share stopped answering.
	// 503 and not 422: nothing about the request is wrong, and the admin's remedy
	// is to wait or fix the mount, not to edit the drive. It names the root
	// because every /drives route is operator-only — the member-facing sentences
	// (driveRefusal) deliberately do not.
	driveRootUnansweredMsg = "this deployment's drive host root %q did not answer in time, so the ceiling that " +
		"governs host_path drives could not be checked and nothing was written. The share is on a mount that is " +
		"not responding — re-send once it is back."
)

// userDriveHostRootCheckBounded is userDriveHostRootCheck with every filesystem
// question inside driveShareProbe's bound. The ceiling's RULES are unchanged —
// it is the same closure, run on the same root — so a deployment whose shares
// answer sees exactly the behaviour it saw before.
func (s *Server) userDriveHostRootCheckBounded(ctx context.Context) types.UserDriveHostRootCheck {
	check := s.userDriveHostRootCheck()
	return func(root string) error {
		err, ok := s.driveShareProbe(ctx, "root:"+root, func() error { return check(root) })
		if !ok {
			return fmt.Errorf("%w: %s", errDriveRootUnanswered, root)
		}
		return err
	}
}

// userDriveHostRootsUsableWithin reports whether ANY configured root could
// actually hold a host_path drive — the honest form of "is host_path available
// here", which is what GET /drives' host_roots_configured publishes — asked
// under the same bound.
//
// IT ASKS THE WRITE BOUNDARY'S OWN CHECK, once per configured root, rather than
// re-deriving what makes a root dead. That is the whole point: the three dead
// shapes (a root of "/", a root under a denied bind prefix, a root that does not
// resolve on this host) are UserDriveHostRootCheck's rules, and a second copy
// here would be a second place for them to drift. A root is usable exactly when
// the deployment would accept a drive rooted AT it, which is the ordinary shape
// (the ceiling names the share's mount point and so does the drive).
//
// EMPTY ROOTS ANSWER false, unchanged: the loop does not run.
//
// A ROOT THAT DID NOT ANSWER IS NOT USABLE, which is the fail-closed direction
// and the honest one: the console enables the host_path option on this bit, and
// offering a backend whose write door is currently answering 503 is the
// offer-and-refuse the field exists to prevent. It is also self-correcting — the
// strand entry disappears when the mount comes back, so the next read says yes.
// ONE BOUND FOR THE WHOLE LOOP, not one per root. driveShareProbe bounds each
// probe at driveShareProbeTimeout, so N distinct dead roots would cost N × that
// on the FIRST request after a mount hangs — the strand marks only short-circuit
// the SECOND request. A deadline on the loop's own context makes every probe
// after the first strand return at once (driveShareProbe selects on ctx.Done),
// so the request's cost is the bound this file's header promises (R-03).
func (s *Server) userDriveHostRootsUsableWithin(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, driveShareProbeTimeout)
	defer cancel()
	check := s.userDriveHostRootCheckBounded(ctx)
	for _, root := range s.cfg.UserDriveHostRoots {
		if check(root) == nil {
			return true
		}
	}
	return false
}

// driveRootRealWithin is driveRootReal under the bound: a stored host_root's
// symlink-resolved form, or "" when it does not resolve OR did not answer.
//
// "" ALREADY MEANS "fall back to the lexical answer" at the one call site
// (driveHostRootNesting), and a share that cannot answer is a share that binds
// nothing, so the fall-back is the same decision the documented resolve-failure
// case makes. The drive's OWN root has been through the bounded ceiling check by
// then, so an unanswered root has already produced the 503 above.
func (s *Server) driveRootRealWithin(ctx context.Context, root string) string {
	var real string
	// Assigned INSIDE the probe and read only on ok: the receive from the probe's
	// channel happens after check() returned, so this is ordered. On !ok the
	// goroutine is still running and the value is never read.
	if _, ok := s.driveShareProbe(ctx, "root:"+root, func() error {
		real = driveRootReal(root)
		return nil
	}); !ok {
		return ""
	}
	return real
}

// driveRootCeilingRefusal turns a bounded ceiling error into the status and the
// sentence the drive write answers with — the "decided answer" half of the
// bound: an unanswered probe is a 503 about the deployment, a refusal is the
// 422 it always was.
func driveRootCeilingRefusal(root string, err error) (int, string) {
	if errors.Is(err, errDriveRootUnanswered) {
		return http.StatusServiceUnavailable, fmt.Sprintf(driveRootUnansweredMsg, root)
	}
	return http.StatusUnprocessableEntity, "invalid drive: " + err.Error()
}

// driveWriteAuditData is the `drive.write` payload: the WHOLE ROW, minus nothing
// (a drive carries no secret — a share credential is the operator's, held
// host-side — and host_root is the single most audit-worthy field on it, since
// it is the host tree this row authorized binding into other people's
// sandboxes), plus what the re-home guard decided.
//
// THE RE-HOME DETAIL IS OMITTED WHEN THERE IS NONE (B5-F6). `rehomed:true` says
// a move happened; the refusal the admin overrode to get here said WHICH
// identity fields moved and HOW MANY allocations went with them, and the row
// discarded both — so the log could not answer "which objects were orphaned",
// which is the only question a re-home raises afterwards. Present exactly when
// `rehomed` is true, so an auditor can filter on the key rather than on a zero
// that also means "nothing moved".
//
// It lives here rather than in user_drives.go for that file's size ceiling.
func driveWriteAuditData(saved types.UserDrive, rehome driveRehome) map[string]any {
	data := map[string]any{
		"name":          saved.Name,
		"backend":       saved.Backend,
		"host_root":     saved.HostRoot,
		"storage_class": saved.StorageClass,
		"home_template": saved.HomeTemplate,
		"size_mib":      saved.SizeMiB,
		"writable":      saved.Writable,
		"reclaim":       saved.Reclaim,
		// The one field that separates a cosmetic edit from one that moved every
		// allocated member's storage. Without it both are the same `drive.write`
		// row and the orphaning is invisible in the log.
		"rehomed": rehome.confirmed,
	}
	if rehome.confirmed {
		data["rehomed_fields"] = rehome.fields
		data["rehomed_subjects"] = rehome.subjects
	}
	return data
}
