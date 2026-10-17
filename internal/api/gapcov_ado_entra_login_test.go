// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

// gapCovLoginGrant is a login grant that names every scope of the fixture's row.
func gapCovLoginGrant(f *adoFixture) oidc.LoginGrant {
	granted := strings.Join(append([]string{"openid", "profile", "email", entraOfflineAccessScope}, f.cfg.Scopes...), " ")
	return oidc.LoginGrant{RefreshToken: "the-refresh-token-this-login-earned", Scope: granted, Expiry: adoTestNow.Add(time.Hour)}
}

// A credential that cannot be put on record for masking is not stored, and the failure is audited
// as a store error without the token.
func TestGapCovCaptureLoginGrantNotStoredWhenMaskingCannotRecordIt(t *testing.T) {
	f := newADOFixture(t)
	reg := secretmask.NewRegistry()
	reg.SetBackend(&gapCovMaskBackend{mergeErr: errors.New("gapcov: corpus write refused")})
	f.srv.cfg.MaskRegistry = reg

	f.srv.CaptureLoginGrant(context.Background(), "a-person", gapCovLoginGrant(f))

	if _, found := f.stored(t, "a-person"); found {
		t.Fatal("a credential was stored though masking could not record it")
	}
	rows := f.audit.find(adoSignInCapturedAction)
	if len(rows) != 1 || rows[0].Outcome != "failure" || !strings.Contains(string(rows[0].Data), `"reason":"store_error"`) {
		t.Fatalf("audit rows = %+v, want one store_error failure", rows)
	}
	if strings.Contains(string(rows[0].Data), "the-refresh-token-this-login-earned") {
		t.Errorf("the failure row carries the token: %s", rows[0].Data)
	}
}

// A suspension that overtakes the login leaves nothing stored and no capture row, success or failure.
func TestGapCovCaptureLoginGrantStoresNothingWhenTheIdentityIsSuspendedMidLogin(t *testing.T) {
	f := newADOFixture(t)
	f.srv.cfg.Store = &gapCovSharer{refuse: store.ErrIdentityDeactivated}

	f.srv.CaptureLoginGrant(context.Background(), "a-person", gapCovLoginGrant(f))

	if _, found := f.stored(t, "a-person"); found {
		t.Fatal("a credential was stored for a suspended identity")
	}
	if rows := f.audit.find(adoSignInCapturedAction); len(rows) != 0 {
		t.Fatalf("audit rows = %+v, want none", rows)
	}
}

// Failing to retire the replaced token does not undo the capture: the credential is stored and the
// success row written.
func TestGapCovCaptureLoginGrantStaysCapturedWhenTheReplacedTokenCannotBeRetired(t *testing.T) {
	f := newADOFixture(t)
	reg := secretmask.NewRegistry()
	reg.SetBackend(&gapCovMaskBackend{addErr: errors.New("gapcov: retire refused")})
	f.srv.cfg.MaskRegistry = reg
	logs := miscCovCaptureLogs(t)

	f.srv.CaptureLoginGrant(context.Background(), "a-person", gapCovLoginGrant(f))

	blob, found := f.stored(t, "a-person")
	if !found || blob.RefreshToken != "the-refresh-token-this-login-earned" {
		t.Fatalf("stored = %+v (found %v), want the captured credential", blob, found)
	}
	if rows := f.audit.find(adoSignInCapturedAction); len(rows) != 1 || rows[0].Outcome != "success" {
		t.Fatalf("audit rows = %+v, want one success row", rows)
	}
	if _, ok := logs.find("the replaced Azure DevOps sign-in token could not be retired"); !ok {
		t.Error("the failed retirement was not logged")
	}
}
