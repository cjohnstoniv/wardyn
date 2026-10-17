// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// savePolicyStore accepts a policy write and hands the row back, the one thing
// the save doors need from the store here.
type savePolicyStore struct{ store.Store }

func (savePolicyStore) CreatePolicy(_ context.Context, p types.RunPolicy) (types.RunPolicy, error) {
	return p, nil
}

func (savePolicyStore) UpdatePolicy(_ context.Context, id uuid.UUID, name string, spec types.RunPolicySpec) (types.RunPolicy, error) {
	return types.RunPolicy{ID: id, Name: name, Spec: spec}, nil
}

func policiesRequirementsServer(t *testing.T) *Server {
	t.Helper()
	h := newHarness(t)
	cfg := baseTestConfig(h, savePolicyStore{})
	// The operator holds "stripe-key" and "deploy-pat"; the other names are missing.
	cfg.Secrets = &memSecrets{m: map[string][]byte{"stripe-key": []byte("v"), "deploy-pat": []byte("v")}}
	return New(cfg)
}

const allKindsSpec = `{"min_confinement_class":"CC2","allowed_domains":["api.stripe.com"],"eligible_grants":[` +
	`{"kind":"api_key","scope":{"host":"api.stripe.com","secret_name":"stripe-key"}},` +
	`{"kind":"git_pat","scope":{"host":"github.com","secret_name":"deploy-pat"}},` +
	`{"kind":"ssh_key","scope":{"host":"github.com","key_secret_ref":"deploy-ssh","known_hosts_secret_ref":"deploy-hosts"}},` +
	`{"kind":"env_secret","scope":{"name":"CORP_TOKEN","secret_name":"corp-token"}},` +
	`{"kind":"file_secret","scope":{"file":"creds","secret_name":"stripe-key"}}]}`

func savedPolicy(t *testing.T, w interface {
	Bytes() []byte
}) client.PolicySaved {
	t.Helper()
	var got client.PolicySaved
	if err := json.Unmarshal(w.Bytes(), &got); err != nil {
		t.Fatalf("decode %q: %v", w.Bytes(), err)
	}
	return got
}

func requirementStatuses(reqs []client.ComponentRequirement) map[string]string {
	out := map[string]string{}
	for _, r := range reqs {
		out[r.Name] = r.Status
	}
	return out
}

// TestPoliciesRequirements_CreateAndUpdateReportEveryGrantKind: both policy save
// doors list the stored secret each api_key, git_pat, ssh_key, env_secret and
// file_secret grant names, once each, present or missing in the saver's
// namespace, beside the stored policy, which still decodes as a policy.
func TestPoliciesRequirements_CreateAndUpdateReportEveryGrantKind(t *testing.T) {
	srv := policiesRequirementsServer(t)
	body := `{"name":"payments","spec":` + allKindsSpec + `}`
	want := map[string]string{
		"stripe-key": "present", "deploy-pat": "present",
		"deploy-ssh": "missing", "deploy-hosts": "missing", "corp-token": "missing",
	}
	for _, door := range []struct {
		method, path string
		code         int
	}{
		{http.MethodPost, "/api/v1/policies", http.StatusCreated},
		{http.MethodPut, "/api/v1/policies/" + uuid.NewString(), http.StatusOK},
	} {
		w := do(t, srv, door.method, door.path, adminToken, body)
		if w.Code != door.code {
			t.Fatalf("%s %s = %d: %s", door.method, door.path, w.Code, w.Body.String())
		}
		saved := savedPolicy(t, w.Body)
		if saved.Name != "payments" || saved.Spec.MinConfinementClass != types.CC2 {
			t.Errorf("%s: the stored policy is no longer readable beside the list: %+v", door.method, saved.RunPolicy)
		}
		got := requirementStatuses(saved.Requirements)
		if len(saved.Requirements) != len(want) {
			t.Fatalf("%s: requirements = %+v, want %d rows, stripe-key once", door.method, saved.Requirements, len(want))
		}
		for name, status := range want {
			if got[name] != status {
				t.Errorf("%s: %s = %q, want %q (all: %+v)", door.method, name, got[name], status, saved.Requirements)
			}
		}
		for _, r := range saved.Requirements {
			if (r.Status == "missing") != (r.Fix == "add_secret") || r.Kind != "secret" {
				t.Errorf("%s: row %+v: a missing secret carries the add_secret fix, a present one none", door.method, r)
			}
		}
	}
}

// TestPoliciesRequirements_NoSecretIsAnEmptyList: a policy naming no stored
// secret answers [] and never null, the shape the console can iterate.
func TestPoliciesRequirements_NoSecretIsAnEmptyList(t *testing.T) {
	srv := policiesRequirementsServer(t)
	w := do(t, srv, http.MethodPost, "/api/v1/policies", adminToken, `{"name":"plain","spec":{"min_confinement_class":"CC2"}}`)
	if w.Code != http.StatusCreated || !strings.Contains(w.Body.String(), `"requirements":[]`) {
		t.Fatalf("= %d %s, want 201 with requirements as []", w.Code, w.Body.String())
	}
}

