// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// fakeImageBuilder satisfies ImageBuilder for the BYOI validation tests. The
// validation these tests exercise fails CLOSED before any resolution/store
// write, so the builder is never actually invoked — it only needs to make
// s.cfg.ImageBuilder non-nil for the "builder wired" case.
type fakeImageBuilder struct{}

func (fakeImageBuilder) BuildDevcontainer(context.Context, string, string, string, io.Writer) (string, error) {
	return "", nil
}
func (fakeImageBuilder) BuildFromDevcontainerFiles(context.Context, map[string]string, string, io.Writer) (string, error) {
	return "", nil
}
func (fakeImageBuilder) FinalizeBase(context.Context, string, string, io.Writer) (string, error) {
	return "", nil
}

// TestBYOI_ImageAndDevcontainerAreMutuallyExclusive asserts the HTTP-layer XOR
// (fails closed before any store write, like the confinement-class checks).
func TestBYOI_ImageAndDevcontainerAreMutuallyExclusive(t *testing.T) {
	h := newHarness(t)
	h.srv.cfg.ImageBuilder = fakeImageBuilder{}
	w := do(t, h.srv, http.MethodPost, "/api/v1/runs", adminToken,
		`{"agent":"claude-code","image":"ubuntu:24.04","devcontainer_repo":"org/repo"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for image+devcontainer_repo, got %d: %s", w.Code, w.Body.String())
	}
}

// TestBYOI_ImageWithNoBuilderIs400 asserts an explicitly chosen image with no
// ImageBuilder wired is a hard 400 (a chosen image must never be silently
// swapped for the convention image, unlike devcontainer_repo which degrades).
func TestBYOI_ImageWithNoBuilderIs400(t *testing.T) {
	h := newHarness(t) // no ImageBuilder on the default harness config
	w := do(t, h.srv, http.MethodPost, "/api/v1/runs", adminToken,
		`{"agent":"claude-code","image":"ubuntu:24.04"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when a custom image is chosen but no builder is wired, got %d: %s", w.Code, w.Body.String())
	}
}

// TestBYOI_MemberDenied403 pins item 5 + the HIGH-3 review fix: a member's
// explicit req.Image OR req.DevcontainerRepo is a 403 (denyMemberCustomImage)
// — both are operator surface (a devcontainer_repo hands the builder just as
// much attacker-reachable repo content as a raw image) — ahead of and
// distinct from the 400 shape validation checks above (an admin with the
// identical request gets those 400s, never a 403). Checked on BOTH the create
// and the preflight paths, since preflight must refuse it exactly as create
// would.
func TestBYOI_MemberDenied403(t *testing.T) {
	h := newHarness(t)
	h.srv.cfg.OIDC = &oidc.Authenticator{}
	h.srv.router = h.srv.routes()
	member := ssoSession(t, "sub-member", "member@corp.example", oidc.RoleMember)

	bodies := map[string]string{
		"image":             `{"agent":"claude-code","image":"ubuntu:24.04"}`,
		"devcontainer_repo": `{"agent":"claude-code","devcontainer_repo":"org/repo"}`,
	}
	for _, path := range []string{"/api/v1/runs", "/api/v1/runs/preflight"} {
		for field, body := range bodies {
			t.Run(path+"/"+field, func(t *testing.T) {
				w := doSSO(t, h.srv, http.MethodPost, path, member, body)
				if w.Code != http.StatusForbidden {
					t.Fatalf("member %s on %s: code = %d, want 403: %s", field, path, w.Code, w.Body.String())
				}
			})
		}
	}

	// The admin path is UNCHANGED: no builder wired still 400s, never 403 —
	// denyMemberCustomImage must be a strict ADDITION ahead of the existing
	// checks, not a replacement for them. devcontainer_repo alone degrades
	// gracefully with no builder wired (unlike image, a hard 400) — assert the
	// weaker "not 403" instead of a specific code so this doesn't pin an
	// unrelated behavior.
	admin := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)
	w := doSSO(t, h.srv, http.MethodPost, "/api/v1/runs", admin, bodies["image"])
	if w.Code != http.StatusBadRequest {
		t.Fatalf("admin image, no builder: code = %d, want 400 (unchanged): %s", w.Code, w.Body.String())
	}
	if w := doSSO(t, h.srv, http.MethodPost, "/api/v1/runs", admin, bodies["devcontainer_repo"]); w.Code == http.StatusForbidden {
		t.Fatalf("admin devcontainer_repo: code = %d, must never be 403", w.Code)
	}
}
