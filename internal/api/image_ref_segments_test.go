// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// imageCaptureStore is createCaptureStore that also takes a catalog image,
// the write a byo base_image makes.
type imageCaptureStore struct{ createCaptureStore }

func (s *imageCaptureStore) UpsertBaseImage(ctx context.Context, b types.BaseImageEntry) (types.BaseImageEntry, error) {
	return s.lib.UpsertBaseImage(ctx, b)
}

// TestImageRefDotSegmentsRefused: an image ref is also a capability value the
// console builds a control path from, so `../policy/<uuid>` is refused at every
// door that stores one — a workspace's base_image, the catalog, and a grant —
// while an ordinary ref, registry port and digest included, is taken.
func TestImageRefDotSegmentsRefused(t *testing.T) {
	bad := []string{"../policy/x", "ghcr.io/../policy/x:1", "ghcr.io/./img", "ghcr.io//img", "ghcr.io/img/"}
	good := []string{"ghcr.io/org/img:tag", "localhost:5000/org/img", "golang:1.26", "ghcr.io/org/img@sha256:" + strings.Repeat("a", 64)}

	post := func(t *testing.T, path, body string) int {
		t.Helper()
		srv := New(baseTestConfig(newHarness(t), &imageCaptureStore{}))
		return do(t, srv, http.MethodPost, path, adminToken, body).Code
	}
	for _, img := range bad {
		t.Run("refused: "+img, func(t *testing.T) {
			if code := post(t, "/api/v1/workspaces", fmt.Sprintf(`{"name":"w","base_image":{"kind":"byo","image":%q}}`, img)); code != http.StatusBadRequest {
				t.Errorf("POST /workspaces = %d, want 400", code)
			}
			if code := post(t, "/api/v1/base-images", fmt.Sprintf(`{"kind":"byo","image":%q}`, img)); code != http.StatusBadRequest {
				t.Errorf("POST /base-images = %d, want 400", code)
			}
			srv, _ := permServer(t)
			body := fmt.Sprintf(`{"subject_type":"user_type","subject":"standard","capability":"image","value":%q,"effect":"allow"}`, img)
			if w := doSSO(t, srv, http.MethodPost, "/api/v1/permissions/grants", permAdmin(t), body); w.Code != http.StatusBadRequest {
				t.Errorf("POST /permissions/grants = %d, want 400: %s", w.Code, w.Body.String())
			}
		})
	}
	for _, img := range good {
		t.Run("taken: "+img, func(t *testing.T) {
			if code := post(t, "/api/v1/workspaces", fmt.Sprintf(`{"name":"w","base_image":{"kind":"byo","image":%q}}`, img)); code != http.StatusCreated {
				t.Errorf("POST /workspaces = %d, want 201", code)
			}
			if code := post(t, "/api/v1/base-images", fmt.Sprintf(`{"kind":"byo","image":%q}`, img)); code != http.StatusCreated {
				t.Errorf("POST /base-images = %d, want 201", code)
			}
			if _, err := canonicalGrantValue(capImage, img); err != nil {
				t.Errorf("canonicalGrantValue(image, %q) = %v, want accepted", img, err)
			}
		})
	}
}
