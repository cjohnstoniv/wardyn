// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ── Governance (migration 0052) ─────────────────────────────────────────────

// GovernanceProfileRequest is the POST /governance/profiles / PUT
// /governance/profiles/{id} body — one named ceiling. ID/CreatedAt/UpdatedAt/
// CreatedBy are never accepted from the wire, matching DriveRequest: the id
// comes from the path (an update) or the server (a create), and provenance is
// always server-assigned.
type GovernanceProfileRequest struct {
	Name    string           `json:"name"`
	Ceiling RunPolicySpec    `json:"ceiling"`
	Limits  GovernanceLimits `json:"limits"`
}

// GovernanceProfileResponse is a profile write's body: the saved profile plus
// any omission warnings the write raised (a ceiling grant the deployment no
// longer provisions — see internal/api/governance.go's reintersect note).
type GovernanceProfileResponse struct {
	Profile  GovernanceProfile `json:"profile"`
	Warnings []string          `json:"warnings,omitempty"`
}

// GovernanceAssignmentRequest is POST /governance/assignments's body. There is
// no PUT for an assignment: the natural key (subject_type, subject) upserts,
// repointing an existing binding rather than accumulating a second one, the
// same shape DriveGrantRequest already takes for a drive allocation.
type GovernanceAssignmentRequest struct {
	SubjectType CapabilitySubjectType `json:"subject_type"`
	Subject     string                `json:"subject"`
	ProfileID   uuid.UUID             `json:"profile_id"`
	Priority    int                   `json:"priority"`
}

// GovernanceDocument is GET /governance's body and ApplyGovernance's
// parameter: every profile plus every assignment. `wardyn governance get`
// prints this verbatim; ApplyGovernance strict-decodes it back.
type GovernanceDocument struct {
	Profiles    []GovernanceProfile    `json:"profiles"`
	Assignments []GovernanceAssignment `json:"assignments"`
}

// GovernanceChangeDiff is a pending change's server-rendered diff: the target's
// current and proposed rows (both redacted for read) and the changed field
// paths. The client never computes its own.
type GovernanceChangeDiff struct {
	Before  json.RawMessage `json:"before,omitempty"`
	After   json.RawMessage `json:"after,omitempty"`
	Changed []string        `json:"changed,omitempty"`
}

// GovernanceChange is one governance write held for a second approver: the
// body of a 202's pending_change, and the row the /governance/changes routes
// list, approve and reject. State is pending, applied, rejected, expired or
// stale.
type GovernanceChange struct {
	ID         uuid.UUID            `json:"id"`
	TargetKind string               `json:"target_kind"`
	Op         string               `json:"op"`
	TargetKey  string               `json:"target_key"`
	State      string               `json:"state"`
	ProposedBy string               `json:"proposed_by"`
	ProposedAt time.Time            `json:"proposed_at"`
	ExpiresAt  time.Time            `json:"expires_at"`
	Diff       GovernanceChangeDiff `json:"diff"`
}

// GovernanceDeferredWrite is an assignment ApplyGovernanceResult did not send
// because the profile it names has a write pending approval. Apply again once
// that change is approved.
type GovernanceDeferredWrite struct {
	SubjectType CapabilitySubjectType `json:"subject_type"`
	Subject     string                `json:"subject"`
	// Profile is the pending profile's name.
	Profile string `json:"profile"`
}

// PendingApprovalError is returned when a write was held for a second approver
// instead of applied (the server answered 202 with a pending_change). It is not
// a failure of the request: nothing is applied until the change is approved
// (ApproveGovernanceChange, or `wardyn governance changes approve`).
type PendingApprovalError struct {
	// Changes are the writes held for approval.
	Changes []GovernanceChange
	// Deferred are the apply's writes that depend on a pending one and were
	// not sent (ApplyGovernance only).
	Deferred []GovernanceDeferredWrite
	// PruneSkipped is true when prune was requested and did not run to
	// completion because a write was pending (ApplyGovernance only).
	PruneSkipped bool
}

func (e *PendingApprovalError) Error() string {
	ids := make([]string, len(e.Changes))
	for i, ch := range e.Changes {
		ids[i] = ch.ID.String()
	}
	msg := fmt.Sprintf("pending approval: %d governance change(s) stored, not applied (%s)",
		len(e.Changes), strings.Join(ids, ", "))
	if n := len(e.Deferred); n > 0 {
		msg += fmt.Sprintf("; %d dependent write(s) deferred", n)
	}
	if e.PruneSkipped {
		msg += "; prune skipped"
	}
	return msg
}

// GetGovernance returns every governance profile and assignment. GET
// /api/v1/governance.
func (c *Client) GetGovernance(ctx context.Context) (GovernanceDocument, error) {
	var out GovernanceDocument
	err := c.do(ctx, http.MethodGet, "/api/v1/governance", nil, &out)
	return out, err
}

