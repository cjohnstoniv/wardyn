// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"time"

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
