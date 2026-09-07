// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestBrokeredForgePATIsWithheldFromBothHalvesOfDispatch (F216-R1) pins the two
// halves of dispatch to ONE answer to "which PAT may this run use for this
// forge?".
//
// A git_pat grant for a forge the run is BROKERED for is withheld from the
// sandbox env and audited as withheld (dropBrokeredGrants, run.git_pat.
// brokered_forge) precisely because the git-broker route is meant to be the
// only route to that forge. ProxyConfig.PATGrants was built from the UNFILTERED
// map, so the proxy's /wardyn/git/ allowlist still published that same host:
// dispatch withheld the credential on one half and granted a route to it on the
// other. Nothing in this package closed that; the only thing that did was
// brokeredForgeMintKind's refusal in the mint handler — a guard one layer down
// standing in for a computation that belongs at dispatch, and the kind of
// single remaining barrier the ssh_key/git_pat withholding exists because of.
//
// It drives dispatchRun rather than the helpers for the reason the sibling flag
// test states: the helpers were each green on their own while the LANE was the
// thing that disagreed with itself.
func TestBrokeredForgePATIsWithheldFromBothHalvesOfDispatch(t *testing.T) {
	const brokeredForge = "github.com" // in gitBrokerForges: this run is brokered for it
	const otherForge = "gitlab.corp.io"

	fr := &fakeRunner{}
	srv, _, _, run := dispatchTeardownFixture(t, fr, types.RunPending)
	run.Task = "" // composition only: no agent exec, no completion watcher
	srv.dispatchRun(context.Background(), run, ceilingForDispatch(governanceCeiling{}), dispatchParams{
		RunToken: "run-token", Image: "wardyn/claude-code:latest",
		// A github_token grant for a repo on the brokered forge is what makes the
		// run brokered for it (the same map confineGitBrokerEgress keys on).
		GitGrants: map[string]uuid.UUID{"org/repo": uuid.New()},
		GitPATGrants: map[string]string{
			brokeredForge: uuid.NewString(),
			otherForge:    uuid.NewString(),
		},
	})
	spec := fr.lastSpec

	if _, ok := spec.ProxyConfig.PATGrants[brokeredForge]; ok {
		t.Errorf("ProxyConfig.PATGrants still carries %q: %v\n"+
			"dispatch withheld that PAT from the sandbox and audited it as withheld, then published a brokered "+
			"route to the same forge — the proxy half must be built from dropBrokeredGrants' kept map, not the raw one",
			brokeredForge, spec.ProxyConfig.PATGrants)
	}
	// The withholding is per HOST, not per run: a non-brokered forge's PAT is
	// untouched, or this "fix" would have broken every mixed-forge run.
	if _, ok := spec.ProxyConfig.PATGrants[otherForge]; !ok {
		t.Errorf("ProxyConfig.PATGrants = %v, want %q still brokered proxy-side — only the BROKERED forge is withheld",
			spec.ProxyConfig.PATGrants, otherForge)
	}
	// The sandbox half is unchanged and still says the same thing.
	if got := spec.Env["WARDYN_GIT_PAT_GRANTS"]; strings.Contains(got, brokeredForge) {
		t.Errorf("WARDYN_GIT_PAT_GRANTS = %q, want no %q entry", got, brokeredForge)
	}
	if got := spec.Env["WARDYN_GIT_PAT_BROKER_HOSTS"]; strings.Contains(got, brokeredForge) {
		t.Errorf("WARDYN_GIT_PAT_BROKER_HOSTS = %q, want no %q entry — the sandbox must not be told to route a "+
			"withheld forge through the broker either", got, brokeredForge)
	}
}
