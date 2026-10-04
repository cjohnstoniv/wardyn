// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/maskmanifest"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestMiscCovRunPATRow(t *testing.T) {
	run := uuid.MustParse("00000000-0000-4000-8000-0000000000f1")
	auth := uuid.MustParse("00000000-0000-4000-8000-0000000000f2")
	valid := time.Date(2026, 6, 7, 8, 9, 10, 0, time.UTC)
	sn := adoEntraScopeSnapshot{OwnerSubject: "sub-owner", ProviderRowID: "row-1", Organisation: "contoso"}

	row, err := runPATRow(run, sn, adoPAT{AuthorizationID: auth.String(), Scope: "vso.code", ValidTo: valid})
	if err != nil {
		t.Fatal(err)
	}
	want := store.RunPAT{RunID: run, AuthorizationID: auth, Owner: "sub-owner", ProviderRowID: "row-1", Org: "contoso", Scope: "vso.code", ValidTo: valid}
	if row != want {
		t.Errorf("row = %+v, want %+v", row, want)
	}

	_, err = runPATRow(run, sn, adoPAT{AuthorizationID: "not-a-uuid"})
	if err == nil || !strings.Contains(err.Error(), `"not-a-uuid"`) {
		t.Errorf("err = %v, want a refusal naming the bad authorization id", err)
	}
}

// Every way a mint can fail is answered with the status, reason and sentence that tells the proxy whether
// to retry, and the person what to fix.
func TestMiscCovADORunPATRefusalTable(t *testing.T) {
	lifespan := &adoPATError{Status: http.StatusBadRequest, PatTokenError: adoPATErrLifespan}
	for _, tc := range []struct {
		name   string
		err    error
		status int
		reason string
		body   string
	}{
		{"a lock that is held", fmt.Errorf("lock: %w", db.ErrLockBusy), http.StatusServiceUnavailable, reasonLockUnavailable, lockUnavailableMsg},
		{"state that cannot be read", fmt.Errorf("x: %w", errADOPATStateUnavailable), http.StatusServiceUnavailable, reasonLockUnavailable, lockUnavailableMsg},
		{"a deployment that cannot mint", errADOPATUnavailable, http.StatusForbidden, reasonADOPATUnavailable, adoRunPATUnavailable},
		{"a console with no secret", fmt.Errorf("x: %w", ErrADOMintNeedsSecret), http.StatusForbidden, ReasonADOPATNeedsConsoleApp, adoPATNeedsConsoleAppRefusal},
		{"a paused run", errADOPATRunPaused, http.StatusConflict, reasonADOPATRunInactive, adoRunPATPaused},
		{"an ended run", errADOPATRunEnded, http.StatusConflict, reasonADOPATRunInactive, adoRunPATEnded},
		{"a run that cannot be read", errADOPATRunUnread, http.StatusServiceUnavailable, reasonRunUnreadable, adoRunPATRunUnread},
		{"an organisation that restricts creation", &adoPATError{Status: http.StatusForbidden, PatTokenError: adoPATErrAccessDenied}, http.StatusForbidden, reasonADOPATPolicyBlocked, adoRunPATPolicyBlocked},
		{"a life above the organisation's maximum", lifespan, http.StatusForbidden, reasonADOPATLifespanPolicy, adoRunPATLifespan},
		{"a sign-in without the token permissions", &adoPATError{Status: http.StatusUnauthorized}, http.StatusForbidden, reasonADOPATConsentNeeded, adoRunPATConsentNeeded},
		{"a refusal nobody classified", &adoPATError{Status: http.StatusTeapot, PatTokenError: "mystery"}, http.StatusForbidden, reasonADOPATMintRefused, adoRunPATRefused},
		{"a refusal that is transient", &adoPATError{Status: http.StatusBadGateway}, http.StatusServiceUnavailable, reasonADOPATMintRefused, adoRunPATRefused},
		{"a policy refusal that is transient", &adoPATError{Status: http.StatusTooManyRequests, PatTokenError: adoPATErrGlobalPolicy}, http.StatusServiceUnavailable, reasonADOPATPolicyBlocked, adoRunPATPolicyBlocked},
		{"a call that did not complete", errors.New("connection reset"), http.StatusServiceUnavailable, string(ADOEntraFailureUnavailable), adoRunPATUnreachable},
		{"a person who never connected", fmt.Errorf("x: %w", ErrADOEntraNotCaptured), http.StatusForbidden, string(ADOEntraFailureNotCaptured), adoRunPATNotConnected},
		{"a dead credential", fmt.Errorf("x: %w", ErrADOEntraDeadCredential), http.StatusForbidden, string(ADOEntraFailureDeadCredential), adoResolveDeadCredential},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, reason, body := adoRunPATRefusal(tc.err)
			if status != tc.status || reason != tc.reason || body != tc.body {
				t.Errorf("= %d %q %q\nwant %d %q %q", status, reason, body, tc.status, tc.reason, tc.body)
			}
		})
	}
}