// governanceAssignmentKey is the natural key ApplyGovernance and
// handleUpsertGovernanceAssignment both upsert an assignment by.
func governanceAssignmentKey(subjectType CapabilitySubjectType, subject string) string {
	return string(subjectType) + "\x00" + subject
}

// governanceProfileUnchanged reports whether writing p over existing would
// change nothing observable — the check ApplyGovernance runs before every
// profile write so a `get | apply` round trip on an unchanged install issues
// ZERO writes and records ZERO audit rows, rather than re-asserting every
// profile's content on every apply (#1108's stated no-op contract; stricter
// than ApplyDrives, which always re-PUTs/re-POSTs).
func governanceProfileUnchanged(existing, p GovernanceProfile) bool {
	return reflect.DeepEqual(existing.Ceiling, p.Ceiling) && reflect.DeepEqual(existing.Limits, p.Limits)
}

// ApplyGovernance upserts every profile and assignment doc names, over the
// existing POST /governance/profiles, PUT /governance/profiles/{id} and POST
// /governance/assignments routes — there is no bulk-write route, and none is
// added.
//
// A PROFILE is routed BY NAME, not by id — the divergence from ApplyDrives,
// and the reason is Name's role server-side: it is the UNIQUE human handle an
// admin actually authors and assigns by (governance.go's own words), while a
// drive's id is what GetDrives/ApplyDrives round-trip on. Upserting by id
// would make a hand-maintained, version-controlled governance.json (written
// once, with no ids, and re-applied against the same install repeatedly) fail
// its second apply with a 409 name conflict — the exact "config lives in git
// next to the rest of the install" workflow #1108 exists for. So ApplyGovernance
// reads the CURRENT state first, and: a doc profile whose Name matches an
// existing one is PUT to that existing row's real id (rename is therefore not
// expressible through apply — Name IS the identity a file's entries are
// matched against); a Name with no existing match is POSTed fresh. Either way,
// a profile whose Ceiling and Limits are BYTE-IDENTICAL to what is already
// stored is skipped entirely (governanceProfileUnchanged) — the no-op
// contract's other half, since the server audits every profile write
// unconditionally.
//
// An ASSIGNMENT carries no id on write (server-assigned, same as
// DriveGrantRequest), and is upserted purely by its own natural key
// (subject_type, subject) — skipped, the same way, when the existing row
// already names the same profile at the same priority. Its ProfileID is
// resolved against the SAME apply's own profile list before being sent: an id
// in doc.Assignments naming one of doc.Profiles's ORIGINAL (pre-write) ids is
// translated to that profile's real post-write id, so a document produced by
// GetGovernance against a POPULATED install reproduces both profiles and
// assignments when applied to an EMPTY one, even though the empty install
// mints entirely new profile ids. An assignment whose ProfileID names no
// profile in this same doc is sent exactly as given, trusting it as an
// already-real id on the target (the case for an assignments-only file, or one
// applied twice against the SAME install).
//
// prune, when true, additionally DELETES every server-side profile or
// assignment doc does not name (assignments first, since a profile still
// referenced by a to-be-pruned assignment fails the FK restrict). Without it —
// the default — nothing present server-side but absent from doc is touched,
// matching drive set's own "nothing the file omits is touched" rule.
//
// Every write's saved row replaces the caller's copy of doc in place, so a
// partial failure (returned as the second value) leaves doc's earlier entries
// holding what was actually persisted. On success, the returned document is a
// fresh GetGovernance — the authoritative post-write state.
//
// When the deployment requires a second approver for governance writes, a
// write is held as a pending change instead of applied. ApplyGovernance then
// returns the fresh document and a *PendingApprovalError (check with
// errors.As) rather than reporting success: an assignment naming a profile
// whose write is pending is not sent, and prune does not run. Use
// ApplyGovernanceResult to read the pending and deferred lists as data.
func (c *Client) ApplyGovernance(ctx context.Context, doc GovernanceDocument, prune bool) (GovernanceDocument, error) {
	res, err := c.ApplyGovernanceResult(ctx, doc, prune)
	if err != nil {
		return GovernanceDocument{}, err
	}
	if len(res.Pending) > 0 {
		return res.Document, &PendingApprovalError{
			Changes: res.Pending, Deferred: res.Deferred, PruneSkipped: res.PruneSkipped,
		}
	}
	return res.Document, nil
}

