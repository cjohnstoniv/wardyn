// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

// governance_pending_test.go pins the SDK's handling of a governance write the
// server holds for a second approver: 202 with a {"pending_change": ...} body
// (docs/design/0.8/0.8.6-gov4.md, D2). The fake below answers that exact body
// shape. No SDK method may read the pending body as a saved object, and nothing
// that depends on a pending write may be sent after it.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// pendingFake is a /governance family that answers 202 for the writes it is
// told to hold and applies the rest, recording every request it receives.
type pendingFake struct {
	mu          sync.Mutex
	profiles    []client.GovernanceProfile
	assignments []client.GovernanceAssignment

	holdProfiles    map[string]bool      // profile names whose create/update is held
	holdAssignments map[string]bool      // assignment subjects whose write is held
	holdDeletes     bool                 // every DELETE is held
	heldProfiles    map[string]uuid.UUID // profile names a live change already holds: a write answers 409 naming it
	bigDiff         bool                 // pad the held change's diff past the 2 KiB error-body cap
	heldDiffers     bool                 // the held change is not the refused write (pending_change_matches false)

	reqs        []string // "METHOD path", in arrival order
	assignSent  []client.GovernanceAssignmentRequest
	changes     []client.GovernanceChange
	approveCall string
	rejectBody  string
}

func pendingChange(op, kind, key string) map[string]any {
	return map[string]any{"pending_change": map[string]any{
		"id": uuid.New(), "target_kind": kind, "op": op, "target_key": key,
		"state": "pending", "proposed_by": "alice",
		"proposed_at": time.Now().UTC().Format(time.RFC3339),
		"expires_at":  time.Now().Add(72 * time.Hour).UTC().Format(time.RFC3339),
		"diff":        map[string]any{"before": nil, "after": map[string]any{"name": key}, "changed": []string{"name"}},
	}}
}

