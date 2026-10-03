// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func patGrantRow(id, runID uuid.UUID, host string) types.CredentialGrant {
	return types.CredentialGrant{ID: id, RunID: runID, Spec: types.GrantSpec{
		Kind:  types.GrantGitPAT,
		Scope: mustJSON(map[string]any{"host": host, "secret_ref": "pat-" + host}),
	}}
}

func sortedIDs(ids []uuid.UUID) []uuid.UUID {
	out := slices.Clone(ids)
	slices.SortFunc(out, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	return out
}

// TestDispatch_BrokeredPATGrantIDsAreEveryGitPATRow pins the set to the run's
// stored git_pat rows, not to the per-host maps dispatch narrows. The rows here
// are the ones those maps drop or never hold:
//
//   - two grants on one host, one of which shadows the other (both list orders);
//   - a grant whose lane was vetoed;
//   - a grant withheld because the run is brokered for that forge;
//   - an Azure DevOps grant that is owner-only.
//
// A set built from PATGrants or GitPATGrants passes a one-grant test and misses
// every one of these.
func TestDispatch_BrokeredPATGrantIDsAreEveryGitPATRow(t *testing.T) {
	const brokeredForge = "github.com" // withheld: the run is brokered for it
	shadowed, shadowing := uuid.New(), uuid.New()
	vetoed, withheld, adoOwnerOnly := uuid.New(), uuid.New(), uuid.New()
	apiKey := uuid.New()

	for _, tc := range []struct {
		name  string
		order func(rows []types.CredentialGrant) []types.CredentialGrant
	}{
		{"as listed", func(r []types.CredentialGrant) []types.CredentialGrant { return r }},
		{"reversed", func(r []types.CredentialGrant) []types.CredentialGrant {
			r = slices.Clone(r)
			slices.Reverse(r)
			return r
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fr := &fakeRunner{}
			srv, st, _, run := dispatchTeardownFixture(t, fr, types.RunPending)
			run.Task = "" // composition only: no agent exec, no completion watcher
			st.grants = tc.order([]types.CredentialGrant{
				patGrantRow(shadowed, run.ID, "gitlab.corp.io"),
				patGrantRow(shadowing, run.ID, "gitlab.corp.io"),
				patGrantRow(vetoed, run.ID, "vetoed.corp.io"),
				patGrantRow(withheld, run.ID, brokeredForge),
				patGrantRow(adoOwnerOnly, run.ID, "dev.azure.com"),
				{ID: apiKey, RunID: run.ID, Spec: types.GrantSpec{Kind: types.GrantAPIKey}},
			})
			srv.dispatchRun(context.Background(), run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), dispatchParams{
				RunToken: "run-token", Image: "wardyn/claude-code:latest",
				GitGrants: map[string]uuid.UUID{"org/repo": uuid.New()},
				// Only the shadowing grant and the withheld one reach the map; the
				// shadowed, vetoed and owner-only rows never do.
				GitPATGrants: map[string]string{
					"gitlab.corp.io": shadowing.String(),
					brokeredForge:    withheld.String(),
				},
			})

			got := sortedIDs(fr.lastSpec.ProxyConfig.BrokeredPATGrantIDs)
			want := sortedIDs([]uuid.UUID{shadowed, shadowing, vetoed, withheld, adoOwnerOnly})
			if !slices.Equal(got, want) {
				t.Fatalf("BrokeredPATGrantIDs = %v, want every git_pat row %v (and no api_key row)", got, want)
			}
		})
	}
}

// With the broker off the PAT is minted into the sandbox on purpose, so the set
// stays empty and the proxy relays every mint.
func TestDispatch_BrokerOffCarriesNoBrokeredPATGrantIDs(t *testing.T) {
	fr := &fakeRunner{}
	srv, st, _, run := dispatchTeardownFixture(t, fr, types.RunPending)
	run.Task = ""
	srv.cfg.DisableGitPATBroker = true
	st.grants = []types.CredentialGrant{patGrantRow(uuid.New(), run.ID, "gitlab.corp.io")}
	srv.dispatchRun(context.Background(), run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), dispatchParams{
		RunToken: "run-token", Image: "wardyn/claude-code:latest",
		GitPATGrants: map[string]string{"gitlab.corp.io": st.grants[0].ID.String()},
	})
	if fr.createCalls != 1 {
		t.Fatalf("createCalls = %d, want the run dispatched", fr.createCalls)
	}
	if got := fr.lastSpec.ProxyConfig.BrokeredPATGrantIDs; len(got) != 0 {
		t.Fatalf("BrokeredPATGrantIDs = %v with the broker off, want none", got)
	}
}

