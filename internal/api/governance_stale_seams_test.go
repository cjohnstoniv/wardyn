// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// TestStaleSnapshotIsDecidedOnlyWhereItIsRecorded is F227's cross-cutting half.
//
// F227 is that a member-reachable groups_snapshot_stale 403 left no
// authz.denied row, against docs/OPERATIONS.md:1508's categorical claim that
// every member denial which is not a plain foreign-resource 404 is audited. The
// fix records at the site that DECIDES the refusal rather than at the six-odd
// seams that write it, and governance.go:812 argues that placement at length:
// the three write helpers (writeCeilingError, writeCeilingErrorPrefixed,
// ceilingErrorStatus) are free functions with no server and no context, so
// auditing there would mean one emit per seam and a seam that hands the code
// upward emitting nothing.
//
// THAT ARGUMENT IS ONLY TRUE WHILE THE DECIDING SITES ARE THE ONLY SOURCE. A
// seam that raised errGroupsSnapshotStale itself — a new resolver, a copy of the
// unusable-groups arm, a shortcut that skips effectiveCeiling — would refuse a
// member with the documented sentence and record nothing, which is F227 again at
// a site nobody thought to look at. Nothing executed that claim; the seams are
// spread across policies.go, secrets.go, inline_policy.go,
// runs_create_validate.go, workspace_run.go, profile.go and user_drives.go, and
// the property that makes them all safe lives in none of them.
//
// So it is asserted mechanically: the sentinel is RAISED only at sites that
// RECORD. Both halves matter — a new raising site is a silent denial, and a
// deciding site that stops recording is the original finding.
func TestStaleSnapshotIsDecidedOnlyWhereItIsRecorded(t *testing.T) {
	// The two sites the design names, each of which emits authz.denied for the
	// refusal it decides. An addition here is a claim that a THIRD place may
	// decide this refusal, and it has to bring its own recordAudit with it —
	// which the second half of this test then checks.
	want := map[string]bool{
		"ceilingWithUnusableGroups": true, // governance.go — target governance.ceiling
		"driveWithUnusableGroups":   true, // user_drives_resolve.go — target runs.drive
	}

	raisers, records := staleSentinelSites(t)

	for _, name := range slices.Sorted(maps.Keys(raisers)) {
		if !want[name] {
			t.Errorf("%s raises errGroupsSnapshotStale, which is a member-reachable 403, but it is not one of "+
				"the deciding sites that record it. Either resolve through effectiveCeiling / resolveUserDrive "+
				"so an existing site decides, or emit authz.denied here — a refusal with no row is the whole "+
				"of F227, and docs/OPERATIONS.md:1508 says every member denial that is not a foreign-resource "+
				"404 is audited", name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(want)) {
		if !raisers[name] {
			t.Errorf("%s no longer raises errGroupsSnapshotStale — this guard reads the deciding sites from "+
				"source; re-point it at the new shape rather than deleting it", name)
		}
		if !records[name] {
			t.Errorf("%s decides the groups_snapshot_stale refusal and no longer calls recordAudit: the "+
				"denial stream is the operator's only view of who cannot use the product, and this is the "+
				"site F227 put the row at", name)
		}
	}
}

// staleSentinelSites reports which internal/api functions RETURN
// errGroupsSnapshotStale, and which of those also call recordAudit. errors.Is
// matches are deliberately not counted: matching the sentinel is what the write
// helpers do, and they are not deciding anything.
func staleSentinelSites(t *testing.T) (raisers, records map[string]bool) {
	t.Helper()
	raisers, records = map[string]bool{}, map[string]bool{}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			if staleSentinelReturned(fn) {
				raisers[fn.Name.Name] = true
				if callsNamed(fn, "recordAudit") {
					records[fn.Name.Name] = true
				}
			}
		}
	}
	if len(raisers) == 0 {
		t.Fatal("no function returns errGroupsSnapshotStale — this guard reads the sentinel from source; " +
			"re-point it at the new shape rather than deleting it")
	}
	return raisers, records
}

// staleSentinelReturned reports whether fn RETURNS the sentinel — bare, or
// wrapped through a call such as fmt.Errorf("%w", …).
func staleSentinelReturned(fn *ast.FuncDecl) bool {
	found := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		ret, ok := n.(*ast.ReturnStmt)
		if !ok || found {
			return !found
		}
		for _, res := range ret.Results {
			ast.Inspect(res, func(inner ast.Node) bool {
				if id, ok := inner.(*ast.Ident); ok && id.Name == "errGroupsSnapshotStale" {
					found = true
				}
				return !found
			})
		}
		return !found
	})
	return found
}

func callsNamed(fn *ast.FuncDecl, name string) bool {
	found := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || found {
			return !found
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == name {
			found = true
		}
		return !found
	})
	return found
}

// TestDrivePreviewWritesNoDenial is F227's residue.
//
// The emit landed at the deciding sites, and BOTH of them are reached by the
// admin drive preview: drivePreviewDoorIsOpen resolves the previewed principal's
// ceiling, previewResolveUserDrive resolves their drive. So an admin asking
// "what would carol get" wrote an authz.denied row — naming THE ADMIN as the
// refused principal, because the row is stamped from the request's own identity.
// Executed before the fix: one preview of carol's drive produced
// `authz.denied target=governance.ceiling actor="sub-admin-alice"`.
//
// handlePreviewUserDrive's own doc has always said this endpoint is "STILL NOT
// AUDITED … nothing is minted and nothing changes", so the tree stated the rule
// and then broke it. A denial stream with the wrong person in it is worse than
// the silence F227 set out to fix: the silence was at least honest about who had
// been refused.
func TestDrivePreviewWritesNoDenial(t *testing.T) {
	st := &driveStore{hasGroupTier: true, userTierOnly: true, hasGroupTierAssignments: true}
	audit := &recRecorder{}
	srv := New(Config{Store: st, Audit: audit, RunnerTarget: "docker"})

	body, err := json.Marshal(governancePreviewRequest{UserSubjects: []string{"carol@corp.example"}})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/drives/preview", strings.NewReader(string(body))).
		WithContext(operatorCtx("sub-admin-alice", "alice@corp.example", oidc.RoleAdmin))
	w := httptest.NewRecorder()
	srv.handlePreviewUserDrive(w, r)

	// The ANSWER is unchanged: the preview still refuses, in the launch path's
	// own words, which is what makes it useful to the admin. Only the row goes.
	if w.Code != http.StatusForbidden {
		t.Fatalf("preview = %d, want the 403 the launch would give; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "groups_snapshot_stale") {
		t.Errorf("body = %s, want the stale-snapshot refusal", w.Body.String())
	}

	for _, ev := range audit.events {
		if ev.Action != "authz.denied" {
			continue
		}
		var data map[string]any
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			t.Fatal(err)
		}
		t.Errorf("an ADMIN preview wrote authz.denied target=%s actor=%q reason=%v — nobody was refused "+
			"anything they asked for, and the row names the admin rather than the principal it is about",
			ev.Target, ev.Actor, data["reason"])
	}
}
