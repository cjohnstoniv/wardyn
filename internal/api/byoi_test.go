// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"testing"
)

// fakeImageBuilder satisfies ImageBuilder for the BYOI validation tests. The
// validation these tests exercise fails CLOSED before any resolution/store
// write, so the builder is never actually invoked — it only needs to make
// s.cfg.ImageBuilder non-nil for the "builder wired" case.
type fakeImageBuilder struct{}

func (fakeImageBuilder) BuildDevcontainer(context.Context, string, string, string) (string, error) {
	return "", nil
}
func (fakeImageBuilder) BuildFromDevcontainerFiles(context.Context, map[string]string, string) (string, error) {
	return "", nil
}
func (fakeImageBuilder) FinalizeBase(context.Context, string, string) (string, error) {
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