func (f *pendingFake) server(t *testing.T) *client.Client {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.reqs = append(f.reqs, r.Method+" "+r.URL.RequestURI())
		p := r.URL.Path
		switch {
		case r.Method == http.MethodGet && p == "/api/v1/governance":
			writeJSON(w, http.StatusOK, client.GovernanceDocument{Profiles: f.profiles, Assignments: f.assignments})

		case r.Method == http.MethodPost && p == "/api/v1/governance/profiles":
			var req client.GovernanceProfileRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			if f.answerHeld(w, req.Name) {
				return
			}
			if f.holdProfiles[req.Name] {
				writeJSON(w, http.StatusAccepted, pendingChange("create", "governance_profile", req.Name))
				return
			}
			prof := client.GovernanceProfile{ID: uuid.New(), Name: req.Name, Ceiling: req.Ceiling, Limits: req.Limits}
			f.profiles = append(f.profiles, prof)
			writeJSON(w, http.StatusCreated, client.GovernanceProfileResponse{Profile: prof})

		case r.Method == http.MethodPut && strings.HasPrefix(p, "/api/v1/governance/profiles/"):
			var req client.GovernanceProfileRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			if f.answerHeld(w, req.Name) {
				return
			}
			if f.holdProfiles[req.Name] {
				writeJSON(w, http.StatusAccepted, pendingChange("update", "governance_profile", req.Name))
				return
			}
			id, _ := uuid.Parse(strings.TrimPrefix(p, "/api/v1/governance/profiles/"))
			prof := client.GovernanceProfile{ID: id, Name: req.Name, Ceiling: req.Ceiling, Limits: req.Limits}
			for i := range f.profiles {
				if f.profiles[i].ID == id {
					f.profiles[i] = prof
				}
			}
			writeJSON(w, http.StatusOK, client.GovernanceProfileResponse{Profile: prof})

		case r.Method == http.MethodPost && p == "/api/v1/governance/assignments":
			var req client.GovernanceAssignmentRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			f.assignSent = append(f.assignSent, req)
			if f.holdAssignments[req.Subject] {
				writeJSON(w, http.StatusAccepted, pendingChange("upsert", "governance_assignment", req.Subject))
				return
			}
			a := client.GovernanceAssignment{ID: uuid.New(), SubjectType: req.SubjectType, Subject: req.Subject,
				ProfileID: req.ProfileID, Priority: req.Priority}
			f.assignments = append(f.assignments, a)
			writeJSON(w, http.StatusCreated, a)

		case r.Method == http.MethodDelete && strings.HasPrefix(p, "/api/v1/governance/"):
			if f.holdDeletes {
				writeJSON(w, http.StatusAccepted, pendingChange("delete", "governance_profile", p))
				return
			}
			w.WriteHeader(http.StatusNoContent)

		case r.Method == http.MethodGet && p == "/api/v1/governance/changes":
			writeJSON(w, http.StatusOK, f.changes)

		case r.Method == http.MethodGet && strings.HasPrefix(p, "/api/v1/governance/changes/"):
			writeJSON(w, http.StatusOK, f.changes[0])

		case r.Method == http.MethodPost && strings.HasSuffix(p, "/approve"):
			f.approveCall = p
			ch := f.changes[0]
			ch.State = "applied"
			writeJSON(w, http.StatusOK, ch)

		case r.Method == http.MethodPost && strings.HasSuffix(p, "/reject"):
			b, _ := io.ReadAll(r.Body)
			f.rejectBody = string(b)
			ch := f.changes[0]
			ch.State = "rejected"
			writeJSON(w, http.StatusOK, ch)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return &client.Client{BaseURL: srv.URL, Token: testToken, HTTPClient: srv.Client()}
}

// answerHeld answers the 409 governance_change_pending the real server gives a second proposal at a
// held target, with the held change beside the error, and reports whether it did.
func (f *pendingFake) answerHeld(w http.ResponseWriter, name string) bool {
	id, ok := f.heldProfiles[name]
	if !ok {
		return false
	}
	held := pendingChange("update", "governance_profile", name)["pending_change"].(map[string]any)
	held["id"] = id
	if f.bigDiff {
		held["payload"] = map[string]any{"pad": strings.Repeat("x", 8192)}
	}
	writeJSON(w, http.StatusConflict, map[string]any{
		"error":  "a change to this target is already waiting for approval: " + id.String(),
		"reason": "governance_change_pending", "pending_change": held, "pending_change_matches": !f.heldDiffers,
	})
	return true
}

// writes returns the mutating requests the fake received.
func (f *pendingFake) writes() []string {
	var out []string
	for _, r := range f.reqs {
		if !strings.HasPrefix(r, "GET ") {
			out = append(out, r)
		}
	}
	return out
}

func (f *pendingFake) noNilAssignment(t *testing.T) {
	t.Helper()
	for _, a := range f.assignSent {
		if a.ProfileID == uuid.Nil {
			t.Errorf("assignment %q was sent with a nil profile id", a.Subject)
		}
	}
}

// TestDo_PendingChangeIsNeverDecodedAsSuccess: a 202 pending_change is a typed
// error from every method that goes through the shared path, with the body
// never read into the caller's output.
func TestDo_PendingChangeIsNeverDecodedAsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusAccepted, pendingChange("update", "governance_profile", "p"))
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(srv)

	ctx := context.Background()
	id := uuid.New()
	for name, call := range map[string]func() error{
		"GetGovernance": func() error { _, err := c.GetGovernance(ctx); return err },
		"ApproveGovernanceChange": func() error {
			ch, err := c.ApproveGovernanceChange(ctx, id)
			if ch.ID != uuid.Nil {
				t.Errorf("ApproveGovernanceChange returned a populated change on a pending answer")
			}
			return err
		},
		"KillRun": func() error { _, err := c.KillRun(ctx, id); return err },
	} {
		err := call()
		var pe *client.PendingApprovalError
		if !errors.As(err, &pe) {
			t.Errorf("%s: err = %v, want *PendingApprovalError", name, err)
			continue
		}
		if len(pe.Changes) != 1 || pe.Changes[0].State != "pending" || pe.Changes[0].ProposedBy != "alice" {
			t.Errorf("%s: pending change not decoded: %+v", name, pe.Changes)
		}
	}
}

