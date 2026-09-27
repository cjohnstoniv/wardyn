// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// fakeGovernanceServer models just enough of the real /governance family
// (internal/api/governance.go) to make a round trip through it MEAN
// something: POST /governance/profiles refuses a name another profile already
// holds (409, the real UNIQUE(name) behavior); POST /governance/assignments
// upserts by the natural key (subject_type, subject) rather than accumulating
// a row per call; DELETE /governance/profiles/{id} refuses (409) while the
// profile is still assigned, the real ON DELETE RESTRICT behavior. writeCount
// is incremented on every successful mutating call — the real server audits
// exactly once per such call, so writeCount is the audit-row proxy #1108's
// no-op contract is measured against.
type fakeGovernanceServer struct {
	profiles    map[uuid.UUID]sdk.GovernanceProfile
	assignments map[uuid.UUID]sdk.GovernanceAssignment
	writeCount  int
}

func newFakeGovernanceServer() *fakeGovernanceServer {
	return &fakeGovernanceServer{
		profiles:    map[uuid.UUID]sdk.GovernanceProfile{},
		assignments: map[uuid.UUID]sdk.GovernanceAssignment{},
	}
}

func (f *fakeGovernanceServer) nameConflict(id uuid.UUID, name string) bool {
	for existingID, p := range f.profiles {
		if existingID != id && p.Name == name {
			return true
		}
	}
	return false
}

func (f *fakeGovernanceServer) assignmentByKey(subjectType sdk.CapabilitySubjectType, subject string) (sdk.GovernanceAssignment, bool) {
	for _, a := range f.assignments {
		if a.SubjectType == subjectType && a.Subject == subject {
			return a, true
		}
	}
	return sdk.GovernanceAssignment{}, false
}

func (f *fakeGovernanceServer) profileAssigned(id uuid.UUID) bool {
	for _, a := range f.assignments {
		if a.ProfileID == id {
			return true
		}
	}
	return false
}

func (f *fakeGovernanceServer) document() sdk.GovernanceDocument {
	doc := sdk.GovernanceDocument{}
	for _, p := range f.profiles {
		doc.Profiles = append(doc.Profiles, p)
	}
	for _, a := range f.assignments {
		doc.Assignments = append(doc.Assignments, a)
	}
	sortGovernanceProfiles(doc.Profiles)
	sortGovernanceAssignments(doc.Assignments)
	return doc
}

// sortGovernanceProfiles/sortGovernanceAssignments give a stable order so two
// documents built from the same map state compare equal regardless of Go's
// randomized map iteration (sortDrives/sortGrants, drive_test.go).
func sortGovernanceProfiles(p []sdk.GovernanceProfile) {
	for i := 1; i < len(p); i++ {
		for j := i; j > 0 && p[j].Name < p[j-1].Name; j-- {
			p[j], p[j-1] = p[j-1], p[j]
		}
	}
}

func sortGovernanceAssignments(a []sdk.GovernanceAssignment) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j].Subject < a[j-1].Subject; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}