// TestPoliciesRequirements_ReservedNameIsRefusedAndNeverListed (G-3): a name
// the sinks reserve is refused at save for every grant kind that names a secret,
// by both doors, and the refusal carries no requirements list.
func TestPoliciesRequirements_ReservedNameIsRefusedAndNeverListed(t *testing.T) {
	srv := policiesRequirementsServer(t)
	grant := map[string]func(string) string{
		"api_key": func(n string) string {
			return `{"kind":"api_key","scope":{"host":"api.stripe.com","secret_name":"` + n + `"}}`
		},
		"git_pat": func(n string) string {
			return `{"kind":"git_pat","scope":{"host":"github.com","secret_name":"` + n + `"}}`
		},
		"ssh_key": func(n string) string {
			return `{"kind":"ssh_key","scope":{"host":"github.com","key_secret_ref":"` + n + `"}}`
		},
		"env_secret": func(n string) string {
			return `{"kind":"env_secret","scope":{"name":"CORP_TOKEN","secret_name":"` + n + `"}}`
		},
		"file_secret": func(n string) string {
			return `{"kind":"file_secret","scope":{"file":"creds","secret_name":"` + n + `"}}`
		},
	}
	names := []string{
		bedrockAccessKeyIDSecret, bedrockSecretAccessKeySecret, bedrockSessionTokenSecret,
		types.SubscriptionOAuthSecret, types.ManagedOAuthSecret,
	}
	for kind, mk := range grant {
		for _, name := range names {
			if !sinkReservedSecret(name) {
				t.Fatalf("%q is not sinkReserved: the test names the wrong set", name)
			}
			body := `{"name":"p","spec":{"min_confinement_class":"CC2","allowed_domains":["api.stripe.com"],"eligible_grants":[` + mk(name) + `]}}`
			for _, door := range []struct{ method, path string }{
				{http.MethodPost, "/api/v1/policies"},
				{http.MethodPut, "/api/v1/policies/" + uuid.NewString()},
			} {
				w := do(t, srv, door.method, door.path, adminToken, body)
				if w.Code != http.StatusBadRequest && w.Code != http.StatusUnprocessableEntity {
					t.Errorf("%s %s naming %q via %s = %d %s, want a refusal", kind, door.method, name, door.path, w.Code, w.Body.String())
				}
				if strings.Contains(w.Body.String(), "requirements") {
					t.Errorf("%s %s naming %q: the refusal body carries a requirements list: %s", kind, door.method, name, w.Body.String())
				}
			}
		}
	}
}

// TestPoliciesRequirements_ASecretStoredBetweenSavesReadsPresent: the list is
// read at each save, so the second save sees what the admin added after the
// first.
func TestPoliciesRequirements_ASecretStoredBetweenSavesReadsPresent(t *testing.T) {
	srv := policiesRequirementsServer(t)
	body := `{"name":"p","spec":{"min_confinement_class":"CC2","eligible_grants":[{"kind":"env_secret","scope":{"name":"CORP_TOKEN","secret_name":"corp-token"}}]}}`
	first := savedPolicy(t, do(t, srv, http.MethodPost, "/api/v1/policies", adminToken, body).Body)
	if got := requirementStatuses(first.Requirements); got["corp-token"] != "missing" {
		t.Fatalf("first save = %+v, want corp-token missing", first.Requirements)
	}
	srv.cfg.Secrets.(*memSecrets).m["corp-token"] = []byte("v")
	second := savedPolicy(t, do(t, srv, http.MethodPut, "/api/v1/policies/"+first.ID.String(), adminToken, body).Body)
	if got := requirementStatuses(second.Requirements); got["corp-token"] != "present" {
		t.Fatalf("second save = %+v, want corp-token present", second.Requirements)
	}
}

// TestPoliciesRequirements_UnstorableNamesAreNotListed (C19 review F3): a name
// the secrets API reserves or retires can never be stored, so an add_secret
// fix for it can never succeed. The save still accepts it; the list omits it.
func TestPoliciesRequirements_UnstorableNamesAreNotListed(t *testing.T) {
	srv := policiesRequirementsServer(t)
	reserved := types.AWSSSOAccessTokenSecret
	retired := "anthropic-api-key"
	if !secretsAPIReserved(reserved) || sinkReservedSecret(reserved) || !slices.Contains(retiredModelCredentialNames, retired) || sinkReservedSecret(retired) {
		t.Fatalf("the test names the wrong set: reserved=%v/%v retired=%v/%v", secretsAPIReserved(reserved), sinkReservedSecret(reserved),
			slices.Contains(retiredModelCredentialNames, retired), sinkReservedSecret(retired))
	}
	body := `{"name":"p","spec":{"min_confinement_class":"CC2","allowed_domains":["api.stripe.com"],"eligible_grants":[` +
		`{"kind":"api_key","scope":{"host":"api.stripe.com","secret_name":"` + reserved + `"}},` +
		`{"kind":"api_key","scope":{"host":"api.stripe.com","secret_name":"` + retired + `"}},` +
		`{"kind":"env_secret","scope":{"name":"CORP_TOKEN","secret_name":"corp-token"}}]}}`
	w := do(t, srv, http.MethodPost, "/api/v1/policies", adminToken, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("save = %d: %s, want the policy still accepted", w.Code, w.Body.String())
	}
	got := requirementStatuses(savedPolicy(t, w.Body).Requirements)
	if len(got) != 1 || got["corp-token"] != "missing" {
		t.Fatalf("requirements = %+v, want only corp-token", got)
	}
}
