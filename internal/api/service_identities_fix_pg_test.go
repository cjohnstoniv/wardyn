// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// failingErase wraps a secret store whose per-owner view cannot list while fail is set, the shape of a
// secret store that is briefly unavailable during an erase.
type failingErase struct {
	secretstore.Store
	fail *atomic.Bool
}

func (f failingErase) For(owner string) secretstore.Store {
	if v := f.Store.For(owner); f.fail.Load() {
		return failingList{v}
	}
	return f.Store.For(owner)
}

type failingList struct{ secretstore.Store }

func (failingList) List(context.Context) ([]string, error) {
	return nil, errors.New("secret store unavailable")
}

// TestServiceIdentity_SponsorSinceOnTheDatabaseClock: the sponsor cutoff is written on the database clock,
// the clock a token's created_at is translated onto, so a replica whose clock runs ahead still mints a
// token that authenticates, straight after the create and straight after a sponsor change.
func TestServiceIdentity_SponsorSinceOnTheDatabaseClock(t *testing.T) {
	e := newSvcEnv(t)
	skew := func() time.Time { return time.Now().Add(10 * time.Second) }
	e.h.srv.cfg.Store = store.PG{Pool: e.pool, Now: skew}
	e.h.srv.cfg.Now = skew
	e.mustCreate(t, svcName, svcSponsor)
	first := e.mustMint(t, e.sponsor, svcName)
	if w := e.as(t, first.Token, http.MethodGet, "/api/v1/me", ""); w.Code != http.StatusOK {
		t.Errorf("fresh service token, app clock 10s ahead of the database: %d (auth.fail %q), want 200", w.Code, e.lastAuthFail())
	}
	if w := doSSO(t, e.h.srv, http.MethodPatch, "/api/v1/service-identities/"+svcName, e.sec, `{"sponsor":"`+svcSponsor2+`"}`); w.Code != http.StatusOK {
		t.Fatalf("sponsor change: %d %s", w.Code, w.Body.String())
	}
	second := e.mustMint(t, e.sponsor2, svcName)
	if w := e.as(t, second.Token, http.MethodGet, "/api/v1/me", ""); w.Code != http.StatusOK {
		t.Errorf("token minted right after a sponsor change, app clock 10s ahead: %d (auth.fail %q), want 200", w.Code, e.lastAuthFail())
	}
}

// TestServiceIdentity_SponsorAsSecondHuman: the person who sponsors a service identity is the author of its
// runs for the second-human rule, an admin sponsor included; another admin is a second human, and a service
// whose record cannot be read fails closed. The same verdict at the gate and at its read-only mirror.
func TestServiceIdentity_SponsorAsSecondHuman(t *testing.T) {
	bothActorForms(t, func(t *testing.T, _ types.ActorType) {
		e := newSvcEnv(t)
		e.mustCreate(t, svcName, svcSponsor)
		t.Setenv(envEgressSecondHuman, "1")
		ap := types.ApprovalRequest{Kind: types.ApprovalEgressDomain}
		as := func(sub string) *http.Request {
			ctx := withHumanIdentity(context.Background(), sub, "", oidc.RoleAdmin, types.UserTypeStandard, []string{}, false)
			return httptest.NewRequest(http.MethodPost, "/api/v1/approvals/x/approve", nil).WithContext(ctx)
		}
		gate := func(sub string, run types.AgentRun) (int, bool) {
			w := httptest.NewRecorder()
			_, ok := e.h.srv.requireSecondHuman(w, as(sub), uuid.New(), ap, run, true, true)
			return w.Code, ok
		}
		run := types.AgentRun{CreatedBy: svcPrin, HumanAuthor: svcSponsor}
		if e.h.srv.secondHumanAllows(as(svcSponsor), ap, run) {
			t.Error("the admin who sponsors the service passes the read-only second-human mirror on its run")
		}
		if code, ok := gate(svcSponsor, run); ok || code != http.StatusForbidden {
			t.Errorf("requireSecondHuman for the sponsor: %d ok=%v, want refused 403", code, ok)
		}
		if !e.h.srv.secondHumanAllows(as("another-admin"), ap, run) {
			t.Error("another admin is refused on the service's run")
		}
		if code, ok := gate("another-admin", run); !ok {
			t.Errorf("requireSecondHuman for another admin: %d, want allowed", code)
		}
		ghost := types.AgentRun{CreatedBy: "service:no-such-service"}
		if e.h.srv.secondHumanAllows(as("another-admin"), ap, ghost) {
			t.Error("a run of a service whose record cannot be read is allowed")
		}
		if code, ok := gate("another-admin", ghost); ok || code != http.StatusServiceUnavailable {
			t.Errorf("requireSecondHuman with an unreadable service: %d ok=%v, want refused 503", code, ok)
		}
	})
}

