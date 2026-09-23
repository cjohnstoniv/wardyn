// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// imageCheckErrRunner is the production k8s shape: the wired Orchestrator
// always implements runner.ImageChecker, and answers an error when no
// substrate can check (a k8s-only install) or the Docker daemon is unreachable.
type imageCheckErrRunner struct{ *fakeRunner }

func (imageCheckErrRunner) ImagePresent(context.Context, string) (bool, error) {
	return false, errors.New("orchestrator: no wired substrate supports image presence checks")
}

func TestClaudeSignInImageResolves(t *testing.T) {
	ctx := context.Background()

	t.Run("no pin never resolves", func(t *testing.T) {
		if claudeSignInImageResolves(ctx, nil, nil) {
			t.Fatal("an unpinned claude-code image must not resolve")
		}
	})

	t.Run("pinned with no ImageChecker Runner is trusted", func(t *testing.T) {
		images := map[string]string{"claude-code": "wardyn/agent-claude-code:local"}
		if !claudeSignInImageResolves(ctx, images, &fakeRunner{}) {
			t.Fatal("a pin must be trusted when the wired Runner cannot confirm local presence")
		}
	})

	t.Run("pinned and the presence check errors (k8s) is trusted, unverified", func(t *testing.T) {
		images := map[string]string{"claude-code": "wardyn/agent-claude-code:local"}
		st := resolveClaudeSignInImage(ctx, images, imageCheckErrRunner{&fakeRunner{}})
		if !st.resolved || st.verified {
			t.Fatalf("state = %+v, want resolved and unverified: an inconclusive check must not refuse every subscription write", st)
		}
	})

	t.Run("pinned with no Runner at all is trusted", func(t *testing.T) {
		images := map[string]string{"claude-code": "wardyn/agent-claude-code:local"}
		if !claudeSignInImageResolves(ctx, images, nil) {
			t.Fatal("a pin must be trusted when no Runner is wired (headless)")
		}
	})

	t.Run("pinned and confirmed present resolves", func(t *testing.T) {
		ref := "wardyn/agent-claude-code:local"
		rnr := &imageCheckerRunner{fakeRunner: &fakeRunner{}, present: map[string]bool{ref: true}}
		if !claudeSignInImageResolves(ctx, map[string]string{"claude-code": ref}, rnr) {
			t.Fatal("a pin the Docker runner confirms present must resolve")
		}
	})

	t.Run("pinned but NOT present on the Docker runner does not resolve", func(t *testing.T) {
		ref := "wardyn/agent-claude-code:local"
		rnr := &imageCheckerRunner{fakeRunner: &fakeRunner{}, present: map[string]bool{}}
		if claudeSignInImageResolves(ctx, map[string]string{"claude-code": ref}, rnr) {
			t.Fatal("a pin baked into compose/run-host defaults must NOT resolve until the image is actually built — " +
				"that's the whole reason the live check exists")
		}
	})

	t.Run("whitespace-only pin does not resolve", func(t *testing.T) {
		if claudeSignInImageResolves(ctx, map[string]string{"claude-code": "  "}, nil) {
			t.Fatal("a blank pin must not resolve")
		}
	})
}

func TestClaudeSignInImageCheck(t *testing.T) {
	ctx := context.Background()

	t.Run("unpinned warns and names the fix", func(t *testing.T) {
		chk := claudeSignInImageCheck(ctx, nil, nil)
		if chk.Status != "warn" {
			t.Fatalf("status = %q, want warn", chk.Status)
		}
		if chk.Blocking {
			t.Fatal("the sign-in image is an optional prerequisite (subscriptions only) — must never block the console")
		}
		if !strings.Contains(chk.Fix, "agent-images-core") {
			t.Errorf("fix names no build command: %q", chk.Fix)
		}
	})

	t.Run("pinned and confirmed present is info", func(t *testing.T) {
		ref := "wardyn/agent-claude-code:local"
		rnr := &imageCheckerRunner{fakeRunner: &fakeRunner{}, present: map[string]bool{ref: true}}
		chk := claudeSignInImageCheck(ctx, map[string]string{"claude-code": ref}, rnr)
		if chk.Status != "info" {
			t.Fatalf("status = %q, want info", chk.Status)
		}
		if !strings.Contains(chk.Detail, ref) {
			t.Errorf("detail does not name the resolved ref: %q", chk.Detail)
		}
	})

	t.Run("pinned but not present warns", func(t *testing.T) {
		ref := "wardyn/agent-claude-code:local"
		rnr := &imageCheckerRunner{fakeRunner: &fakeRunner{}, present: map[string]bool{}}
		chk := claudeSignInImageCheck(ctx, map[string]string{"claude-code": ref}, rnr)
		if chk.Status != "warn" {
			t.Fatalf("status = %q, want warn", chk.Status)
		}
		if !strings.Contains(chk.Detail, ref) {
			t.Errorf("detail does not name the pinned ref: %q", chk.Detail)
		}
	})

	for name, rnr := range map[string]runner.Runner{
		"no ImageChecker":               &fakeRunner{},
		"a presence check errors (k8s)": imageCheckErrRunner{&fakeRunner{}},
	} {
		t.Run("pinned and "+name+" is info that says it is unverified", func(t *testing.T) {
			chk := claudeSignInImageCheck(ctx, map[string]string{"claude-code": "wardyn/agent-claude-code:local"}, rnr)
			if chk.Status != "info" {
				t.Fatalf("status = %q, want info", chk.Status)
			}
			if !strings.Contains(chk.Detail, "cannot confirm") {
				t.Errorf("detail does not disclose the check is unverified: %q", chk.Detail)
			}
		})
	}
}
