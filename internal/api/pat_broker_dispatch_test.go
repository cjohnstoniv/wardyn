// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// D2's load-bearing property, and the one a partial implementation would miss:
// when the never-resident lane is ON, the grant ids must NOT reach the sandbox.
//
// Leaving them in place would let the in-sandbox credential helper mint the PAT
// exactly as before — the broker would be running, the clone would work, and the
// credential would still be resident. The feature would look shipped and change
// nothing.
func TestPATBroker_WithholdsGrantsFromTheSandbox(t *testing.T) {
	run := types.AgentRun{ID: uuid.New()}
	pat := map[string]string{"gitlab.com": uuid.New().String()}

	t.Run("broker ON: the sandbox gets hosts, never grant ids", func(t *testing.T) {
		env := map[string]string{}
		applyDispatchModeEnv(env, run, false, "exec", "", false, "", nil,
			map[string]string{"gitlab.com": pat["gitlab.com"]}, nil, nil, true)

		if got, ok := env["WARDYN_GIT_PAT_GRANTS"]; ok {
			t.Errorf("WARDYN_GIT_PAT_GRANTS = %q — with the broker on the grant ids must be WITHHELD, or the in-sandbox helper mints the PAT anyway and it is resident despite the broker", got)
		}
		if got := env["WARDYN_GIT_PAT_BROKER_HOSTS"]; got != "gitlab.com" {
			t.Errorf("WARDYN_GIT_PAT_BROKER_HOSTS = %q, want %q — agent-run needs the host list to rewrite the URL", got, "gitlab.com")
		}
		// The host list must carry NO grant id: it is the one PAT-related value
		// the sandbox still sees, so it must not be mintable.
		for _, v := range env {
			if v == pat["gitlab.com"] {
				t.Error("a grant id reached the sandbox env despite the broker being on")
			}
		}
	})

	t.Run("broker OFF: pre-0.7 behaviour, unchanged", func(t *testing.T) {
		env := map[string]string{}
		applyDispatchModeEnv(env, run, false, "exec", "", false, "", nil,
			map[string]string{"gitlab.com": pat["gitlab.com"]}, nil, nil, false)

		if env["WARDYN_GIT_PAT_GRANTS"] == "" {
			t.Error("with the broker off the grant ids must still reach the sandbox — that is what the escape hatch preserves")
		}
		if got, ok := env["WARDYN_GIT_PAT_BROKER_HOSTS"]; ok {
			t.Errorf("WARDYN_GIT_PAT_BROKER_HOSTS = %q with the broker off — agent-run would rewrite URLs to a route that brokers nothing", got)
		}
	})

	t.Run("no PAT grants: neither variable appears either way", func(t *testing.T) {
		for _, on := range []bool{true, false} {
			env := map[string]string{}
			applyDispatchModeEnv(env, run, false, "exec", "", false, "", nil, nil, nil, nil, on)
			if _, ok := env["WARDYN_GIT_PAT_BROKER_HOSTS"]; ok {
				t.Errorf("broker=%v: a run with no PAT grants must not get a broker host list", on)
			}
		}
	})
}

// patBrokerGrants is the dispatch-side conversion. A malformed id must drop the
// host rather than broker it with a nil grant, which would mint nothing and 502
// every clone against that host.
func TestPATBrokerGrants(t *testing.T) {
	valid := uuid.New()
	got := patBrokerGrants(map[string]string{
		"GitLab.com":   valid.String(),
		"broken.local": "not-a-uuid",
	}, true)

	if len(got) != 1 {
		t.Fatalf("got %d grants, want 1 — a malformed id must drop the host, not broker it", len(got))
	}
	g, ok := got["gitlab.com"]
	if !ok {
		t.Fatal("the host key must be lower-cased to match the proxy's lookup")
	}
	if g.GrantID != valid {
		t.Errorf("GrantID = %v, want %v", g.GrantID, valid)
	}

	if patBrokerGrants(map[string]string{"gitlab.com": valid.String()}, false) != nil {
		t.Error("with the lane off the allowlist must be nil, so the route 403s")
	}
}
