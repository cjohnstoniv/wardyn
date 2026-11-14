// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// baseImagesEndpointFake mirrors sourcesEndpointFake (sources_test.go) for the
// tier-2 catalog's own delete-in-use twin: in-use is a loud 409 naming the
// workspaces, ?force=1 detaches them, and a genuinely-absent id 404s on BOTH
// paths — the same properties DeleteSource pins for the tier-1 library.
type baseImagesEndpointFake struct {
	store.Store
	used []string // WorkspacesUsingBaseImage answer
	// lateUse points at the image after the handler's listing: a forced DeleteBaseImage detaches these too.
	lateUse  []string
	raced    bool // a forced DeleteBaseImage answers ErrConflict: a workspace pointed at the image while it ran
	notFound bool // DeleteBaseImage answer for a genuinely-absent id
	deleted  *uuid.UUID
	detached bool
}

func (s *baseImagesEndpointFake) WorkspacesUsingBaseImage(context.Context, uuid.UUID) ([]string, error) {
	return s.used, nil
}

func (s *baseImagesEndpointFake) DeleteBaseImage(_ context.Context, id uuid.UUID, detach bool) ([]string, error) {
	if s.notFound {
		return nil, store.ErrNotFound
	}
	if s.raced || (!detach && len(s.used) > 0) {
		// Mirrors the real store's atomic NOT EXISTS guard: a non-force
		// delete while something is in use never reaches the delete.
		return nil, store.ErrConflict
	}
	s.deleted, s.detached = &id, detach
	if !detach {
		return []string{}, nil
	}
	return append(append([]string{}, s.used...), s.lateUse...), nil
}

// TestBaseImages_DeleteInUseIsLoud is the base-image twin of
// TestSources_DeleteInUseIsLoud: in use refuses with a 409 naming every
// workspace, force=1 detaches (base_image_id -> NULL, the derived
// recommended build) and deletes.
func TestBaseImages_DeleteInUseIsLoud(t *testing.T) {
	h := newHarness(t)
	fake := &baseImagesEndpointFake{used: []string{"payments-ws", "review-ws"}}
	srv := New(baseTestConfig(h, fake))
	id := uuid.New()

	w := do(t, srv, http.MethodDelete, "/api/v1/base-images/"+id.String(), adminToken, "")
	if w.Code != http.StatusConflict {
		t.Fatalf("in-use delete: code = %d, want 409; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "payments-ws") || !strings.Contains(w.Body.String(), "review-ws") {
		t.Errorf("409 body must NAME the workspaces using it: %s", w.Body.String())
	}
	if fake.deleted != nil {
		t.Error("a refused delete must not delete")
	}

	forced := do(t, srv, http.MethodDelete, "/api/v1/base-images/"+id.String()+"?force=1", adminToken, "")
	if forced.Code != http.StatusNoContent {
		t.Fatalf("forced: code = %d, want 204; body=%s", forced.Code, forced.Body.String())
	}
	if fake.deleted == nil || !fake.detached {
		t.Error("force=1 must detach-and-delete")
	}
}

// TestBaseImages_ForcedDeleteAuditsWhatTheDetachChanged is the base-image twin of
// TestSources_ForcedDeleteNamesWhatTheDetachChanged: the audit row names a workspace that
// pointed at the image after the handler's listing.
func TestBaseImages_ForcedDeleteAuditsWhatTheDetachChanged(t *testing.T) {
	h := newHarness(t)
	fake := &baseImagesEndpointFake{used: []string{"payments-ws"}, lateUse: []string{"late-ws"}}
	srv := New(baseTestConfig(h, fake))
	id := uuid.New()

	w := do(t, srv, http.MethodDelete, "/api/v1/base-images/"+id.String()+"?force=1", adminToken, "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("forced: code = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	evs := auditActions(h, "base_image.delete")
	if len(evs) != 1 || !strings.Contains(string(evs[0].Data), "late-ws") {
		t.Errorf("base_image.delete audit = %+v, want one row naming late-ws", evs)
	}
}

// TestBaseImages_ForcedDeleteRacedByANewUseDoesNotOfferForce: a forced delete the store refuses because
// a workspace pointed at the image while it ran answers 409 without the non-force sentence, which
// counts the handler's own listing and tells the caller to pass ?force=1.
func TestBaseImages_ForcedDeleteRacedByANewUseDoesNotOfferForce(t *testing.T) {
	h := newHarness(t)
	fake := &baseImagesEndpointFake{raced: true}
	srv := New(baseTestConfig(h, fake))

	w := do(t, srv, http.MethodDelete, "/api/v1/base-images/"+uuid.New().String()+"?force=1", adminToken, "")
	if w.Code != http.StatusConflict {
		t.Fatalf("raced forced delete: code = %d, want 409; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "A workspace started using this base image during the delete, so it was not deleted. Try again.") {
		t.Errorf("a raced forced delete's 409 lacks its own sentence: %s", body)
	}
	if strings.Contains(body, "force=1") || strings.Contains(body, "0 workspace") {
		t.Errorf("a forced delete's 409 offered force or counted the stale listing: %s", body)
	}
	if evs := auditActions(h, "base_image.delete"); len(evs) != 0 {
		t.Errorf("a refused delete wrote %d base_image.delete rows, want none", len(evs))
	}
}

// TestBaseImages_DeleteUnknownIDIs404 is the base-image twin of
// TestSources_DeleteUnknownIDIs404: a genuinely-absent id must 404 on both
// the plain and ?force=1 paths, never read as "in use".
func TestBaseImages_DeleteUnknownIDIs404(t *testing.T) {
	h := newHarness(t)
	fake := &baseImagesEndpointFake{notFound: true}
	srv := New(baseTestConfig(h, fake))
	id := uuid.New()

	w := do(t, srv, http.MethodDelete, "/api/v1/base-images/"+id.String(), adminToken, "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown id: code = %d, want 404; body=%s", w.Code, w.Body.String())
	}

	forced := do(t, srv, http.MethodDelete, "/api/v1/base-images/"+id.String()+"?force=1", adminToken, "")
	if forced.Code != http.StatusNotFound {
		t.Fatalf("unknown id force=1: code = %d, want 404; body=%s", forced.Code, forced.Body.String())
	}
}