// TestDo_202WithoutPendingChangeStillDecodes: 202 alone is not a pending
// change. These four routes answer 202 with their own bodies.
func TestDo_202WithoutPendingChangeStillDecodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/kill"):
			writeJSON(w, http.StatusAccepted, map[string]any{"id": uuid.New(), "state": "KILLED"})
		case strings.HasSuffix(r.URL.Path, "/record"):
			writeJSON(w, http.StatusAccepted, map[string]any{"record_run_id": "r1", "task_key": "k", "mode": "open"})
		default: // /scan on a workspace and on a source
			writeJSON(w, http.StatusAccepted, map[string]any{"state": "scanning", "pending": true})
		}
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(srv)
	ctx := context.Background()
	id := uuid.New()

	if kr, err := c.KillRun(ctx, id); err != nil || kr.State != "KILLED" {
		t.Errorf("KillRun = %+v, %v; want the decoded body", kr, err)
	}
	if rt, err := c.RecordWorkspaceTask(ctx, id, "k"); err != nil || rt.RecordRunID != "r1" {
		t.Errorf("RecordWorkspaceTask = %+v, %v; want the decoded body", rt, err)
	}
	for name, call := range map[string]func() (json.RawMessage, error){
		"ScanWorkspace": func() (json.RawMessage, error) { return c.ScanWorkspace(ctx, id) },
		"ScanSource":    func() (json.RawMessage, error) { return c.ScanSource(ctx, id) },
	} {
		raw, err := call()
		if err != nil || !strings.Contains(string(raw), `"scanning"`) {
			t.Errorf("%s = %s, %v; want the decoded body", name, raw, err)
		}
	}
}

func TestApplyGovernance_CreateUpdateDeleteEachPending(t *testing.T) {
	ctx := context.Background()

	t.Run("create", func(t *testing.T) {
		f := &pendingFake{holdProfiles: map[string]bool{"new": true}}
		c := f.server(t)
		res, err := c.ApplyGovernanceResult(ctx, client.GovernanceDocument{
			Profiles: []client.GovernanceProfile{{Name: "new"}}}, false)
		if err != nil {
			t.Fatalf("ApplyGovernanceResult: %v", err)
		}
		if len(res.Pending) != 1 || res.Pending[0].Op != "create" {
			t.Fatalf("pending = %+v, want one create", res.Pending)
		}
		if len(res.Document.Profiles) != 0 {
			t.Errorf("document holds %d profiles, want 0 (the create is not applied)", len(res.Document.Profiles))
		}
	})

	t.Run("update", func(t *testing.T) {
		existing := client.GovernanceProfile{ID: uuid.New(), Name: "p", Limits: client.GovernanceLimits{MaxConcurrentRuns: 1}}
		f := &pendingFake{profiles: []client.GovernanceProfile{existing}, holdProfiles: map[string]bool{"p": true}}
		c := f.server(t)
		res, err := c.ApplyGovernanceResult(ctx, client.GovernanceDocument{
			Profiles: []client.GovernanceProfile{{Name: "p", Limits: client.GovernanceLimits{MaxConcurrentRuns: 9}}}}, false)
		if err != nil {
			t.Fatalf("ApplyGovernanceResult: %v", err)
		}
		if len(res.Pending) != 1 || res.Pending[0].Op != "update" {
			t.Fatalf("pending = %+v, want one update", res.Pending)
		}
		if got := res.Document.Profiles[0].Limits.MaxConcurrentRuns; got != 1 {
			t.Errorf("document shows limit %d, want the unchanged 1", got)
		}
	})

	t.Run("delete", func(t *testing.T) {
		existing := client.GovernanceProfile{ID: uuid.New(), Name: "gone"}
		f := &pendingFake{profiles: []client.GovernanceProfile{existing}, holdDeletes: true}
		c := f.server(t)
		res, err := c.ApplyGovernanceResult(ctx, client.GovernanceDocument{}, true)
		if err != nil {
			t.Fatalf("ApplyGovernanceResult: %v", err)
		}
		if len(res.Pending) != 1 || res.Pending[0].Op != "delete" || !res.PruneSkipped {
			t.Fatalf("result = %+v, want one pending delete and PruneSkipped", res)
		}
	})
}

func TestApplyGovernance_MixedAppliedAndPending(t *testing.T) {
	f := &pendingFake{holdProfiles: map[string]bool{"held": true}}
	c := f.server(t)
	_, err := c.ApplyGovernanceResult(context.Background(), client.GovernanceDocument{
		Profiles: []client.GovernanceProfile{{Name: "free"}, {Name: "held"}, {Name: "also-free"}}}, false)
	if err != nil {
		t.Fatalf("ApplyGovernanceResult: %v", err)
	}
	res, _ := c.ApplyGovernanceResult(context.Background(), client.GovernanceDocument{}, false)
	var names []string
	for _, p := range res.Document.Profiles {
		names = append(names, p.Name)
	}
	if strings.Join(names, ",") != "free,also-free" {
		t.Errorf("applied profiles = %v, want free and also-free (independent writes still apply)", names)
	}

	// ApplyGovernance reports the same document plus the typed error.
	f2 := &pendingFake{holdProfiles: map[string]bool{"held": true}}
	doc, err := f2.server(t).ApplyGovernance(context.Background(), client.GovernanceDocument{
		Profiles: []client.GovernanceProfile{{Name: "free"}, {Name: "held"}}}, false)
	var pe *client.PendingApprovalError
	if !errors.As(err, &pe) || len(pe.Changes) != 1 {
		t.Fatalf("ApplyGovernance err = %v, want *PendingApprovalError with one change", err)
	}
	if len(doc.Profiles) != 1 || doc.Profiles[0].Name != "free" {
		t.Errorf("ApplyGovernance document = %+v, want only the applied profile", doc.Profiles)
	}
}

