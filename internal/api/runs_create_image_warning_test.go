// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"testing"
)

// fakeDevcontainerBuilder is an ImageBuilder that answers every build with a
// fixed tag — enough for the negative control, which only needs the builder to
// be WIRED.
type fakeDevcontainerBuilder struct{ tag string }

func (b fakeDevcontainerBuilder) FinalizeBase(context.Context, string, string, io.Writer) (string, error) {
	return b.tag, nil
}

func (b fakeDevcontainerBuilder) BuildDevcontainer(context.Context, string, string, string, io.Writer) (string, error) {
	return b.tag, nil
}

func (b fakeDevcontainerBuilder) BuildFromDevcontainerFiles(context.Context, map[string]string, string, io.Writer) (string, error) {
	return b.tag, nil
}

// resolveCreateRunImage fails a workspace base_image CLOSED when no
// ImageBuilder is wired (PARITY-4) but lets devcontainer_repo fall through to
// the convention image. The fall-through is correct — a hard refusal would
// break every no-builder deployment that has been launching this way — but it
// was SILENT on the only channel this door has: the caller got a 201 for a run
// whose sandbox is not the one they asked for, and the only record was an INFO
// setup row no CLI or API client reads.
func TestCreateRun_DevcontainerWithoutBuilderWarnsOnThe201(t *testing.T) {
	srv, _, _, _ := abortHarness(t, nil)
	srv.cfg.ImageBuilder = nil

	w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken,
		`{"agent":"claude-code","task":"echo hi","devcontainer_repo":"org/repo"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("code = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	var created createRunResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !slices.Contains(created.Warnings, devcontainerNoBuilderWarning) {
		t.Errorf("warnings = %v\nwant the no-builder devcontainer sentence — the run launched on the convention image and nothing said so", created.Warnings)
	}
}

// TestCreateRun_DevcontainerWithABuilderCarriesNoWarning is the negative
// control: with a builder wired the build really happened, so the sentence
// would be a lie.
func TestCreateRun_DevcontainerWithABuilderCarriesNoWarning(t *testing.T) {
	srv, _, _, _ := abortHarness(t, nil)
	srv.cfg.ImageBuilder = fakeDevcontainerBuilder{tag: "wardyn-devcontainer/built:latest"}

	w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken,
		`{"agent":"claude-code","task":"echo hi","devcontainer_repo":"org/repo"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("code = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	var created createRunResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if slices.Contains(created.Warnings, devcontainerNoBuilderWarning) {
		t.Errorf("warnings = %v; a wired builder built the devcontainer — there is nothing to warn about", created.Warnings)
	}
}
