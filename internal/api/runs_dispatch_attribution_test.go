// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// attrStore serves the run's profile to runProfile, which the shared fake store
// answers with none.
type attrStore struct {
	*govEscapeStore
	profiles []types.GovernanceProfile
}

func (s *attrStore) ListGovernanceProfiles(context.Context) ([]types.GovernanceProfile, error) {
	return s.profiles, nil
}

// GetGovernanceProfileChain is the run's profile alone: each one here is standalone.
func (s *attrStore) GetGovernanceProfileChain(_ context.Context, id uuid.UUID) ([]types.GovernanceProfile, error) {
	for _, p := range s.profiles {
		if p.ID == id {
			return []types.GovernanceProfile{p}, nil
		}
	}
	return nil, store.ErrNotFound
}

// TestDispatchWritesTheRunsAttribution: a run under a governance profile, with or
// without a contact, hands the sidecar attribution; a run under none gets the
// site's policy_help when set, and otherwise the key is absent (an older proxy
// refuses it).
func TestDispatchWritesTheRunsAttribution(t *testing.T) {
	contact := &policyref.Contact{Owner: "Platform security", Email: "sec@example.com", RequestURL: "https://help.example.com/access"}
	for _, tc := range []struct {
		name    string
		profile *types.GovernanceProfile
		contact *policyref.Contact
		site    types.SiteConfig
		want    string // "" = no attribution key at all
	}{
		{"profile with no contact", limitsProfile("leased", types.GovernanceLimits{}), nil, types.SiteConfig{},
			`"attribution":{"source":"profile","name":"leased"}`},
		{"profile with a contact", limitsProfile("leased", types.GovernanceLimits{}), contact, types.SiteConfig{},
			`"attribution":{"source":"profile","name":"leased","owner":"Platform security","email":"sec@example.com","request_url":"https://help.example.com/access"}`},
		{"a contactless profile keeps its name and borrows policy_help", limitsProfile("leased", types.GovernanceLimits{}), nil,
			types.SiteConfig{PolicyHelp: &policyref.Contact{RequestURL: "https://site.example.com/help"}},
			`"attribution":{"source":"profile","name":"leased","request_url":"https://site.example.com/help"}`},
		{"a profile's own contact wins over policy_help", limitsProfile("leased", types.GovernanceLimits{}), contact,
			types.SiteConfig{PolicyHelp: &policyref.Contact{RequestURL: "https://site.example.com/help"}},
			`"attribution":{"source":"profile","name":"leased","owner":"Platform security","email":"sec@example.com","request_url":"https://help.example.com/access"}`},
		{"no profile, policy_help set", nil, nil, types.SiteConfig{PolicyHelp: &policyref.Contact{RequestURL: "https://site.example.com/help"}},
			`"attribution":{"source":"deployment","request_url":"https://site.example.com/help"}`},
		{"no profile, no policy_help", nil, nil, types.SiteConfig{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := &capStore{}
			if tc.profile != nil {
				tc.profile.Contact = tc.contact
				cs = assignedStore(tc.profile)
			}
			srv, st := runLimitsFixture(t, cs)
			st.siteConfig = tc.site
			if tc.profile != nil {
				srv.cfg.Store = &attrStore{govEscapeStore: st, profiles: []types.GovernanceProfile{*tc.profile}}
			}
			fr := srv.cfg.Runner.(*fakeRunner)
			createdRun(t, srv, st, govSession(t, govMemberSub, []string{"eng"}, false))
			fr.waitForSandbox(t)

			raw, err := runner.BuildProxyConfig(fr.lastSpec.RunID, fr.lastSpec.ProxyConfig, runner.ProxyListenPort)
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if strings.Contains(string(raw), "attribution") {
					t.Errorf("config names attribution: %s", raw)
				}
				return
			}
			if !strings.Contains(string(raw), tc.want) {
				t.Errorf("config = %s, want it to carry %s", raw, tc.want)
			}
		})
	}
}