func TestApplyGovernance_AssignmentToPendingProfileIsDeferred(t *testing.T) {
	ctx := context.Background()

	t.Run("pending create, file ids", func(t *testing.T) {
		fileID := uuid.New()
		f := &pendingFake{holdProfiles: map[string]bool{"new": true}}
		c := f.server(t)
		res, err := c.ApplyGovernanceResult(ctx, client.GovernanceDocument{
			Profiles: []client.GovernanceProfile{{ID: fileID, Name: "new"}},
			Assignments: []client.GovernanceAssignment{
				{SubjectType: client.CapabilitySubjectUser, Subject: "bob", ProfileID: fileID}},
		}, false)
		if err != nil {
			t.Fatalf("ApplyGovernanceResult: %v", err)
		}
		if len(res.Deferred) != 1 || res.Deferred[0].Subject != "bob" || res.Deferred[0].Profile != "new" {
			t.Errorf("deferred = %+v, want bob -> new", res.Deferred)
		}
		if len(f.assignSent) != 0 {
			t.Errorf("%d assignment(s) were sent, want none", len(f.assignSent))
		}
		f.noNilAssignment(t)
	})

	t.Run("pending update of an existing profile named by its real id", func(t *testing.T) {
		existing := client.GovernanceProfile{ID: uuid.New(), Name: "p"}
		f := &pendingFake{profiles: []client.GovernanceProfile{existing}, holdProfiles: map[string]bool{"p": true}}
		c := f.server(t)
		res, err := c.ApplyGovernanceResult(ctx, client.GovernanceDocument{
			Profiles: []client.GovernanceProfile{{Name: "p", Limits: client.GovernanceLimits{MaxConcurrentRuns: 3}}},
			Assignments: []client.GovernanceAssignment{
				{SubjectType: client.CapabilitySubjectGroup, Subject: "eng", ProfileID: existing.ID}},
		}, false)
		if err != nil {
			t.Fatalf("ApplyGovernanceResult: %v", err)
		}
		if len(res.Deferred) != 1 || len(f.assignSent) != 0 {
			t.Errorf("deferred = %+v, sent = %d; want the assignment deferred and not sent", res.Deferred, len(f.assignSent))
		}
	})

	t.Run("an assignment to an unaffected profile is still sent", func(t *testing.T) {
		f := &pendingFake{holdProfiles: map[string]bool{"held": true}}
		c := f.server(t)
		freeID, heldID := uuid.New(), uuid.New()
		res, err := c.ApplyGovernanceResult(ctx, client.GovernanceDocument{
			Profiles: []client.GovernanceProfile{{ID: freeID, Name: "free"}, {ID: heldID, Name: "held"}},
			Assignments: []client.GovernanceAssignment{
				{SubjectType: client.CapabilitySubjectUser, Subject: "u1", ProfileID: freeID},
				{SubjectType: client.CapabilitySubjectUser, Subject: "u2", ProfileID: heldID}},
		}, false)
		if err != nil {
			t.Fatalf("ApplyGovernanceResult: %v", err)
		}
		if len(f.assignSent) != 1 || f.assignSent[0].Subject != "u1" {
			t.Errorf("sent = %+v, want only u1", f.assignSent)
		}
		if len(res.Deferred) != 1 || res.Deferred[0].Subject != "u2" {
			t.Errorf("deferred = %+v, want only u2", res.Deferred)
		}
		f.noNilAssignment(t)
	})

	t.Run("a pending assignment write is recorded, not decoded", func(t *testing.T) {
		existing := client.GovernanceProfile{ID: uuid.New(), Name: "p"}
		f := &pendingFake{profiles: []client.GovernanceProfile{existing}, holdAssignments: map[string]bool{"carol": true}}
		c := f.server(t)
		res, err := c.ApplyGovernanceResult(ctx, client.GovernanceDocument{
			Assignments: []client.GovernanceAssignment{
				{SubjectType: client.CapabilitySubjectUser, Subject: "carol", ProfileID: existing.ID}},
		}, false)
		if err != nil || len(res.Pending) != 1 || res.Pending[0].TargetKind != "governance_assignment" {
			t.Errorf("result = %+v, err = %v; want one pending assignment change", res, err)
		}
		if len(res.Document.Assignments) != 0 {
			t.Errorf("document holds %d assignments, want 0", len(res.Document.Assignments))
		}
	})
}