// GovernanceApplyResult is ApplyGovernanceResult's answer.
type GovernanceApplyResult struct {
	// Document is a fresh GetGovernance after the apply: what is actually in
	// force, which excludes every pending write.
	Document GovernanceDocument `json:"document"`
	// Pending are the writes held for a second approver. Nothing in them is
	// applied until they are approved.
	Pending []GovernanceChange `json:"pending,omitempty"`
	// Deferred are the assignments not sent because the profile they name has
	// a pending write. Apply again once that change is approved.
	Deferred []GovernanceDeferredWrite `json:"deferred,omitempty"`
	// PruneSkipped is true when prune was requested and did not run to
	// completion because a write was pending.
	PruneSkipped bool `json:"prune_skipped,omitempty"`
}

// ApplyGovernanceResult is ApplyGovernance with the pending-approval outcome
// returned as data, not an error: a 202 pending change is an expected result
// under four-eyes, not a failure. See ApplyGovernance for the matching and
// ordering rules.
//
// A profile or assignment write the server holds for approval is recorded in
// Pending. An assignment naming a profile with a pending write is recorded in
// Deferred and never sent (its profile id is not final). Prune runs only when
// no write in the apply is pending, and stops at the first pending delete;
// PruneSkipped says so.
func (c *Client) ApplyGovernanceResult(ctx context.Context, doc GovernanceDocument, prune bool) (GovernanceApplyResult, error) {
	var res GovernanceApplyResult
	current, err := c.GetGovernance(ctx)
	if err != nil {
		return res, fmt.Errorf("read current governance state: %w", err)
	}
	profileByName := make(map[string]GovernanceProfile, len(current.Profiles))
	currentIDToName := make(map[uuid.UUID]string, len(current.Profiles))
	for _, p := range current.Profiles {
		profileByName[p.Name] = p
		currentIDToName[p.ID] = p.Name
	}
	assignmentByKey := make(map[string]GovernanceAssignment, len(current.Assignments))
	for _, a := range current.Assignments {
		assignmentByKey[governanceAssignmentKey(a.SubjectType, a.Subject)] = a
	}

	// fileIDToName/nameToRealID translate an assignment's ProfileID from
	// "whatever id this SAME file's profile entry carried" to "the id that
	// profile actually holds on THIS server" — see the doc comment above.
	fileIDToName := make(map[uuid.UUID]string, len(doc.Profiles))
	nameToRealID := make(map[string]uuid.UUID, len(doc.Profiles))
	// pendingProfile holds the names of profiles whose write was held for
	// approval: an assignment naming one is deferred, never sent.
	pendingProfile := make(map[string]bool)

	for i, p := range doc.Profiles {
		saved := p
		if existing, ok := profileByName[p.Name]; ok && governanceProfileUnchanged(existing, p) {
			saved = existing
		} else {
			req := GovernanceProfileRequest{Name: p.Name, Ceiling: p.Ceiling, Limits: p.Limits}
			var resp GovernanceProfileResponse
			var pending *GovernanceChange
			var werr error
			if ok {
				pending, werr = c.doPending(ctx, http.MethodPut, "/api/v1/governance/profiles/"+existing.ID.String(), req, &resp)
			} else {
				pending, werr = c.doPending(ctx, http.MethodPost, "/api/v1/governance/profiles", req, &resp)
			}
			if werr != nil {
				return GovernanceApplyResult{}, fmt.Errorf("apply governance profile %q: %w", p.Name, werr)
			}
			if pending != nil {
				res.Pending = append(res.Pending, *pending)
				pendingProfile[p.Name] = true
			} else {
				saved = resp.Profile
			}
		}
		doc.Profiles[i] = saved
		if p.ID != uuid.Nil {
			fileIDToName[p.ID] = p.Name
		}
		nameToRealID[p.Name] = saved.ID
	}

	for i, a := range doc.Assignments {
		name, named := fileIDToName[a.ProfileID]
		if !named {
			name, named = currentIDToName[a.ProfileID]
		}
		if named && pendingProfile[name] {
			res.Deferred = append(res.Deferred, GovernanceDeferredWrite{
				SubjectType: a.SubjectType, Subject: a.Subject, Profile: name,
			})
			continue
		}
		if name, ok := fileIDToName[a.ProfileID]; ok {
			if real, ok := nameToRealID[name]; ok {
				a.ProfileID = real
			}
		}
		key := governanceAssignmentKey(a.SubjectType, a.Subject)
		if existing, ok := assignmentByKey[key]; ok &&
			existing.ProfileID == a.ProfileID && existing.Priority == a.Priority {
			doc.Assignments[i] = existing
			continue
		}
		req := GovernanceAssignmentRequest{
			SubjectType: a.SubjectType, Subject: a.Subject,
			ProfileID: a.ProfileID, Priority: a.Priority,
		}
		var saved GovernanceAssignment
		pending, err := c.doPending(ctx, http.MethodPost, "/api/v1/governance/assignments", req, &saved)
		if err != nil {
			return GovernanceApplyResult{}, fmt.Errorf("apply governance assignment (%s %q): %w", a.SubjectType, a.Subject, err)
		}
		if pending != nil {
			res.Pending = append(res.Pending, *pending)
			continue
		}
		doc.Assignments[i] = saved
	}

	if prune {
		// A pending write means the state prune would compare against is not
		// final, so no delete is issued after one (ApplyGovernance's doc).
		res.PruneSkipped = len(res.Pending) > 0
		if !res.PruneSkipped {
			res.PruneSkipped, err = c.pruneGovernance(ctx, current, doc, &res)
			if err != nil {
				return GovernanceApplyResult{}, err
			}
		}
	}

	res.Document, err = c.GetGovernance(ctx)
	return res, err
}

