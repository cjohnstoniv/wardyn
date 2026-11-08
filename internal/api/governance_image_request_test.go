// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// imgCreateBare posts body to the create handler as a signed-in person, on a server with no store: the
// checks before the offer read answer without one.
func imgCreateBare(body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, requestsPath, strings.NewReader(body)).WithContext(elevPersonCtx("sub-img-bare", []string{}, false))
	(&Server{}).handleCreateGovernanceRequest(w, r)
	return w
}

// TestImageRequestKindRows: kind image is a row of the request kinds and of the held-change table, decided
// by its rule's approver and rejected with its own row.
func TestImageRequestKindRows(t *testing.T) {
	k, ok := governanceRequestKinds["image"]
	if !ok || k.target != types.GovernanceTargetImageRequest || k.decider != types.DeciderKindImage {
		t.Fatalf("governanceRequestKinds[image] = %+v, %v; want target %s, decider image", k, ok, types.GovernanceTargetImageRequest)
	}
	held, ok := governanceChangeKinds[types.GovernanceTargetImageRequest]
	if !ok || held.apply == nil || held.rejectAction != "governance.image_request.reject" {
		t.Fatalf("governanceChangeKinds[%s] = %+v, %v; want an apply and the reject row", types.GovernanceTargetImageRequest, held, ok)
	}
	if !held.forChange(types.GovernanceChange{TargetKind: types.GovernanceTargetImageRequest}).approver.scoped {
		t.Error("an image request is not decided by its rule (requestApprover)")
	}
}

// TestImageRequestTargetKey: the key is the subject and the digest, so two names for the same bytes are
// one pending request.
func TestImageRequestTargetKey(t *testing.T) {
	const hex = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	a := imageRequestKey("sub-x", "registry.example.com/team/tool:1@sha256:"+hex)
	b := imageRequestKey("sub-x", "mirror.example.com/tool@sha256:"+hex)
	if a != "sub-x|sha256:"+hex || a != b {
		t.Errorf("keys = %q and %q, want both sub-x|sha256:<hex>", a, b)
	}
}

func TestImageRequestRequiresDigest(t *testing.T) {
	const hex = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	for name, ref := range map[string]string{
		"a tag only":         "registry.example.com/team/tool:1",
		"a path traversal":   "registry.example.com/../tool@sha256:" + hex,
		"a backslash":        `registry.example.com\tool@sha256:` + hex,
		"a short digest":     "registry.example.com/team/tool@sha256:abc",
		"a star in the name": "ghcr.io/team/*@sha256:" + hex,
		"no image":           "",
	} {
		w := imgCreateBare(`{"kind":"image","image":"` + strings.ReplaceAll(ref, `\`, `\\`) + `","reason":"need it"}`)
		if w.Code != http.StatusBadRequest || wireReason(t, w) != reasonImageRequestNotPinned {
			t.Errorf("%s = %d %s, want 400 %s", name, w.Code, w.Body, reasonImageRequestNotPinned)
		}
	}
}

func TestImageRequestRefusesUnitAndDuration(t *testing.T) {
	const ref = "registry.example.com/team/tool@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	for name, body := range map[string]string{
		"unit_id":  `{"kind":"image","image":"` + ref + `","reason":"x","unit_id":"00000000-0000-0000-0000-000000000001"}`,
		"duration": `{"kind":"image","image":"` + ref + `","reason":"x","duration":"1h"}`,
	} {
		if w := imgCreateBare(body); w.Code != http.StatusBadRequest || wireReason(t, w) != reasonInvalidRequestBody {
			t.Errorf("%s = %d %s, want 400 %s", name, w.Code, w.Body, reasonInvalidRequestBody)
		}
	}
}
