// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/ed25519"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/runneridentity"
	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// savedIdentity writes a runner identity bound to url into a fresh state directory.
func savedIdentity(t *testing.T, id uuid.UUID, url string) string {
	t.Helper()
	state := filepath.Join(t.TempDir(), "runner")
	key, err := runneridentity.Generate(state)
	if err != nil {
		t.Fatal(err)
	}
	fp := runnerwire.Fingerprint(key.Public().(ed25519.PublicKey))
	if err := runneridentity.Save(state, runneridentity.Identity{RunnerID: id, PrivateKey: key, Fingerprint: fp, OrgURLSHA256: federation.OrgURLSHA256(url)}); err != nil {
		t.Fatal(err)
	}
	return state
}

// claim --pool puts the runner it just claimed in the person's own pool of that name, makes the pool
// first when there is none, and --default makes it the person's default.
func TestRunnerClaimPutsTheRunnerInAPool(t *testing.T) {
	id, existing, created := uuid.New(), uuid.New(), uuid.New()
	claimed := types.RunnerRegistration{RunnerID: id, State: types.RunnerClaimed}
	for _, c := range []struct {
		name     string
		routes   map[string]any
		pool     string
		wantPool uuid.UUID
		wantMake bool
	}{
		{"new pool", map[string]any{
			"GET /api/v1/runner-pools":     sdk.RunnerPoolList{},
			"POST /api/v1/me/runner-pools": sdk.RunnerPool{ID: created, Name: "Laptop", Scope: sdk.RunnerPoolUser},
		}, "Laptop", created, true},
		{"existing pool", map[string]any{
			"GET /api/v1/runner-pools": sdk.RunnerPoolList{Pools: []sdk.RunnerPoolCatalogueEntry{
				{RunnerPoolChoice: sdk.RunnerPoolChoice{ID: uuid.New(), Name: "Laptop", Scope: sdk.RunnerPoolOrganization}},
				// an administrator's list also carries another person's pool of the same name, as a manage-only row
				{RunnerPoolChoice: sdk.RunnerPoolChoice{ID: uuid.New(), Name: "Laptop", Scope: sdk.RunnerPoolUser, Availability: sdk.RunnerPoolUnavailable, Reason: runnerpool.ReasonNotFound}, Owner: "bob"},
				{RunnerPoolChoice: sdk.RunnerPoolChoice{ID: existing, Name: "Laptop", Scope: sdk.RunnerPoolUser}}}},
			"GET /api/v1/runner-pools/" + existing.String():              sdk.RunnerPool{ID: existing, Name: "Laptop", Scope: sdk.RunnerPoolUser},
			"GET /api/v1/runner-pools/" + existing.String() + "/members": []sdk.RunnerPoolMemberView{},
		}, "Laptop", existing, false},
	} {
		routes := c.routes
		routes["POST /api/v1/me/runners/"+id.String()+"/claim"] = claimed
		routes["GET /api/v1/me/runners"] = []sdk.RunnerView{{ID: id, Name: "laptop-7", State: types.RunnerClaimed}}
		routes["PUT /api/v1/me/runner-pools/"+c.wantPool.String()+"/runners/"+id.String()] = sdk.RunnerPoolMember{PoolID: c.wantPool}
		routes["GET /api/v1/me/runner-pool-defaults"] = sdk.RunnerPoolDefaults{}
		routes["PUT /api/v1/me/runner-pool-defaults"] = sdk.RunnerPoolDefaults{}
		srv := newPoolRoutes(t, routes)
		state := savedIdentity(t, id, srv.URL)
		out, err := execCmdCapture(t, "runner", "claim", "--state-dir", state, "--pool", c.pool, "--default", "--url", srv.URL, "--token", "t")
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !srv.sent(http.MethodPut, "/api/v1/me/runner-pools/"+c.wantPool.String()+"/runners/"+id.String()) ||
			!srv.sent(http.MethodPut, "/api/v1/me/runner-pool-defaults") ||
			srv.sent(http.MethodPost, "/api/v1/me/runner-pools") != (c.wantPool == created) ||
			!strings.Contains(out, "added to Laptop") || !strings.Contains(out, "default user pool set to Laptop") {
			t.Errorf("%s: sent %+v and printed %q", c.name, srv.reqs, out)
		}
	}
}

// A refused pool step leaves the claim done and says so; --default alone is refused before the claim.
func TestRunnerClaimPoolFailureAndFlagRules(t *testing.T) {
	id := uuid.New()
	srv := newPoolRoutes(t, map[string]any{
		"POST /api/v1/me/runners/" + id.String() + "/claim": types.RunnerRegistration{RunnerID: id, State: types.RunnerClaimed},
	})
	state := savedIdentity(t, id, srv.URL)
	err := execCmd(t, "runner", "claim", "--state-dir", state, "--pool", "Laptop", "--url", srv.URL, "--token", "t")
	if err == nil || !strings.Contains(err.Error(), "the runner is claimed but is not in pool") {
		t.Fatalf("a refused pool step = %v, want a sentence that the claim stands", err)
	}
	n := len(srv.reqs)
	if err := execCmd(t, "runner", "claim", "--state-dir", state, "--default", "--url", srv.URL, "--token", "t"); err == nil || len(srv.reqs) != n {
		t.Fatalf("--default without --pool = %v after %d new requests, want a refusal before any", err, len(srv.reqs)-n)
	}
}

// With only another person's same-named pool in the list, the person's own pool is made, not theirs used.
func TestRunnerClaimPoolPassesOverAnotherPersonsPool(t *testing.T) {
	id, theirs, created := uuid.New(), uuid.New(), uuid.New()
	srv := newPoolRoutes(t, map[string]any{
		"POST /api/v1/me/runners/" + id.String() + "/claim": types.RunnerRegistration{RunnerID: id, State: types.RunnerClaimed},
		"GET /api/v1/runner-pools": sdk.RunnerPoolList{Pools: []sdk.RunnerPoolCatalogueEntry{
			{RunnerPoolChoice: sdk.RunnerPoolChoice{ID: theirs, Name: "Laptop", Scope: sdk.RunnerPoolUser, Availability: sdk.RunnerPoolUnavailable, Reason: runnerpool.ReasonNotFound}, Owner: "bob"}}},
		"POST /api/v1/me/runner-pools": sdk.RunnerPool{ID: created, Name: "Laptop", Scope: sdk.RunnerPoolUser},
		"GET /api/v1/me/runners":       []sdk.RunnerView{{ID: id, State: types.RunnerClaimed}},
		"PUT *":                        sdk.RunnerPoolMember{PoolID: created},
	})
	state := savedIdentity(t, id, srv.URL)
	if _, err := execCmdCapture(t, "runner", "claim", "--state-dir", state, "--pool", "Laptop", "--url", srv.URL, "--token", "t"); err != nil {
		t.Fatal(err)
	}
	if !srv.sent(http.MethodPost, "/api/v1/me/runner-pools") || srv.sent(http.MethodPut, "/api/v1/me/runner-pools/"+theirs.String()+"/runners/"+id.String()) ||
		!srv.sent(http.MethodPut, "/api/v1/me/runner-pools/"+created.String()+"/runners/"+id.String()) {
		t.Fatalf("sent %+v", srv.reqs)
	}
}
