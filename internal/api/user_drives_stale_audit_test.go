// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestDriveStaleSnapshotRefusalIsAudited is F317.
//
// groups_snapshot_stale has TWO deciding sites, not one. F227 audited the
// governance resolver's; the drive resolver's mirror-image branch
// (driveWithUnusableGroups) raised the identical member-reachable 403 and
// recorded nothing — while docs/AUDIT-ACTIONS.md:208 and
// docs/OPERATIONS.md:1695 both told operators the reason is emitted "at the ONE
// site that decides it".
//
// The shape that makes it total is a deployment with group-tier DRIVE grants and
// NO group-tier governance assignment: the ceiling resolves fine, so the audited
// site never fires, and the drives door refuses with an entirely empty denial
// stream. Executed on the pre-fix tree: 0 authz.denied rows out of 0 events.
func TestDriveStaleSnapshotRefusalIsAudited(t *testing.T) {
	// hasGroupTier is HasGroupTierDriveGrants; hasGroupTierAssignments stays
	// FALSE, which is the whole point — it is what keeps the governance twin
	// silent and leaves this seam as the only one that could have spoken.
	t.Run("the drive seam writes authz.denied with the documented reason", func(t *testing.T) {
		st := &driveStore{hasGroupTier: true, userTierOnly: true, hasGroupTierAssignments: false}
		srv, rec := driveRunServer(st, "docker")
		ctx := driveMemberCtx(nil, false)

		mount, ok, w := driveSeed(t, srv, driveRunRequest(true, nil), governanceCeiling{}, ctx)
		if ok || mount != nil {
			t.Fatalf("the launch mounted a drive it cannot resolve: %+v", mount)
		}
		if w.Code != http.StatusForbidden {
			t.Fatalf("launch = %d, want 403; body=%s", w.Code, w.Body.String())
		}

		var found int
		for _, ev := range rec.events {
			if ev.Action != "authz.denied" {
				continue
			}
			var data map[string]any
			if err := json.Unmarshal(ev.Data, &data); err != nil {
				t.Fatal(err)
			}
			if data["reason"] != "groups_snapshot_stale" {
				continue
			}
			found++
			// runs.drive, matching denyMemberDrive — the other refusal this seam
			// writes. governance.ceiling would file it under a resolve that did
			// not happen and did not fail.
			if ev.Target != "runs.drive" {
				t.Errorf("target = %q, want %q — the drive seam's own target, the one denyMemberDrive uses",
					ev.Target, "runs.drive")
			}
			if ev.Actor != "sub-drive-bob" {
				t.Errorf("actor = %q, want the refused principal", ev.Actor)
			}
		}
		if found == 0 {
			t.Fatalf("a member-reachable 403 at the DRIVE seam produced NO authz.denied row (%d events "+
				"recorded). On a deployment with group-tier drive grants and no group-tier governance "+
				"assignment this is the only deciding site, so the denial stream is empty for a member who "+
				"cannot launch at all", len(rec.events))
		}
		if found != 1 {
			t.Errorf("one refused launch produced %d denial rows, want 1 — the resolver is asked once per "+
				"request, and the count has to mean denials rather than resolves", found)
		}
	})

	// THE CONTROL, and it is the same one the governance twin carries: the SAME
	// unanswerable snapshot on a deployment that has authored no group-tier
	// DRIVE grant is not a refusal at all (nothing an unknown group could have
	// been hiding), so it must write no denial either. Without this arm the new
	// row would be noise on every pre-0.6 cookie rather than signal.
	t.Run("a deployment with no group-tier drive grant writes no denial", func(t *testing.T) {
		st := &driveStore{hasGroupTier: false, userTierOnly: true, hasGroupTierAssignments: false}
		srv, rec := driveRunServer(st, "docker")
		before := len(rec.events)
		if _, _, w := driveSeed(t, srv, driveRunRequest(true, nil), governanceCeiling{}, driveMemberCtx(nil, false)); w.Code == http.StatusForbidden {
			t.Fatalf("a member on a deployment with no group-tier drive grant was refused; body=%s", w.Body.String())
		}
		for _, ev := range rec.events[before:] {
			if ev.Action != "authz.denied" {
				continue
			}
			var data map[string]any
			if err := json.Unmarshal(ev.Data, &data); err != nil {
				t.Fatal(err)
			}
			if data["reason"] == "groups_snapshot_stale" {
				t.Errorf("a member who was NOT refused produced a groups_snapshot_stale denial — the row has "+
					"to mean a refusal happened, or an operator counting them is counting sessions (%v)", data)
			}
		}
	})
}