func TestApplyGovernance_PruneNeverFollowsAPendingWrite(t *testing.T) {
	ctx := context.Background()
	stale := client.GovernanceProfile{ID: uuid.New(), Name: "stale"}
	staleAssign := client.GovernanceAssignment{ID: uuid.New(), SubjectType: client.CapabilitySubjectUser, Subject: "old", ProfileID: stale.ID}

	t.Run("a pending write before prune skips it", func(t *testing.T) {
		f := &pendingFake{
			profiles: []client.GovernanceProfile{stale}, assignments: []client.GovernanceAssignment{staleAssign},
			holdProfiles: map[string]bool{"new": true},
		}
		c := f.server(t)
		res, err := c.ApplyGovernanceResult(ctx, client.GovernanceDocument{
			Profiles: []client.GovernanceProfile{{Name: "new"}}}, true)
		if err != nil {
			t.Fatalf("ApplyGovernanceResult: %v", err)
		}
		if !res.PruneSkipped {
			t.Error("PruneSkipped = false, want true")
		}
		for _, w := range f.writes() {
			if strings.HasPrefix(w, "DELETE ") {
				t.Errorf("a prune write was issued after a pending write: %s", w)
			}
		}
	})

	t.Run("prune stops at its first pending delete", func(t *testing.T) {
		f := &pendingFake{
			profiles: []client.GovernanceProfile{stale}, assignments: []client.GovernanceAssignment{staleAssign},
			holdDeletes: true,
		}
		c := f.server(t)
		res, err := c.ApplyGovernanceResult(ctx, client.GovernanceDocument{}, true)
		if err != nil {
			t.Fatalf("ApplyGovernanceResult: %v", err)
		}
		deletes := 0
		for _, w := range f.writes() {
			if strings.HasPrefix(w, "DELETE ") {
				deletes++
			}
		}
		if deletes != 1 || len(res.Pending) != 1 || !res.PruneSkipped {
			t.Errorf("deletes = %d, pending = %d, skipped = %v; want 1, 1, true", deletes, len(res.Pending), res.PruneSkipped)
		}
	})

	t.Run("no pending write: prune runs", func(t *testing.T) {
		f := &pendingFake{profiles: []client.GovernanceProfile{stale}}
		c := f.server(t)
		res, err := c.ApplyGovernanceResult(ctx, client.GovernanceDocument{}, true)
		if err != nil || res.PruneSkipped || len(res.Pending) != 0 {
			t.Errorf("result = %+v, err = %v; want a clean prune", res, err)
		}
		if len(f.writes()) != 1 {
			t.Errorf("writes = %v, want the one delete", f.writes())
		}
	})
}

func TestGovernanceChanges_ListGetApproveReject(t *testing.T) {
	ctx := context.Background()
	ch := client.GovernanceChange{ID: uuid.New(), TargetKind: "governance_profile", Op: "update",
		TargetKey: "k", State: "pending", ProposedBy: "alice"}
	f := &pendingFake{changes: []client.GovernanceChange{ch}}
	c := f.server(t)

	got, err := c.ListGovernanceChanges(ctx, "pending")
	if err != nil || len(got) != 1 || got[0].ID != ch.ID {
		t.Fatalf("ListGovernanceChanges = %+v, %v", got, err)
	}
	if last := f.reqs[len(f.reqs)-1]; last != "GET /api/v1/governance/changes?state=pending" {
		t.Errorf("list request = %q", last)
	}
	if _, err := c.ListGovernanceChanges(ctx, ""); err != nil {
		t.Fatalf("ListGovernanceChanges(\"\"): %v", err)
	}
	if last := f.reqs[len(f.reqs)-1]; last != "GET /api/v1/governance/changes" {
		t.Errorf("unfiltered list request = %q, want no query", last)
	}
	if one, err := c.GetGovernanceChange(ctx, ch.ID); err != nil || one.ID != ch.ID {
		t.Fatalf("GetGovernanceChange = %+v, %v", one, err)
	}
	if ap, err := c.ApproveGovernanceChange(ctx, ch.ID); err != nil || ap.State != "applied" ||
		f.approveCall != "/api/v1/governance/changes/"+ch.ID.String()+"/approve" {
		t.Fatalf("ApproveGovernanceChange = %+v, %v (path %q)", ap, err, f.approveCall)
	}
	if rj, err := c.RejectGovernanceChange(ctx, ch.ID, "not now"); err != nil || rj.State != "rejected" ||
		!strings.Contains(f.rejectBody, `"reason":"not now"`) {
		t.Fatalf("RejectGovernanceChange = %+v, %v (body %q)", rj, err, f.rejectBody)
	}
	if _, err := c.RejectGovernanceChange(ctx, ch.ID, ""); err != nil || strings.Contains(f.rejectBody, "reason") {
		t.Errorf("an empty reason must be omitted, body = %q (err %v)", f.rejectBody, err)
	}
}

