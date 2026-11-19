// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc_test

import (
	"net/http/httptest"
	"testing"
	"time"

	writoidc "github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

const testAgentSub = "agent-sub"

// actorClaims is the acting agent's token as the test IdP issues it to the portal; each case edits a
// copy.
func (e *idpEnv) actorClaims() map[string]any {
	now := time.Now()
	return map[string]any{
		"iss": e.httpSrv.URL, "sub": testAgentSub, "aud": testPortalClient,
		"iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(),
	}
}

// verifyActor admits the person's subject token (edited by subj), then verifies the actor token
// (edited by act) against that session.
func verifyActor(t *testing.T, entra bool, subj, act func(map[string]any)) (writoidc.Actor, string) {
	t.Helper()
	env := newIdPEnv(t)
	auth := env.newAuth(t, []string{"corp.example"})
	if entra {
		writoidc.SetEntraForTest(auth)
	}
	sc := env.subjectClaims()
	if subj != nil {
		subj(sc)
	}
	r := httptest.NewRequest("POST", "/api/v1/token", nil)
	sess, denied := auth.VerifySubjectToken(r, env.sign(t, sc), testPortalClient, nil)
	if denied != "" {
		t.Fatalf("subject token refused: %s", denied)
	}
	ac := env.actorClaims()
	if act != nil {
		act(ac)
	}
	actor, denied := auth.VerifyActorToken(r, env.sign(t, ac), testPortalClient, sess)
	if denied == "" && (actor.Sub == "" || actor.Iss != env.httpSrv.URL) {
		t.Fatalf("admitted actor = %+v, want sub %q and the deployment's issuer %s", actor, testAgentSub, env.httpSrv.URL)
	}
	if denied != "" && actor != (writoidc.Actor{}) {
		t.Fatalf("refused actor carries %+v, want the zero Actor", actor)
	}
	return actor, denied
}

func TestVerifyActorToken_PortalAudience(t *testing.T) {
	for name, edit := range map[string]func(map[string]any){
		"issued to the portal":              nil,
		"the portal among other audiences":  func(c map[string]any) { c["aud"] = []string{testPortalClient, "other-client"} },
		"azp naming the portal is accepted": func(c map[string]any) { c["azp"] = testPortalClient },
	} {
		t.Run(name, func(t *testing.T) {
			actor, denied := verifyActor(t, false, nil, edit)
			if denied != "" || actor.Sub != testAgentSub {
				t.Fatalf("actor = (%+v, %q), want the agent admitted", actor, denied)
			}
		})
	}
}

// A token issued for Wardyn itself can never name an actor, even beside the portal's audience: a
// person's own Wardyn sign-in token must not be replayed as an agent.
func TestVerifyActorToken_WardynAudienceRefused(t *testing.T) {
	const wardyn = "wardyn-client" // newIdPEnv's client id
	for name, aud := range map[string]any{
		"Wardyn only":            wardyn,
		"Wardyn beside portal":   []string{testPortalClient, wardyn},
		"another client only":    "other-client",
		"no audience at all":     []string{},
		"two others, not portal": []string{"other-client", "third-client"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, denied := verifyActor(t, false, nil, func(c map[string]any) { c["aud"] = aud }); denied != writoidc.ActorTokenAudience {
				t.Fatalf("denied = %q, want %q", denied, writoidc.ActorTokenAudience)
			}
		})
	}
}

func TestVerifyActorToken_NoIat(t *testing.T) {
	if _, denied := verifyActor(t, false, nil, func(c map[string]any) { delete(c, "iat") }); denied != writoidc.ActorTokenInvalid {
		t.Fatalf("denied = %q, want %q", denied, writoidc.ActorTokenInvalid)
	}
}

func TestVerifyActorToken_NoSub(t *testing.T) {
	if _, denied := verifyActor(t, false, nil, func(c map[string]any) { c["sub"] = "" }); denied != writoidc.ActorTokenInvalid {
		t.Fatalf("denied = %q, want %q", denied, writoidc.ActorTokenInvalid)
	}
}

func TestVerifyActorToken_WrongIssuer(t *testing.T) {
	if _, denied := verifyActor(t, false, nil, func(c map[string]any) { c["iss"] = "https://idp.invalid" }); denied != writoidc.ActorTokenInvalid {
		t.Fatalf("denied = %q, want %q", denied, writoidc.ActorTokenInvalid)
	}
}

func TestVerifyActorToken_Expired(t *testing.T) {
	if _, denied := verifyActor(t, false, nil, func(c map[string]any) { c["exp"] = time.Now().Add(-time.Minute).Unix() }); denied != writoidc.ActorTokenInvalid {
		t.Fatalf("denied = %q, want %q", denied, writoidc.ActorTokenInvalid)
	}
}

