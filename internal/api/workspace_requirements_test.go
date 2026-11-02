// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// This file covers Task 1 — PUT /workspaces/{id}/requirements — plus the pure
// key-grammar helpers (splitRequirementKey, validateWorkspaceRequirement) the
// endpoint and the fold in runs_create.go both consult.

// splitRequirementKey / validateWorkspaceRequirement (pure helpers)

func TestSplitRequirementKey(t *testing.T) {
	cases := []struct {
		key      string
		wantTyp  string
		wantRest string
		wantOK   bool
	}{
		{"secret:acme-key", "secret", "acme-key", true},
		{"egress:api.github.com", "egress", "api.github.com", true},
		{"write:/home/user/repo", "write", "/home/user/repo", true},
		// FIRST colon only — a write path may itself legally contain a colon.
		{"write:/home/user:repo", "write", "/home/user:repo", true},
		{"integration:corp-artifactory", "integration", "corp-artifactory", true},
		{"nocolon", "", "", false},
		{"secret:", "", "", false},   // empty suffix
		{"unknown:x", "", "", false}, // not one of the four known tokens
		{"", "", "", false},
	}
	for _, c := range cases {
		typ, rest, ok := splitRequirementKey(c.key)
		if typ != c.wantTyp || rest != c.wantRest || ok != c.wantOK {
			t.Errorf("splitRequirementKey(%q) = (%q,%q,%v), want (%q,%q,%v)",
				c.key, typ, rest, ok, c.wantTyp, c.wantRest, c.wantOK)
		}
	}
}

func TestValidateWorkspaceRequirement(t *testing.T) {
	valid := types.WorkspaceRequirement{Level: "required", Provenance: "operator_set"}
	cases := []struct {
		name string
		key  string
		req  types.WorkspaceRequirement
		ok   bool
	}{
		{"valid secret", "secret:acme-key", valid, true},
		{"valid egress", "egress:api.github.com", valid, true},
		{"valid write", "write:/home/user/repo", valid, true},
		{"valid optional/scan_seeded", "secret:acme-key", types.WorkspaceRequirement{Level: "optional", Provenance: "scan_seeded"}, true},
		{"valid integration", "integration:corp-artifactory", valid, true},
		// Deliberately accepted: a workspace may name an integration BEFORE it is
		// configured. The contract states an intent; the fold degrades to
		// "opens nothing" until the row exists, so ordering isn't the
		// operator's problem.
		{"integration that does not exist yet", "integration:not-configured-yet", valid, true},
		{"bad integration id (uppercase+space)", "integration:Corp Artifactory", valid, false},
		{"bad integration id (empty)", "integration:", valid, false},
		{"bad key grammar", "nocolon", valid, false},
		{"unknown type prefix", "env:FOO", valid, false},
		{"bad secret name (uppercase+space)", "secret:BAD NAME", valid, false},
		{"reserved secret name", "secret:wardyn-signing-key", valid, false},
		{"bad egress (wildcard)", "egress:*.example.com", valid, false},
		{"bad egress (scheme)", "egress:https://example.com", valid, false},
		{"bad egress (uppercase)", "egress:Example.com", valid, false},
		{"bad write (relative)", "write:relative/path", valid, false},
		{"bad write (denied prefix)", "write:/etc/passwd", valid, false},
		{"bad level", "secret:acme-key", types.WorkspaceRequirement{Level: "nope", Provenance: "operator_set"}, false},
		{"bad provenance", "secret:acme-key", types.WorkspaceRequirement{Level: "required", Provenance: "nope"}, false},
	}
	for _, c := range cases {
		if msg := validateWorkspaceRequirement(c.key, c.req); (msg == "") != c.ok {
			t.Errorf("%s: validateWorkspaceRequirement(%q, %+v) = %q, want ok=%v", c.name, c.key, c.req, msg, c.ok)
		}
	}
}

// HTTP-level: auth gating

func TestWorkspaceRequirementsRouteRequiresAdminAuth(t *testing.T) {
	h := newHarness(t)
	path := "/api/v1/workspaces/" + uuid.New().String() + "/requirements"
	body := `{"requirements":{}}`
	if w := do(t, h.srv, http.MethodPut, path, "", body); w.Code != http.StatusUnauthorized {
		t.Errorf("no token: code = %d, want 401", w.Code)
	}
	if w := do(t, h.srv, http.MethodPut, path, "wrong", body); w.Code != http.StatusUnauthorized {
		t.Errorf("wrong token: code = %d, want 401", w.Code)
	}
}

