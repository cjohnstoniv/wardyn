// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The workload profile of the issuer verifier (#3787): a machine's token from an organization's own provider,
// checked against one inbound trust. It reuses the portal verifier's cached discovery, key set and outage
// handling; only the claim rules differ.

package oidc

import (
	"context"
	"encoding/json"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// WorkloadRegistration is one inbound trust as the verifier reads it. Issuer, Audience, Subject and the claim are
// exact bytes; nothing is normalised.
type WorkloadRegistration struct {
	Issuer             string
	InternalIssuer     string // "" or the address discovery and keys are fetched from
	Audience           string
	Subject            string
	RequiredClaim      string // "" means no extra claim is required
	RequiredClaimValue string
	MaxTokenAge        time.Duration // 0 means 300 s; at most workloadMaxTokenAge
}

// WorkloadSubject is a verified workload token. It names no Wardyn principal: the trust decides that.
type WorkloadSubject struct {
	Issuer   string
	Subject  string
	IssuedAt time.Time
	Expiry   time.Time // the token's exp: an exchanged token never lives past it
	Matched  []string  // the extra claim that held, at most one
}

// workloadMaxTokenAge bounds a trust's maximum token age, as the inbound_trusts table's check does.
const workloadMaxTokenAge = time.Hour

// SubjectTokenSubjectMismatch refuses a workload token whose sub is not the trust's.
const SubjectTokenSubjectMismatch = "subject_token_subject_mismatch"

// WorkloadCandidate reads iss, aud and sub from the UNVERIFIED payload of a compact RS256 token, only so the caller
// can choose the one trust to verify the token against. ok is false unless each is a non-empty JSON string and aud
// is exactly one value. Nothing it returns is trusted, and nothing is fetched.
func WorkloadCandidate(raw string) (iss, aud, sub string, ok bool) {
	jws, err := jose.ParseSignedCompact(raw, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil || len(jws.Signatures) != 1 {
		return "", "", "", false
	}
	var c portalClaims
	if json.Unmarshal(jws.UnsafePayloadWithoutVerification(), &c) != nil || c == nil {
		return "", "", "", false
	}
	iss, okI := c.str("iss")
	auds, okA := c.audience()
	sub, okS := c.str("sub")
	if !okI || !okA || !okS || iss == "" || sub == "" || len(auds) != 1 || auds[0] == "" {
		return "", "", "", false
	}
	return iss, auds[0], sub, true
}

// VerifyWorkload checks a workload's token against one inbound trust, with VerifyDetail's order and outage handling:
// the registration, an RS256-only parse before any key is looked at, the cached verifier for the trust's issuer,
// its health, the signature, a recheck, the exact issuer twice, then the workload claim rules.
func (p *PortalIssuers) VerifyWorkload(ctx context.Context, reg WorkloadRegistration, raw string) (WorkloadSubject, PortalRefusal) {
	if reg.Issuer == "" || reg.Audience == "" || reg.Subject == "" ||
		reg.MaxTokenAge < 0 || reg.MaxTokenAge > workloadMaxTokenAge ||
		(reg.RequiredClaim == "") != (reg.RequiredClaimValue == "") {
		return WorkloadSubject{}, PortalRefusal{Reason: PortalRegistrationInvalid, Class: PortalRefusedRequest}
	}
	// Only RS256 is ever verified: HS*, none and anything else is refused before a key is looked at.
	if jws, err := jose.ParseSignedCompact(raw, []jose.SignatureAlgorithm{jose.RS256}); err != nil || len(jws.Signatures) != 1 {
		return WorkloadSubject{}, tokenRefusal(SubjectTokenInvalid)
	}
	v, err := p.verifierFor(PortalRegistration{Issuer: reg.Issuer, InternalIssuer: reg.InternalIssuer})
	if err != nil {
		return WorkloadSubject{}, PortalRefusal{Reason: PortalIssuerUnavailable, Cause: portalFetchCause(err), Class: PortalRefusedUnavailable}
	}
	box := &portalBox{}
	ctx = withPortalBox(ctx, box)
	if cause, retry := v.prepare(ctx); cause != "" {
		return WorkloadSubject{}, PortalRefusal{Reason: PortalIssuerUnavailable, Cause: cause, Class: PortalRefusedUnavailable, RetryAfter: retry}
	}
	if v.afterPrepare != nil {
		v.afterPrepare()
	}
	tok, err := v.idv.Verify(ctx, raw)
	if err != nil {
		failure := verifyFailure(box, err)
		if failure.Cause == portalCauseKeysUnreachable {
			failure.RetryAfter = v.outageWait() // this exchange's own key fetch failed: the door's answer for an outage
		}
		return WorkloadSubject{}, v.underRefusal(failure)
	}
	// A refresh can have failed, or withdrawn the key this token was verified under, since the health check
	// above: look again before anything is admitted.
	if refusal := v.recheck(box); refusal.Reason != "" {
		return WorkloadSubject{}, v.underRefusal(refusal)
	}
	// go-oidc lets one non-identical spelling of an issuer through; Wardyn compares the bytes.
	if tok.Issuer != reg.Issuer {
		return WorkloadSubject{}, tokenRefusal(SubjectTokenInvalid)
	}
	var claims portalClaims
	if tok.Claims(&claims) != nil || claims == nil {
		return WorkloadSubject{}, tokenRefusal(SubjectTokenInvalid)
	}
	// The library fills Issuer by Go's struct decoding, which also matches a member spelled "ISS" or "Iss". The
	// signed claim is the exact member "iss", read from the verified payload's own map.
	if iss, ok := claims.str("iss"); !ok || iss != reg.Issuer {
		return WorkloadSubject{}, tokenRefusal(SubjectTokenInvalid)
	}
	return p.checkWorkload(reg, claims)
}

// checkWorkload runs every rule after the signature and the issuer: time, then the audience, the subject and the one
// optional claim, each exact.
func (p *PortalIssuers) checkWorkload(reg WorkloadRegistration, c portalClaims) (WorkloadSubject, PortalRefusal) {
	maxAge := reg.MaxTokenAge
	if maxAge <= 0 {
		maxAge = portalDefaultMaxAge
	}
	times, denial := portalCheckTimes(c, p.clock(), maxAge)
	if denial != "" {
		return WorkloadSubject{}, tokenRefusal(denial)
	}
	if aud, ok := c.audience(); !ok || len(aud) != 1 || aud[0] != reg.Audience {
		return WorkloadSubject{}, tokenRefusal(SubjectTokenAudienceMismatch)
	}
	sub, ok := c.str("sub")
	if !ok || sub != reg.Subject {
		return WorkloadSubject{}, tokenRefusal(SubjectTokenSubjectMismatch)
	}
	out := WorkloadSubject{Issuer: reg.Issuer, Subject: sub, IssuedAt: times.issuedAt, Expiry: times.expiry}
	if reg.RequiredClaim != "" {
		got, ok := c.str(reg.RequiredClaim)
		if !ok || got != reg.RequiredClaimValue {
			return WorkloadSubject{}, tokenRefusal(SubjectTokenClaimMismatch)
		}
		out.Matched = []string{reg.RequiredClaim}
	}
	return out, PortalRefusal{}
}
