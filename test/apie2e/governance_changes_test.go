// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package apie2e

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// ownDatabase points WARDYN_TEST_PG at a fresh database for the rest of the test. These tests run
// prune applies and read the whole governance document, so they must not share state with the other
// tests of this package, which all use the one database the DSN names.
func ownDatabase(t *testing.T) {
	t.Helper()
	dsn := os.Getenv("WARDYN_TEST_PG")
	if dsn == "" {
		t.Skip("WARDYN_TEST_PG not set; skipping apie2e black-box test")
	}
	ctx := context.Background()
	admin, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	name := "wardyn_gov_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		admin.Close()
		t.Fatalf("create database %s: %v", name, err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=$1 AND pid <> pg_backend_pid()`, name)
		_, _ = admin.Exec(context.Background(), `DROP DATABASE IF EXISTS `+name)
		admin.Close()
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse WARDYN_TEST_PG: %v", err)
	}
	u.Path = "/" + name
	t.Setenv("WARDYN_TEST_PG", u.String())
}

// recordingTransport records every request an SDK client sends, so a test can say what was never sent.
type recordingTransport struct {
	mu   sync.Mutex
	reqs []string
}

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.reqs = append(r.reqs, req.Method+" "+req.URL.Path)
	r.mu.Unlock()
	return http.DefaultTransport.RoundTrip(req)
}

func (r *recordingTransport) sent(prefix string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, q := range r.reqs {
		if strings.HasPrefix(q, prefix) {
			n++
		}
	}
	return n
}

func (r *recordingTransport) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reqs = nil
}

func (h *harness) pendingChanges() int {
	h.t.Helper()
	var n int
	if err := h.pool.QueryRow(context.Background(), `SELECT count(*) FROM governance_changes WHERE state = 'pending'`).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

func spec(domains ...string) client.RunPolicySpec {
	return client.RunPolicySpec{AllowedDomains: domains, MinConfinementClass: types.CC2}
}

// TestGovernanceChanges_SDKAgainstTheRealHandler drives pkg/client against the real
// /governance/changes handler with the four-eyes switch on and a seeded security_admin token (its own
// principal and email), never the admin token. It covers create, update, delete and a mixed
// applied/pending document, and pins what the SDK must not do under a 202: decode the pending body as
// a saved profile (so no assignment carries uuid.Nil) and prune against a state that is not final.
func TestGovernanceChanges_SDKAgainstTheRealHandler(t *testing.T) {
	ownDatabase(t)
	t.Setenv("WARDYN_GOVERNANCE_SECOND_HUMAN", "true")
	wide := types.RunPolicySpec{
		AllowedDomains:      []string{"api.anthropic.com", "pypi.org", "github.com", "npmjs.org"},
		MinConfinementClass: types.CC2,
	}
	h := newHarness(t, harnessOpts{defaultPolicy: &wide})
	ctx := context.Background()

	alice := h.securityAdminClient("sub-alice", "alice@corp.example")
	bob := h.securityAdminClient("sub-bob", "bob@corp.example")
	rec := &recordingTransport{}
	alice.HTTPClient = &http.Client{Transport: rec, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	approveAll := func() int {
		t.Helper()
		pending, err := bob.ListGovernanceChanges(ctx, "pending")
		if err != nil {
			t.Fatalf("list pending: %v", err)
		}
		for _, ch := range pending {
			got, err := bob.ApproveGovernanceChange(ctx, ch.ID)
			if err != nil {
				t.Fatalf("approve %s (%s %s): %v", ch.ID, ch.TargetKind, ch.Op, err)
			}
			if got.State != "applied" {
				t.Fatalf("approved change state = %q, want applied", got.State)
			}
		}
		return len(pending)
	}
	profileNamed := func(name string) (client.GovernanceProfile, bool) {
		t.Helper()
		doc, err := bob.GetGovernance(ctx)
		if err != nil {
			t.Fatalf("GetGovernance: %v", err)
		}
		for _, p := range doc.Profiles {
			if p.Name == name {
				return p, true
			}
		}
		return client.GovernanceProfile{}, false
	}

	// CREATE: a new profile and an assignment that names it by a file-local id. The profile write is
	// held (202); the assignment names a profile that does not exist yet, so it is deferred, never
	// sent, and in particular never sent with the zero id a mis-decoded 202 would give it.
	fileID := uuid.New()
	doc := client.GovernanceDocument{
		Profiles: []client.GovernanceProfile{{ID: fileID, Name: "contractors", Ceiling: spec("pypi.org")}},
		Assignments: []client.GovernanceAssignment{{
			SubjectType: client.CapabilitySubjectUser, Subject: "dev@corp.example", ProfileID: fileID, Priority: 1,
		}},
	}
	res, err := alice.ApplyGovernanceResult(ctx, doc, true)
	if err != nil {
		t.Fatalf("ApplyGovernanceResult(create): %v", err)
	}
	if len(res.Pending) != 1 || res.Pending[0].TargetKind != "governance_profile" || res.Pending[0].State != "pending" {
		t.Fatalf("create: pending = %+v, want one pending governance_profile change", res.Pending)
	}
	if len(res.Deferred) != 1 || res.Deferred[0].Profile != "contractors" || !res.PruneSkipped {
		t.Fatalf("create: deferred = %+v prune_skipped = %v, want the assignment deferred and prune skipped", res.Deferred, res.PruneSkipped)
	}
	if n := rec.sent("POST /api/v1/governance/assignments"); n != 0 {
		t.Fatalf("an assignment was sent %d time(s) while its profile was pending", n)
	}
	if n := rec.sent("DELETE "); n != 0 {
		t.Fatalf("prune ran (%d deletes) with a write pending", n)
	}
	if h.pendingChanges() != 1 {
		t.Fatalf("%d pending rows after the 202, want 1", h.pendingChanges())
	}
	if _, ok := profileNamed("contractors"); ok {
		t.Fatal("the held profile exists before approval")
	}
	// The plain method keeps its signature and fails loudly with a typed error.
	_, err = alice.ApplyGovernance(ctx, client.GovernanceDocument{
		Profiles: []client.GovernanceProfile{{Name: "interns", Ceiling: spec("pypi.org")}},
	}, false)
	var pend *client.PendingApprovalError
	if !errors.As(err, &pend) || len(pend.Changes) != 1 {
		t.Fatalf("ApplyGovernance under four-eyes = %v, want a *PendingApprovalError naming one change", err)
	}
	// A reviewer may reject: nothing is applied, and the reason travels with the change.
	rejected, err := bob.RejectGovernanceChange(ctx, pend.Changes[0].ID, "not needed")
	if err != nil || rejected.State != "rejected" {
		t.Fatalf("reject = %+v, %v; want a rejected change", rejected, err)
	}
	if _, ok := profileNamed("interns"); ok {
		t.Fatal("a rejected profile exists")
	}
	// A self-approval is refused: alice is not a second human.
	if _, err := alice.ApproveGovernanceChange(ctx, res.Pending[0].ID); err == nil {
		t.Fatal("alice approved her own change")
	} else {
		var ae *client.APIError
		if !errors.As(err, &ae) || ae.Status != http.StatusForbidden || ae.Reason != "second_human_required" {
			t.Fatalf("self-approval = %v, want 403 second_human_required", err)
		}
	}
	if n := approveAll(); n != 1 {
		t.Fatalf("approved %d changes, want the profile create", n)
	}
	contractors, ok := profileNamed("contractors")
	if !ok || len(contractors.Ceiling.AllowedDomains) != 1 {
		t.Fatalf("the approved profile = %+v (found %v)", contractors, ok)
	}

	// Now the profile exists, so the same document sends its assignment: held too (an assignment
	// write is never exempt), and approved by the second human.
	rec.reset()
	doc.Profiles[0].ID = contractors.ID
	doc.Assignments[0].ProfileID = contractors.ID
	res, err = alice.ApplyGovernanceResult(ctx, doc, false)
	if err != nil || len(res.Pending) != 1 || res.Pending[0].TargetKind != "governance_assignment" || len(res.Deferred) != 0 {
		t.Fatalf("assignment apply = %+v, %v; want one pending governance_assignment change and nothing deferred", res, err)
	}
	if n := rec.sent("PUT /api/v1/governance/profiles/") + rec.sent("POST /api/v1/governance/profiles"); n != 0 {
		t.Fatalf("an unchanged profile was written %d time(s)", n)
	}
	if n := approveAll(); n != 1 {
		t.Fatalf("approved %d changes, want the assignment", n)
	}
	after, err := bob.GetGovernance(ctx)
	if err != nil || len(after.Assignments) != 1 || after.Assignments[0].ProfileID != contractors.ID || after.Assignments[0].ProfileID == uuid.Nil {
		t.Fatalf("the approved assignment = %+v, %v", after.Assignments, err)
	}

	// UPDATE, mixed: widen "contractors" (held) and narrow a second profile (exempt: applied at once).
	// A second profile, proposed by bob and approved by alice: neither is the admin token.
	if _, err := bob.ApplyGovernance(ctx, client.GovernanceDocument{
		Profiles: []client.GovernanceProfile{{Name: "wide-set", Ceiling: spec("pypi.org", "github.com")}},
	}, false); !errors.As(err, &pend) {
		t.Fatalf("creating the second profile = %v, want pending", err)
	}
	pendingBob, err := alice.ListGovernanceChanges(ctx, "pending")
	if err != nil || len(pendingBob) != 1 {
		t.Fatalf("alice sees %d pending changes (%v), want bob's one", len(pendingBob), err)
	}
	if _, err := alice.ApproveGovernanceChange(ctx, pendingBob[0].ID); err != nil {
		t.Fatalf("alice approving bob's change: %v", err)
	}

	rec.reset()
	mixed := client.GovernanceDocument{
		Profiles: []client.GovernanceProfile{
			{Name: "contractors", Ceiling: spec("pypi.org", "github.com")}, // widening: held
			{Name: "wide-set", Ceiling: spec("pypi.org")},                  // narrowing: applied
		},
		Assignments: []client.GovernanceAssignment{{
			SubjectType: client.CapabilitySubjectUser, Subject: "dev@corp.example", ProfileID: contractors.ID, Priority: 1,
		}},
	}
	res, err = alice.ApplyGovernanceResult(ctx, mixed, true)
	if err != nil {
		t.Fatalf("mixed apply: %v", err)
	}
	if len(res.Pending) != 1 || res.Pending[0].Op != "update" {
		t.Fatalf("mixed: pending = %+v, want exactly the widening update", res.Pending)
	}
	if !res.PruneSkipped || rec.sent("DELETE ") != 0 {
		t.Fatalf("mixed: prune_skipped = %v, deletes sent %d; prune must not run with a write pending", res.PruneSkipped, rec.sent("DELETE "))
	}
	narrowed, ok := profileNamed("wide-set")
	if !ok || len(narrowed.Ceiling.AllowedDomains) != 1 {
		t.Fatalf("the narrowing update was not applied directly: %+v (found %v)", narrowed, ok)
	}
	if got, _ := profileNamed("contractors"); len(got.Ceiling.AllowedDomains) != 1 {
		t.Fatalf("the held widening is already in force: %v", got.Ceiling.AllowedDomains)
	}
	if n := approveAll(); n != 1 {
		t.Fatalf("approved %d changes, want the widening", n)
	}
	if got, _ := profileNamed("contractors"); len(got.Ceiling.AllowedDomains) != 2 {
		t.Fatalf("the approved widening is not in force: %v", got.Ceiling.AllowedDomains)
	}

	// DELETE: prune. With nothing else pending it runs, and each delete it issues is itself held.
	rec.reset()
	res, err = alice.ApplyGovernanceResult(ctx, client.GovernanceDocument{
		Profiles: []client.GovernanceProfile{{Name: "contractors", Ceiling: spec("pypi.org", "github.com")}},
	}, true)
	if err != nil {
		t.Fatalf("prune apply: %v", err)
	}
	if len(res.Pending) != 1 || res.Pending[0].TargetKind != "governance_assignment" || res.Pending[0].Op != "delete" || !res.PruneSkipped {
		t.Fatalf("prune: %+v, want the assignment delete held and the rest of the prune skipped", res)
	}
	if n := approveAll(); n != 1 {
		t.Fatalf("approved %d changes, want the assignment delete", n)
	}
	if doc, _ := bob.GetGovernance(ctx); len(doc.Assignments) != 0 {
		t.Fatalf("the approved delete left %+v", doc.Assignments)
	}
	res, err = alice.ApplyGovernanceResult(ctx, client.GovernanceDocument{
		Profiles: []client.GovernanceProfile{{Name: "contractors", Ceiling: spec("pypi.org", "github.com")}},
	}, true)
	if err != nil || len(res.Pending) != 1 || res.Pending[0].TargetKind != "governance_profile" || res.Pending[0].Op != "delete" {
		t.Fatalf("second prune = %+v, %v; want the wide-set profile delete held", res, err)
	}
	if n := approveAll(); n != 1 {
		t.Fatalf("approved %d changes, want the profile delete", n)
	}
	if _, ok := profileNamed("wide-set"); ok {
		t.Fatal("the approved delete left the profile")
	}

	// The whole run never sent an assignment with the zero id.
	var nilAssignments int
	if err := h.pool.QueryRow(ctx, `SELECT count(*) FROM governance_assignments WHERE profile_id = $1`, uuid.Nil).Scan(&nilAssignments); err != nil || nilAssignments != 0 {
		t.Fatalf("%d assignments carry the nil profile id (%v)", nilAssignments, err)
	}
	if h.pendingChanges() != 0 {
		t.Fatalf("%d changes still pending at the end", h.pendingChanges())
	}
}
