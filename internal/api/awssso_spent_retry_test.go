// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

// casDeleteSecrets is revisionedSecrets whose Delete is the compare-and-set
// the Postgres store's is: a delete guarded by a revision the row moved past
// keeps the row.
type casDeleteSecrets struct{ revisionedSecrets }

func (c casDeleteSecrets) For(owner string) secretstore.Store {
	return casDeleteSecrets{revisionedSecrets{c.Store.For(owner)}}
}

func (c casDeleteSecrets) Delete(ctx context.Context, name string) error {
	if want, ok := secretstore.IfRevisionFrom(ctx); ok {
		if rev, err := c.Revision(ctx, name); err != nil || rev != want {
			return errors.Join(secretstore.ErrRevisionChanged, err)
		}
	}
	return c.Store.Delete(ctx, name)
}

// spentRetryServer is ssoRefreshServer over a store that keeps revisions, as
// the Postgres store does: the only kind a spent answer is re-read against.
func spentRetryServer(t *testing.T) (*Server, awsSSOBlob) {
	t.Helper()
	s, _, blob := ssoRefreshServer(t)
	s.cfg.Secrets = casDeleteSecrets{revisionedSecrets{s.cfg.Secrets}}
	return s, blob
}

func answerInvalidGrant(w http.ResponseWriter) {
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid_grant", "error_description": "refresh token is invalid"})
}

// #2137: a renewal the authority answers invalid_grant to, because another
// writer redeemed the same refresh token first and stored the pair it got
// (a rolling upgrade's other replica renews under the person's key), serves
// that winner's pair instead of asking the person to sign in again.
func TestAWSSSORefresh_SpentAfterAWinnerStoredServesTheWinnersPair(t *testing.T) {
	s, blob := spentRetryServer(t)
	winner := blob
	winner.AccessToken, winner.RefreshToken = "winner-access-token-1234567890", "winner-refresh-token-1234567890"
	winner.ExpiresAt = awsSSOTestFixedNow.Add(2 * time.Hour)
	calls := fakeOIDC(t, func(w http.ResponseWriter, _ map[string]string, _ int) {
		storeSSOBlob(t, s, winner)
		answerInvalidGrant(w)
	})

	got, msg := s.refreshAWSSSOBlob(context.Background(), awsSSOTestScope(), blob)
	if msg != "" || got.AccessToken != winner.AccessToken {
		t.Fatalf("refresh = %q %q, want the winner's pair served with no sign-in door", got.AccessToken, msg)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("CreateToken calls = %d, want 1: the winner's pair needed no renewal", n)
	}
	if b := storedSSOBlob(t, s); b != winner {
		t.Errorf("the winner's stored pair changed: %+v", b)
	}
}

// With nothing stored since the read under the lock, a spent answer stands:
// the sign-in door, as before.
func TestAWSSSORefresh_SpentWithTheRowUnmovedKeepsTheSignInDoor(t *testing.T) {
	s, blob := spentRetryServer(t)
	calls := fakeOIDC(t, func(w http.ResponseWriter, _ map[string]string, _ int) { answerInvalidGrant(w) })

	if _, msg := s.refreshAWSSSOBlob(context.Background(), awsSSOTestScope(), blob); msg != awsSSORefreshSpentSentence {
		t.Fatalf("refresh answered %q, want the spent sentence", msg)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("CreateToken calls = %d, want 1", n)
	}
}

// The second chance is one: a retry that is answered spent too, after yet
// another pair was stored, is final.
func TestAWSSSORefresh_SpentRetriesExactlyOnce(t *testing.T) {
	s, blob := spentRetryServer(t)
	calls := fakeOIDC(t, func(w http.ResponseWriter, _ map[string]string, call int) {
		if call <= 3 { // a loop would show as a third call
			next := blob
			next.RefreshToken = fmt.Sprintf("stored-since-%d-refresh-token-1234567890", call)
			storeSSOBlob(t, s, next)
		}
		answerInvalidGrant(w)
	})

	if _, msg := s.refreshAWSSSOBlob(context.Background(), awsSSOTestScope(), blob); msg != awsSSORefreshSpentSentence {
		t.Fatalf("refresh answered %q, want the spent sentence", msg)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("CreateToken calls = %d, want 2: the first attempt and exactly one retry", n)
	}
}
