// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// credentialStore adds the two reads a ticket's credential binding asks (a token's liveness, a CLI session's
// state) to the harness store, with the credentials the test lets stand.
type credentialStore struct {
	store.Store
	tokens   map[uuid.UUID]bool
	sessions map[uuid.UUID]store.CLISessionRow
}

func (s *credentialStore) GetLiveAPIToken(_ context.Context, id uuid.UUID) (types.APIToken, error) {
	if !s.tokens[id] {
		return types.APIToken{}, store.ErrNotFound
	}
	return types.APIToken{ID: id}, nil
}

func (s *credentialStore) GetCLISession(_ context.Context, id uuid.UUID) (store.CLISessionRow, error) {
	row, ok := s.sessions[id]
	if !ok {
		return store.CLISessionRow{}, store.ErrNotFound
	}
	return row, nil
}

// TestAttachTicket_BoundToItsCredential: a ticket minted with a personal token or a CLI sign-in's access token
// carries that credential (the session, for a CLI sign-in), and the open terminal's authority check ends it when
// the token is revoked, the session is signed out or has expired; a CLI refresh, which only replaces the access
// token, leaves it open.
func TestAttachTicket_BoundToItsCredential(t *testing.T) {
	h := newUIHarness(t, okBackend())
	cs := &credentialStore{Store: h.srv.cfg.Store, tokens: map[uuid.UUID]bool{}, sessions: map[uuid.UUID]store.CLISessionRow{}}
	h.srv.cfg.Store = cs
	tokenID, sessionID, accessID := uuid.New(), uuid.New(), uuid.New()
	cs.tokens[tokenID], cs.tokens[accessID] = true, true
	cs.sessions[sessionID] = store.CLISessionRow{ID: sessionID, ExpiresAt: h.srv.cfg.Now().Add(24 * time.Hour)}

	mint := func(ctx context.Context) ticketActor {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, "/api/v1/runs/"+h.run.ID.String()+"/attach/ticket", nil).WithContext(ctx)
		w := httptest.NewRecorder()
		h.srv.issueAttachTicket(w, r, store.AttachTicket{RunID: h.run.ID, ActorType: types.ActorHuman, Principal: h.owner, Role: oidc.RoleUser}, h.srv.cfg.Now())
		if w.Code != http.StatusOK {
			t.Fatalf("mint = %d %s", w.Code, w.Body)
		}
		var out struct {
			Ticket string `json:"ticket"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		ta, ok, err := consumeAttachTicket(context.Background(), h.srv.cfg.Store, out.Ticket, h.run.ID, h.srv.cfg.Now())
		if err != nil || !ok {
			t.Fatalf("consume = %v %v", ok, err)
		}
		return ta
	}
	check := func(ta ticketActor) error {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+h.run.ID.String()+"/attach", nil)
		fn := h.srv.attachAuthority(r.WithContext(withTicketActor(r.Context(), ta)), h.run.ID)
		if fn == nil {
			t.Fatal("a ticket bound to a credential has no re-check")
		}
		return fn(context.Background())
	}

	byToken := mint(withAPITokenID(context.Background(), tokenID))
	if byToken.tokenID != tokenID || byToken.cliSessionID != uuid.Nil {
		t.Fatalf("token ticket binds token %v session %v, want %v and none", byToken.tokenID, byToken.cliSessionID, tokenID)
	}
	byCLI := mint(withCLISessionID(withAPITokenID(context.Background(), accessID), sessionID))
	if byCLI.cliSessionID != sessionID || byCLI.tokenID != uuid.Nil {
		t.Fatalf("CLI ticket binds token %v session %v, want none and %v", byCLI.tokenID, byCLI.cliSessionID, sessionID)
	}
	if ta := mint(context.Background()); ta.tokenID != uuid.Nil || ta.cliSessionID != uuid.Nil {
		t.Fatalf("a ticket minted with no token binds %v %v, want none", ta.tokenID, ta.cliSessionID)
	}

	if err := check(byToken); err != nil {
		t.Fatalf("token terminal with the token live: %v", err)
	}
	if err := check(byCLI); err != nil {
		t.Fatalf("CLI terminal with the session live: %v", err)
	}
	// A refresh replaces the access token; the session, which the terminal is bound to, goes on.
	delete(cs.tokens, accessID)
	if err := check(byCLI); err != nil {
		t.Fatalf("CLI terminal after a refresh replaced its access token: %v", err)
	}
	delete(cs.tokens, tokenID)
	if err := check(byToken); !errors.Is(err, errAttachAuthorityEnded) {
		t.Fatalf("token terminal after the token was revoked: %v, want %v", err, errAttachAuthorityEnded)
	}
	now := h.srv.cfg.Now()
	cs.sessions[sessionID] = store.CLISessionRow{ID: sessionID, ExpiresAt: now.Add(24 * time.Hour), EndedAt: &now}
	if err := check(byCLI); !errors.Is(err, errAttachAuthorityEnded) {
		t.Fatalf("CLI terminal after the computer signed out: %v, want %v", err, errAttachAuthorityEnded)
	}
	cs.sessions[sessionID] = store.CLISessionRow{ID: sessionID, ExpiresAt: now.Add(-time.Minute)}
	if err := check(byCLI); !errors.Is(err, errAttachAuthorityEnded) {
		t.Fatalf("CLI terminal after the session expired: %v, want %v", err, errAttachAuthorityEnded)
	}
	delete(cs.sessions, sessionID)
	if err := check(byCLI); !errors.Is(err, errAttachAuthorityEnded) {
		t.Fatalf("CLI terminal after the session row went: %v, want %v", err, errAttachAuthorityEnded)
	}
}

// TestUISession_BoundToItsCredential: a relay session opened from a ticket minted with a personal token or a CLI
// sign-in carries that credential in its signed cookie, and its re-check refuses with 403 once the token is
// revoked or the computer signed out; a session from a console login has nothing to check, and a CLI
// refresh, which only replaces the access token, does not end the session.
func TestUISession_BoundToItsCredential(t *testing.T) {
	h := newUIHarness(t, okBackend())
	cs := &credentialStore{Store: h.srv.cfg.Store, tokens: map[uuid.UUID]bool{}, sessions: map[uuid.UUID]store.CLISessionRow{}}
	h.srv.cfg.Store = cs
	tokenID, sessionID := uuid.New(), uuid.New()
	cs.tokens[tokenID] = true
	cs.sessions[sessionID] = store.CLISessionRow{ID: sessionID, ExpiresAt: h.srv.cfg.Now().Add(24 * time.Hour)}
	open := func(ta ticketActor) uiSession {
		ta.actorType, ta.principal, ta.role, ta.authorizedAt = types.ActorHuman, h.owner, oidc.RoleUser, h.srv.cfg.Now()
		sess := h.srv.newUISession(h.run, ta, "code", uiTestPort, time.Hour)
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.AddCookie(&http.Cookie{Name: uiCookieName, Value: h.srv.encodeUISession(sess)})
		got, ok := h.srv.decodeUISession(r, h.srv.cfg.Now())
		if !ok {
			t.Fatal("the session cookie did not decode")
		}
		if got.CredToken != ta.tokenID || got.CredSession != ta.cliSessionID {
			t.Fatalf("cookie credential = %v/%v, want %v/%v", got.CredToken, got.CredSession, ta.tokenID, ta.cliSessionID)
		}
		return got
	}
	refused := func(sess uiSession) *uiDialError {
		return h.srv.uiSessionStillAuthorized(context.Background(), sess, h.run)
	}

	byToken := open(ticketActor{tokenID: tokenID})
	byCLI := open(ticketActor{cliSessionID: sessionID})
	console := open(ticketActor{})
	for name, sess := range map[string]uiSession{"token": byToken, "CLI": byCLI, "console": console} {
		if de := refused(sess); de != nil {
			t.Fatalf("%s session with its credential live: refused %+v", name, de)
		}
	}
	delete(cs.tokens, tokenID)
	if de := refused(byToken); de == nil || de.status != http.StatusForbidden || de.reason != uiDeniedReasonRevoked {
		t.Fatalf("token session after the token was revoked: %+v, want 403 %s", de, uiDeniedReasonRevoked)
	}
	if de := refused(byCLI); de != nil {
		t.Fatalf("CLI session after a refresh replaced its access token: %+v", de)
	}
	now := h.srv.cfg.Now()
	cs.sessions[sessionID] = store.CLISessionRow{ID: sessionID, ExpiresAt: now.Add(24 * time.Hour), EndedAt: &now}
	if de := refused(byCLI); de == nil || de.status != http.StatusForbidden || de.reason != uiDeniedReasonRevoked {
		t.Fatalf("CLI session after the computer signed out: %+v, want 403 %s", de, uiDeniedReasonRevoked)
	}
	if de := refused(console); de != nil {
		t.Fatalf("console session refused by a credential check: %+v", de)
	}
}
