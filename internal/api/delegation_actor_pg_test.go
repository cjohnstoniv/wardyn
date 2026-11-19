// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	gooidctest "github.com/coreos/go-oidc/v3/oidc/oidctest"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The acting agent named on a portal's exchange (actor_token), through the whole path: the real
// identity provider, the real exchange, the Postgres delegate store and a real run create. Skips
// without WARDYN_TEST_PG.

const delegAgent = "agent-sub"

// actorToken is the agent's token as the identity provider issues it to the portal; mut edits the
// claims first.
func (e *delegationPG) actorToken(t *testing.T, mut func(map[string]any)) string {
	t.Helper()
	now := time.Now()
	c := map[string]any{
		"iss": e.iss, "sub": delegAgent, "aud": delegPortalClient,
		"iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(),
	}
	if mut != nil {
		mut(c)
	}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return gooidctest.SignIDToken(e.priv, "k", "RS256", string(raw))
}

func (e *delegationPG) exchangeWithActor(t *testing.T, id uuid.UUID, cred, subject, actor string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{
		"grant_type":         {grantTypeTokenExchange},
		"subject_token":      {subject},
		"subject_token_type": {tokenTypeAccessToken},
		"actor_token":        {actor},
		"actor_token_type":   {tokenTypeIDToken},
	}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/token", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.SetBasicAuth(id.String(), cred)
	w := httptest.NewRecorder()
	panicFails(t, e.h.srv.Handler()).ServeHTTP(w, r)
	return w
}

