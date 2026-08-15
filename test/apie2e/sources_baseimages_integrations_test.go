// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package apie2e

// This file closes the coverage gap the hardening review found: the
// Postgres-backed apie2e suite had ZERO round-trips against the three route
// families 0.4.5 added — the tier-1 source library, the tier-2 base-image
// catalog, and integrations. Base images and integrations have no SDK
// methods (pkg/client's package doc lists them as SDK-uncovered), so those
// two drive raw HTTP through doAdmin, exactly like the composer tests do for
// their own SDK-uncovered routes (see compose_test.go).

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// ─── sources (tier 1) ───────────────────────────────────────────────────────

// TestSources_CRUDAndLifecycle exercises the source library end to end over
// the SDK: create with a seeded requirements contract -> get -> list contains
// it -> scan a real dir (200, status -> scanned, the operator-set requirement
// survives the scan's rebuild of the scan_seeded subset) -> attaching it to a
// workspace makes a non-forced delete 409 -> force=1 detaches and deletes.
func TestSources_CRUDAndLifecycle(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	ctx := context.Background()

	dir := t.TempDir()
	created, err := h.sdk.CreateSource(ctx, client.SourceRequest{
		Kind:    client.SourceLocalDir,
		Locator: dir,
		Requirements: map[string]client.WorkspaceRequirement{
			"egress:example.com": {Level: "required", Provenance: "operator_set"},
		},
	})
	if err != nil {
		t.Fatalf("CreateSource: %v", err)
	}
	if created.ID == uuid.Nil || created.Kind != client.SourceLocalDir || created.Locator != dir {
		t.Fatalf("created source = %+v", created)
	}
	if req, ok := created.Requirements["egress:example.com"]; !ok || req.Level != "required" {
		t.Fatalf("created source requirements = %+v, want the seeded egress:example.com row", created.Requirements)
	}
	t.Cleanup(func() { _, _ = h.sdk.DeleteSource(context.Background(), created.ID, true) })

	got, err := h.sdk.GetSource(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetSource: %v", err)
	}
	if got.ID != created.ID || got.Locator != dir {
		t.Errorf("GetSource = %+v, want id=%s locator=%s", got, created.ID, dir)
	}

	all, err := h.sdk.ListSources(ctx)
	if err != nil {
		t.Fatalf("ListSources: %v", err)
	}
	if !slices.ContainsFunc(all, func(s types.Source) bool { return s.ID == created.ID }) {
		t.Errorf("ListSources missing %s", created.ID)
	}

	// Scan: a real (empty) temp dir scans clean (200 with the profile).
	raw, err := h.sdk.ScanSource(ctx, created.ID)
	if err != nil {
		t.Fatalf("ScanSource: %v", err)
	}
	var profile workspacescan.WorkspaceProfile
	if err := json.Unmarshal(raw, &profile); err != nil {
		t.Fatalf("decode scan profile: %v (body=%s)", err, raw)
	}
	rescanned, err := h.sdk.GetSource(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetSource after scan: %v", err)
	}
	if rescanned.Status != client.WorkspaceScanned {
		t.Errorf("source status after scan = %q, want %q", rescanned.Status, client.WorkspaceScanned)
	}
	// The operator-set requirement must survive the scan's rebuild of the
	// scan_seeded subset (discovery never clobbers an operator's own row).
	if req, ok := rescanned.Requirements["egress:example.com"]; !ok || req.Provenance != "operator_set" {
		t.Errorf("scan clobbered the operator-set requirement: %+v", rescanned.Requirements)
	}

	// Attach: a workspace whose local_dir source names the SAME canonical
	// locator dedupes onto this exact source row (canonicalSourceIdentity) —
	// how a source becomes "in use" through the public API. An ephemeral
	// scratch dir rides ALONGSIDE it (W6-S1-1): force-detaching the ONLY
	// attachment a workspace has is exactly what STORE-1 refuses
	// (workspacesOrphanedBySource, internal/store/store_sources.go) — this
	// mirrors TestPG_DeleteSourceInUse's ws-c, so this fixture exercises the
	// same contract force=1 actually enforces, not the case it refuses.
	ws, err := h.sdk.CreateWorkspace(ctx, client.WorkspaceRequest{
		Name: "apie2e-src-attach-" + uuid.NewString(),
		Sources: []client.WorkspaceSource{
			{Type: client.WorkspaceSourceTypeLocalDir, Path: dir},
			{Type: client.WorkspaceSourceTypeEphemeral, Target: "/home/agent/scratch"},
		},
	})
	if err != nil {
		t.Fatalf("CreateWorkspace (source attach): %v", err)
	}
	t.Cleanup(func() { _ = h.sdk.DeleteWorkspace(context.Background(), ws.ID) })
	var sourceAttachment *client.WorkspaceAttachment
	for i := range ws.Attachments {
		if ws.Attachments[i].SourceID != nil && *ws.Attachments[i].SourceID == created.ID {
			sourceAttachment = &ws.Attachments[i]
		}
	}
	if len(ws.Attachments) != 2 || sourceAttachment == nil {
		t.Fatalf("workspace attachments = %+v, want two (the source + the ephemeral scratch dir), one pointing at source %s", ws.Attachments, created.ID)
	}

	// In use: a non-forced delete 409s naming the attaching workspace.
	_, err = h.sdk.DeleteSource(ctx, created.ID, false)
	assertAPIStatus(t, err, http.StatusConflict)

	// force=1: detaches everywhere and deletes, echoing which workspace(s) it
	// detached (W6-S1-2: the operator's only signal — nothing 422s downstream).
	detachedFrom, err := h.sdk.DeleteSource(ctx, created.ID, true)
	if err != nil {
		t.Fatalf("DeleteSource(force=true): %v", err)
	}
	if !slices.Contains(detachedFrom, ws.Name) {
		t.Errorf("DeleteSource(force=true) detachedFrom = %v, want it to include %q", detachedFrom, ws.Name)
	}
	_, gerr := h.sdk.GetSource(ctx, created.ID)
	assertAPIStatus(t, gerr, http.StatusNotFound)
}

