// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestClientRunTellsConnectFailedEachFailure: the hook hears each failed attempt, so the runner can
// report it once it connects again.
func TestClientRunTellsConnectFailedEachFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	failed := make(chan error, 8)
	hooks := idleHooks()
	hooks.ConnectFailed = func(err error) { failed <- err }
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	client, err := NewClient(ClientConfig{OrgURL: server.URL, RunnerID: uuid.NewString(), PrivateKey: key, Hooks: hooks})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()
	select {
	case err := <-failed:
		if err == nil || ConnectStage(err) != types.RunnerStageHealthz {
			t.Fatalf("hook got %v (stage %q), want a healthz failure", err, ConnectStage(err))
		}
	case <-ctx.Done():
		t.Fatal("ConnectFailed never called")
	}
	cancel()
	<-done
}

// TestClientRunSkipsTheOrgsGoawayInConnectFailed: a GOAWAY is the organisation sending the runner away,
// not a failure to report back to it; the next attempt's failure is.
func TestClientRunSkipsTheOrgsGoawayInConnectFailed(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	row := types.Runner{ID: uuid.New(), PublicKey: pub, OrgURLSHA256: federation.OrgURLSHA256("https://org.example"),
		State: types.RunnerUnclaimed, CreatedAt: time.Now().Add(-types.RunnerUnclaimedTTL)}
	refused := errors.New("dial tcp 192.0.2.1:443: connect: connection refused")
	failed := make(chan error, 8)
	hooks := idleHooks()
	hooks.ConnectFailed = func(err error) { failed <- err }
	dials := 0
	client, err := NewClient(ClientConfig{OrgURL: "https://org.example", RunnerID: row.ID.String(), PrivateKey: key, Hooks: hooks,
		Dial: func(context.Context) (Conn, error) {
			dials++
			if dials > 1 {
				return nil, refused
			}
			org, run := Loopback()
			go func() {
				defer org.Close()
				_, _ = AuthenticateRunner(ctx, org, row, "https://org.example")
			}()
			return run, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()
	select {
	case err := <-failed:
		if !errors.Is(err, refused) {
			t.Fatalf("first ConnectFailed = %v, want the second attempt's refusal, not the GOAWAY", err)
		}
	case <-ctx.Done():
		t.Fatal("ConnectFailed never called")
	}
	cancel()
	<-done
}

func TestConnectStageNamesTheDoctorStage(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("connect: %w", ErrAuth), types.RunnerStageIdentity},
		{&net.OpError{Op: "proxyconnect", Err: errors.New("refused")}, types.RunnerStageProxy},
		{fmt.Errorf("tls: %w", x509.UnknownAuthorityError{}), types.RunnerStageTLS},
		{&net.DNSError{Err: "no such host", Name: "org.example"}, types.RunnerStageDNS},
		{&net.OpError{Op: "dial", Err: errors.New("refused")}, types.RunnerStageTLS},
		{errors.New("unexpected status 503"), types.RunnerStageHealthz},
	} {
		if got := ConnectStage(c.err); got != c.want {
			t.Errorf("ConnectStage(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}
