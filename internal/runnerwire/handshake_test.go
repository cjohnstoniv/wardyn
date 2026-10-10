// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestHandshakeStoredOrgBinding(t *testing.T) {
	for _, stored := range []string{"", "wrong", federation.OrgURLSHA256("https://org.example/")} {
		t.Run(stored, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			pub, key, _ := ed25519.GenerateKey(rand.Reader)
			row := types.Runner{ID: uuid.New(), PublicKey: pub, OrgURLSHA256: stored, State: types.RunnerClaimed}
			org, run := Loopback()
			defer run.Close()
			done := make(chan error, 1)
			clientDone := make(chan struct{})
			go func() {
				defer org.Close()
				_, err := AuthenticateRunner(ctx, org, row, "https://org.example")
				if err == nil {
					err = WriteControl(ctx, org, TypeAuthOK, AuthOK{SessionID: uuid.NewString(), PingInterval: PingInterval})
					<-clientDone
				}
				done <- err
			}()
			clockCalled := false
			_, clientErr := authenticateOrg(ctx, run, row.ID.String(), "https://org.example/", key, nil, func(hello Hello, received time.Time) error {
				clockCalled = true
				if hello.ServerTime.IsZero() || received == received.Round(0) {
					t.Error("clock did not preserve server time and monotonic receipt")
				}
				return nil
			})
			close(clientDone)
			serverErr := <-done
			want := stored == federation.OrgURLSHA256("https://org.example")
			if clockCalled != want {
				t.Fatalf("clock hook called across invalid org binding: %v", clockCalled)
			}
			if want && (clientErr != nil || serverErr != nil) {
				t.Fatalf("normalized binding rejected: %v / %v", clientErr, serverErr)
			}
			if !want && (!errors.Is(serverErr, ErrAuth) || clientErr == nil) {
				t.Fatalf("bad stored binding accepted: %v / %v", clientErr, serverErr)
			}
		})
	}
}

func TestHandshakeRefusesExpiredUnclaimed(t *testing.T) {
	now := time.Now()
	row := types.Runner{State: types.RunnerUnclaimed, CreatedAt: now.Add(-types.RunnerUnclaimedTTL)}
	if !row.ClaimExpired(now) || row.ClaimExpired(now.Add(-time.Nanosecond)) {
		t.Fatal("claim expiry boundary changed")
	}
	a, b := Loopback()
	defer a.Close()
	defer b.Close()
	if _, err := AuthenticateRunner(context.Background(), a, row, "https://org.example"); !errors.Is(err, ErrAuth) {
		t.Fatal(err)
	}
	row.CreatedAt = time.Time{}
	if !row.ClaimExpired(now) {
		t.Fatal("zero creation time accepted")
	}
}