// HTTP-level: validation (fails before any store call, so a no-Store
// harness is sufficient — parseIDParam only needs a well-formed UUID).

func TestSetWorkspaceRequirements_Validation(t *testing.T) {
	h := newHarness(t)
	path := "/api/v1/workspaces/" + uuid.New().String() + "/requirements"
	cases := []struct {
		name, body string
	}{
		{"invalid json", `{not json`},
		{"unknown field (typo)", `{"requirement":{}}`},
		{"no colon in key", `{"requirements":{"secretfoo":{"level":"required","provenance":"operator_set"}}}`},
		{"unknown type prefix", `{"requirements":{"env:FOO":{"level":"required","provenance":"operator_set"}}}`},
		{"empty suffix", `{"requirements":{"secret:":{"level":"required","provenance":"operator_set"}}}`},
		{"invalid secret name (uppercase)", `{"requirements":{"secret:BAD":{"level":"required","provenance":"operator_set"}}}`},
		{"reserved secret name", `{"requirements":{"secret:wardyn-signing-key":{"level":"required","provenance":"operator_set"}}}`},
		{"invalid egress host (wildcard)", `{"requirements":{"egress:*.example.com":{"level":"required","provenance":"operator_set"}}}`},
		{"invalid egress host (scheme)", `{"requirements":{"egress:https://example.com":{"level":"required","provenance":"operator_set"}}}`},
		{"invalid write path (relative)", `{"requirements":{"write:relative/path":{"level":"required","provenance":"operator_set"}}}`},
		{"invalid write path (denied prefix)", `{"requirements":{"write:/etc/passwd":{"level":"required","provenance":"operator_set"}}}`},
		{"invalid level", `{"requirements":{"secret:ok":{"level":"nope","provenance":"operator_set"}}}`},
		{"invalid provenance", `{"requirements":{"secret:ok":{"level":"required","provenance":"nope"}}}`},
	}
	for _, c := range cases {
		w := do(t, h.srv, http.MethodPut, path, adminToken, c.body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: code = %d, want 400; body=%s", c.name, w.Code, w.Body.String())
		}
	}
}

