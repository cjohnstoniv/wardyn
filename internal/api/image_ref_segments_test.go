// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

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
	t.Run(`refused: a backslash, ..\policy\x`, func(t *testing.T) {
		if code := post(t, "/api/v1/base-images", `{"kind":"byo","image":"..\\policy\\x"}`); code != http.StatusBadRequest {
			t.Errorf("POST /base-images = %d, want 400", code)
		}
		if _, err := canonicalGrantValue(capImage, `..\policy\x`); err == nil {
			t.Error(`canonicalGrantValue(image, ..\policy\x) accepted it`)
		}
	})
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

// TestAvailabilityTargetIsDecoded: the availability route's value is judged
// and stored as the ref it spells, not as its escapes — so an encoded ref
// lands under the real ref and an encoded dot segment or backslash is refused.
func TestAvailabilityTargetIsDecoded(t *testing.T) {
	const prefix = "/api/v1/permissions/availability/image/"
	t.Run("an encoded ref stores as the real ref", func(t *testing.T) {
		const ref = "ghcr.io/acme/agent:1"
		srv, st := permServer(t)
		st.grants = []types.CapabilityGrant{grant(types.CapabilitySubjectUserType, utDev, capImage, ref, types.CapabilityAllow)}
		w := doSSO(t, srv, http.MethodPut, prefix+"ghcr.io%2Facme%2Fagent:1", permAdmin(t), `{"restricted":true}`)
		if w.Code != http.StatusOK || !st.restricted[capImage][ref] || len(st.restricted[capImage]) != 1 {
			t.Fatalf("PUT = %d %s, restricted = %v; want %q alone restricted", w.Code, w.Body.String(), st.restricted, ref)
		}
	})
	// No client can send a bad escape (net/url refuses to build the request),
	// so the unescape error is driven at availabilityTarget itself.
	t.Run("refused: a bad escape", func(t *testing.T) {
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("kind", capImage)
		rctx.URLParams.Add("*", "ghcr.io%zz")
		r := httptest.NewRequest(http.MethodPut, prefix+"x", nil)
		r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
		w := httptest.NewRecorder()
		if _, _, ok := availabilityTarget(w, r); ok || w.Code != http.StatusBadRequest {
			t.Fatalf("availabilityTarget = ok %v, %d %s; want 400", ok, w.Code, w.Body.String())
		}
	})
	for _, enc := range []string{"%2e%2e%2Fpolicy%2Fx", "..%5Cpolicy%5Cx"} {
		t.Run("refused: "+enc, func(t *testing.T) {
			srv, st := permServer(t)
			w := doSSO(t, srv, http.MethodPut, prefix+enc, permAdmin(t), `{"restricted":false}`)
			if w.Code != http.StatusBadRequest || len(st.restricted[capImage]) != 0 {
				t.Fatalf("PUT = %d %s, restricted = %v; want 400 and nothing written", w.Code, w.Body.String(), st.restricted)
			}
		})
	}
}