// TestMePollsAreNotDenials is the other half of F317: the row must mean a
// refusal happened, not that a console is open.
//
// GET /me reaches BOTH groups_snapshot_stale deciding sites — the drive resolver
// for user_drive, the governance resolver for user_drive_denied_by_profile — so
// a member with an unanswerable group snapshot was writing denial rows on a
// TIMER, for a request that refuses nobody. Executed on the RC before the fix:
// three polls, three authz.denied/governance.ceiling rows; adding the drive
// seam's own emit would have made it six.
//
// This is the audit-row form of the rule resolveMeUserDrive already applies to
// the refusal metric and the WARN, and it is the stronger case: a row is the
// operator's COUNT of who was refused.
func TestMePollsAreNotDenials(t *testing.T) {
	// Both group-tier reads true, so both deciding sites are live and both would
	// fire — the deployment shape where the amplification is worst.
	st := &driveStore{hasGroupTier: true, userTierOnly: true, hasGroupTierAssignments: true}
	srv, rec := driveRunServer(st, "docker")
	ctx := driveMemberCtx(nil, true)

	for range 3 {
		// The state is still REPORTED: the mark suppresses the operator's row,
		// never the member's answer. A /me that stopped saying
		// groups_snapshot_stale would be F273 all over again.
		if _, _, reason := meDriveBody(t, srv, ctx); reason != driveUnavailableGroups {
			t.Fatalf("user_drive_unavailable = %q, want %q — the mark must suppress the audit row, not the "+
				"answer the member acts on", reason, driveUnavailableGroups)
		}
	}
	if n := len(driveDenialReasons(t, rec)); n != 0 {
		t.Errorf("three GET /me polls wrote %d authz.denied rows (%v) — a display read on a timer is not a "+
			"denial, and an operator counting denials would be counting page views. The member never asked "+
			"for a run", n, driveDenialReasons(t, rec))
	}

	// AND THE ENFORCEMENT PATH IS UNTOUCHED, on the same server, the same store
	// and the same member — which is what makes the suppression a SCOPING rather
	// than a hole. The launch really is refused, so it really is recorded.
	//
	// ONE ROW PER REFUSED REQUEST, not two, and that is a property of the order
	// rather than of the mark: handleCreateRun resolves the ceiling (runs.go:153)
	// BEFORE it reaches the drive seam (runs.go:170), so on a deployment where
	// both group-tier reads are true the ceiling refuses first and the drive
	// resolver is never asked. Which site speaks is therefore decided by the
	// deployment shape, and both are asserted below rather than assumed.
	t.Run("the ceiling seam records for an enforcement caller", func(t *testing.T) {
		before := len(driveDenialReasons(t, rec))
		if _, err := srv.effectiveCeiling(ctx); err == nil {
			t.Fatal("the ceiling resolved for a member whose group snapshot cannot answer it")
		}
		got := driveDenialReasons(t, rec)[before:]
		if len(got) != 1 || got[0] != "governance.ceiling" {
			t.Errorf("an enforcement ceiling resolve wrote %v, want exactly one governance.ceiling row — the "+
				"display-read mark must not reach a caller that is refusing somebody", got)
		}
	})

	// …and the shape where the DRIVE seam is the only site that can speak: no
	// group-tier governance assignment, so the ceiling resolves fine and the
	// refusal is decided at the drives door. This is the deployment F317 is
	// about, where an unaudited drive seam left the denial stream entirely empty.
	t.Run("the drive seam records for an enforcement caller", func(t *testing.T) {
		st := &driveStore{hasGroupTier: true, userTierOnly: true, hasGroupTierAssignments: false}
		srv, rec := driveRunServer(st, "docker")
		ctx := driveMemberCtx(nil, true)
		if _, ok, w := driveSeed(t, srv, driveRunRequest(true, nil), governanceCeiling{}, ctx); ok || w.Code != http.StatusForbidden {
			t.Fatalf("launch = %d, ok = %v; want the 403 the display read was warning about", w.Code, ok)
		}
		got := driveDenialReasons(t, rec)
		if len(got) != 1 || got[0] != "runs.drive" {
			t.Errorf("one refused launch wrote %v, want exactly one runs.drive row — the display-read mark "+
				"must not reach an enforcement caller, or F317 is closed by silencing the seam rather than "+
				"scoping it", got)
		}
	})
}

// driveDenialReasons lists the TARGET of every groups_snapshot_stale denial row
// recorded so far, in order — the two deciding sites are told apart by target.
func driveDenialReasons(t *testing.T, rec *recRecorder) []string {
	t.Helper()
	var out []string
	for _, ev := range rec.events {
		if ev.Action != "authz.denied" {
			continue
		}
		var data map[string]any
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			t.Fatal(err)
		}
		if r, _ := data["reason"].(string); r == "groups_snapshot_stale" {
			out = append(out, ev.Target)
		}
	}
	return out
}
