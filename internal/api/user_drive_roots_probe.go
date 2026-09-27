// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The admin doors' half of the share bound.
//
// The ADMIN doors — GET /drives, the host_root ceiling on a drive write, and the
// nesting gate's symlink resolution — run uncancellable EvalSymlinks/stat calls
// on a server that deliberately sets no WriteTimeout, so a hung hard-mounted NAS
// would hang them and leak a kernel thread per attempt. They share the member
// doors' driveShareProbe bound and strand memory. The dead-root rules still live
// only in runner.UserDriveHostRootCheck; this file decides only HOW LONG to wait.
// The key is "root:"+root, the member path's key for the same subject
// (driveShareBindFailure), deliberately: one hung share is one strand, whoever
// asks, so admin and member callers short-circuit on each other's probe.
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
// actually hold a host_path drive — what GET /drives' host_roots_configured
// publishes — asked under the same bound. It asks UserDriveHostRootCheck once per
// root rather than re-deriving what makes a root dead, so the rules cannot drift.
// Empty roots answer false.
//
// A root that did not answer is not usable (fail-closed: the console enables the
// host_path option on this bit, and offering a backend whose write door answers
// 503 is the offer-and-refuse the field prevents); the strand clears when the
// mount returns. One bound for the whole loop, not one per root: N dead roots
// would otherwise cost N × driveShareProbeTimeout on the first request, and the
// loop's deadline makes every later probe return at once (ctx.Done).
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

// driveWriteAuditData is the `drive.write` payload: the WHOLE ROW (a drive
// carries no secret, and host_root is its most audit-worthy field: the host tree
// this row authorized binding into other people's sandboxes), plus what the
// re-home guard decided. The re-home detail — which identity fields moved and how
// many allocations went with them, the only way to answer "which objects were
// orphaned" — is present exactly when `rehomed` is true, so an auditor can filter
// on the key rather than on a zero that also means "nothing moved".
//
// It lives here rather than in user_drives.go for that file's size ceiling.
func driveWriteAuditData(saved types.UserDrive, rehome driveRehome) map[string]any {
	data := map[string]any{
		"name":          saved.Name,
		"backend":       saved.Backend,
		"host_root":     saved.HostRoot,
		"storage_class": saved.StorageClass,
		"home_template": saved.HomeTemplate,
		"object_scheme": saved.ObjectScheme,
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

// driveNameMovesTheObject reports whether renaming a drive from before to after
// changes the object its members bind — i.e. whether the two names fold to
// different slugs. It asks types.DriveObjectName rather than re-implementing the
// fold, over one fixed backend and one fixed home, so a naming change in types is
// inherited instead of drifted from.
//
// scheme is the row's STORED object_scheme, never the request's: on
// DriveObjectSchemeID the minted name is `wardyn-drive-<id>-<home>` and the name
// plays no part, so an id-scheme rename must probe as unchanged.
//
// Lives here rather than beside driveIdentityFields for user_drives.go's own
// file-size ceiling, the same reason driveWriteAuditData does.
func driveNameMovesTheObject(before, after string, scheme types.DriveObjectScheme) bool {
	const probeHome = "probe"
	object := func(name string) string {
		return types.DriveObjectName(types.UserDrive{Name: name, Backend: types.DriveBackendK8sPVC, ObjectScheme: scheme}, probeHome)
	}
	return object(before) != object(after)
}

// driveObjectSchemeMoves reports whether a PUT's object_scheme differs from the
// stored drive's — but ONLY when the request actually STATES one.
//
// The field is not client-authored (the store derives it and never reads the
// request's; see types.DriveObjectScheme), so a client that omits it must not
// trip the ?confirm=rehome gate on every edit of an allocated drive. A request
// that CLAIMS a different scheme is refused with the same 409 as every other
// identity field, rather than silently swallowed by a write that ignores it.
func driveObjectSchemeMoves(before, after types.DriveObjectScheme) bool {
	return after != "" && after != before
}
