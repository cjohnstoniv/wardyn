// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
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

	for name, tc := range map[string]struct {
		rnr  runner.Runner
		want string
	}{
		"no ImageChecker": {&fakeRunner{}, "cannot confirm"},
		// The check itself failed (k8s, or an unreachable Docker daemon): the
		// row names that failure, not a runner that can never check.
		"a presence check errors": {imageCheckErrRunner{&fakeRunner{}}, "presence check could not answer: orchestrator:"},
	} {
		t.Run("pinned and "+name+" is info that says it is unverified", func(t *testing.T) {
			chk := claudeSignInImageCheck(ctx, map[string]string{"claude-code": "wardyn/agent-claude-code:local"}, tc.rnr)
			if chk.Status != "info" {
				t.Fatalf("status = %q, want info", chk.Status)
			}
			if !strings.Contains(chk.Detail, tc.want) {
				t.Errorf("detail = %q, want it to contain %q", chk.Detail, tc.want)
			}
		})
	}
}

// wedgedImageRunner answers no image question until its context ends: a dockerd that
// accepts the connection and never replies.
type wedgedImageRunner struct{ *fakeRunner }

func (wedgedImageRunner) ImagePresent(ctx context.Context, _ string) (bool, error) {
	<-ctx.Done()
	return false, ctx.Err()
}

func (wedgedImageRunner) DescribeImage(ctx context.Context, _ string) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}

// /setup/status polls this check on every console tab. A wedged daemon must cost it
// storePingTimeout and then the inconclusive (fail-open) arm, never the request's life.
func TestSetupStatusImageProbesAreBoundedAgainstAWedgedDaemon(t *testing.T) {
	prev := storePingTimeout
	storePingTimeout = 100 * time.Millisecond
	t.Cleanup(func() { storePingTimeout = prev })
	// The request's own context is the unbounded one a client that never hangs up carries.
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	rnr := wedgedImageRunner{&fakeRunner{}}

	start := time.Now()
	chk := claudeSignInImageCheck(ctx, map[string]string{"claude-code": "wardyn/agent-claude-code:local"}, rnr)
	if took := time.Since(start); took > 4*time.Second {
		t.Errorf("claudeSignInImageCheck took %s on a wedged daemon; want about storePingTimeout", took)
	}
	if chk.Status != "info" || !strings.Contains(chk.Detail, "could not answer") {
		t.Errorf("check = %+v, want the inconclusive (fail-open) row", chk)
	}

	start = time.Now()
	got := describeLocalImage(ctx, rnr.DescribeImage, "wardyn/agent-claude-code:local")
	if took := time.Since(start); took > 4*time.Second {
		t.Errorf("describeLocalImage took %s on a wedged daemon; want about storePingTimeout", took)
	}
	if !strings.Contains(got, "cannot tell") {
		t.Errorf("describeLocalImage = %q, want the cannot-tell sentence", got)
	}
}

// countingImageRunner counts the substrate presence probes /setup/status makes.
type countingImageRunner struct {
	*fakeRunner
	probes atomic.Int32
}

func (r *countingImageRunner) ImagePresent(context.Context, string) (bool, error) {
	r.probes.Add(1)
	return true, nil
}

// A member's checks are discarded, so their poll must not wait on the Docker daemon
// for a row nobody reads; an operator's still carries the row.
func TestSetupStatus_ClaudeSignInImageProbeIsOperatorOnly(t *testing.T) {
	h := newHarness(t)
	cfg := baseTestConfig(h, &integStore{govEscapeStore: newGovEscapeStore(&capStore{})})
	rnr := &countingImageRunner{fakeRunner: &fakeRunner{}}
	cfg.Runner = rnr
	cfg.AgentImages = map[string]string{"claude-code": "wardyn/agent-claude-code:local"}
	cfg.Secrets = &memSecrets{m: map[string][]byte{}}
	cfg.OIDC = &oidc.Authenticator{}
	srv := New(cfg)

	code, st := decodeSetupSSO(t, srv, ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin))
	if code != http.StatusOK {
		t.Fatalf("admin GET /setup/status: code = %d", code)
	}
	if rnr.probes.Load() == 0 || !slices.ContainsFunc(st.Checks, func(c SetupCheck) bool { return c.ID == "claude_signin_image" }) {
		t.Fatalf("the operator's status lost the claude_signin_image row (probes=%d)", rnr.probes.Load())
	}
	rnr.probes.Store(0)
	if code, _ := decodeSetupSSO(t, srv, ssoSession(t, "sub-member", "member@corp.example", oidc.RoleUser)); code != http.StatusOK {
		t.Fatalf("member GET /setup/status: code = %d", code)
	}
	if n := rnr.probes.Load(); n != 0 {
		t.Errorf("a member's poll made %d image presence probe(s); it must not wait on the substrate", n)
	}
}

// slowImageRunner answers after a delay longer than the setup page's bound.
type slowImageRunner struct {
	*fakeRunner
	delay time.Duration
}

func (r slowImageRunner) ImagePresent(ctx context.Context, _ string) (bool, error) {
	select {
	case <-time.After(r.delay):
		return false, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

// The setup page's bound must not reach the write-time refusal (E4): a slow but
// healthy daemon that answers "not present" still refuses the write, as before.
func TestClaudeSignInImageResolves_WriteTimeCheckIsNotBounded(t *testing.T) {
	prev := storePingTimeout
	storePingTimeout = 50 * time.Millisecond
	t.Cleanup(func() { storePingTimeout = prev })
	rnr := slowImageRunner{&fakeRunner{}, 300 * time.Millisecond}
	if claudeSignInImageResolves(context.Background(), map[string]string{"claude-code": "wardyn/agent-claude-code:local"}, rnr) {
		t.Fatal("a slow daemon that answers not-present must still refuse the write; the setup bound failed it open")
	}
}
