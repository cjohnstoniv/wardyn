// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A run a portal launches for a named agent keeps the agent: the run create writes created_via_actor
// beside created_via, and the run's JSON shows both. Skips without WARDYN_TEST_PG.
func TestDelegatedRunKeepsTheActor(t *testing.T) {
	e := newDelegationPG(t)
	portal, cred := e.registerPortal(t, delegGroup)
	e.storePersonSecret(t)
	strict := e.storePolicy(t, "strict", "x", true)
	want := types.DelegationActor{Sub: delegAgent, Iss: e.iss}
	tok := accessTokenOf(t, e.exchangeWithActor(t, portal, cred,
		e.subjectToken(t, delegPerson, delegPersonEmail, []string{delegGroup}, nil), e.actorToken(t, nil)))
	created := mustCreate(t, do(t, e.h.srv, http.MethodPost, "/api/v1/runs", tok, `{"agent":"claude-code","task":"t","policy_id":"`+strict+`"}`))
	var run struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &run); err != nil || run.ID == "" {
		t.Fatalf("create body %s: %v", created.Body, err)
	}
	w := do(t, e.h.srv, http.MethodGet, "/api/v1/runs/"+run.ID, tok, "")
	if w.Code != http.StatusOK {
		t.Fatalf("get run = %d %s", w.Code, w.Body)
	}
	var got types.AgentRun
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.CreatedVia == nil || got.CreatedViaActor == nil || *got.CreatedViaActor != want {
		t.Fatalf("created_via/created_via_actor = %v/%+v, want the portal and %+v (%s)", got.CreatedVia, got.CreatedViaActor, want, w.Body)
	}
}

// A portal that named no agent launches a run whose JSON has no created_via_actor key.
func TestDelegatedRunWithoutActorHasNoCreatedViaActor(t *testing.T) {
	e := newDelegationPG(t)
	portal, cred := e.registerPortal(t, delegGroup)
	e.storePersonSecret(t)
	strict := e.storePolicy(t, "strict", "x", true)
	tok := e.delegate(t, portal, cred, delegPerson, delegPersonEmail, delegGroup)
	created := mustCreate(t, do(t, e.h.srv, http.MethodPost, "/api/v1/runs", tok, `{"agent":"claude-code","task":"t","policy_id":"`+strict+`"}`))
	var run struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	w := do(t, e.h.srv, http.MethodGet, "/api/v1/runs/"+run.ID, tok, "")
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &keys); err != nil {
		t.Fatal(err)
	}
	if _, has := keys["created_via_actor"]; has || keys["created_via"] == nil {
		t.Fatalf("run JSON %s, want created_via and no created_via_actor", w.Body)
	}
}