func TestVerifyActorToken_NotAToken(t *testing.T) {
	env := newIdPEnv(t)
	auth := env.newAuth(t, []string{"corp.example"})
	r := httptest.NewRequest("POST", "/api/v1/token", nil)
	sess, denied := auth.VerifySubjectToken(r, env.sign(t, env.subjectClaims()), testPortalClient, nil)
	if denied != "" {
		t.Fatal(denied)
	}
	if _, denied := auth.VerifyActorToken(r, "wdg_not-a-jwt", testPortalClient, sess); denied != writoidc.ActorTokenInvalid {
		t.Fatalf("denied = %q, want %q", denied, writoidc.ActorTokenInvalid)
	}
}

// The actor is compared with the subject token's own sub, not the principal admission resolved, and on
// Entra also by tenant and object id, since there sub is pairwise per app registration.
func TestVerifyActorToken_IsSubject(t *testing.T) {
	for _, tc := range []struct {
		name  string
		entra bool
		subj  func(map[string]any)
		act   func(map[string]any)
		want  string
	}{
		{name: "the person's own sub", act: func(c map[string]any) { c["sub"] = "person-sub" }, want: writoidc.ActorIsSubject},
		{name: "Entra: same tenant and object id under another sub", entra: true,
			subj: func(c map[string]any) { c["tid"], c["oid"] = keyTenant, keyObject },
			act:  func(c map[string]any) { c["sub"], c["tid"], c["oid"] = "portal-pairwise", keyTenant, keyObject },
			want: writoidc.ActorIsSubject},
		{name: "Entra: another object id", entra: true,
			subj: func(c map[string]any) { c["tid"], c["oid"] = keyTenant, keyObject },
			act:  func(c map[string]any) { c["tid"], c["oid"] = keyTenant, "99999999-8888-7777-6666-555555555555" }},
		{name: "not Entra: object id claims are not compared",
			subj: func(c map[string]any) { c["tid"], c["oid"] = keyTenant, keyObject },
			act:  func(c map[string]any) { c["tid"], c["oid"] = keyTenant, keyObject }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, denied := verifyActor(t, tc.entra, tc.subj, tc.act); denied != tc.want {
				t.Fatalf("denied = %q, want %q", denied, tc.want)
			}
		})
	}
}

// may_act (RFC 8693 section 4.4) on the person's token names who may act for them. A malformed one
// never refuses the subject token itself, only an actor.
func TestVerifyActorToken_MayActMismatch(t *testing.T) {
	for name, mayAct := range map[string]any{
		"another sub":           map[string]any{"sub": "other-agent"},
		"the sub, other issuer": map[string]any{"sub": testAgentSub, "iss": "https://idp.invalid"},
		"no sub named":          map[string]any{"iss": "https://idp.invalid"},
		"an empty object":       map[string]any{},
		"not an object":         "agent-sub",
		"a list":                []string{testAgentSub},
	} {
		t.Run(name, func(t *testing.T) {
			_, denied := verifyActor(t, false, func(c map[string]any) { c["may_act"] = mayAct }, nil)
			if denied != writoidc.ActorNotPermitted {
				t.Fatalf("denied = %q, want %q", denied, writoidc.ActorNotPermitted)
			}
		})
	}
}

func TestVerifyActorToken_MayActMatch(t *testing.T) {
	for name, mayAct := range map[string]func(iss string) any{
		"the sub":            func(string) any { return map[string]any{"sub": testAgentSub} },
		"the sub and issuer": func(iss string) any { return map[string]any{"sub": testAgentSub, "iss": iss} },
		"null is no claim":   func(string) any { return nil },
	} {
		t.Run(name, func(t *testing.T) {
			actor, denied := verifyActorWithIssuer(t, mayAct)
			if denied != "" || actor.Sub != testAgentSub {
				t.Fatalf("actor = (%+v, %q), want the agent admitted", actor, denied)
			}
		})
	}
}

// verifyActorWithIssuer is verifyActor for a may_act that names the test IdP's own issuer, which
// is only known once the env exists.
func verifyActorWithIssuer(t *testing.T, mayAct func(iss string) any) (writoidc.Actor, string) {
	t.Helper()
	env := newIdPEnv(t)
	auth := env.newAuth(t, []string{"corp.example"})
	sc := env.subjectClaims()
	sc["may_act"] = mayAct(env.httpSrv.URL)
	r := httptest.NewRequest("POST", "/api/v1/token", nil)
	sess, denied := auth.VerifySubjectToken(r, env.sign(t, sc), testPortalClient, nil)
	if denied != "" {
		t.Fatalf("subject token refused: %s", denied)
	}
	return auth.VerifyActorToken(r, env.sign(t, env.actorClaims()), testPortalClient, sess)
}