// miscCovCreates is what the token API was asked to create, in order, read under its lock.
func miscCovCreates(fx *adoPATFixture) []adoPATRequest {
	fx.pats.mu.Lock()
	defer fx.pats.mu.Unlock()
	return slices.Clone(fx.pats.creates)
}

// miscCovPATStore is the minted-PAT fixture's store with the sign-in end counter and a failing token record.
type miscCovPATStore struct {
	*adoPATStore
	mu        sync.Mutex
	endsCalls int
	endsFn    func(call int) (store.ADOSignInEndState, error)
	insertErr error
}

func (s *miscCovPATStore) BeginADOSignInEnd(context.Context, string, string) error { return nil }
func (s *miscCovPATStore) FinishADOSignInEnd(context.Context, string) error        { return nil }

func (s *miscCovPATStore) ADOSignInEnds(context.Context, string) (store.ADOSignInEndState, error) {
	s.mu.Lock()
	s.endsCalls++
	call, fn := s.endsCalls, s.endsFn
	s.mu.Unlock()
	if fn != nil {
		return fn(call)
	}
	return store.ADOSignInEndState{}, nil
}

func (s *miscCovPATStore) InsertRunPAT(ctx context.Context, p store.RunPAT) error {
	if s.insertErr != nil {
		return s.insertErr
	}
	return s.adoPATStore.InsertRunPAT(ctx, p)
}

// miscCovPATClient is the token API answering with exactly what a test says.
type miscCovPATClient struct {
	mu      sync.Mutex
	result  adoPAT
	revoked []string
}

func (c *miscCovPATClient) Create(context.Context, string, string, adoPATRequest) (adoPAT, error) {
	return c.result, nil
}

func (c *miscCovPATClient) Revoke(_ context.Context, _, _, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.revoked = append(c.revoked, id)
	return nil
}

func (c *miscCovPATClient) revokedIDs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.revoked)
}

// miscCovMintLane is a lane that has not dispatched, with what a direct mint needs.
type miscCovMintLane struct {
	*adoPATFixture
	ctx     context.Context
	sn      adoEntraScopeSnapshot
	cfg     ADOEntraConfig
	validTo time.Time
	wrapped *miscCovPATStore
}

func newMiscCovMintLane(t *testing.T) *miscCovMintLane {
	t.Helper()
	fx := newADOPATLane(t)
	ctx := context.Background()
	sn := fx.ado.snapshot()
	cfg, status, reason, _ := fx.srv.adoEntraConfigFor(ctx, sn)
	if status != 0 {
		t.Fatalf("the lane's sign-in configuration is unusable: %d %s", status, reason)
	}
	w := &miscCovPATStore{adoPATStore: fx.st}
	fx.srv.cfg.Store = w
	return &miscCovMintLane{adoPATFixture: fx, ctx: ctx, sn: sn, cfg: cfg, validTo: adoTestNow.Add(8 * time.Hour).UTC().Truncate(time.Second), wrapped: w}
}

func (l *miscCovMintLane) mint(caps []adoscope.Capability) (adoPAT, error) {
	return l.srv.mintRunPAT(l.ctx, l.run.ID, l.sn, l.cfg, caps, l.validTo, adoPATMintDispatch)
}

func (l *miscCovMintLane) deniedRefusals(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, ev := range l.audit.find(adoPATAuditMintDenied) {
		var d map[string]any
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprint(d["refusal"]))
	}
	return out
}

