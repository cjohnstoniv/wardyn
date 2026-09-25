// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// blipSiteConfigStore fails the FIRST GetSiteConfig and answers every later one
// — a dropped pool connection, which is what a site-config read failure at
// dispatch usually is.
type blipSiteConfigStore struct {
	store.Store
	calls atomic.Int32
	cfg   types.SiteConfig
}

func (s *blipSiteConfigStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	if s.calls.Add(1) == 1 {
		return types.SiteConfig{}, errors.New("conn closed by peer")
	}
	return s.cfg, nil
}

// alwaysFailSiteConfigStore never answers.
type alwaysFailSiteConfigStore struct {
	store.Store
	calls atomic.Int32
}

func (s *alwaysFailSiteConfigStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	s.calls.Add(1)
	return types.SiteConfig{}, errors.New("pool is down")
}

// TestSiteConfigForDispatch_RetriesOnce is per owner decision 3.
//
// The dispatch-time site-config read decides WHOSE model credential a run may
// use, and a failed read is REFUSED downstream (enforceReadableRosterForCredential)
// rather than degraded to the operator namespace — the right answer for a roster
// that genuinely cannot be read, and needlessly harsh for one dropped
// connection. One retry, no delay: a dead pool answers instantly.
func TestSiteConfigForDispatch_RetriesOnce(t *testing.T) {
	h := newHarness(t)
	want := providersConfig([]types.GitProvider{githubRow(admitRowID, false, admitBaseURL)})
	st := &blipSiteConfigStore{cfg: want}
	srv := New(baseTestConfig(h, st))

	sc, err := srv.siteConfigForDispatch(context.Background())
	if err != nil {
		t.Fatalf("a single transient read failure was not retried: %v", err)
	}
	if !providersConfigured(sc) {
		t.Error("the retry returned an empty site config; the credential scope would be decided from nothing")
	}
	if got := st.calls.Load(); got != 2 {
		t.Errorf("GetSiteConfig calls = %d, want exactly 2 (one retry, never a loop)", got)
	}
}

// TestSiteConfigForDispatch_StillFailsClosed is the negative control: a
// roster that really cannot be read still surfaces as an error, so the refusal
// downstream is unchanged and a per_user member is never served the
// deployment-wide session.
func TestSiteConfigForDispatch_StillFailsClosed(t *testing.T) {
	h := newHarness(t)
	st := &alwaysFailSiteConfigStore{}
	srv := New(baseTestConfig(h, st))

	if _, err := srv.siteConfigForDispatch(context.Background()); err == nil {
		t.Fatal("an unreadable roster was reported as readable — the credential scope would fall open to the operator namespace")
	}
	if got := st.calls.Load(); got != 2 {
		t.Errorf("GetSiteConfig calls = %d, want exactly 2 — one retry, never a retry storm inside POST /runs", got)
	}
}

// TestDispatch_RefusedRosterLeavesNoBedrockCredentialInTheEnv is the
// serving-door half: when the retry does not help and the run WOULD have been
// served a Bedrock credential, the dispatch is refused with nothing resident.
func TestDispatch_RefusedRosterLeavesNoBedrockCredentialInTheEnv(t *testing.T) {
	h := newHarness(t)
	st := &mechanismGateStore{}
	cfg := bedrockBearerCfg()
	cfg.Identity, cfg.Audit, cfg.Store = h.idp, h.audit, st
	srv := New(cfg)
	run := types.AgentRun{ID: uuid.New(), Agent: "claude-code", State: types.RunStarting}
	policy := &types.RunPolicySpec{}
	sandboxEnv := map[string]string{}

	if _, ok := srv.resolveLLMInjections(context.Background(), run, dispatchParams{}, policy, sandboxEnv,
		nil, "http://wardyn-proxy:3128", artifactRedirectPlan{}, false, types.SiteConfig{}, false, false, bedrockCredUngraded()); ok {
		t.Fatal("dispatch went ahead on an unreadable roster")
	}
	for k := range sandboxEnv {
		t.Errorf("sandbox env carries %s after a refused dispatch; nothing may be resident", k)
	}
	if !st.failed {
		t.Error("the refused run was not marked FAILED")
	}
}
