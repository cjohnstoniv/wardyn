// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc

import (
	"encoding/json"
	"net/http"
	"slices"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
)

// The actor half of a portal's token exchange (RFC 8693): the portal may also present a token the
// identity provider issued TO THE PORTAL for the agent acting for the person, and VerifyActorToken
// proves which agent that is. The actor is recorded, never authorised: the person stays the owner and
// the actor of everything the delegated token does, at user reach.
//
// On by default, with no setting: naming an actor grants nothing, a request that sends one was refused
// before, and a portal that sends none gets the same token and audit rows as before.

// Actor-token refusals. Each is the exchange's 400 invalid_grant and the denied row's reason.
const (
	// ActorTokenInvalid: the actor token failed to verify (signature, issuer, expiry), or carries no iat or no sub.
	ActorTokenInvalid = "actor_token_invalid"
	// ActorTokenAudience: the actor token was not issued to the portal, or was also issued for this deployment.
	ActorTokenAudience = "actor_token_audience"
	// ActorIsSubject: the actor token names the person the subject token names.
	ActorIsSubject = "actor_is_subject"
	// ActorNotPermitted: the subject token's may_act claim does not name the actor.
	ActorNotPermitted = "actor_not_permitted"
)

// Actor is the agent an admitted actor token names: its subject and issuer.
type Actor struct {
	Sub string
	Iss string
}

// MayAct is the may_act claim's subject and issuer. A claim that does not decode as an object is kept
// as an empty MayAct, which names no actor, so it refuses every actor and never the subject token.
type MayAct struct {
	Sub string `json:"sub"`
	Iss string `json:"iss"`
}

// tokenIdentity is a verified token's own sub, and Entra's tenant and object id.
type tokenIdentity struct{ sub, tid, oid string }

// readMayAct reads tok's optional may_act claim; nil when absent or null.
func readMayAct(tok *gooidc.IDToken) *MayAct {
	var c struct {
		MayAct json.RawMessage `json:"may_act"`
	}
	if err := tok.Claims(&c); err != nil || len(c.MayAct) == 0 || string(c.MayAct) == "null" {
		return nil
	}
	m := &MayAct{}
	if err := json.Unmarshal(c.MayAct, m); err != nil {
		return &MayAct{}
	}
	return m
}

// VerifyActorToken verifies raw as the agent acting for subject, the session VerifySubjectToken just
// admitted for the same exchange. There is no new trust root: the token must verify against this
// deployment's own issuer and key set, be unexpired, and carry an iat and a sub.
//
// Its audience must hold portalClientID and must NOT hold this deployment's client id: the agent's
// token was issued for the portal, and a token issued for Wardyn (a person's own sign-in) can never be
// replayed as an actor.
//
// The actor must not be the person: its sub is compared with the subject token's own sub and, on Entra
// (where sub is pairwise per app registration), its tenant and object id with the subject token's. This
// is defence in depth; the audience rule is the control. When the subject token carries may_act, the
// actor's sub, and its issuer if may_act names one, must match it.
//
// No person admission runs on the actor: being named grants it nothing to admit. denied is non-empty on
// any refusal, with a zero Actor.
func (a *Authenticator) VerifyActorToken(r *http.Request, raw, portalClientID string, subject Session) (Actor, string) {
	if a.subjectVerifier == nil {
		return Actor{}, ActorTokenInvalid
	}
	tok, err := a.subjectVerifier.Verify(r.Context(), raw)
	if err != nil || tok.IssuedAt.IsZero() || tok.Subject == "" {
		return Actor{}, ActorTokenInvalid
	}
	if portalClientID == "" || portalClientID == a.cfg.ClientID ||
		!slices.Contains(tok.Audience, portalClientID) || slices.Contains(tok.Audience, a.cfg.ClientID) {
		return Actor{}, ActorTokenAudience
	}
	if tok.Subject == subject.subjectToken.sub {
		return Actor{}, ActorIsSubject
	}
	if a.entra && subject.subjectToken.oid != "" {
		var ec struct {
			Tid string `json:"tid"`
			Oid string `json:"oid"`
		}
		if tok.Claims(&ec) == nil && ec.Tid == subject.subjectToken.tid && ec.Oid == subject.subjectToken.oid {
			return Actor{}, ActorIsSubject
		}
	}
	if m := subject.MayAct; m != nil && (m.Sub != tok.Subject || (m.Iss != "" && m.Iss != tok.Issuer)) {
		return Actor{}, ActorNotPermitted
	}
	return Actor{Sub: tok.Subject, Iss: tok.Issuer}, ""
}
