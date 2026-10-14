// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// mintIssue is where a credential this request mints sits against the revocation cuts: the request's
// authority time, and the revocation sequence the credential records. The credential inherits the sequence
// of the one that admitted the request when it was issued to principal, so a cut the admitting credential
// has not yet seen ends the new one as well; otherwise (a cookie from before the sequence existed, a
// principal the request does not speak for) it reads the current sequence. A zero Seq is a deployment
// whose revocations do not order cuts, and the credential is compared on time.
func (s *Server) mintIssue(ctx context.Context, principal string, authorizedAt time.Time) (oidc.Issue, error) {
	if s.cfg.SessionRevocations == nil {
		return oidc.Issue{At: authorizedAt}, nil
	}
	if seq := oidc.RevocationSeqFor(ctx, principal); seq > 0 {
		return oidc.Issue{At: authorizedAt, Seq: seq}, nil
	}
	seq, err := oidc.IssueSeq(ctx, s.cfg.SessionRevocations)
	return oidc.Issue{At: authorizedAt, Seq: seq}, err
}

// admittedIssue is the Issue of the credential that admitted this request, for a check that asks whether
// that credential has been cut since: its own sequence when it was issued to principal, else time.
func admittedIssue(ctx context.Context, principal string, at time.Time) oidc.Issue {
	return oidc.Issue{At: at, Seq: oidc.RevocationSeqFor(ctx, principal)}
}

// admittedByToken reports whether an API token or a delegated token admitted this request. Neither is ended by
// a browser-only cut (sign-out): their authentication checks the owner at epoch -1.
func admittedByToken(ctx context.Context) bool {
	_, delegated := audit.DelegationFrom(ctx)
	return delegated || apiTokenIDFromContext(ctx) != uuid.Nil
}

// admittedRevoked is the late re-check of the credential that admitted this request, asked as that credential's
// own check asks it: a token is ended by a revoke or by its owner's identity, never by a browser-only cut; a
// session cookie by either cut. rev must not be nil.
func (s *Server) admittedRevoked(ctx context.Context, principal string, at time.Time) (bool, error) {
	rev, issue := s.cfg.SessionRevocations, admittedIssue(ctx, principal, at)
	if admittedByToken(ctx) {
		status, err := oidc.CheckSession(ctx, rev, principal, oidcEmailFromContext(ctx), issue, -1)
		return status != oidc.SessionLive, err
	}
	return oidc.IsRevoked(ctx, rev, principal, oidcEmailFromContext(ctx), issue)
}

// ticketIssue is mintIssue for an attach ticket. A ticket is ended by a browser-only cut too, which a token's
// sequence does not order, so a ticket a token mints records the sequence read now: a cut before the read
// leaves it, one during or after the mint ends it. A revoke since the token's own issue is the late
// re-check's (admittedRevoked).
func (s *Server) ticketIssue(ctx context.Context, principal string, authorizedAt time.Time) (oidc.Issue, error) {
	if s.cfg.SessionRevocations == nil || !admittedByToken(ctx) {
		return s.mintIssue(ctx, principal, authorizedAt)
	}
	seq, err := oidc.IssueSeq(ctx, s.cfg.SessionRevocations)
	return oidc.Issue{At: authorizedAt, Seq: seq}, err
}
