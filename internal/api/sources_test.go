// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The tier-1 library endpoints. The properties worth pinning at the API layer:
// POST is an upsert by CANONICAL identity (the library's whole point), a
// source contract refuses integration: keys (tier-3-only, owner decision),
// and delete-in-use is a loud 409 naming the attaching workspaces.

type sourcesEndpointFake struct {
	store.Store
	lib      sourceLibraryFake
	attached []string // WorkspacesAttaching answer
	deleted  *uuid.UUID
	detached bool
}

func (s *sourcesEndpointFake) UpsertSource(ctx context.Context, src types.Source) (types.Source, error) {
	return s.lib.UpsertSource(ctx, src)
}
func (s *sourcesEndpointFake) GetSource(_ context.Context, id uuid.UUID) (types.Source, error) {
	for _, src := range s.lib.sources {
		if src.ID == id {
			return src, nil
		}
	}
	return types.Source{}, store.ErrNotFound
}
func (s *sourcesEndpointFake) WorkspacesAttaching(context.Context, uuid.UUID) ([]string, error) {
	return s.attached, nil
}
func (s *sourcesEndpointFake) DeleteSource(_ context.Context, id uuid.UUID, detach bool) error {
	if !detach && len(s.attached) > 0 {
		// Mirrors the real store's atomic NOT EXISTS guard: a non-force
		// delete while something is attached never reaches the delete.
		return store.ErrConflict
	}
	s.deleted, s.detached = &id, detach
	return nil
}

func TestSources_UpsertByCanonicalIdentity(t *testing.T) {
	h := newHarness(t)
	fake := &sourcesEndpointFake{}
	srv := New(baseTestConfig(h, fake))

	first := do(t, srv, http.MethodPost, "/api/v1/sources", adminToken,
		`{"kind":"local_dir","locator":"/home/me/payments/"}`)
	if first.Code != http.StatusCreated {
		t.Fatalf("first POST: code = %d, want 201; body=%s", first.Code, first.Body.String())
	}
	var created types.Source
	if err := json.Unmarshal(first.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	// Canonicalized: the trailing slash never reaches the library, and the
	// name derives from the last segment.
	if created.Locator != "/home/me/payments" || created.Name != "payments" {
		t.Errorf("created = %+v, want canonical locator + derived name", created)
	}

	// The same dir, differently spelled: the SAME entry back, 200 not 201.
	second := do(t, srv, http.MethodPost, "/api/v1/sources", adminToken,
		`{"kind":"local_dir","locator":"/home/me/payments"}`)
	if second.Code != http.StatusOK {
		t.Fatalf("re-POST: code = %d, want 200 (upsert hit); body=%s", second.Code, second.Body.String())
	}
	var again types.Source
	_ = json.Unmarshal(second.Body.Bytes(), &again)
	if again.ID != created.ID {
		t.Error("identity re-POST must return the existing library entry, not a new one")
	}
}

func TestSources_ContractRefusesIntegrationKeys(t *testing.T) {
	h := newHarness(t)
	srv := New(baseTestConfig(h, &sourcesEndpointFake{}))
	w := do(t, srv, http.MethodPost, "/api/v1/sources", adminToken,
		`{"kind":"repo","locator":"acme/widgets","requirements":{"integration:corp-artifactory":{"level":"required","provenance":"operator_set"}}}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "integrations compose at the workspace") {
		t.Errorf("body = %s, want the tier-3-only explanation", w.Body.String())
	}
}

func TestSources_DeleteInUseIsLoud(t *testing.T) {
	h := newHarness(t)
	fake := &sourcesEndpointFake{attached: []string{"payments-ws", "review-ws"}}
	srv := New(baseTestConfig(h, fake))
	id := uuid.New()

	w := do(t, srv, http.MethodDelete, "/api/v1/sources/"+id.String(), adminToken, "")
	if w.Code != http.StatusConflict {
		t.Fatalf("in-use delete: code = %d, want 409; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "payments-ws") || !strings.Contains(w.Body.String(), "review-ws") {
		t.Errorf("409 body must NAME the attaching workspaces: %s", w.Body.String())
	}
	if fake.deleted != nil {
		t.Error("a refused delete must not delete")
	}

	forced := do(t, srv, http.MethodDelete, "/api/v1/sources/"+id.String()+"?force=1", adminToken, "")
	if forced.Code != http.StatusNoContent {
		t.Fatalf("forced: code = %d, want 204; body=%s", forced.Code, forced.Body.String())
	}
	if fake.deleted == nil || !fake.detached {
		t.Error("force=1 must detach-and-delete")
	}
}

// A repo locator's identity dedupes case-INsensitively on scheme+host, but
// hydrate must serve the operator's clone path back EXACTLY as authored —
// self-hosted GitLab/Gitea/Bitbucket paths are case-sensitive, so lowercasing
// the whole locator clones the wrong URL or 404s.
func TestSources_RepoLocatorCanonicalizesHostOnly(t *testing.T) {
	h := newHarness(t)
	srv := New(baseTestConfig(h, &sourcesEndpointFake{}))
	w := do(t, srv, http.MethodPost, "/api/v1/sources", adminToken,
		`{"kind":"repo","locator":"https://Git.Corp.Example/MyGroup/MyRepo.git"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("code = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	var created types.Source
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	const want = "https://git.corp.example/MyGroup/MyRepo.git"
	if created.Locator != want {
		t.Errorf("Locator = %q, want %q (scheme+host lowercased, path preserved as authored)", created.Locator, want)
	}
}

func TestCanonicalRepoLocator_HostOnlyLowercased(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"https URL, mixed-case host and path", "https://Git.Corp.Example/MyGroup/MyRepo.git", "https://git.corp.example/MyGroup/MyRepo.git"},
		{"scp-form, mixed-case host, path untouched", "git@GitHub.com:MyOrg/MyRepo.git", "git@github.com:MyOrg/MyRepo.git"},
		{"bare org/name slug: no host component to normalize", "MyOrg/MyRepo", "MyOrg/MyRepo"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := canonicalRepoLocator(c.in); got != c.want {
				t.Errorf("canonicalRepoLocator(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// The create path routes embedded sources through the library now: two
// workspaces naming the same dir share ONE library entry.
func TestCreateWorkspace_EmbeddedSourcesUpsertIntoLibrary(t *testing.T) {
	h := newHarness(t)
	fake := &createCaptureStore{}
	srv := New(baseTestConfig(h, fake))

	for _, name := range []string{"a", "b"} {
		w := do(t, srv, http.MethodPost, "/api/v1/workspaces", adminToken,
			`{"name":"`+name+`","sources":[{"type":"local_dir","path":"/home/me/payments"}]}`)
		if w.Code != http.StatusCreated {
			t.Fatalf("create %s: code = %d; body=%s", name, w.Code, w.Body.String())
		}
	}
	if got := len(fake.lib.sources); got != 1 {
		t.Errorf("library entries = %d, want 1 (same dir, one source)", got)
	}
	// And the persisted workspace carries the attachment.
	if len(fake.created.Attachments) != 1 || fake.created.Attachments[0].SourceID == nil {
		t.Errorf("captured attachments = %+v, want one source ref", fake.created.Attachments)
	}
}