// A token is created only for a lane that can record and revoke it, with a scope that names something.
func TestMiscCovMintRefusesBeforeCreatingAnything(t *testing.T) {
	t.Run("a store that cannot record tokens", func(t *testing.T) {
		l := newMiscCovMintLane(t)
		l.srv.cfg.Store = l.st.adoCapStore
		if _, err := l.mint(l.ado.caps); !errors.Is(err, errADOPATUnavailable) {
			t.Fatalf("err = %v, want errADOPATUnavailable", err)
		}
		if l.pats.createCount() != 0 {
			t.Errorf("%d tokens created", l.pats.createCount())
		}
	})
	t.Run("capabilities that name no scope", func(t *testing.T) {
		l := newMiscCovMintLane(t)
		if _, err := l.mint(nil); err == nil || !strings.Contains(err.Error(), "no capabilities") {
			t.Fatalf("err = %v, want the scope refusal", err)
		}
		if l.pats.createCount() != 0 {
			t.Errorf("%d tokens created", l.pats.createCount())
		}
	})
	t.Run("a sign-in end count that cannot be read", func(t *testing.T) {
		l := newMiscCovMintLane(t)
		boom := errors.New("ends store down")
		l.wrapped.endsFn = func(int) (store.ADOSignInEndState, error) { return store.ADOSignInEndState{}, boom }
		if _, err := l.mint(l.ado.caps); !errors.Is(err, boom) {
			t.Fatalf("err = %v, want the count's error", err)
		}
		if l.pats.createCount() != 0 || len(l.deniedRefusals(t)) != 1 {
			t.Errorf("created %d tokens, denied rows %v; want none created and one denial", l.pats.createCount(), l.deniedRefusals(t))
		}
	})
}

// A token that could not be recorded is revoked at once and never handed out, whichever record failed.
func TestMiscCovMintRevokesATokenItCouldNotRecord(t *testing.T) {
	t.Run("the masking renderings", func(t *testing.T) {
		l := newMiscCovMintLane(t)
		pool, closedErr := miscCovClosedPool(t)
		reg := secretmask.NewRegistry()
		l.srv.cfg.MaskRegistry = reg
		l.srv.cfg.MaskManifests = maskmanifest.New(pool, nil, reg)
		_, err := l.mint(l.ado.caps)
		if !errors.Is(err, closedErr) || !strings.HasPrefix(err.Error(), "record the run token's masking renderings") {
			t.Fatalf("err = %v, want the masking record's error", err)
		}
		if l.pats.createCount() != 1 || len(l.pats.revokedIDs()) != 1 || len(l.st.unrevoked(t)) != 0 {
			t.Errorf("created %d, revoked %v, rows %d; want the one token revoked and no row", l.pats.createCount(), l.pats.revokedIDs(), len(l.st.unrevoked(t)))
		}
	})
	t.Run("the token row", func(t *testing.T) {
		l := newMiscCovMintLane(t)
		l.wrapped.insertErr = errors.New("insert refused")
		_, err := l.mint(l.ado.caps)
		if !errors.Is(err, l.wrapped.insertErr) || !strings.HasPrefix(err.Error(), "record the run's personal access token") {
			t.Fatalf("err = %v, want the record's error", err)
		}
		if len(l.pats.revokedIDs()) != 1 || len(l.audit.find(adoPATAuditMint)) != 0 {
			t.Errorf("revoked %v, mint rows %d; want the token revoked and no success row", l.pats.revokedIDs(), len(l.audit.find(adoPATAuditMint)))
		}
	})
	t.Run("an authorization id that is not a uuid", func(t *testing.T) {
		l := newMiscCovMintLane(t)
		client := &miscCovPATClient{result: adoPAT{AuthorizationID: "not-a-uuid", Token: "fakepat" + strings.Repeat("z", 44)}}
		l.srv.adoPATs = client
		_, err := l.mint(l.ado.caps)
		if err == nil || !strings.Contains(err.Error(), `"not-a-uuid"`) {
			t.Fatalf("err = %v, want the bad id named", err)
		}
		if got := client.revokedIDs(); !slices.Equal(got, []string{"not-a-uuid"}) {
			t.Errorf("revoked %v, want the unrecordable token revoked", got)
		}
	})
}

// A token the API returns with no expiry or scope is recorded with the ones asked for.
func TestMiscCovMintFillsAnExpiryAndScopeTheAPIOmitted(t *testing.T) {
	l := newMiscCovMintLane(t)
	auth := uuid.MustParse("00000000-0000-4000-8000-0000000000f3")
	l.srv.adoPATs = &miscCovPATClient{result: adoPAT{AuthorizationID: auth.String(), Token: "fakepat" + strings.Repeat("y", 44)}}
	pat, err := l.mint(l.ado.caps)
	if err != nil {
		t.Fatal(err)
	}
	wantScope, _ := adoscope.PATScope(l.ado.caps)
	if !pat.ValidTo.Equal(l.validTo) || pat.Scope != wantScope {
		t.Errorf("pat valid_to %v scope %q, want %v and %q", pat.ValidTo, pat.Scope, l.validTo, wantScope)
	}
	rows := l.st.unrevoked(t)
	if len(rows) != 1 || !rows[0].ValidTo.Equal(l.validTo) || rows[0].Scope != wantScope || rows[0].AuthorizationID != auth {
		t.Errorf("recorded rows = %+v, want one with the filled expiry and scope", rows)
	}
}