// ─── base images (tier 2) ──────────────────────────────────────────────────

// baseImageDoc decodes the base-image JSON shape (types.BaseImageEntry) for
// the raw-HTTP calls below — the SDK has no base-image methods.
type baseImageDoc struct {
	ID    string   `json:"id"`
	Kind  string   `json:"kind"`
	Name  string   `json:"name"`
	Image string   `json:"image"`
	Steps []string `json:"steps,omitempty"`
}

// TestBaseImages_CRUDAndDeleteInUse exercises the base-image catalog over raw
// HTTP (no SDK coverage): create -> list contains it -> get -> attaching it to
// a workspace makes a non-forced delete 409 -> force=1 detaches (the
// workspace's base_image_id goes NULL — the derived-recommended-build marker,
// per handleDeleteBaseImage's doc) and deletes.
func TestBaseImages_CRUDAndDeleteInUse(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	ctx := context.Background()
	base := h.srv.URL + "/api/v1/base-images"

	image := "registry.example.com/apie2e-" + uuid.NewString() + ":v1"
	reqBody, _ := json.Marshal(map[string]any{"kind": "registry", "name": "apie2e base image", "image": image})
	status, raw := doAdmin(t, http.MethodPost, base, reqBody)
	if status != http.StatusCreated {
		t.Fatalf("POST /base-images status = %d, want 201 (body=%s)", status, raw)
	}
	var created baseImageDoc
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatalf("decode created base image: %v (body=%s)", err, raw)
	}
	if created.ID == "" || created.Image != image || created.Kind != "registry" {
		t.Fatalf("created base image = %+v", created)
	}
	t.Cleanup(func() { doAdmin(t, http.MethodDelete, base+"/"+created.ID+"?force=1", nil) })

	// W7-S1-1: there is no GET /base-images/{id} (DEADCODE-1, sources.go) — a
	// base image's own catalog row is read back via the list, never a
	// by-id route (mountLibraryRoutes registers no Get for it).
	status, raw = doAdmin(t, http.MethodGet, base, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /base-images status = %d (body=%s)", status, raw)
	}
	var list struct {
		BaseImages []baseImageDoc `json:"base_images"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("decode base image list: %v (body=%s)", err, raw)
	}
	if !slices.ContainsFunc(list.BaseImages, func(b baseImageDoc) bool { return b.ID == created.ID }) {
		t.Errorf("base image list missing %s", created.ID)
	}

	// Attach: a workspace whose base_image names the SAME (kind, image, steps)
	// identity dedupes onto this exact catalog row.
	ws, err := h.sdk.CreateWorkspace(ctx, client.WorkspaceRequest{
		Name:      "apie2e-baseimg-attach-" + uuid.NewString(),
		BaseImage: &types.WorkspaceBaseImage{Kind: "registry", Image: image},
	})
	if err != nil {
		t.Fatalf("CreateWorkspace (base image attach): %v", err)
	}
	t.Cleanup(func() { _ = h.sdk.DeleteWorkspace(context.Background(), ws.ID) })
	if ws.BaseImageID == nil || ws.BaseImageID.String() != created.ID {
		t.Fatalf("workspace base_image_id = %v, want %s", ws.BaseImageID, created.ID)
	}

	// In use: a non-forced delete 409s.
	status, raw = doAdmin(t, http.MethodDelete, base+"/"+created.ID, nil)
	if status != http.StatusConflict {
		t.Fatalf("DELETE /base-images/{id} (in use) status = %d, want 409 (body=%s)", status, raw)
	}

	// force=1: detaches (base_image_id -> NULL) and deletes.
	status, raw = doAdmin(t, http.MethodDelete, base+"/"+created.ID+"?force=1", nil)
	if status != http.StatusNoContent {
		t.Fatalf("DELETE /base-images/{id}?force=1 status = %d, want 204 (body=%s)", status, raw)
	}
	fresh, err := h.sdk.GetWorkspace(ctx, ws.ID)
	if err != nil {
		t.Fatalf("GetWorkspace after force-detach: %v", err)
	}
	if fresh.BaseImageID != nil {
		t.Errorf("workspace base_image_id after force-detach = %v, want nil (falls back to the derived recommended build)", fresh.BaseImageID)
	}

	// No GET /base-images/{id} to re-probe (W7-S1-1, see above) — assert
	// absence from the list instead.
	status, raw = doAdmin(t, http.MethodGet, base, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /base-images after delete status = %d (body=%s)", status, raw)
	}
	var listAfter struct {
		BaseImages []baseImageDoc `json:"base_images"`
	}
	if err := json.Unmarshal(raw, &listAfter); err != nil {
		t.Fatalf("decode base image list after delete: %v (body=%s)", err, raw)
	}
	if slices.ContainsFunc(listAfter.BaseImages, func(b baseImageDoc) bool { return b.ID == created.ID }) {
		t.Errorf("base image list still contains %s after delete", created.ID)
	}
}

// ─── integrations ───────────────────────────────────────────────────────────

// integrationDoc decodes the wire shape GET/PUT/adopt integrations all share
// (api.SetupIntegration, which flattens types.Integration's fields plus
// "source" and "capabilities") — only the fields this file's assertions need.
type integrationDoc struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
	// Secrets is the base-component secret list (role + secret_name + delivery).
	Secrets []struct {
		Role       string `json:"role"`
		SecretName string `json:"secret_name"`
	} `json:"secrets,omitempty"`
	Docs   string `json:"docs,omitempty"`
	Source string `json:"source"` // "stored" | "legacy"
}

// roleSecret picks the secret name stored for role ("" when absent).
func (d integrationDoc) roleSecret(role string) string {
	for _, s := range d.Secrets {
		if s.Role == role {
			return s.SecretName
		}
	}
	return ""
}

// TestIntegrations_ListAdoptAndPutBack drives the integrations surface over
// raw HTTP (no SDK coverage): a present anthropic-api-key secret derives a
// LEGACY row with no write path of its own; GET /integrations lists it (the
// "detail" a client reads — there is no separate GET /integrations/{id}
// route, so a client's detail view is one entry picked out of the list);
// adopt persists it verbatim (source flips legacy -> stored, and adopting
// twice 409s); PUT then proves the newly-stored row is actually editable — a
// full-replacement round-trip of its own fields plus one real change.
func TestIntegrations_ListAdoptAndPutBack(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	ctx := context.Background()
	base := h.srv.URL + "/api/v1/integrations"

	const secretName = "anthropic-api-key"
	if err := h.sdk.SetSecret(ctx, secretName, "sk-ant-apie2e-"+uuid.NewString()); err != nil {
		t.Fatalf("SetSecret: %v", err)
	}
	t.Cleanup(func() { _ = h.sdk.DeleteSecret(context.Background(), secretName) })

	status, raw := doAdmin(t, http.MethodGet, base, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /integrations status = %d (body=%s)", status, raw)
	}
	var list struct {
		Integrations []integrationDoc `json:"integrations"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("decode integrations list: %v (body=%s)", err, raw)
	}
	idx := slices.IndexFunc(list.Integrations, func(in integrationDoc) bool { return in.ID == "anthropic_api_key" })
	if idx < 0 {
		t.Fatalf("integrations list missing the anthropic_api_key legacy row: %+v", list.Integrations)
	}
	legacy := list.Integrations[idx]
	if legacy.Source != "legacy" {
		t.Fatalf("anthropic_api_key source = %q, want legacy (not yet adopted)", legacy.Source)
	}
	if legacy.Kind != "anthropic_api_key" || legacy.roleSecret("api_key") != secretName {
		t.Fatalf("anthropic_api_key legacy row = %+v", legacy)
	}

	status, raw = doAdmin(t, http.MethodPost, base+"/"+legacy.ID+"/adopt", nil)
	if status != http.StatusOK {
		t.Fatalf("POST /integrations/{id}/adopt status = %d, want 200 (body=%s)", status, raw)
	}
	var adopted integrationDoc
	if err := json.Unmarshal(raw, &adopted); err != nil {
		t.Fatalf("decode adopted integration: %v (body=%s)", err, raw)
	}
	if adopted.Source != "stored" {
		t.Fatalf("adopted integration source = %q, want stored", adopted.Source)
	}
	t.Cleanup(func() { doAdmin(t, http.MethodDelete, base+"/"+legacy.ID, nil) })

	// Adopting again 409s: it is already stored.
	status, _ = doAdmin(t, http.MethodPost, base+"/"+legacy.ID+"/adopt", nil)
	if status != http.StatusConflict {
		t.Errorf("re-adopt status = %d, want 409", status)
	}

	// PUT-back: the documented contract is a full replacement, so round-trip
	// the stored row's own fields plus one real change, proving a row reached
	// only through adopt is actually editable (integrations.go's PUT used to
	// reject every colon-qualified id; this one has none, so it is unaffected
	// either way — see the sibling review row on that).
	const newDocs = "https://docs.anthropic.com/apie2e-test"
	putBody, _ := json.Marshal(map[string]any{
		"name": adopted.Name, "kind": adopted.Kind,
		"secrets": []map[string]any{{"role": "api_key", "secret_name": secretName,
			"delivery": map[string]string{"mode": "proxy_header", "header": "x-api-key", "format": "%s"}}},
		"docs": newDocs,
	})
	status, raw = doAdmin(t, http.MethodPut, base+"/"+legacy.ID, putBody)
	if status != http.StatusOK {
		t.Fatalf("PUT /integrations/{id} status = %d, want 200 (body=%s)", status, raw)
	}
	var updated integrationDoc
	if err := json.Unmarshal(raw, &updated); err != nil {
		t.Fatalf("decode updated integration: %v (body=%s)", err, raw)
	}
	if updated.Docs != newDocs || updated.Source != "stored" {
		t.Fatalf("PUT-back result = %+v, want docs=%q source=stored", updated, newDocs)
	}

	// The write landed: a fresh list still shows it stored, with the new docs.
	status, raw = doAdmin(t, http.MethodGet, base, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /integrations (after PUT) status = %d (body=%s)", status, raw)
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("decode integrations list: %v (body=%s)", err, raw)
	}
	idx = slices.IndexFunc(list.Integrations, func(in integrationDoc) bool { return in.ID == "anthropic_api_key" })
	if idx < 0 || list.Integrations[idx].Docs != newDocs || list.Integrations[idx].Source != "stored" {
		t.Fatalf("integrations list after PUT = %+v, want the persisted docs on a stored row", list.Integrations)
	}
}
