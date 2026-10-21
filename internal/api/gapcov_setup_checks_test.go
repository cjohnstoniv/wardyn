// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/keydomain"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestGapCovKeyCustodyChecksSingularWording(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	rows := keyCustodyChecks(keyCustody{
		Domains: []string{"finance"},
		Changes: keydomain.Changes{Count: 1, Latest: now.Add(-30 * time.Hour), LatestBy: "admin@example.test"},
		Now:     now,
	})
	byID := map[string]SetupCheck{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	if c := byID["key_domains"]; c.Status != "ok" || c.Detail != "1 key domain, proven at boot: finance." {
		t.Errorf("one domain: %+v", c)
	}
	want := "1 key-domain assignment change in the last 30 days, the latest 1 day ago by admin@example.test. Each one moves where that person's next keys are made."
	if c := byID["key_domain_changes"]; c.Status != "warn" || c.Detail != want {
		t.Errorf("one change a day and a half ago: %+v", c)
	}
}

// When Postgres does not answer the key-custody reads, the key-custody rows are left out, not shown
// from a guess.
func TestGapCovKeyCustodyRowsAreOmittedWhenTheCountCannotBeRead(t *testing.T) {
	pool, _ := miscCovClosedPool(t)
	s := &Server{cfg: Config{
		KeyDomains:    keydomain.NewService(pool, []string{"finance"}),
		PrincipalKeys: true,
		Now:           func() time.Time { return time.Now().UTC().Truncate(time.Second) },
	}}
	if rows := s.keyCustodyRows(t.Context()); rows != nil {
		t.Fatalf("keyCustodyRows = %+v, want none when Postgres does not answer", rows)
	}
}

// The runner_limits row counts the claimed runners that take no run for want of limits, names both list
// commands and the fix, never blocks setup, and shows only to an operator.
func TestSetupStatusRunnerLimitsRow(t *testing.T) {
	srv, f, _ := newRunnerRegistrationServer(t)
	rowFor := func(cookie *http.Cookie) (SetupCheck, bool) {
		t.Helper()
		w := doSSO(t, srv, http.MethodGet, "/api/v1/setup/status", cookie, "")
		if w.Code != http.StatusOK {
			t.Fatalf("GET /setup/status = %d %s", w.Code, w.Body.String())
		}
		var st SetupStatus
		if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
			t.Fatal(err)
		}
		for _, c := range st.Checks {
			if c.ID == "runner_limits" {
				return c, true
			}
		}
		return SetupCheck{}, false
	}
	admin := ssoSession(t, "admin", "admin@corp.example", oidc.RoleAdmin)
	member := ssoSession(t, "alice", "alice@corp.example", oidc.RoleUser)
	if c, ok := rowFor(admin); ok {
		t.Fatalf("no runner lacks limits, but the row shows: %+v", c)
	}

	f.rows[uuid.New()] = types.Runner{State: types.RunnerClaimed}
	f.rows[uuid.New()] = types.Runner{State: types.RunnerClaimed}
	f.rows[uuid.New()] = types.Runner{State: types.RunnerClaimed, Limits: &types.RunnerLimits{RunsAtOnce: 2}}
	f.rows[uuid.New()] = types.Runner{State: types.RunnerUnclaimed}
	f.rows[uuid.New()] = types.Runner{State: types.RunnerRevoked}
	c, ok := rowFor(admin)
	if !ok || c.Status != "warn" || c.Blocking || !strings.HasPrefix(c.Detail, "2 runners have no limits set") {
		t.Fatalf("operator row = %+v, %v; want a non-blocking warn counting the 2 claimed runners with no limits", c, ok)
	}
	for _, want := range []string{"wardyn runner list --all --no-limits", "wardyn runner remote list --no-limits", "wardyn runner remote propose", "wardyn runner set"} {
		if !strings.Contains(c.Fix, want) {
			t.Errorf("fix lacks %q: %s", want, c.Fix)
		}
	}
	if c, ok := rowFor(member); ok {
		t.Errorf("a member sees the runner_limits row: %+v", c)
	}
}

func TestRunnerLimitsCheckWording(t *testing.T) {
	if _, ok := runnerLimitsCheck(0); ok {
		t.Error("no unset runner still produced a row")
	}
	if c, _ := runnerLimitsCheck(1); !strings.HasPrefix(c.Detail, "1 runner has no limits set") {
		t.Errorf("one runner: %q", c.Detail)
	}
}

// The credential_delivery row names every stored delivery row the server cannot build and what it can build
// for it, in an override as in the global rows; it is absent for a buildable document, never blocks, shows
// only to an operator, and changes nothing.
func TestSetupStatusCredentialDeliveryRow(t *testing.T) {
	srv, f, _ := newRunnerRegistrationServer(t)
	rowFor := func(cookie *http.Cookie) (SetupCheck, bool) {
		t.Helper()
		w := doSSO(t, srv, http.MethodGet, "/api/v1/setup/status", cookie, "")
		if w.Code != http.StatusOK {
			t.Fatalf("GET /setup/status = %d %s", w.Code, w.Body.String())
		}
		var st SetupStatus
		if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
			t.Fatal(err)
		}
		for _, c := range st.Checks {
			if c.ID == "credential_delivery" {
				return c, true
			}
		}
		return SetupCheck{}, false
	}
	admin := ssoSession(t, "admin", "admin@corp.example", oidc.RoleAdmin)
	member := ssoSession(t, "alice", "alice@corp.example", oidc.RoleUser)

	f.deliveryPolicy = json.RawMessage(`{"classes":{"api_key":{"mode":"via_org"},"ado_minted_pat":{"mode":"runner_resident"}},"by_trust":{"personal":{"classes":{"env_secret":{"mode":"runner_resident"}}}}}`)
	if c, ok := rowFor(admin); ok {
		t.Fatalf("a fully buildable document shows the row: %+v", c)
	}

	const stored = `{"classes":{"api_key":{"mode":"via_org"},"git_pat_broker":{"mode":"runner_resident"}},"by_trust":{"personal":{"classes":{"bedrock_role_credentials":{"mode":"runner_resident"}}}}}`
	f.deliveryPolicy = json.RawMessage(stored)
	c, ok := rowFor(admin)
	if !ok || c.Status != "warn" || c.Blocking {
		t.Fatalf("operator row = %+v, %v; want a non-blocking warn", c, ok)
	}
	for _, want := range []string{
		`2 stored credential delivery rows name a mode the server cannot build`,
		`class "git_pat_broker" cannot be delivered as "runner_resident" yet (the server can build: refuse)`,
		`class "by_trust.personal.bedrock_role_credentials" cannot be delivered as "runner_resident" yet (the server can build: refuse)`,
	} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("detail lacks %q: %s", want, c.Detail)
		}
	}
	if strings.Contains(c.Detail, "api_key") {
		t.Errorf("a buildable row is named: %s", c.Detail)
	}
	if string(f.deliveryPolicy) != stored {
		t.Errorf("the check rewrote the stored document: %s", f.deliveryPolicy)
	}
	if c, ok := rowFor(member); ok {
		t.Errorf("a member sees the credential_delivery row: %+v", c)
	}
}
