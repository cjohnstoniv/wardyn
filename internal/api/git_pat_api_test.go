// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func patAPISpec(forge string) types.RunPolicySpec {
	return types.RunPolicySpec{MinConfinementClass: types.CC2, EligibleGrants: []types.GrantSpec{
		patGrantSpec(`"api":true,"forge":"` + forge + `","repos":["team/app"]`),
	}}
}

// api: true is accepted for the forges with a table, refused on the generic forge
// that has none, and on Bitbucket Server until the deployment flag is on.
func TestPolicyWriteGitPATAPI(t *testing.T) {
	for _, forge := range []string{types.PATForgeGitLab, types.PATForgeGitea} {
		if err := validatePolicySpec(patAPISpec(forge)); err != nil {
			t.Errorf("api on %s: %v, want it accepted", forge, err)
		}
	}
	if err := validatePolicySpec(patAPISpec(types.PATForgeGeneric)); err == nil || !strings.Contains(err.Error(), "generic") {
		t.Errorf("api on generic: %v, want a refusal naming the generic forge", err)
	}
	if err := validatePolicySpec(types.RunPolicySpec{MinConfinementClass: types.CC2, EligibleGrants: []types.GrantSpec{patGrantSpec(`"api":true`)}}); err == nil {
		t.Error("api with no forge (generic) was accepted")
	}

	t.Setenv(envGitPATAPIBitbucketServer, "")
	if err := validatePolicySpec(patAPISpec(types.PATForgeBitbucketServer)); err == nil || !strings.Contains(err.Error(), envGitPATAPIBitbucketServer) {
		t.Errorf("api on bitbucket_server with the flag off: %v, want a refusal naming the flag", err)
	}
	h := newHarness(t)
	w := do(t, h.srv, http.MethodPost, "/api/v1/policies", adminToken, `{"name":"bbs","spec":`+string(mustJSON(patAPISpec(types.PATForgeBitbucketServer)))+`}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), envGitPATAPIBitbucketServer) {
		t.Errorf("POST /policies = %d %s, want 400 naming the flag", w.Code, w.Body.String())
	}
	t.Setenv(envGitPATAPIBitbucketServer, "on")
	if err := validatePolicySpec(patAPISpec(types.PATForgeBitbucketServer)); err != nil {
		t.Errorf("api on bitbucket_server with the flag on: %v, want it accepted", err)
	}
}

// A stored bitbucket_server API grant fails dispatch with its own reason while the
// flag is off, so turning the flag off after a write still holds; with it on, the
// run launches and carries the grant to the proxy with a per-run CA to terminate
// the host.
func TestDispatchBitbucketServerAPIFollowsTheFlag(t *testing.T) {
	rows := func(runID uuid.UUID) []types.CredentialGrant {
		return []types.CredentialGrant{patRowWith(uuid.New(), runID, "bitbucket.corp.io",
			map[string]any{"api": true, "forge": types.PATForgeBitbucketServer, "repos": []string{"TEAM/app"}})}
	}
	t.Setenv(envGitPATAPIBitbucketServer, "")
	fr, st, audit, run := dispatchWithPATRows(t, false, nil, rows, winningByHost)
	requireRefusedWith(t, fr, st, audit, run, reasonGitPATAPIForgeDisabled)

	t.Setenv(envGitPATAPIBitbucketServer, "true")
	fr, _, _, _ = dispatchWithPATRows(t, false, nil, rows, winningByHost)
	if fr.createCalls != 1 {
		t.Fatalf("createCalls = %d, want the run dispatched with the flag on", fr.createCalls)
	}
	g := fr.lastSpec.ProxyConfig.PATGrants["bitbucket.corp.io"]
	if !g.API || g.Forge != types.PATForgeBitbucketServer || g.Repos == nil {
		t.Fatalf("PATGrants = %+v, want the API grant carried with its forge and repos", fr.lastSpec.ProxyConfig.PATGrants)
	}
	if fr.lastSpec.ProxyConfig.MITMCACertPEM == "" || fr.lastSpec.ProxyConfig.MITMCAKeyPEM == "" {
		t.Fatal("a run with an API grant has no per-run MITM CA, so the proxy could not terminate the host")
	}
}

// A run with no API grant is not given a CA on that account.
func TestDispatchWithoutAPIGrantNeedsNoCA(t *testing.T) {
	fr, _, _, _ := dispatchWithPATRows(t, false, nil, func(runID uuid.UUID) []types.CredentialGrant {
		return []types.CredentialGrant{patRowWith(uuid.New(), runID, "gitlab.corp.io", map[string]any{"repos": []string{"team/app"}, "forge": "gitlab"})}
	}, winningByHost)
	if fr.createCalls != 1 || fr.lastSpec.ProxyConfig.MITMCACertPEM != "" {
		t.Fatalf("createCalls = %d, MITM CA %q, want a dispatched run with no CA", fr.createCalls, fr.lastSpec.ProxyConfig.MITMCACertPEM)
	}
	g := fr.lastSpec.ProxyConfig.PATGrants["gitlab.corp.io"]
	if g.API {
		t.Fatalf("PATGrants = %+v, want api off", g)
	}
}

// An API grant on a host another lane serves is refused like any narrowing there.
func TestDispatchRefusesAPIOnAHostAnotherLaneServes(t *testing.T) {
	fr, st, audit, run := dispatchWithPATRows(t, false, nil, func(runID uuid.UUID) []types.CredentialGrant {
		return []types.CredentialGrant{patRowWith(uuid.New(), runID, "dev.azure.com", map[string]any{"api": true, "forge": "gitlab"})}
	}, winningByHost)
	requireRefusedWith(t, fr, st, audit, run, reasonGitPATNarrowingUnsupportedHost)
}

func TestPATNarrowingRefusalNamesTheDisabledForge(t *testing.T) {
	grants := []types.GrantSpec{{Kind: types.GrantGitPAT, Scope: json.RawMessage(
		`{"host":"bitbucket.corp.io","secret_name":"pat","api":true,"forge":"bitbucket_server"}`)}}
	if reason, _ := patNarrowingRefusal(grants, patNarrowingEnv{brokerOn: true}); reason != reasonGitPATAPIForgeDisabled {
		t.Fatalf("reason = %q, want %q", reason, reasonGitPATAPIForgeDisabled)
	}
	if reason, detail := patNarrowingRefusal(grants, patNarrowingEnv{brokerOn: true, bbsAPI: true}); reason != "" {
		t.Fatalf("reason = %q (%s), want it accepted with the flag on", reason, detail)
	}
	// The broker comes first: with it off nothing is enforced at all.
	if reason, _ := patNarrowingRefusal(grants, patNarrowingEnv{}); reason != reasonGitPATNarrowingNeedsBroker {
		t.Fatalf("reason = %q, want %q", reason, reasonGitPATNarrowingNeedsBroker)
	}
}