// serviceRunRunning has the service token launch a run and marks it RUNNING, returning its run token.
func (e svcEnv) serviceRunRunning(t *testing.T, tok types.APIToken) (types.AgentRun, string) {
	t.Helper()
	w := e.as(t, tok.Token, http.MethodPost, "/api/v1/runs", `{"agent":"claude-code","task":"t","policy_id":"`+e.plainPolicy(t, "p-"+uuid.NewString())+`"}`)
	var created createRunResponse
	if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &created) != nil {
		t.Fatalf("service run create: %d %s", w.Code, w.Body.String())
	}
	if _, err := e.pool.Exec(context.Background(), `UPDATE agent_runs SET state = 'RUNNING' WHERE id = $1`, created.ID); err != nil {
		t.Fatal(err)
	}
	run, err := e.st.GetRun(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	return run, e.ids.last().token
}

// TestServiceIdentity_RenewAndReviveFollowTheIdentity: a renewed or revived service run carries the CURRENT
// sponsor, read from the identity's row, and once the identity is retired the run stops renewing and cannot
// be revived.
func TestServiceIdentity_RenewAndReviveFollowTheIdentity(t *testing.T) {
	e := newSvcEnv(t)
	e.mustCreate(t, svcName, svcSponsor)
	run, runToken := e.serviceRunRunning(t, e.mustMint(t, e.sponsor, svcName))
	if w := doSSO(t, e.h.srv, http.MethodPatch, "/api/v1/service-identities/"+svcName, e.sec, `{"sponsor":"`+svcSponsor2+`"}`); w.Code != http.StatusOK {
		t.Fatalf("sponsor change: %d", w.Code)
	}
	renew := func() *httptest.ResponseRecorder {
		return do(t, e.h.srv, http.MethodPost, "/api/v1/internal/token/renew", runToken, "")
	}
	if w := renew(); w.Code != http.StatusOK || e.ids.last().sponsor != svcSponsor2 {
		t.Fatalf("renew after a sponsor change: %d, sponsor claim %q, want 200 and %q", w.Code, e.ids.last().sponsor, svcSponsor2)
	}
	if _, rerr := e.h.srv.reviveMintIdentity(context.Background(), run); rerr != nil || e.ids.last().sponsor != svcSponsor2 {
		t.Fatalf("revive after a sponsor change: %v, sponsor claim %q, want %q", rerr, e.ids.last().sponsor, svcSponsor2)
	}
	if w := doSSO(t, e.h.srv, http.MethodDelete, "/api/v1/service-identities/"+svcName, e.sec, ""); w.Code != http.StatusOK {
		t.Fatalf("retire: %d %s", w.Code, w.Body.String())
	}
	if w := renew(); w.Code != http.StatusForbidden || respReason(w) != "service_identity_inactive" {
		t.Errorf("renew of a retired service's run: %d %s, want 403 service_identity_inactive", w.Code, w.Body.String())
	}
	if _, rerr := e.h.srv.reviveMintIdentity(context.Background(), run); rerr == nil || rerr.status != http.StatusConflict || rerr.reason != "service_identity_inactive" {
		t.Errorf("revive of a retired service's run: %+v, want 409 service_identity_inactive", rerr)
	}
}

// TestServiceIdentity_StreamEndsWhenTheServiceDoes: an open events stream of a service ends when the identity
// is suspended or its sponsor changes, and not before.
func TestServiceIdentity_StreamEndsWhenTheServiceDoes(t *testing.T) {
	e := newSvcEnv(t)
	e.mustCreate(t, svcName, svcSponsor)
	opened := time.Now().UTC()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/x/events", nil).
		WithContext(withServiceIdentity(context.Background(), svcName, svcSponsor))
	if e.h.srv.sessionRevokedSince(req, svcPrin, opened) {
		t.Fatal("the stream of an active service ended")
	}
	patch := func(body string) {
		t.Helper()
		if w := doSSO(t, e.h.srv, http.MethodPatch, "/api/v1/service-identities/"+svcName, e.sec, body); w.Code != http.StatusOK {
			t.Fatalf("PATCH %s: %d", body, w.Code)
		}
	}
	patch(`{"suspended":true}`)
	if !e.h.srv.sessionRevokedSince(req, svcPrin, opened) {
		t.Error("the stream of a suspended service did not end")
	}
	patch(`{"suspended":false}`)
	if e.h.srv.sessionRevokedSince(req, svcPrin, opened) {
		t.Error("the stream of a resumed service still ends")
	}
	patch(`{"sponsor":"` + svcSponsor2 + `"}`)
	if !e.h.srv.sessionRevokedSince(req, svcPrin, opened) {
		t.Error("the stream opened under the previous sponsor did not end")
	}
}