func TestSetWorkspaceRequirements_TooManyEntriesRejected(t *testing.T) {
	h := newHarness(t)
	reqs := map[string]map[string]string{}
	for i := 0; i < maxWorkspaceRequirements+1; i++ {
		reqs[fmt.Sprintf("secret:req-%d", i)] = map[string]string{"level": "required", "provenance": "operator_set"}
	}
	body, err := json.Marshal(map[string]any{"requirements": reqs})
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/workspaces/" + uuid.New().String() + "/requirements"
	w := do(t, h.srv, http.MethodPut, path, adminToken, string(body))
	if w.Code != http.StatusBadRequest {
		t.Errorf("over-cap requirements: code = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestSetWorkspaceRequirements_BadID(t *testing.T) {
	h := newHarness(t)
	w := do(t, h.srv, http.MethodPut, "/api/v1/workspaces/not-a-uuid/requirements", adminToken, `{"requirements":{}}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("bad id: code = %d, want 400", w.Code)
	}
}

// HTTP-level: happy path

// requirementsStoreFake serves one workspace and captures what
// SetWorkspaceRequirements was called with — a narrower sibling of
// workspaceStoreFake (workspaces_test.go), which does not implement this
// scoped setter.
type requirementsStoreFake struct {
	store.Store
	ws  types.Workspace
	set map[string]types.WorkspaceRequirement
}

func (s *requirementsStoreFake) GetWorkspace(context.Context, uuid.UUID) (types.Workspace, error) {
	return s.ws, nil
}

func (s *requirementsStoreFake) SetWorkspaceRequirements(_ context.Context, _ uuid.UUID, reqs map[string]types.WorkspaceRequirement) (types.Workspace, error) {
	s.set = reqs
	s.ws.Requirements = reqs
	return s.ws, nil
}

func TestSetWorkspaceRequirements_HappyPath(t *testing.T) {
	h := newHarness(t)
	id := uuid.New()
	fake := &requirementsStoreFake{ws: types.Workspace{ID: id, Name: "w"}}
	srv := New(baseTestConfig(h, fake))
	body := `{"requirements":{` +
		`"secret:acme-key":{"level":"required","provenance":"operator_set"},` +
		`"egress:api.stripe.com":{"level":"optional","provenance":"operator_set"},` +
		`"write:/srv/app":{"level":"required","provenance":"scan_seeded"}}}`
	w := do(t, srv, http.MethodPut, "/api/v1/workspaces/"+id.String()+"/requirements", adminToken, body)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if len(fake.set) != 3 {
		t.Fatalf("stored requirements = %+v, want 3 entries", fake.set)
	}
	if got := fake.set["secret:acme-key"]; got.Level != "required" || got.Provenance != "operator_set" {
		t.Errorf("secret:acme-key = %+v", got)
	}
	if got := fake.set["write:/srv/app"]; got.Level != "required" || got.Provenance != "scan_seeded" {
		t.Errorf("write:/srv/app = %+v", got)
	}
	var got types.Workspace
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got.Requirements) != 3 {
		t.Errorf("response requirements = %+v, want 3 entries", got.Requirements)
	}
	found := false
	for _, ev := range h.audit.events {
		if ev.Action == "workspace.requirements.write" {
			found = true
		}
	}
	if !found {
		t.Error("want a workspace.requirements.write audit event")
	}
}

// TestSetWorkspaceRequirements_IntegrationOnlyPreservesContract pins the
// HANDLER half of the h4 server guard: the wizard's step ③ now persists a
// named integration through THIS endpoint on every "Continue", so
// handleSetWorkspaceRequirements/scopedWorkspaceWrite must forward whatever
// row the store hands back into the JSON response VERBATIM — not
// reconstruct/prune it — so naming an integration doesn't ALSO cost the
// operator their scan profile, approved egress, recordings, or build cache in
// the response the wizard reads.
//
// This test uses a FAKE store (requirementsStoreFake), so it can only catch
// the handler dropping/blanking a field on the way to the response — it
// CANNOT catch the real SQL UPDATE widening to clobber a column, since the
// fake's SetWorkspaceRequirements just mutates one field of the struct it was
// seeded with and hands the whole thing back. That SQL-level guarantee is
// what TestPG_SetWorkspaceRequirements_AntiClobber (internal/store/store_workspace_pg_test.go)
// pins, against a real Postgres.
func TestSetWorkspaceRequirements_IntegrationOnlyPreservesContract(t *testing.T) {
	h := newHarness(t)
	id := uuid.New()
	fake := &requirementsStoreFake{ws: types.Workspace{
		ID:               id,
		Name:             "w",
		Status:           types.WorkspaceScanned,
		Profile:          json.RawMessage(`{"languages":["Go"]}`),
		ApprovedEgress:   []string{"api.github.com"},
		RecordResults:    json.RawMessage(`{"smoke":{"status":"recorded"}}`),
		ImageRef:         "wardyn-workspace/w:abc123",
		BuiltProfileHash: "deadbeef",
	}}
	srv := New(baseTestConfig(h, fake))
	body := `{"requirements":{"integration:anthropic_api_key":{"level":"required","provenance":"operator_set"}}}`
	w := do(t, srv, http.MethodPut, "/api/v1/workspaces/"+id.String()+"/requirements", adminToken, body)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got types.Workspace
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Status != types.WorkspaceScanned {
		t.Errorf("status = %q, want preserved %q", got.Status, types.WorkspaceScanned)
	}
	if string(got.Profile) != `{"languages":["Go"]}` {
		t.Errorf("profile = %s, want preserved", got.Profile)
	}
	if len(got.ApprovedEgress) != 1 || got.ApprovedEgress[0] != "api.github.com" {
		t.Errorf("approved_egress = %v, want preserved", got.ApprovedEgress)
	}
	if string(got.RecordResults) != `{"smoke":{"status":"recorded"}}` {
		t.Errorf("record_results = %s, want preserved", got.RecordResults)
	}
	if got.ImageRef != "wardyn-workspace/w:abc123" || got.BuiltProfileHash != "deadbeef" {
		t.Errorf("image cache = (%q,%q), want preserved", got.ImageRef, got.BuiltProfileHash)
	}
	if len(got.Requirements) != 1 {
		t.Errorf("requirements = %+v, want the one new integration row", got.Requirements)
	} else if row := got.Requirements["integration:anthropic_api_key"]; row.Level != "required" {
		t.Errorf("integration row = %+v, want level=required", row)
	}
}

// TestSetWorkspaceRequirements_ResponseOmitsAvailableToYou is F1: this write
// response never goes through either GET stamper (availabilityStamper), so it
// must not carry an "available_to_you" key at all — a plain bool field would
// serialize the zero value "false" here on every write, which the console
// then stores straight into state and reads as an authoritative refusal (see
// types.Workspace.AvailableToYou's own doc comment). Decoding into a raw
// map, not a typed struct, is deliberate: a typed decode into *bool would
// silently accept an absent key by leaving it nil, hiding exactly the
// regression this pins (a present `"available_to_you":false`).
func TestSetWorkspaceRequirements_ResponseOmitsAvailableToYou(t *testing.T) {
	h := newHarness(t)
	id := uuid.New()
	fake := &requirementsStoreFake{ws: types.Workspace{ID: id, Name: "w"}}
	srv := New(baseTestConfig(h, fake))
	w := do(t, srv, http.MethodPut, "/api/v1/workspaces/"+id.String()+"/requirements", adminToken,
		`{"requirements":{"secret:acme-key":{"level":"required","provenance":"operator_set"}}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if _, present := raw["available_to_you"]; present {
		t.Errorf("response carries \"available_to_you\" = %s, want the key absent entirely", raw["available_to_you"])
	}
}

func TestSetWorkspaceRequirements_UnknownWorkspaceIs404(t *testing.T) {
	h := newHarness(t)
	fake := &requirementsStoreFake{}
	// The embedded nil store.Store surfaces as a generic error unless
	// SetWorkspaceRequirements itself reports store.ErrNotFound; mirror
	// handleSetApprovedEgress's sibling test shape by overriding it here.
	srv := New(baseTestConfig(h, notFoundRequirementsStore{fake}))
	w := do(t, srv, http.MethodPut, "/api/v1/workspaces/"+uuid.New().String()+"/requirements",
		adminToken, `{"requirements":{}}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

// notFoundRequirementsStore makes SetWorkspaceRequirements report
// store.ErrNotFound, for the 404 test above.
type notFoundRequirementsStore struct{ *requirementsStoreFake }

func (n notFoundRequirementsStore) SetWorkspaceRequirements(context.Context, uuid.UUID, map[string]types.WorkspaceRequirement) (types.Workspace, error) {
	return types.Workspace{}, store.ErrNotFound
}

// A write: key naming a folder bound to a person's runner is checked by its text alone: the path
// lives on that runner, so what this host has at it (here a link into a denied folder) is not the
// folder's to answer for. The same key on an unbound folder still gets the host check.
func TestSetWorkspaceRequirements_WriteKeyOnRunnerBoundFolder(t *testing.T) {
	link := filepath.Join(t.TempDir(), "src")
	if err := os.Symlink("/etc", link); err != nil {
		t.Fatal(err)
	}
	runnerID := uuid.New()
	body := `{"requirements":{"write:` + link + `":{"level":"required","provenance":"operator_set"}}}`
	for _, c := range []struct {
		name string
		src  types.WorkspaceSource
		want int
	}{
		{"bound to a runner", boundSource(link, runnerID), http.StatusOK},
		{"on this host", types.WorkspaceSource{Type: types.WorkspaceSourceTypeLocalDir, Path: link}, http.StatusBadRequest},
		{"bound, other folder", boundSource(link+"-other", runnerID), http.StatusBadRequest},
	} {
		h := newHarness(t)
		id := uuid.New()
		fake := &requirementsStoreFake{ws: types.Workspace{ID: id, Name: "w", Sources: []types.WorkspaceSource{c.src}}}
		srv := New(baseTestConfig(h, fake))
		w := do(t, srv, http.MethodPut, "/api/v1/workspaces/"+id.String()+"/requirements", adminToken, body)
		if w.Code != c.want {
			t.Errorf("%s: code = %d, want %d; body=%s", c.name, w.Code, c.want, w.Body.String())
		}
	}
}

// A runner-bound folder is still held to the text deny-list.
func TestSetWorkspaceRequirements_WriteKeyOnRunnerBoundFolderDenied(t *testing.T) {
	h := newHarness(t)
	id := uuid.New()
	fake := &requirementsStoreFake{ws: types.Workspace{ID: id, Name: "w", Sources: []types.WorkspaceSource{boundSource("/etc/ssh", uuid.New())}}}
	srv := New(baseTestConfig(h, fake))
	body := `{"requirements":{"write:/etc/ssh":{"level":"required","provenance":"operator_set"}}}`
	if w := do(t, srv, http.MethodPut, "/api/v1/workspaces/"+id.String()+"/requirements", adminToken, body); w.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// boundFolderSubPathLayout is a bound folder dir/proj with a host-side link to a denied folder inside
// it (proj/link) and beside it (proj2, other): what this host has at those paths tells a text-only
// answer (200) from a host check (400).
func boundFolderSubPathLayout(t *testing.T) (dir string) {
	t.Helper()
	dir = t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "proj"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, l := range []string{"proj/link", "proj2", "other"} {
		if err := os.Symlink("/etc", filepath.Join(dir, l)); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// boundFolderPut is a PUT of one write: key against a workspace holding srcs, whose store reads with getErr.
func boundFolderPut(t *testing.T, srcs []types.WorkspaceSource, path string, getErr error) int {
	t.Helper()
	h := newHarness(t)
	id := uuid.New()
	fake := &requirementsStoreFake{ws: types.Workspace{ID: id, Name: "w", Sources: srcs}}
	var st store.Store = fake
	if getErr != nil {
		st = boundFolderFailingStore{fake, getErr}
	}
	srv := New(baseTestConfig(h, st))
	body := `{"requirements":{"write:` + path + `":{"level":"required","provenance":"operator_set"}}}`
	return do(t, srv, http.MethodPut, "/api/v1/workspaces/"+id.String()+"/requirements", adminToken, body).Code
}

// boundFolderFailingStore reads no workspace.
type boundFolderFailingStore struct {
	*requirementsStoreFake
	err error
}

func (f boundFolderFailingStore) GetWorkspace(context.Context, uuid.UUID) (types.Workspace, error) {
	return types.Workspace{}, f.err
}

// A write: key under a folder bound to a runner is checked by its text alone, like the folder itself;
// nothing outside the folder gets that, and a path the folders disagree about does not either.
func TestSetWorkspaceRequirements_boundFolderSubPath(t *testing.T) {
	dir := boundFolderSubPathLayout(t)
	proj := filepath.Join(dir, "proj")
	a, b := uuid.New(), uuid.New()
	local := func(p string) types.WorkspaceSource {
		return types.WorkspaceSource{Type: types.WorkspaceSourceTypeLocalDir, Path: p}
	}
	for _, c := range []struct {
		name string
		srcs []types.WorkspaceSource
		path string
		want int
	}{
		{"sub-path of a bound folder", []types.WorkspaceSource{boundSource(proj, a)}, proj + "/link", http.StatusOK},
		{"deeper sub-path", []types.WorkspaceSource{boundSource(proj, a)}, proj + "/sub/deeper", http.StatusOK},
		{"the folder itself", []types.WorkspaceSource{boundSource(proj, a)}, proj, http.StatusOK},
		{"sibling sharing the name's prefix", []types.WorkspaceSource{boundSource(proj, a)}, dir + "/proj2", http.StatusBadRequest},
		{"dot-dot out of the folder, as written", []types.WorkspaceSource{boundSource(proj, a)}, proj + "/../other", http.StatusBadRequest},
		{"dot-dot out of the folder, cleaned", []types.WorkspaceSource{boundSource(proj, a)}, dir + "/other", http.StatusBadRequest},
		{"dot-dot inside a sub-path", []types.WorkspaceSource{boundSource(proj, a)}, proj + "/sub/../link", http.StatusBadRequest},
		{"relative", []types.WorkspaceSource{boundSource(proj, a)}, "proj/link", http.StatusBadRequest},
		{"absolute, denied, outside", []types.WorkspaceSource{boundSource(proj, a)}, "/etc/ssh", http.StatusBadRequest},
		{"denied by name, under a bound folder", []types.WorkspaceSource{boundSource(proj, a)}, proj + "/docker.sock", http.StatusBadRequest},
		{"under a folder on this host only", []types.WorkspaceSource{local(proj)}, proj + "/link", http.StatusBadRequest},
		{"also a folder on this host", []types.WorkspaceSource{boundSource(proj, a), local(proj + "/link")}, proj + "/link", http.StatusBadRequest},
		{"also bound to another runner", []types.WorkspaceSource{boundSource(proj, a), boundSource(proj+"/link", b)}, proj + "/link", http.StatusBadRequest},
		{"under folders bound to two runners", []types.WorkspaceSource{boundSource(proj, a), boundSource(dir, b)}, proj + "/link", http.StatusBadRequest},
		{"under folders bound to one runner", []types.WorkspaceSource{boundSource(proj, a), boundSource(dir, a)}, proj + "/link", http.StatusOK},
	} {
		if got := boundFolderPut(t, c.srcs, c.path, nil); got != c.want {
			t.Errorf("%s: code = %d, want %d", c.name, got, c.want)
		}
	}
}

// A workspace that cannot be read answers no for a sub-path too: the host check runs.
func TestSetWorkspaceRequirements_boundFolderSubPathReadFails(t *testing.T) {
	dir := boundFolderSubPathLayout(t)
	proj := filepath.Join(dir, "proj")
	if got := boundFolderPut(t, []types.WorkspaceSource{boundSource(proj, uuid.New())}, proj+"/link", errors.New("read failed")); got != http.StatusBadRequest {
		t.Errorf("code = %d, want 400", got)
	}
}
