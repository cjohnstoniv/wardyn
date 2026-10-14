// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	writoidc "github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// orderedRevocations is a store that orders cuts by sequence: it hands out seq on RevocationSeq and records
// the Issue every cookie check asks about.
type orderedRevocations struct {
	identityRevocations
	seq    int64
	seqErr error
	issues []writoidc.Issue
}

func (o *orderedRevocations) RevocationSeq(context.Context) (int64, error) { return o.seq, o.seqErr }
func (o *orderedRevocations) IsSessionRevokedSeq(context.Context, string, string, int64) (bool, error) {
	return o.revoked, o.err
}
func (o *orderedRevocations) SessionStatus(_ context.Context, _, _ string, issue writoidc.Issue, _ int64) (writoidc.SessionStatus, error) {
	o.issues = append(o.issues, issue)
	return o.status, o.err
}

// A sign-in records the revocation sequence on its cookie, the middleware hands it to the store with the
// cookie's issue time, and it reaches the request context for the credentials the request mints.
func TestSessionCookieCarriesTheRevocationSequence(t *testing.T) {
	env := newIdPEnv(t)
	rev := &orderedRevocations{seq: 41}
	auth := env.newRoleMappingAuth(t, nil, "", nil, nil, func(cfg *writoidc.Config) { cfg.Revocations = rev })
	env.buildIDToken(t, "pairwise-1", "pat@corp.example", roleCallbackNonce, time.Now().Add(time.Hour))
	w, _ := doCallbackVia(t, auth, auth.CallbackHandler)
	rev.issues = nil
	sess, fields := sessionPayload(t, w)
	if sess.RevocationSeq != 41 || fields["rseq"] != float64(41) {
		t.Fatalf("cookie rseq = %d (%v), want 41", sess.RevocationSeq, fields["rseq"])
	}

	var ctxSeq, otherSeq int64
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, c := range w.Result().Cookies() {
		if c.Name == "wardyn_session" {
			req.AddCookie(c)
		}
	}
	auth.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		ctxSeq = writoidc.RevocationSeqFor(r.Context(), "pairwise-1")
		otherSeq = writoidc.RevocationSeqFor(r.Context(), "someone-else")
	})).ServeHTTP(httptest.NewRecorder(), req)
	if n := len(rev.issues); n == 0 || rev.issues[n-1].Seq != 41 || rev.issues[n-1].At.IsZero() {
		t.Errorf("the store was asked about %+v, want the cookie's sequence 41 and its issue time", rev.issues)
	}
	if ctxSeq != 41 || otherSeq != 0 {
		t.Errorf("context sequence for the session's own sub = %d, for another = %d; want 41 and 0", ctxSeq, otherSeq)
	}
}

// A sign-in whose sequence cannot be read is refused, not issued unordered; a store that does not order
// cuts issues a cookie with no sequence, ordered on time as before.
func TestSignInWithoutAnOrderingValue(t *testing.T) {
	env := newIdPEnv(t)
	rev := &orderedRevocations{seqErr: errors.New("pg down")}
	auth := env.newRoleMappingAuth(t, nil, "", nil, nil, func(cfg *writoidc.Config) { cfg.Revocations = rev })
	env.buildIDToken(t, "pairwise-1", "pat@corp.example", roleCallbackNonce, time.Now().Add(time.Hour))
	w, _ := doCallbackVia(t, auth, auth.CallbackHandler)
	if sessionIssuedBy(w) || !containsAuthError(w.Result().Header.Get("Location"), "role_check_unavailable") {
		t.Errorf("a sign-in that could not read the revocation sequence was issued (%v), Location %q", sessionIssuedBy(w), w.Result().Header.Get("Location"))
	}

	env = newIdPEnv(t)
	auth = env.newRoleMappingAuth(t, nil, "", nil, nil, func(cfg *writoidc.Config) { cfg.Revocations = &plainRevocations{} })
	env.buildIDToken(t, "pairwise-1", "pat@corp.example", roleCallbackNonce, time.Now().Add(time.Hour))
	w, _ = doCallbackVia(t, auth, auth.CallbackHandler)
	if sess, fields := sessionPayload(t, w); sess.RevocationSeq != 0 || fields["rseq"] != nil {
		t.Errorf("an unordered store stamped rseq %d (%v)", sess.RevocationSeq, fields["rseq"])
	}
}

// IsRevoked orders by sequence when the credential has one and the store orders cuts, else by time.
func TestIsRevokedOrdersBySequenceOnlyWhenBothSidesHaveOne(t *testing.T) {
	ctx := context.Background()
	at := time.Now()
	ordered := &orderedRevocations{}
	ordered.revoked = true
	plain := &plainRevocations{revoked: false}
	for _, c := range []struct {
		name  string
		rev   writoidc.SessionRevocations
		issue writoidc.Issue
		want  bool
	}{
		{"sequence on an ordered store", ordered, writoidc.Issue{At: at, Seq: 5}, true},
		{"no sequence falls back to time", &orderedRevocations{identityRevocations: identityRevocations{revoked: false}}, writoidc.Issue{At: at}, false},
		{"sequence on a store that cannot order uses time", plain, writoidc.Issue{At: at, Seq: 5}, false},
	} {
		got, err := writoidc.IsRevoked(ctx, c.rev, "s", "", c.issue)
		if err != nil || got != c.want {
			t.Errorf("%s: IsRevoked = %v, %v; want %v", c.name, got, err, c.want)
		}
	}
	if seq, err := writoidc.IssueSeq(ctx, plain); seq != 0 || err != nil {
		t.Errorf("IssueSeq on an unordered store = %d, %v; want 0", seq, err)
	}
}

// orderLoggingGate is a gate and an ordered store that note, in one shared log, when the sign-in reads the
// revocation sequence and when it issues the identity.
type orderLoggingGate struct {
	fakeGate
	log *[]string
}

func (g *orderLoggingGate) Issue(ctx context.Context, f writoidc.LoginFacts) (int64, error) {
	*g.log = append(*g.log, "issue identity")
	return g.fakeGate.Issue(ctx, f)
}

type orderLoggingRevocations struct {
	orderedRevocations
	log *[]string
}

func (o *orderLoggingRevocations) RevocationSeq(ctx context.Context) (int64, error) {
	*o.log = append(*o.log, "read sequence")
	return o.orderedRevocations.RevocationSeq(ctx)
}

// The sign-in reads the revocation sequence before it issues the identity: an unbind that commits after
// the read draws a greater sequence and ends the cookie, which an unbind between the two reads would not.
func TestSignInReadsTheRevocationSequenceBeforeItIssuesTheIdentity(t *testing.T) {
	var log []string
	rev := &orderLoggingRevocations{orderedRevocations: orderedRevocations{seq: 9}, log: &log}
	gate := &orderLoggingGate{log: &log}
	env := newIdPEnv(t)
	auth := env.newRoleMappingAuth(t, nil, "", nil, nil, func(cfg *writoidc.Config) { cfg.Revocations = rev; cfg.Identities = gate })
	env.buildIDToken(t, "pairwise-1", "pat@corp.example", roleCallbackNonce, time.Now().Add(time.Hour))
	w, _ := doCallbackVia(t, auth, auth.CallbackHandler)
	if !sessionIssuedBy(w) {
		t.Fatalf("sign-in refused: %q", w.Result().Header.Get("Location"))
	}
	if len(log) != 2 || log[0] != "read sequence" || log[1] != "issue identity" {
		t.Fatalf("sign-in steps in order %q; want the sequence read, then the identity issued", log)
	}
}