// TestServiceIdentity_TokenCapHoldsInTheStore: the 20-live-token cap is counted in the minting transaction
// under the identity's row lock, so the 21st is refused through the store path and parallel mints at the
// edge admit exactly the slots left.
func TestServiceIdentity_TokenCapHoldsInTheStore(t *testing.T) {
	e := newSvcEnv(t)
	e.mustCreate(t, svcName, svcSponsor)
	mint := func() error {
		f := false
		_, err := e.st.CreateServiceToken(context.Background(), svcName, svcSponsor, apiTokenMaxPerPrincipal, types.APIToken{
			ID: uuid.New(), Principal: svcPrin, Role: oidc.RoleUser, UserType: types.UserTypeStandard, Groups: []string{},
			GroupsTruncated: &f, Name: "t", CreatedAt: time.Now().UTC(), ExpiresAt: ptrTime(time.Now().Add(time.Hour)),
		}, "wdn_"+uuid.NewString())
		return err
	}
	for i := 0; i < apiTokenMaxPerPrincipal-3; i++ {
		if err := mint(); err != nil {
			t.Fatalf("mint %d: %v", i, err)
		}
	}
	var wg sync.WaitGroup
	errs := make([]error, 12)
	for i := range errs {
		wg.Add(1)
		go func() { defer wg.Done(); errs[i] = mint() }()
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, store.ErrServiceTokenCap):
			t.Errorf("parallel mint: %v, want success or the cap", err)
		}
	}
	if ok != 3 {
		t.Errorf("%d of 12 parallel mints succeeded with 3 slots left, want exactly 3", ok)
	}
	if err := mint(); !errors.Is(err, store.ErrServiceTokenCap) {
		t.Errorf("the 21st token through the store: %v, want ErrServiceTokenCap", err)
	}
	if w := e.mintReq(t, e.sponsor, svcName, `{}`); w.Code != http.StatusUnprocessableEntity || respReason(w) != "api_token_cap_reached" {
		t.Errorf("the 21st token through the API: %d %s, want 422 api_token_cap_reached", w.Code, w.Body.String())
	}
}

// TestServiceIdentity_MintRefusedWithoutARevocationStore: with no revocation store the sponsor's session
// cannot be checked, so the token would never authenticate; the mint is refused rather than handed out dead.
func TestServiceIdentity_MintRefusedWithoutARevocationStore(t *testing.T) {
	e := newSvcEnv(t)
	e.mustCreate(t, svcName, svcSponsor)
	e.h.srv.cfg.SessionRevocations = nil
	if w := e.mintReq(t, e.sponsor, svcName, `{}`); w.Code != http.StatusConflict || respReason(w) != "service_identity_inactive" {
		t.Errorf("sponsor mint with no revocation store: %d %s, want 409 service_identity_inactive", w.Code, w.Body.String())
	}
}

// TestLocalMode_AttributionOverrideRefusesAMechanismPrincipal: the dev-only X-Wardyn-Principal override cannot
// name a service identity or another mechanism principal; the configured seat answers instead. A local: name
// and a plain name are still honoured.
func TestLocalMode_AttributionOverrideRefusesAMechanismPrincipal(t *testing.T) {
	cfg := newHarness(t).srv.cfg
	cfg.LocalMode, cfg.LocalOperator, cfg.LocalLoopback = true, "local:seat", true
	srv := New(cfg)
	t.Cleanup(srv.WaitBackground)
	for header, want := range map[string]string{
		"service:ci-bot": "local:seat", "Service:CI-bot": "local:seat", "admin-token": "local:seat", "device:abc": "local:seat",
		"subject:123": "local:seat", "local:carol": "local:carol", "alice": "alice", "": "local:seat",
	} {
		var got string
		h := srv.humanOrAdminAuth(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { _, got = actorFromRequest(r) }))
		req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
		req.Host, req.RemoteAddr = "127.0.0.1:8080", "127.0.0.1:54321"
		if header != "" {
			req.Header.Set("X-Wardyn-Principal", header)
		}
		h.ServeHTTP(httptest.NewRecorder(), req)
		if got != want {
			t.Errorf("override %q attributed to %q, want %q", header, got, want)
		}
	}
}

// TestServiceIdentity_StreamClockBehindTheDatabase: a stream opened by a replica whose clock runs behind the
// database is not ended by the cutoff written a moment earlier on the database clock.
func TestServiceIdentity_StreamClockBehindTheDatabase(t *testing.T) {
	e := newSvcEnv(t)
	behind := func() time.Time { return time.Now().Add(-10 * time.Second) }
	e.h.srv.cfg.Store = store.PG{Pool: e.pool, Now: behind}
	e.h.srv.cfg.Now = behind
	e.mustCreate(t, svcName, svcSponsor)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/x/events", nil).
		WithContext(withServiceIdentity(context.Background(), svcName, svcSponsor))
	if e.h.srv.sessionRevokedSince(req, svcPrin, behind()) {
		t.Error("a stream opened after the create, on a clock 10s behind the database, ended")
	}
}