// TestApplyGovernance_RepeatApplyDuringTheWindowIsStillPending: the same document applied again while
// a held change still sits at a target is a 409 naming that change. It reports as pending, defers what
// depends on it, and the writes after it are still sent.
func TestApplyGovernance_RepeatApplyDuringTheWindowIsStillPending(t *testing.T) {
	for _, big := range []bool{false, true} {
		heldID := uuid.New()
		held := client.GovernanceProfile{ID: uuid.New(), Name: "held", Limits: client.GovernanceLimits{MaxConcurrentRuns: 1}}
		f := &pendingFake{
			profiles:     []client.GovernanceProfile{held},
			heldProfiles: map[string]uuid.UUID{"held": heldID},
			bigDiff:      big,
		}
		c := f.server(t)
		heldFileID, laterFileID := uuid.New(), uuid.New()
		res, err := c.ApplyGovernanceResult(context.Background(), client.GovernanceDocument{
			Profiles: []client.GovernanceProfile{
				{ID: heldFileID, Name: "held", Limits: client.GovernanceLimits{MaxConcurrentRuns: 9}},
				{ID: laterFileID, Name: "later"},
			},
			Assignments: []client.GovernanceAssignment{
				{SubjectType: client.CapabilitySubjectUser, Subject: "bob", ProfileID: heldFileID},
				{SubjectType: client.CapabilitySubjectUser, Subject: "carol", ProfileID: laterFileID},
			},
		}, false)
		if err != nil {
			t.Fatalf("big=%v: a repeat apply during the window failed: %v", big, err)
		}
		if len(res.Pending) != 1 || res.Pending[0].ID != heldID {
			t.Fatalf("big=%v: pending = %+v, want the existing change %s", big, res.Pending, heldID)
		}
		if len(res.Deferred) != 1 || res.Deferred[0].Subject != "bob" {
			t.Errorf("big=%v: deferred = %+v, want bob (his profile is held)", big, res.Deferred)
		}
		if len(f.assignSent) != 1 || f.assignSent[0].Subject != "carol" {
			t.Errorf("big=%v: assignments sent = %+v, want only carol: the writes after the held one still go", big, f.assignSent)
		}
	}
}

// TestApplyGovernance_HeldChangeThatIsNotThisWriteFails: a 409 whose held change is not the refused
// proposal (pending_change_matches false: another payload, op or proposer) is the refusal, not a
// pending result, so an edited document never reports the old change as its own.
func TestApplyGovernance_HeldChangeThatIsNotThisWriteFails(t *testing.T) {
	heldID := uuid.New()
	f := &pendingFake{
		profiles:     []client.GovernanceProfile{{ID: uuid.New(), Name: "held", Limits: client.GovernanceLimits{MaxConcurrentRuns: 1}}},
		heldProfiles: map[string]uuid.UUID{"held": heldID},
		heldDiffers:  true,
	}
	res, err := f.server(t).ApplyGovernanceResult(context.Background(), client.GovernanceDocument{
		Profiles: []client.GovernanceProfile{{ID: uuid.New(), Name: "held", Limits: client.GovernanceLimits{MaxConcurrentRuns: 9}}},
	}, false)
	if err == nil || !strings.Contains(err.Error(), heldID.String()) {
		t.Fatalf("apply = %+v, %v; want an error naming the held change %s", res, err, heldID)
	}
	if len(res.Pending) != 0 {
		t.Errorf("pending = %+v, want none", res.Pending)
	}
}