// A sign-in whose end count cannot be read after the record is not shown to be clear of an end: the token
// is revoked, its row closed, and the mint is denied.
func TestMiscCovMintRevokesATokenWhoseSignInEndCannotBeChecked(t *testing.T) {
	l := newMiscCovMintLane(t)
	boom := errors.New("ends store down")
	l.wrapped.endsFn = func(call int) (store.ADOSignInEndState, error) {
		if call >= 2 {
			return store.ADOSignInEndState{}, boom
		}
		return store.ADOSignInEndState{}, nil
	}
	if _, err := l.mint(l.ado.caps); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the count's error", err)
	}
	if len(l.pats.revokedIDs()) != 1 || len(l.st.unrevoked(t)) != 0 {
		t.Errorf("revoked %v, live rows %d; want the token revoked and its row closed", l.pats.revokedIDs(), len(l.st.unrevoked(t)))
	}
	if got := l.auditReasons(adoPATAuditRevoke); !slices.Equal(got, []string{"ends_unreadable"}) {
		t.Errorf("revoke reasons = %v, want [ends_unreadable]", got)
	}
	if len(l.audit.find(adoPATAuditMint)) != 0 {
		t.Error("a success row was written for a token that was never handed out")
	}
}

func TestMiscCovDispatchRefusesWhenTheRunsTokenLockIsHeld(t *testing.T) {
	fx := newADOPATLane(t)
	fx.st.edit(fx.run.ID, func(r *types.AgentRun) { r.State = types.RunStarting })
	fx.run.State = types.RunStarting
	locker := &miscCovLocker{refuse: db.ErrLockBusy}
	fx.srv.locks.override = locker
	if fx.dispatch(t) {
		t.Fatal("dispatch went ahead without the run's token lock")
	}
	if fx.pats.createCount() != 0 || len(fx.st.grants) != 0 {
		t.Errorf("a refused launch created %d tokens and authored %d grants", fx.pats.createCount(), len(fx.st.grants))
	}
	if fx.st.hint != lockUnavailableMsg {
		t.Errorf("failure hint = %q, want the retryable lock sentence", fx.st.hint)
	}
}

// A resolve for a run that cannot be read, has ended, or whose token lock is held creates nothing and
// says why.
func TestMiscCovResolveRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(*adoPATFixture)
		status int
		reason string
	}{
		{"a run that cannot be read", func(fx *adoPATFixture) {
			fx.st.mu.Lock()
			delete(fx.st.runs, fx.run.ID)
			fx.st.mu.Unlock()
		}, http.StatusServiceUnavailable, reasonRunUnreadable},
		{"a run that has ended", func(fx *adoPATFixture) {
			fx.st.edit(fx.run.ID, func(r *types.AgentRun) { r.State = types.RunCompleted })
		}, http.StatusConflict, reasonADOPATRunInactive},
		{"a token lock that is held", func(fx *adoPATFixture) {
			fx.srv.locks.override = &miscCovLocker{refuse: db.ErrLockBusy}
		}, http.StatusServiceUnavailable, reasonLockUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newADOPATFixture(t)
			creates := fx.pats.createCount()
			tc.setup(fx)
			w, _ := fx.resolve(t, "dev.azure.com", nil)
			if w.Code != tc.status || errorReason(w) != tc.reason {
				t.Errorf("resolve = %d %s (%s), want %d %s", w.Code, errorReason(w), w.Body, tc.status, tc.reason)
			}
			if fx.pats.createCount() != creates {
				t.Errorf("a refused resolve created %d tokens", fx.pats.createCount()-creates)
			}
		})
	}
}

