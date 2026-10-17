// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// A record session renews an expired AWS sign-in at its own door, as run create
// does: its run row is inserted before dispatch, so a session that cannot be
// renewed (or whose renewal cannot be kept) is refused with no row.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// recordSessionStore is an integStore that can also hold a record session's
// workspace claim and result, so a launch the door does not refuse reaches its
// run row.
type recordSessionStore struct{ *integStore }

func (s recordSessionStore) ClaimWorkspaceActiveRun(_ context.Context, id uuid.UUID, runID uuid.UUID, _ *uuid.UUID) (types.Workspace, bool, error) {
	ws := types.Workspace{ID: id, Status: types.WorkspaceScanned}
	ws.ActiveRunID = &runID
	return ws, true, nil
}

func (s recordSessionStore) ClearWorkspaceActiveRun(context.Context, uuid.UUID, uuid.UUID) (bool, error) {
	return true, nil
}

func (s recordSessionStore) SetWorkspaceRecordResult(_ context.Context, id uuid.UUID, _ string, _ json.RawMessage, _ string) (types.Workspace, bool, error) {
	return types.Workspace{ID: id, Status: types.WorkspaceScanned}, true, nil
}

// recordRenewalFixture is a record door whose launcher holds an expired
// session, with a refresh token, for the one Bedrock SSO provider, and that
// launcher's bearer.
func recordRenewalFixture(t *testing.T) (*Server, *types.Workspace, string) {
	t.Helper()
	ws := &types.Workspace{ID: uuid.New(), Name: "hello", Status: types.WorkspaceScanned,
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: govWorkspaceRepo}}}
	srv := providerRunFixture(t, types.SiteConfig{ModelProviders: providerBlock(awsSSOTestProvider())}, &capStore{}, ws)
	bearer := providerAdminToken(srv, createRenewalOwner)
	srv.cfg.Store = recordSessionStore{srv.cfg.Store.(*integStore)}
	srv.cfg.Secrets = &memSecrets{m: map[string][]byte{}, owned: map[string]map[string][]byte{}}
	storeSSOBlobFor(t, srv, createRenewalOwner, createRenewalBlob())
	return srv, ws, bearer
}

// recordRunRows counts the run rows the fixture's store holds.
func recordRunRows(srv *Server) int {
	st := srv.cfg.Store.(recordSessionStore)
	st.mu.Lock()
	defer st.mu.Unlock()
	return len(st.runs)
}

// An expired session whose refresh token AWS refuses: the 422 sign-in door,
// before any row.
func TestRecordRun_RenewsAtTheDoorAndRefusesADeadSession(t *testing.T) {
	srv, ws, bearer := recordRenewalFixture(t)
	calls := fakeOIDC(t, func(w http.ResponseWriter, _ map[string]string, _ int) { invalidGrant(w) })

	code, body := recordDoor(t, srv, bearer, ws, false)
	var b errorBody
	_ = json.Unmarshal([]byte(body), &b)
	if code != http.StatusUnprocessableEntity || b.Reason != llmRefusalAuditReason || b.Provider != "bedrock-sso" {
		t.Errorf("record = %d %s, want the 422 sign-in door: the record session was not refused although its sign-in cannot be renewed", code, body)
	}
	if calls.Load() != 1 {
		t.Errorf("CreateToken calls = %d, want 1: the record door renews", calls.Load())
	}
	if n := recordRunRows(srv); n != 0 {
		t.Errorf("run rows = %d, want none: a run row exists for a sign-in that cannot be renewed", n)
	}
}

// A renewal that cannot be stored: the record door answers the renewal's own
// 503 sentence, as POST /runs does, not the bare read failure.
func TestRecordRun_RenewsAtTheDoorAndRefusesAnUnsavedRenewal(t *testing.T) {
	srv, ws, bearer := recordRenewalFixture(t)
	renewedOIDC(t)
	srv.cfg.Secrets = putScript{Store: srv.cfg.Secrets, only: providerSecretName(awsSSOTestProviderUID, providerSSOPart),
		n: new(atomic.Int32), hook: failEvery("secret store is wedged")}

	code, body := recordDoor(t, srv, bearer, ws, false)
	if code != http.StatusServiceUnavailable || !strings.Contains(body, mpBRPersistFailed) {
		t.Errorf("record = %d %s, want the 503 naming the unsaved renewal", code, body)
	}
	if n := recordRunRows(srv); n != 0 {
		t.Errorf("run rows = %d, want none", n)
	}
}

// An expired session with a live refresh token is renewed and stored by the
// record session's provider choice, before its row: the session may launch.
func TestRecordRun_RenewsAtTheDoorAndStoresTheRenewedPair(t *testing.T) {
	srv, ws, _ := recordRenewalFixture(t)
	calls := renewedOIDC(t)
	choice, err := srv.recordProviderChoice(context.Background(), createRenewalOwner, *ws)
	if err != nil || !choice.chosen {
		t.Fatalf("record provider choice = %+v, %v, want bedrock-sso chosen", choice, err)
	}
	if calls.Load() != 1 {
		t.Errorf("CreateToken calls = %d, want 1: the record door renews", calls.Load())
	}
	if got, _ := createRenewalStored(t, srv); got.RefreshToken != "rotated-refresh-token-abcdefghij" {
		t.Errorf("stored pair = %+v, want the renewed one: dispatch's bootstrap reads the store", got)
	}
}
