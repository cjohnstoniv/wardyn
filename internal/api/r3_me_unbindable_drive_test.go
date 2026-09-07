// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestMeWithholdsAnUnbindableDrive is F269.
//
// driveIsMountableHere ran at the launch door and at the ADMIN preview, and
// never on the member's own surface. So /me offered a mountable-looking
// allocation — name, size, writable, home_name, user_drive_unavailable "" — for
// a drive this deployment refuses 422 at launch ("directory carol does not exist
// on the share — ask an admin to create it"). The New Run card drew the checkbox
// and its writable sentence for a mount the create path was going to reject.
//
// The fixture is the launch side's own: a real share with one member's home in
// it, and a member whose directory nobody created.
func TestMeWithholdsAnUnbindableDrive(t *testing.T) {
	newShare := func(t *testing.T) (string, *driveStore) {
		t.Helper()
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, "bob"), 0o755); err != nil {
			t.Fatal(err)
		}
		d := driveFixture(func(d *types.UserDrive) {
			d.Backend, d.HomeTemplate, d.HostRoot = types.DriveBackendHostPath, types.HomeTemplateSub, root
			d.Writable = true
		})
		return root, &driveStore{drive: d, grant: grantFixture(d.ID, nil), tier: types.CapabilitySubjectUser}
	}
	memberCtx := func(sub string) context.Context {
		return withOIDCGroups(operatorCtx(sub, sub+"@corp.example", oidc.RoleMember), nil)
	}

	t.Run("/me and the launch agree about a missing home directory", func(t *testing.T) {
		root, st := newShare(t)
		srv, _ := driveShareServer(st, []string{root})
		// carol is allocated in Wardyn and absent on the NAS — the
		// offboarding-in-reverse case the launch path already refuses.
		ctx := memberCtx("carol")

		ud, denied, reason := meDriveBody(t, srv, ctx)
		if ud != nil {
			t.Errorf("/me offered user_drive = %v for an allocation the launch refuses 422 — the New Run card "+
				"draws a checkbox for a mount that cannot happen", ud)
		}
		if reason != driveUnavailableUnmountable {
			t.Errorf("user_drive_unavailable = %q, want %q — the documented meaning of that token is 'an "+
				"allocation EXISTS and cannot be mounted … a share that is not there', which is this state exactly",
				reason, driveUnavailableUnmountable)
		}
		if denied != "" {
			t.Errorf("user_drive_denied_by_profile = %q, want empty — no profile is involved", denied)
		}

		// AND THE LAUNCH REALLY DOES REFUSE IT, asserted here rather than
		// assumed: a test where /me withheld a drive the launch would have
		// mounted is the opposite defect.
		mount, ok, w := driveSeed(t, srv, driveRunRequest(true, nil), governanceCeiling{}, ctx)
		if ok || mount != nil {
			t.Fatalf("the launch MOUNTED the drive /me withheld: %+v", mount)
		}
		if w.Code != http.StatusUnprocessableEntity {
			t.Errorf("launch = %d, want 422", w.Code)
		}
	})

	// THE POSITIVE CONTROL: a bindable share is still offered, or this "fix"
	// would be /me withholding every drive.
	t.Run("a bindable drive is still offered", func(t *testing.T) {
		root, st := newShare(t)
		srv, _ := driveShareServer(st, []string{root})
		ud, _, reason := meDriveBody(t, srv, memberCtx("bob"))
		if ud == nil || reason != "" {
			t.Errorf("/me withheld a drive that binds (user_drive=%v reason=%q) — the gate must refuse the "+
				"deployment's own failures, not the feature", ud, reason)
		}
	})

	// A /ME POLL IS NOT A REFUSAL. The decision is shared with the launch door;
	// the REFUSAL is not. Running the writer here — even against a throwaway
	// recorder — would inflate wardyn_user_drive_refused_total on a timer and
	// fill the operator's log with WARNs for a member who never asked for a run.
	t.Run("a /me read moves no refusal metric and writes no warning", func(t *testing.T) {
		root, st := newShare(t)
		srv, _ := driveShareServer(st, []string{root})

		var buf bytes.Buffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
		t.Cleanup(func() { slog.SetDefault(prev) })

		before := driveRefusedMetric(t, srv)
		for range 5 {
			meDriveBody(t, srv, memberCtx("carol"))
		}
		if after := driveRefusedMetric(t, srv); after != before {
			t.Errorf("five /me polls moved the drive-refusal metric %d -> %d — a display read must not count "+
				"refusals the member never asked for", before, after)
		}
		if strings.Contains(buf.String(), "a run was refused its drive") {
			t.Errorf("a /me poll logged a run refusal:\n%s", buf.String())
		}
	})
}

// driveRefusedMetric reads the total across every reason from /metrics.
//
// R1 F321: this helper could not fail. It scraped /metrics on a Server with no
// AdminToken, so the response was a 401 whose 60-byte body contains no series at
// all; it then matched the prefix `wardyn_user_drive_refused_total`, which
// nothing emits — the exposition is `wardyn_drive_refusals_total`
// (metrics.go). Two independent reasons to return 0 unconditionally, so the
// subtest that reads "a /me read moves no refusal metric" passed with the
// regression injected: adding s.metrics.driveRefused(...) to the top of
// resolveMeUserDrive left it green.
//
// THE FATALS ARE THE FIX, not the prefix. A metric helper that silently returns
// 0 when it read nothing is a helper that turns every assertion built on it into
// a tautology, and the two defects above were each individually enough to do
// that. So a non-200 and an empty match are now failures in their own right, and
// the next way this helper stops seeing the series — a renamed metric, a
// re-tiered /metrics, a harness that stops carrying the token — fails loudly
// instead of quietly reporting that nothing happened.
func driveRefusedMetric(t *testing.T, srv *Server) int {
	t.Helper()
	w := do(t, srv, http.MethodGet, "/metrics", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /metrics = %d, want 200: a scrape that was REFUSED reads back as every counter at zero, "+
			"which makes 'the metric did not move' true by construction (body=%s)", w.Code, w.Body.String())
	}
	n, matched := 0, 0
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if !strings.HasPrefix(line, "wardyn_drive_refusals_total") {
			continue
		}
		matched++
		f := strings.Fields(line)
		if len(f) == 2 {
			var v int
			if _, err := fmt.Sscan(f[1], &v); err == nil {
				n += v
			}
		}
	}
	if matched == 0 {
		t.Fatalf("no wardyn_drive_refusals_total series in /metrics — every series is printed from the first "+
			"scrape (metrics.go), so matching none means this helper is reading the wrong name and any "+
			"assertion over its result is vacuous. Body:\n%s", w.Body.String())
	}
	return n
}