// A grant list that cannot be read with the broker on fails the dispatch: with
// no set the proxy would relay a raw mint of a PAT the run holds.
func TestDispatch_GrantListFailureFailsTheDispatch(t *testing.T) {
	fr := &fakeRunner{}
	srv, st, _, run := dispatchTeardownFixture(t, fr, types.RunPending)
	run.Task = ""
	st.grantsErr = errors.New("store: connection reset by peer")
	srv.dispatchRun(context.Background(), run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), dispatchParams{
		RunToken: "run-token", Image: "wardyn/claude-code:latest",
	})
	if fr.createCalls != 0 {
		t.Fatalf("createCalls = %d, want no sandbox after a grant-list failure", fr.createCalls)
	}
	if got := st.State(); got != types.RunFailed {
		t.Fatalf("run state = %s, want %s", got, types.RunFailed)
	}
}

func storedBrokeredPATConfig(t *testing.T, f *reviveFixture) (patHostGrant uuid.UUID) {
	t.Helper()
	cfg, err := proxy.LoadConfigBytes(f.rs.cfg)
	if err != nil {
		t.Fatal(err)
	}
	return cfg.PATGrants["pat.example"].GrantID
}

// A revive drops the lane of a host the owner's profile now denies, and must
// keep that grant's id in the set: the sandbox can still name it at the mint
// relay.
func TestReviveRun_DroppedPATLaneKeepsItsGrantIDInTheSet(t *testing.T) {
	f := newReviveFixture(t)
	denied := storedBrokeredPATConfig(t, f)
	kept := uuid.New()
	f.rs.credGrants = []types.CredentialGrant{
		patGrantRow(denied, f.run.ID, "pat.example"),
		patGrantRow(kept, f.run.ID, "git.example"),
	}
	if code := f.revive(t); code != http.StatusOK {
		t.Fatalf("revive: code %d, want 200", code)
	}
	cfg := f.newConfig(t)
	if _, ok := cfg.PATGrants["pat.example"]; ok {
		t.Fatal("fixture: the denied host's lane survived the revive")
	}
	if got, want := sortedIDs(cfg.BrokeredPATGrantIDs), sortedIDs([]uuid.UUID{denied, kept}); !slices.Equal(got, want) {
		t.Fatalf("BrokeredPATGrantIDs = %v, want %v: dropping a lane must not drop its id", got, want)
	}
}

// A config rendered under 0.8.5 carries no brokered_pat_grant_ids. The revive
// recomputes the set from the run's grants, and refuses when it cannot.
func TestReviveRun_PreUpgradeConfigGetsTheSetRecomputed(t *testing.T) {
	t.Run("broker on", func(t *testing.T) {
		f := newReviveFixture(t)
		stored, err := proxy.LoadConfigBytes(f.rs.cfg)
		if err != nil || len(stored.BrokeredPATGrantIDs) != 0 {
			t.Fatalf("fixture: stored config must predate the field (err %v, ids %v)", err, stored.BrokeredPATGrantIDs)
		}
		a, b := uuid.New(), uuid.New()
		f.rs.credGrants = []types.CredentialGrant{
			patGrantRow(a, f.run.ID, "pat.example"),
			patGrantRow(b, f.run.ID, "other.example"),
			{ID: uuid.New(), RunID: f.run.ID, Spec: types.GrantSpec{Kind: types.GrantAPIKey}},
		}
		if code := f.revive(t); code != http.StatusOK {
			t.Fatalf("revive: code %d, want 200", code)
		}
		if got, want := sortedIDs(f.newConfig(t).BrokeredPATGrantIDs), sortedIDs([]uuid.UUID{a, b}); !slices.Equal(got, want) {
			t.Fatalf("BrokeredPATGrantIDs = %v, want every git_pat id %v", got, want)
		}
	})

	t.Run("grant list failure refuses the revive", func(t *testing.T) {
		f := newReviveFixture(t)
		f.rs.credGrantsErr = errors.New("store: connection reset by peer")
		w := do(t, f.srv, http.MethodPost, "/api/v1/runs/"+f.run.ID.String()+"/revive", adminToken, "")
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("revive = %d, want 503 (body %s)", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "git_pat grants") {
			t.Fatalf("body %s does not name the git_pat grant read as the refusal", w.Body.String())
		}
		if len(f.rr.replaced) != 0 {
			t.Fatalf("ReplaceProxy calls = %d, want none", len(f.rr.replaced))
		}
	})

	t.Run("broker off", func(t *testing.T) {
		f := newReviveFixture(t)
		f.srv.cfg.DisableGitPATBroker = true
		f.rs.credGrants = []types.CredentialGrant{patGrantRow(uuid.New(), f.run.ID, "pat.example")}
		if code := f.revive(t); code != http.StatusOK {
			t.Fatalf("revive: code %d, want 200", code)
		}
		if got := f.newConfig(t).BrokeredPATGrantIDs; len(got) != 0 {
			t.Fatalf("BrokeredPATGrantIDs = %v with the broker off, want none", got)
		}
	})
}