func TestMiscCovRevokeUnrecordedRunPAT(t *testing.T) {
	t.Run("a store that keeps no token rows has nothing to close", func(t *testing.T) {
		fx := newADOPATFixture(t)
		fx.srv.cfg.Store = fx.st.adoCapStore
		fx.srv.revokeUnrecordedRunPAT(t.Context(), fx.run.ID, fx.ado.snapshot(), adoPAT{AuthorizationID: uuid.NewString()})
		if len(fx.pats.revokedIDs()) != 0 {
			t.Errorf("revoked %v with no store to close the row in", fx.pats.revokedIDs())
		}
	})
	t.Run("an authorization id that is not a uuid is not revoked", func(t *testing.T) {
		fx := newADOPATFixture(t)
		fx.srv.revokeUnrecordedRunPAT(t.Context(), fx.run.ID, fx.ado.snapshot(), adoPAT{AuthorizationID: "not-a-uuid"})
		if len(fx.pats.revokedIDs()) != 0 || len(fx.st.marks) != 0 {
			t.Errorf("revoked %v, closed %v; want nothing for an id that names no token", fx.pats.revokedIDs(), fx.st.marks)
		}
	})
	t.Run("a recorded token is revoked and its row closed as unrecorded", func(t *testing.T) {
		fx := newADOPATFixture(t)
		rows := fx.st.unrevoked(t)
		if len(rows) != 1 {
			t.Fatalf("rows = %+v, want the dispatch token", rows)
		}
		fx.srv.revokeUnrecordedRunPAT(t.Context(), fx.run.ID, fx.ado.snapshot(), adoPAT{AuthorizationID: rows[0].AuthorizationID.String(), Scope: rows[0].Scope, ValidTo: rows[0].ValidTo})
		if got := fx.pats.revokedIDs(); !slices.Equal(got, []string{rows[0].AuthorizationID.String()}) {
			t.Errorf("revoked %v, want the token", got)
		}
		if len(fx.st.marks) != 1 || fx.st.marks[0].reason != adoPATRevokeUnrecorded || fx.st.marks[0].id != rows[0].AuthorizationID {
			t.Errorf("closed %+v, want the row closed as %q", fx.st.marks, adoPATRevokeUnrecorded)
		}
	})
}

// A token created after a restart carries what the run was approved for the whole run, when the
// approvals can be read, and only what was asked for when they cannot.
func TestMiscCovRestartTokenCarriesTheRunsStandingApprovals(t *testing.T) {
	widen := func(t *testing.T, fx *adoPATFixture) {
		t.Helper()
		fx.ok(t, "dev.azure.com", nil)
		ask := url.Values{"capability": {string(adoscope.CapPR)}, "first_use": {string(types.FirstUseWaitForReview)},
			"method": {"POST"}, "path": {"/contoso/proj/_apis/git/repositories/app/pullrequests"}}
		w, _ := fx.resolve(t, "dev.azure.com", ask)
		id := pendingID(t, w, adoCapabilityPendingState)
		if _, err := fx.fa.Decide(context.Background(), id, types.ActorHuman,
			types.ApprovalDecision{State: types.ApprovalApproved, Scope: types.ScopeRun}); err != nil {
			t.Fatal(err)
		}
		if err := fx.srv.dropRunPAT(t.Context(), fx.run.ID); err != nil { // a restart empties the process's cache
			t.Fatal(err)
		}
	}
	lastScope := func(fx *adoPATFixture) string {
		creates := miscCovCreates(fx)
		if len(creates) == 0 {
			t.Fatal("no token was created")
		}
		return creates[len(creates)-1].Scope
	}
	base, _ := adoscope.PATScope([]adoscope.Capability{adoscope.CapCodeRead, adoscope.CapCodeWrite})
	withPR, _ := adoscope.PATScope([]adoscope.Capability{adoscope.CapCodeRead, adoscope.CapCodeWrite, adoscope.CapPR})

	for _, tc := range []struct {
		name      string
		setup     func(*adoPATFixture)
		wantScope string
	}{
		{"approvals readable", func(*adoPATFixture) {}, withPR},
		{"approvals unreadable", func(fx *adoPATFixture) { fx.fa.listErr = errors.New("approvals down") }, base},
		{"no approvals source at all", func(fx *adoPATFixture) { fx.srv.cfg.Approvals = nil }, base},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newADOPATFixture(t)
			widen(t, fx)
			tc.setup(fx)
			fx.ok(t, "dev.azure.com", nil)
			if got := lastScope(fx); got != tc.wantScope {
				t.Errorf("restart token scope = %q, want %q", got, tc.wantScope)
			}
			got := fx.auditReasons(adoPATAuditMint)
			if len(got) == 0 || got[len(got)-1] != adoPATMintRestart {
				t.Errorf("mint reasons = %v, want the last to be restart", got)
			}
		})
	}
}