func writeErrorJSON(w http.ResponseWriter, status int, msg string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func (f *fakeGovernanceServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/governance":
			_ = json.NewEncoder(w).Encode(f.document())

		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/governance/profiles":
			var req sdk.GovernanceProfileRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			if f.nameConflict(uuid.Nil, req.Name) {
				writeErrorJSON(w, http.StatusConflict, "a governance profile named "+req.Name+" already exists")
				return
			}
			p := sdk.GovernanceProfile{ID: uuid.New(), Name: req.Name, Ceiling: req.Ceiling, Limits: req.Limits}
			f.profiles[p.ID] = p
			f.writeCount++
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(sdk.GovernanceProfileResponse{Profile: p})

		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/api/v1/governance/profiles/"):
			id, err := uuid.Parse(strings.TrimPrefix(r.URL.Path, "/api/v1/governance/profiles/"))
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			var req sdk.GovernanceProfileRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			if f.nameConflict(id, req.Name) {
				writeErrorJSON(w, http.StatusConflict, "a governance profile named "+req.Name+" already exists")
				return
			}
			p := sdk.GovernanceProfile{ID: id, Name: req.Name, Ceiling: req.Ceiling, Limits: req.Limits}
			f.profiles[id] = p
			f.writeCount++
			_ = json.NewEncoder(w).Encode(sdk.GovernanceProfileResponse{Profile: p})

		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/v1/governance/profiles/"):
			id, err := uuid.Parse(strings.TrimPrefix(r.URL.Path, "/api/v1/governance/profiles/"))
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if _, ok := f.profiles[id]; !ok {
				writeErrorJSON(w, http.StatusNotFound, "governance profile not found")
				return
			}
			if f.profileAssigned(id) {
				writeErrorJSON(w, http.StatusConflict, "this governance profile is still assigned")
				return
			}
			delete(f.profiles, id)
			f.writeCount++
			w.WriteHeader(http.StatusNoContent)

		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/governance/assignments":
			var req sdk.GovernanceAssignmentRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			if _, ok := f.profiles[req.ProfileID]; !ok {
				writeErrorJSON(w, http.StatusNotFound, "governance profile not found")
				return
			}
			existing, had := f.assignmentByKey(req.SubjectType, req.Subject)
			a := sdk.GovernanceAssignment{SubjectType: req.SubjectType, Subject: req.Subject,
				ProfileID: req.ProfileID, Priority: req.Priority}
			if had {
				a.ID = existing.ID
				w.WriteHeader(http.StatusOK)
			} else {
				a.ID = uuid.New()
				w.WriteHeader(http.StatusCreated)
			}
			f.assignments[a.ID] = a
			f.writeCount++
			_ = json.NewEncoder(w).Encode(a)

		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/v1/governance/assignments/"):
			id, err := uuid.Parse(strings.TrimPrefix(r.URL.Path, "/api/v1/governance/assignments/"))
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if _, ok := f.assignments[id]; !ok {
				writeErrorJSON(w, http.StatusNotFound, "governance assignment not found")
				return
			}
			delete(f.assignments, id)
			f.writeCount++
			w.WriteHeader(http.StatusNoContent)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

// runGovernanceGet runs `governance get` against url and returns what it
// printed and any error.
func runGovernanceGet(t *testing.T, url string) (stdout string, err error) {
	t.Helper()
	root := rootCmd()
	out := &strings.Builder{}
	root.SetArgs([]string{"governance", "get", "--url", url, "--token", "tok"})
	root.SetOut(out)
	root.SetErr(&strings.Builder{})
	err = root.Execute()
	return out.String(), err
}

// runGovernanceApply marshals doc to a temp file, runs `governance apply` on
// it (with --prune when asked), and returns what it printed and any error.
func runGovernanceApply(t *testing.T, url string, doc sdk.GovernanceDocument, prune bool) (stdout string, err error) {
	t.Helper()
	b, merr := json.Marshal(doc)
	if merr != nil {
		t.Fatalf("marshal doc: %v", merr)
	}
	path := filepath.Join(t.TempDir(), "governance.json")
	if werr := os.WriteFile(path, b, 0o600); werr != nil {
		t.Fatalf("write doc: %v", werr)
	}
	args := []string{"governance", "apply", path, "--url", url, "--token", "tok"}
	if prune {
		args = append(args, "--prune")
	}
	root := rootCmd()
	out := &strings.Builder{}
	root.SetArgs(args)
	root.SetOut(out)
	root.SetErr(&strings.Builder{})
	err = root.Execute()
	return out.String(), err
}

// seedGovernanceProfiles exercises a real ceiling and non-zero limits, not
// just the zero value.
func seedGovernanceProfiles() []sdk.GovernanceProfile {
	return []sdk.GovernanceProfile{
		{Name: "contractor", Ceiling: sdk.RunPolicySpec{MinConfinementClass: sdk.CC2},
			Limits: sdk.GovernanceLimits{DenyTaskModeExec: true, MaxConcurrentRuns: 2}},
		{Name: "eng-default", Ceiling: sdk.RunPolicySpec{MinConfinementClass: sdk.CC1, AllowAllEgress: true},
			Limits: sdk.GovernanceLimits{MaxEphemeralDiskMiB: 4096}},
	}
}

// seedGovernanceAssignments exercises every types.CapabilitySubjectType.
// profileByName resolves a seed profile's real (post-write) id.
func seedGovernanceAssignments(profileByName map[string]uuid.UUID) []sdk.GovernanceAssignment {
	return []sdk.GovernanceAssignment{
		{SubjectType: sdk.CapabilitySubjectUser, Subject: "alice", ProfileID: profileByName["contractor"]},
		{SubjectType: sdk.CapabilitySubjectGroup, Subject: "eng", ProfileID: profileByName["eng-default"], Priority: 5},
	}
}

// bootstrapGovernance applies seedGovernanceProfiles and
// seedGovernanceAssignments to a fresh fake server and returns the profile
// name -> real id map the assignments were written against.
func bootstrapGovernance(t *testing.T, url string) map[string]uuid.UUID {
	t.Helper()
	if _, err := runGovernanceApply(t, url, sdk.GovernanceDocument{Profiles: seedGovernanceProfiles()}, false); err != nil {
		t.Fatalf("bootstrap profiles: %v", err)
	}
	getOut, err := runGovernanceGet(t, url)
	if err != nil {
		t.Fatalf("bootstrap get: %v", err)
	}
	var doc sdk.GovernanceDocument
	if err := json.Unmarshal([]byte(getOut), &doc); err != nil {
		t.Fatalf("unmarshal bootstrap get: %v", err)
	}
	byName := map[string]uuid.UUID{}
	for _, p := range doc.Profiles {
		byName[p.Name] = p.ID
	}
	if _, err := runGovernanceApply(t, url, sdk.GovernanceDocument{Assignments: seedGovernanceAssignments(byName)}, false); err != nil {
		t.Fatalf("bootstrap assignments: %v", err)
	}
	return byName
}

// TestGovernanceApply_GetApplyRoundTripIsANoOp is #1108's stated acceptance:
// `wardyn governance get > f && wardyn governance apply f` changes nothing —
// ZERO writes, ZERO audit rows, on a populated install. Stricter than drive
// apply's own round-trip test (which only checks the state ends up the same):
// the real server audits every successful profile/assignment write
// unconditionally, so an apply that blindly re-PUTs/re-POSTs unchanged rows
// would pass a state-equality check while still spamming the audit log.
// fake.writeCount is incremented on every mutating call the fake server
// handles, so it stands in for that audit-row count directly.
func TestGovernanceApply_GetApplyRoundTripIsANoOp(t *testing.T) {
	fake := newFakeGovernanceServer()
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)

	bootstrapGovernance(t, srv.URL)
	if len(fake.profiles) != 2 || len(fake.assignments) != 2 {
		t.Fatalf("bootstrap left %d profiles / %d assignments, want 2 / 2", len(fake.profiles), len(fake.assignments))
	}

	getOut, err := runGovernanceGet(t, srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	var before sdk.GovernanceDocument
	if err := json.Unmarshal([]byte(getOut), &before); err != nil {
		t.Fatalf("unmarshal get output: %v", err)
	}

	fake.writeCount = 0
	if _, err := runGovernanceApply(t, srv.URL, before, false); err != nil {
		t.Fatalf("round-trip apply: %v", err)
	}
	if fake.writeCount != 0 {
		t.Errorf("round-trip apply issued %d writes, want 0 (get | apply must be a no-op)", fake.writeCount)
	}

	afterOut, err := runGovernanceGet(t, srv.URL)
	if err != nil {
		t.Fatalf("post-round-trip get: %v", err)
	}
	var after sdk.GovernanceDocument
	if err := json.Unmarshal([]byte(afterOut), &after); err != nil {
		t.Fatalf("unmarshal post-round-trip get output: %v", err)
	}
	sortGovernanceProfiles(before.Profiles)
	sortGovernanceProfiles(after.Profiles)
	sortGovernanceAssignments(before.Assignments)
	sortGovernanceAssignments(after.Assignments)
	beforeJSON, _ := json.Marshal(before)
	afterJSON, _ := json.Marshal(after)
	if string(beforeJSON) != string(afterJSON) {
		t.Errorf("get before apply(get()) != get after — the round trip changed something.\nbefore: %s\nafter:  %s", beforeJSON, afterJSON)
	}
}

// TestGovernanceApply_EmptyInstallReproducesState is #1108's other stated
// acceptance: applying a file captured from a populated install reproduces
// its profiles and assignments on an EMPTY one — even though the empty
// install mints entirely fresh profile ids, so ApplyGovernance must translate
// each assignment's ProfileID from "whatever id the source install's `get`
// reported" to "the id this install actually gave that same-named profile".
func TestGovernanceApply_EmptyInstallReproducesState(t *testing.T) {
	source := newFakeGovernanceServer()
	srcSrv := httptest.NewServer(source.handler())
	t.Cleanup(srcSrv.Close)
	bootstrapGovernance(t, srcSrv.URL)

	getOut, err := runGovernanceGet(t, srcSrv.URL)
	if err != nil {
		t.Fatalf("get from source: %v", err)
	}
	var captured sdk.GovernanceDocument
	if err := json.Unmarshal([]byte(getOut), &captured); err != nil {
		t.Fatalf("unmarshal captured document: %v", err)
	}

	target := newFakeGovernanceServer()
	tgtSrv := httptest.NewServer(target.handler())
	t.Cleanup(tgtSrv.Close)

	if _, err := runGovernanceApply(t, tgtSrv.URL, captured, false); err != nil {
		t.Fatalf("apply to empty install: %v", err)
	}
	if len(target.profiles) != 2 {
		t.Fatalf("target has %d profiles, want 2", len(target.profiles))
	}
	if len(target.assignments) != 2 {
		t.Fatalf("target has %d assignments, want 2", len(target.assignments))
	}
	byName := map[string]sdk.GovernanceProfile{}
	for _, p := range target.profiles {
		byName[p.Name] = p
	}
	contractor, ok := byName["contractor"]
	if !ok {
		t.Fatal("target has no \"contractor\" profile")
	}
	var sourceContractorID uuid.UUID
	for id, p := range source.profiles {
		if p.Name == "contractor" {
			sourceContractorID = id
		}
	}
	if sourceContractorID == contractor.ID {
		t.Fatalf("target's contractor profile reused the source install's id %s — the empty install must mint its own", contractor.ID)
	}
	var aliceAssignment *sdk.GovernanceAssignment
	for _, a := range target.assignments {
		if a.SubjectType == sdk.CapabilitySubjectUser && a.Subject == "alice" {
			cp := a
			aliceAssignment = &cp
		}
	}
	if aliceAssignment == nil {
		t.Fatal("target has no assignment for user alice")
	}
	if aliceAssignment.ProfileID != contractor.ID {
		t.Errorf("alice's assignment points at profile id %s, want the target's own contractor id %s",
			aliceAssignment.ProfileID, contractor.ID)
	}
}

// TestGovernanceApply_RejectsUnknownField pins the same strict-decode
// contract drive apply and site-config apply both take: apply upserts exactly
// what the file states, so a typo'd key must be a parse error, not a silently
// dropped field.
func TestGovernanceApply_RejectsUnknownField(t *testing.T) {
	fake := newFakeGovernanceServer()
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)

	path := filepath.Join(t.TempDir(), "governance.json")
	if err := os.WriteFile(path, []byte(`{"profils":[]}`), 0o600); err != nil {
		t.Fatalf("write doc: %v", err)
	}
	root := rootCmd()
	root.SetArgs([]string{"governance", "apply", path, "--url", srv.URL, "--token", "tok"})
	root.SetOut(&strings.Builder{})
	root.SetErr(&strings.Builder{})
	err := root.Execute()
	if err == nil {
		t.Fatal("apply with an unknown field succeeded, want a decode error")
	}
	if !strings.Contains(err.Error(), "profils") {
		t.Errorf("error %q does not name the unknown field", err)
	}
}

// TestGovernanceApply_Prune pins #1108's other named behavior: a profile or
// assignment present server-side but absent from the file is left alone
// UNLESS --prune is passed, in which case it is deleted (assignments first,
// since a profile still referenced by a stale assignment fails the real
// server's ON DELETE RESTRICT).
func TestGovernanceApply_Prune(t *testing.T) {
	fake := newFakeGovernanceServer()
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)
	byName := bootstrapGovernance(t, srv.URL)

	// A file naming only "eng-default" and its own assignment — "contractor"
	// and alice's assignment to it are both absent.
	reduced := sdk.GovernanceDocument{
		Profiles: []sdk.GovernanceProfile{{Name: "eng-default",
			Ceiling: sdk.RunPolicySpec{MinConfinementClass: sdk.CC1, AllowAllEgress: true},
			Limits:  sdk.GovernanceLimits{MaxEphemeralDiskMiB: 4096}}},
		Assignments: []sdk.GovernanceAssignment{
			{SubjectType: sdk.CapabilitySubjectGroup, Subject: "eng", ProfileID: byName["eng-default"], Priority: 5},
		},
	}

	if _, err := runGovernanceApply(t, srv.URL, reduced, false); err != nil {
		t.Fatalf("apply without --prune: %v", err)
	}
	if len(fake.profiles) != 2 {
		t.Errorf("apply without --prune left %d profiles, want 2 (nothing omitted should be removed)", len(fake.profiles))
	}
	if len(fake.assignments) != 2 {
		t.Errorf("apply without --prune left %d assignments, want 2 (nothing omitted should be removed)", len(fake.assignments))
	}

	if _, err := runGovernanceApply(t, srv.URL, reduced, true); err != nil {
		t.Fatalf("apply with --prune: %v", err)
	}
	if len(fake.profiles) != 1 {
		t.Errorf("apply with --prune left %d profiles, want 1 (the omitted profile must be removed)", len(fake.profiles))
	}
	if len(fake.assignments) != 1 {
		t.Errorf("apply with --prune left %d assignments, want 1 (the omitted assignment must be removed)", len(fake.assignments))
	}
	for _, p := range fake.profiles {
		if p.Name != "eng-default" {
			t.Errorf("prune left an unexpected profile %q", p.Name)
		}
	}
}
