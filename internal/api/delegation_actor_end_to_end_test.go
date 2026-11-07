// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/gatewayidentity"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The acting agent end to end on the stores the API reads: a portal's agent rides a gateway token, a
// queued launch's start rows and an attach ticket's replayed rows, not only the first request's.

var testActor = types.DelegationActor{Sub: "agent-sub", Iss: "https://idp.example"}

// delegatedRunWithActor makes the fixture's running run one a portal launched for testActor.
func (f *gatewayFixture) delegatedRunWithActor(t *testing.T, actor *types.DelegationActor) types.Delegate {
	t.Helper()
	d := f.delegatedRun(t)
	f.run.CreatedViaActor = actor
	f.st.set(func(s *gatewayStore) { s.run.CreatedViaActor = actor })
	return d
}

func gatewayClaimsOf(t *testing.T, token string) (gatewayidentity.Claims, map[string]any) {
	t.Helper()
	parsed, err := jwt.ParseSigned(token, []jose.SignatureAlgorithm{jose.ES256})
	if err != nil {
		t.Fatal(err)
	}
	var c gatewayidentity.Claims
	var m map[string]any
	if err := parsed.UnsafeClaimsWithoutVerification(&c, &m); err != nil {
		t.Fatal(err)
	}
	return c, m
}

func gatewayMintedToken(t *testing.T, body []byte) string {
	t.Helper()
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Token == "" {
		t.Fatalf("mint body %s: %v", body, err)
	}
	return out.Token
}

// A delegated run's model and MCP tokens both name the portal in wardyn_delegate and the agent as the
// inner actor, under the Wardyn run.
func TestGatewayMintDelegatedRunNamesPortalAndActor(t *testing.T) {
	f := newGatewayFixture(t)
	f.sealMCP(t, testGatewayMCPComponent())
	d := f.delegatedRunWithActor(t, &testActor)
	for name, mint := range map[string]func() []byte{
		"provider": func() []byte {
			w := f.mint(t)
			if w.Code != 200 {
				t.Fatalf("mint = %d %s", w.Code, w.Body)
			}
			return w.Body.Bytes()
		},
		"mcp": func() []byte {
			w := f.mintMCP(t, "tools")
			if w.Code != 200 {
				t.Fatalf("mint = %d %s", w.Code, w.Body)
			}
			return w.Body.Bytes()
		},
	} {
		t.Run(name, func(t *testing.T) {
			c, m := gatewayClaimsOf(t, gatewayMintedToken(t, mint()))
			if c.Subject != "alice@example.com" || c.Actor.Sub != "spiffe://wardyn.local/agent-run/"+f.run.ID.String() {
				t.Errorf("sub/act.sub = %q/%q, want the person and the run", c.Subject, c.Actor.Sub)
			}
			if c.Actor.Act == nil || c.Actor.Act.Sub != testActor.Sub || c.Actor.Act.Iss != testActor.Iss {
				t.Errorf("act.act = %+v, want the agent", c.Actor.Act)
			}
			if m["wardyn_delegate"] != d.ID.String() {
				t.Errorf("wardyn_delegate = %v, want %s", m["wardyn_delegate"], d.ID)
			}
		})
	}
}

// A delegated run whose portal named no agent: the portal claim, and the flat act it always had.
// A run nobody delegated gets neither.
func TestGatewayMintDelegatedRunWithoutActor(t *testing.T) {
	f := newGatewayFixture(t)
	d := f.delegatedRunWithActor(t, nil)
	_, m := gatewayClaimsOf(t, gatewayMintedToken(t, f.mint(t).Body.Bytes()))
	if m["wardyn_delegate"] != d.ID.String() {
		t.Errorf("wardyn_delegate = %v, want %s", m["wardyn_delegate"], d.ID)
	}
	if act, _ := m["act"].(map[string]any); len(act) != 1 {
		t.Errorf("act = %v, want the run's sub alone", m["act"])
	}

	g := newGatewayFixture(t)
	_, m = gatewayClaimsOf(t, gatewayMintedToken(t, g.mint(t).Body.Bytes()))
	if _, has := m["wardyn_delegate"]; has {
		t.Error("a self-launched run's token carries wardyn_delegate")
	}
}

// A queued launch's start has no request: the actor rides the stored authority, so the start's rows
// name the agent the launch did.
func TestQueueAuthorityCarriesTheActor(t *testing.T) {
	actor := testActor
	via := types.DelegationVia{Delegate: uuid.New(), Grant: uuid.New(), Actor: &actor}
	ctx := audit.WithDelegation(withHumanIdentity(context.Background(), "sub-p", "", "user", "", nil, false), via)
	raw, err := json.Marshal(captureQueueAuthority(ctx))
	if err != nil {
		t.Fatal(err)
	}
	var back queueAuthority
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	got, ok := audit.DelegationFrom(back.context(context.Background()))
	if !ok || got.Delegate != via.Delegate || got.Grant != via.Grant || got.Actor == nil || *got.Actor != actor {
		t.Fatalf("via after the round trip = %+v (%v), want %+v with the actor", got, ok, via)
	}

	// No actor: the stored authority is byte-identical to what a launch wrote before.
	plain := types.DelegationVia{Delegate: via.Delegate, Grant: via.Grant}
	raw, err = json.Marshal(captureQueueAuthority(audit.WithDelegation(context.Background(), plain)))
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	if _, has := keys["actor"]; has {
		t.Errorf("a launch with no agent stored an actor: %s", raw)
	}
}

// A delegated ticket replays the agent onto the rows the ticket lane writes, as it does the portal.
func TestAttachTicketReplaysTheActor(t *testing.T) {
	st := newMemTicketStore()
	actor := testActor
	via := types.DelegationVia{Delegate: uuid.New(), Grant: uuid.New(), Actor: &actor}
	run, now := uuid.New(), time.Unix(1_700_000_000, 0)
	tok, err := mintAttachTicket(audit.WithDelegation(context.Background(), via), st, run, types.ActorHuman, "alice", oidc.RoleUser, oidc.Issue{At: now})
	if err != nil {
		t.Fatal(err)
	}
	ta, ok, err := consumeAttachTicket(context.Background(), st, tok, run, now)
	if err != nil || !ok {
		t.Fatalf("redeem: ok=%v err=%v", ok, err)
	}
	if ta.via == nil || ta.via.Actor == nil || *ta.via.Actor != actor {
		t.Fatalf("ticket via = %+v, want the actor", ta.via)
	}
	got, ok := audit.DelegationFrom(withTicketActor(context.Background(), ta))
	if !ok || got.Actor == nil || *got.Actor != actor {
		t.Fatalf("replayed via = %+v (%v), want the actor", got, ok)
	}
}