// pruneGovernance deletes every assignment, then every profile, in current that
// doc does not name. It stops at the first delete the server holds for approval
// (recording it in res.Pending) and reports that as skipped=true, because the
// deletes after it would run against a state that is not final.
func (c *Client) pruneGovernance(ctx context.Context, current, doc GovernanceDocument, res *GovernanceApplyResult) (skipped bool, err error) {
	keepAssignment := make(map[string]bool, len(doc.Assignments))
	for _, a := range doc.Assignments {
		keepAssignment[governanceAssignmentKey(a.SubjectType, a.Subject)] = true
	}
	// Assignments before profiles: a profile doc drops still fails the FK
	// restrict while a stale assignment of it survives.
	for _, a := range current.Assignments {
		if keepAssignment[governanceAssignmentKey(a.SubjectType, a.Subject)] {
			continue
		}
		pending, err := c.doPending(ctx, http.MethodDelete, "/api/v1/governance/assignments/"+a.ID.String(), nil, nil)
		if err != nil {
			return false, fmt.Errorf("prune governance assignment (%s %q): %w", a.SubjectType, a.Subject, err)
		}
		if pending != nil {
			res.Pending = append(res.Pending, *pending)
			return true, nil
		}
	}
	keepProfile := make(map[string]bool, len(doc.Profiles))
	for _, p := range doc.Profiles {
		keepProfile[p.Name] = true
	}
	for _, p := range current.Profiles {
		if keepProfile[p.Name] {
			continue
		}
		pending, err := c.doPending(ctx, http.MethodDelete, "/api/v1/governance/profiles/"+p.ID.String(), nil, nil)
		if err != nil {
			return false, fmt.Errorf("prune governance profile %q: %w", p.Name, err)
		}
		if pending != nil {
			res.Pending = append(res.Pending, *pending)
			return true, nil
		}
	}
	return false, nil
}

// ListGovernanceChanges lists governance changes held for, or decided by, a
// second approver. state narrows the list (pending, applied, rejected, expired
// or stale); "" leaves it to the server's default. GET
// /api/v1/governance/changes.
func (c *Client) ListGovernanceChanges(ctx context.Context, state string) ([]GovernanceChange, error) {
	path := "/api/v1/governance/changes"
	if state != "" {
		path += "?state=" + url.QueryEscape(state)
	}
	var out []GovernanceChange
	err := c.do(ctx, http.MethodGet, path, nil, &out)
	return out, err
}

// GetGovernanceChange returns one governance change with its diff. GET
// /api/v1/governance/changes/{id}.
func (c *Client) GetGovernanceChange(ctx context.Context, id uuid.UUID) (GovernanceChange, error) {
	var out GovernanceChange
	err := c.do(ctx, http.MethodGet, "/api/v1/governance/changes/"+id.String(), nil, &out)
	return out, err
}

// ApproveGovernanceChange approves a pending change, which applies it. The
// caller must be a different human from the proposer; the server answers 409
// when the change is no longer pending or has gone stale. POST
// /api/v1/governance/changes/{id}/approve.
func (c *Client) ApproveGovernanceChange(ctx context.Context, id uuid.UUID) (GovernanceChange, error) {
	var out GovernanceChange
	err := c.do(ctx, http.MethodPost, "/api/v1/governance/changes/"+id.String()+"/approve", nil, &out)
	return out, err
}

// RejectGovernanceChange rejects a pending change; nothing is applied. reason
// is optional ("" omits it). POST /api/v1/governance/changes/{id}/reject.
func (c *Client) RejectGovernanceChange(ctx context.Context, id uuid.UUID, reason string) (GovernanceChange, error) {
	var out GovernanceChange
	body := struct {
		Reason string `json:"reason,omitempty"`
	}{Reason: reason}
	err := c.do(ctx, http.MethodPost, "/api/v1/governance/changes/"+id.String()+"/reject", body, &out)
	return out, err
}
