// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// seqLog is an ordered SessionRevocations double that records how each check was made.
type seqLog struct {
	fakeAuthzSessionRevocations
	mu      sync.Mutex
	revoked bool
	seqs    []int64
	timed   int
}

func (l *seqLog) RevocationSeq(context.Context) (int64, error) { return 100, nil }
func (l *seqLog) IsSessionRevoked(context.Context, string, string, time.Time) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.timed++
	return l.revoked, nil
}
func (l *seqLog) IsSessionRevokedSeq(_ context.Context, _, _ string, seq int64) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seqs = append(l.seqs, seq)
	return l.revoked, nil
}
func (l *seqLog) flip() { l.mu.Lock(); l.revoked = true; l.mu.Unlock() }
func (l *seqLog) asked() (seqs []int64, timed int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]int64(nil), l.seqs...), l.timed
}

var _ oidc.RevocationOrder = (*seqLog)(nil)

// A relay session opened from a ticket carries the ticket's revocation sequence and is re-checked by it.
func TestUIGateway_SessionIsOrderedByTheTicketsSequence(t *testing.T) {
	h := newUIHarness(t, closingBackend("sandbox app"))
	rev := &seqLog{}
	h.srv.cfg.SessionRevocations = rev
	ctx := withOIDCEmail(context.Background(), "alice@corp.example")
	ticket, err := mintAttachTicket(ctx, h.store, h.run.ID, types.ActorHuman, h.owner, oidc.RoleUser, oidc.Issue{At: h.clock.now(), Seq: 7})
	if err != nil {
		t.Fatal(err)
	}
	rec := h.enterWith(ticket)
	cs := setUICookies(rec)
	if rec.Code != http.StatusFound || len(cs) != 1 {
		t.Fatalf("enter: %d %s", rec.Code, rec.Body.String())
	}
	path := uiRelayPrefix(h.run.ID, "code") + "/"
	if r := uiGet(h, path, cs[0]); r.Code != http.StatusOK {
		t.Fatalf("control: %d %s", r.Code, r.Body.String())
	}
	rev.flip()
	h.clock.advance(time.Second)
	if r := uiGet(h, path, cs[0]); r.Code != http.StatusForbidden {
		t.Fatalf("after the cut: %d %s, want 403", r.Code, r.Body.String())
	}
	seqs, timed := rev.asked()
	if timed != 0 || len(seqs) == 0 {
		t.Fatalf("the checks ran by time %d times and by sequence %v; want by sequence only", timed, seqs)
	}
	for _, s := range seqs {
		if s != 7 {
			t.Errorf("a check asked about sequence %d, want the ticket's 7", s)
		}
	}
}

// An open stream is re-checked by the sequence of the credential that opened it, for its own principal only.
func TestRunEvents_StreamIsOrderedByTheAdmittingCredentialsSequence(t *testing.T) {
	rev := &seqLog{}
	srv := &Server{cfg: Config{SessionRevocations: rev, Now: time.Now}}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(withHumanIdentity(req.Context(), "alice", "alice@corp.example", oidc.RoleUser, types.UserTypeStandard, nil, false))
	own := req.WithContext(oidc.WithRevocationSeq(req.Context(), "alice", 55))
	if srv.sessionRevokedSince(own, "alice", time.Now()) {
		t.Fatal("a live session read as revoked")
	}
	rev.flip()
	if !srv.sessionRevokedSince(own, "alice", time.Now()) {
		t.Fatal("a cut after the stream opened did not end it")
	}
	if seqs, timed := rev.asked(); len(seqs) != 2 || seqs[0] != 55 || timed != 0 {
		t.Fatalf("checks by sequence %v and by time %d; want two by sequence 55", seqs, timed)
	}
	if !srv.sessionRevokedSince(req, "alice", time.Now()) {
		t.Fatal("a credential with no sequence read as live after a cut")
	}
	if _, timed := rev.asked(); timed != 1 {
		t.Fatalf("a credential with no sequence was checked by time %d times, want 1", timed)
	}
}

// cuttingRevocations is an ordered double whose time check commits a cut first: the revocation a request
// races in the instant between its admission and its late re-check.
type cuttingRevocations struct {
	fakeAuthzSessionRevocations
	mu  sync.Mutex
	cur int64
}

func (c *cuttingRevocations) RevocationSeq(context.Context) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cur, nil
}
func (c *cuttingRevocations) IsSessionRevoked(context.Context, string, string, time.Time) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cur++
	return false, nil
}
func (*cuttingRevocations) IsSessionRevokedSeq(context.Context, string, string, int64) (bool, error) {
	return false, nil
}

var _ oidc.RevocationOrder = (*cuttingRevocations)(nil)

// The attach ticket records its revocation sequence before the late re-check, so a cut that commits
// between the two sits above the ticket's sequence and ends it.
func TestAttachTicket_RecordsItsSequenceBeforeTheLateRevocationCheck(t *testing.T) {
	ast := newAuthzStore()
	h := newHarness(t)
	cfg := baseTestConfig(h, ast)
	rev := &cuttingRevocations{cur: 10}
	cfg.SessionRevocations = rev
	srv := New(cfg)
	run := uuid.New()
	ast.runs[run] = types.AgentRun{ID: run, CreatedBy: "alice", OperatorOwned: true, State: types.RunRunning, SandboxRef: "sbx-1"}

	w := do(t, srv, http.MethodPost, "/api/v1/runs/"+run.String()+"/attach-ticket", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	ast.mu.Lock()
	defer ast.mu.Unlock()
	if len(ast.tickets) != 1 {
		t.Fatalf("%d tickets minted, want 1", len(ast.tickets))
	}
	rev.mu.Lock()
	defer rev.mu.Unlock()
	for _, tk := range ast.tickets {
		if tk.RevSeq == 0 || tk.RevSeq >= rev.cur {
			t.Fatalf("ticket sequence %d, sequence after the late check %d: a cut that committed during the request does not end the ticket", tk.RevSeq, rev.cur)
		}
	}
}