func accessTokenOf(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("exchange: %d %s", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	tok, _ := out["access_token"].(string)
	if !strings.HasPrefix(tok, delegatedTokenPrefix) {
		t.Fatalf("exchange response = %s", w.Body.String())
	}
	return tok
}

// Every refused actor is a 400 invalid_grant with the generic sentence, one denied delegation.exchange
// row naming the reason and the person, and nothing minted.
func TestTokenExchange_ActorRefusals(t *testing.T) {
	e := newDelegationPG(t)
	portal, cred := e.registerPortal(t, delegGroup)
	subject := func(mut func(map[string]any)) string {
		return e.subjectToken(t, delegPerson, delegPersonEmail, []string{delegGroup}, mut)
	}
	for _, tc := range []struct {
		name, subject, actor, reason string
	}{
		{"an actor token that does not verify", subject(nil), e.actorToken(t, func(c map[string]any) { c["iss"] = "https://elsewhere.invalid" }), "actor_token_invalid"},
		{"an expired actor token", subject(nil), e.actorToken(t, func(c map[string]any) { c["exp"] = time.Now().Add(-time.Minute).Unix() }), "actor_token_invalid"},
		{"an actor token with no iat", subject(nil), e.actorToken(t, func(c map[string]any) { delete(c, "iat") }), "actor_token_invalid"},
		{"an actor token with no sub", subject(nil), e.actorToken(t, func(c map[string]any) { c["sub"] = "" }), "actor_token_invalid"},
		{"a delegated token as actor", subject(nil), delegatedTokenPrefix + "x", "actor_token_invalid"},
		{"an actor token for Wardyn", subject(nil), e.actorToken(t, func(c map[string]any) { c["aud"] = []string{delegPortalClient, "wardyn-client"} }), "actor_token_audience"},
		{"an actor token for another client", subject(nil), e.actorToken(t, func(c map[string]any) { c["aud"] = "other-client" }), "actor_token_audience"},
		{"the person as their own actor", subject(nil), e.actorToken(t, func(c map[string]any) { c["sub"] = delegPerson }), "actor_is_subject"},
		{"an actor the person's may_act does not name", subject(func(c map[string]any) { c["may_act"] = map[string]any{"sub": "other-agent"} }), e.actorToken(t, nil), "actor_not_permitted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, rowsBefore := e.delegatedTokenCount(t), len(e.rows("delegation.exchange"))
			w := e.exchangeWithActor(t, portal, cred, tc.subject, tc.actor)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"invalid_grant"`) ||
				!strings.Contains(w.Body.String(), "the subject token was refused") {
				t.Fatalf("answer = %d %s, want 400 invalid_grant with the generic sentence", w.Code, w.Body.String())
			}
			if after := e.delegatedTokenCount(t); after != before {
				t.Fatalf("delegated tokens %d -> %d: a refused actor minted one", before, after)
			}
			rows := e.rows("delegation.exchange")[rowsBefore:]
			if len(rows) != 1 || rows[0].Outcome != "denied" || rows[0].Actor != delegateActor(portal) || rows[0].Target != delegPerson ||
				!strings.Contains(string(rows[0].Data), `"reason":"`+tc.reason+`"`) {
				t.Fatalf("delegation.exchange rows = %+v, want one denied row naming the person with reason %s", rows, tc.reason)
			}
		})
	}

	// A refused subject never verifies an actor: the subject's own reason is the row's.
	rowsBefore := len(e.rows("delegation.exchange"))
	w := e.exchangeWithActor(t, portal, cred, subject(func(c map[string]any) { c["azp"] = "other-client" }), "not-a-token")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("refused subject with an actor: %d %s", w.Code, w.Body.String())
	}
	if rows := e.rows("delegation.exchange")[rowsBefore:]; len(rows) != 1 || !strings.Contains(string(rows[0].Data), `"reason":"subject_token_audience"`) {
		t.Fatalf("rows = %+v, want the subject's own refusal", rows)
	}
}

// The person's own token presented as the actor, even issued to the portal, is refused.
func TestTokenExchange_ActorIsSubject(t *testing.T) {
	e := newDelegationPG(t)
	portal, cred := e.registerPortal(t, delegGroup)
	own := e.actorToken(t, func(c map[string]any) { c["sub"] = delegPerson })
	w := e.exchangeWithActor(t, portal, cred, e.subjectToken(t, delegPerson, delegPersonEmail, []string{delegGroup}, nil), own)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"invalid_grant"`) {
		t.Fatalf("answer = %d %s, want 400 invalid_grant", w.Code, w.Body.String())
	}
	ex := e.rows("delegation.exchange")
	if len(ex) != 1 || !strings.Contains(string(ex[0].Data), `"reason":"actor_is_subject"`) {
		t.Fatalf("delegation.exchange rows = %+v, want one actor_is_subject refusal", ex)
	}
}

// An admitted actor is on the success row and on the stored token, and reads back with it.
func TestTokenExchange_ActorRecorded(t *testing.T) {
	e := newDelegationPG(t)
	portal, cred := e.registerPortal(t, delegGroup)
	tok := accessTokenOf(t, e.exchangeWithActor(t, portal, cred,
		e.subjectToken(t, delegPerson, delegPersonEmail, []string{delegGroup}, func(c map[string]any) {
			c["may_act"] = map[string]any{"sub": delegAgent}
		}), e.actorToken(t, nil)))
	ex := e.rows("delegation.exchange")
	if len(ex) != 1 || ex[0].Outcome != "success" {
		t.Fatalf("delegation.exchange rows = %+v, want one success", ex)
	}
	var data struct {
		Grant uuid.UUID              `json:"grant"`
		Actor *types.DelegationActor `json:"actor"`
	}
	if err := json.Unmarshal(ex[0].Data, &data); err != nil {
		t.Fatal(err)
	}
	want := types.DelegationActor{Sub: delegAgent, Iss: e.iss}
	if data.Actor == nil || *data.Actor != want {
		t.Fatalf("success row actor = %+v, want %+v (%s)", data.Actor, want, ex[0].Data)
	}
	var sub, iss string
	if err := e.pool.QueryRow(context.Background(), `SELECT actor_sub, actor_iss FROM delegated_tokens WHERE id = $1`, data.Grant).Scan(&sub, &iss); err != nil {
		t.Fatal(err)
	}
	if sub != delegAgent || iss != e.iss {
		t.Fatalf("stored actor = (%q, %q), want (%q, %q)", sub, iss, delegAgent, e.iss)
	}
	got, err := e.st.GetDelegatedTokenByRaw(context.Background(), tok, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got.Actor == nil || *got.Actor != want {
		t.Fatalf("read-back actor = %+v, want %+v", got.Actor, want)
	}
}

// storePersonSecret has the person store their own secret "x", the one the strict policy's owner_only
// git_pat grant names, so a run they own under that policy can be created.
func (e *delegationPG) storePersonSecret(t *testing.T) {
	t.Helper()
	pat := accessSession(t, delegPerson, delegPersonEmail, oidc.RoleUser, []string{delegGroup})
	if w := doSSO(t, e.h.srv, http.MethodPut, "/api/v1/secrets/x", pat, `{"value":"pat-own-secret-value"}`); w.Code/100 != 2 {
		t.Fatalf("person writes their own secret: %d %s", w.Code, w.Body.String())
	}
}

// Every row a delegated request writes, the API's and the identity provider's alike, names the actor in
// data.via.actor beside the portal.
func TestDelegatedRowsCarryTheActor(t *testing.T) {
	e := newDelegationPG(t)
	portal, cred := e.registerPortal(t, delegGroup)
	e.storePersonSecret(t)
	strict := e.storePolicy(t, "strict", "x", true)
	tok := accessTokenOf(t, e.exchangeWithActor(t, portal, cred,
		e.subjectToken(t, delegPerson, delegPersonEmail, []string{delegGroup}, nil), e.actorToken(t, nil)))
	before := len(e.h.audit.snapshot())
	mustCreate(t, do(t, e.h.srv, http.MethodPost, "/api/v1/runs", tok, `{"agent":"claude-code","task":"t","policy_id":"`+strict+`"}`))
	time.Sleep(300 * time.Millisecond) // the detached launch writes after the response
	swept := e.h.audit.snapshot()[before:]
	actions := map[string]bool{}
	want := types.DelegationActor{Sub: delegAgent, Iss: e.iss}
	for _, ev := range swept {
		actions[ev.Action] = true
		v := viaOf(t, ev)
		if v.Delegate != portal || v.Actor == nil || *v.Actor != want {
			t.Fatalf("row %s via = %+v, want portal %s and actor %+v (%s)", ev.Action, v, portal, want, ev.Data)
		}
	}
	if !actions["run.create"] || !actions["identity.mint"] {
		t.Fatalf("swept actions %v, want at least run.create and identity.mint", actions)
	}
}

// A portal that sends no actor gets rows exactly as before: no actor key under via, and none on the
// exchange's success row.
func TestDelegatedRowsWithoutActorUnchanged(t *testing.T) {
	e := newDelegationPG(t)
	portal, cred := e.registerPortal(t, delegGroup)
	e.storePersonSecret(t)
	strict := e.storePolicy(t, "strict", "x", true)
	tok := e.delegate(t, portal, cred, delegPerson, delegPersonEmail, delegGroup)
	ex := e.rows("delegation.exchange")
	if len(ex) != 1 || strings.Contains(string(ex[0].Data), `"actor"`) {
		t.Fatalf("exchange rows = %+v, want one success row with no actor key", ex)
	}
	before := len(e.h.audit.snapshot())
	mustCreate(t, do(t, e.h.srv, http.MethodPost, "/api/v1/runs", tok, `{"agent":"claude-code","task":"t","policy_id":"`+strict+`"}`))
	time.Sleep(300 * time.Millisecond)
	swept := e.h.audit.snapshot()[before:]
	if len(swept) == 0 {
		t.Fatal("the delegated run create wrote no rows")
	}
	for _, ev := range swept {
		var d struct {
			Via map[string]json.RawMessage `json:"via"`
		}
		if err := json.Unmarshal(ev.Data, &d); err != nil || d.Via == nil {
			t.Fatalf("row %s carries no data.via: %s", ev.Action, ev.Data)
		}
		if len(d.Via) != 2 || d.Via["delegate"] == nil || d.Via["grant"] == nil {
			t.Fatalf("row %s via = %s, want exactly delegate and grant", ev.Action, ev.Data)
		}
	}
	var sub, iss string
	if err := e.pool.QueryRow(context.Background(), `SELECT actor_sub, actor_iss FROM delegated_tokens`).Scan(&sub, &iss); err != nil {
		t.Fatal(err)
	}
	if sub != "" || iss != "" {
		t.Fatalf("stored actor = (%q, %q), want empty", sub, iss)
	}
}
