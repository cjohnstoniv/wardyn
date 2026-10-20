// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
)

func stageClient(t *testing.T, orgURL string, link func(bool, string, Stage)) *Client {
	t.Helper()
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	c, err := NewClient(ClientConfig{OrgURL: orgURL, RunnerID: uuid.NewString(), PrivateKey: key, Hooks: idleHooks(), LinkChanged: link})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// A failed connect names where it stopped, from the error's type: a refused upgrade, a certificate that
// is not trusted, and a port nothing listens on each land on their own stage.
func TestConnectRecordsWhereTheAttemptStopped(t *testing.T) {
	refuse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer refuse.Close()
	untrusted := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer untrusted.Close()
	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close()
	for _, c := range []struct {
		name, url string
		want      Stage
	}{
		{"upgrade", refuse.URL, StageUpgrade},
		{"tls", untrusted.URL, StageTLS},
		{"network", closedURL, StageNetwork},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			err := stageClient(t, c.url, nil).Connect(ctx)
			if err == nil {
				t.Fatal("connect succeeded")
			}
			if got := StageOf(err); got != c.want {
				t.Errorf("stage = %q, want %q (%v)", got, c.want, err)
			}
		})
	}
}

// Run tells LinkChanged the stage with the reason, and keeps the error's own text and chain.
func TestRunReportsTheStageOfAFailedAttempt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer server.Close()
	var mu sync.Mutex
	var got []Stage
	var reason string
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	c := stageClient(t, server.URL, func(up bool, why string, stage Stage) {
		mu.Lock()
		defer mu.Unlock()
		if !up && why != "" {
			got, reason = append(got, stage), why
			cancel()
		}
	})
	_ = c.Run(ctx)
	mu.Lock()
	defer mu.Unlock()
	if len(got) == 0 || got[0] != StageUpgrade || !strings.Contains(reason, "403") {
		t.Errorf("link reports stage %v reason %q, want upgrade with the 403", got, reason)
	}
}

func TestStageClassifiesByErrorType(t *testing.T) {
	refused := &proxyRefusedError{err: errors.New("Proxy Authentication Required"), status: http.StatusProxyAuthRequired, proxy: "proxy.corp:3128"}
	cases := []struct {
		name string
		got  Stage
		want Stage
	}{
		{"dns", dialStage(&net.OpError{Op: "dial", Err: &net.DNSError{Name: "org.example", Err: "no such host"}}), StageDNS},
		{"proxy dns is the proxy", dialStage(&net.OpError{Op: "proxyconnect", Err: &net.OpError{Op: "dial", Err: &net.DNSError{Name: "proxy.corp"}}}), StageProxy},
		{"proxy refusal", dialStage(fmt.Errorf("dial: %w", refused)), StageProxy},
		{"certificate", dialStage(fmt.Errorf("dial: %w", x509.UnknownAuthorityError{})), StageTLS},
		{"anything else is the network", dialStage(errors.New("i/o timeout")), StageNetwork},
		{"auth", authStage(fmt.Errorf("%w: proof", ErrAuth)), StageAuth},
		{"goaway", authStage(fmt.Errorf("%w: lapsed", ErrGoaway)), StageRegistration},
		{"revoked", authStage(ErrRevoked), StageRegistration},
		{"a failed handshake read", authStage(errors.New("EOF")), StageSession},
		{"no stage recorded", StageOf(errors.New("plain")), StageSession},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: stage = %q, want %q", c.name, c.got, c.want)
		}
	}
}

// What leaves the runner is a stage from the closed list and a time, never the error's text.
func TestFailureOnTheWireHoldsNoText(t *testing.T) {
	b, err := json.Marshal(Caps{LastFailure: &Failure{Stage: StageProxy, At: time.Unix(1700000000, 0).UTC()}})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	f, _ := m["last_failure"].(map[string]any)
	if len(f) != 2 || f["stage"] != "proxy" || f["at"] == nil {
		t.Errorf("last_failure = %v, want only stage and at", f)
	}
	for _, s := range Stages {
		if !s.Valid() || strings.ContainsAny(string(s), " :/@") {
			t.Errorf("stage %q is not a plain closed word", s)
		}
	}
	if Stage("proxy: dial http://u:p@h").Valid() {
		t.Error("a free-form string passes as a stage")
	}
}

// The handshake's own refusals land on their stages: a hello for another organisation is the key not
// being accepted, and a GOAWAY for a registration the organisation will not admit is the registration.
func TestConnectRecordsTheHandshakeStage(t *testing.T) {
	const orgURL = "https://org.example"
	for _, c := range []struct {
		name string
		hash string
		then func(ctx context.Context, conn Conn)
		want Stage
	}{
		{"another organisation", "not-the-binding", func(context.Context, Conn) {}, StageAuth},
		{"a registration it will not admit", federation.OrgURLSHA256(orgURL), func(ctx context.Context, conn Conn) {
			_, _ = conn.ReadFrame(ctx)
			_ = WriteControl(ctx, conn, TypeGoaway, Reason{Reason: "runner_unclaimed"})
		}, StageRegistration},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			org, local := Loopback()
			go func() {
				nonce := make([]byte, nonceLen)
				_ = WriteControl(ctx, org, TypeHello, Hello{Versions: []int{ProtocolVersion}, Nonce: nonce, ServerTime: time.Now().UTC(), OrgURLSHA256: c.hash})
				c.then(ctx, org)
			}()
			_, key, _ := ed25519.GenerateKey(rand.Reader)
			client, err := NewClient(ClientConfig{OrgURL: orgURL, RunnerID: uuid.NewString(), PrivateKey: key, Hooks: idleHooks(),
				Dial: func(context.Context) (Conn, error) { return local, nil }})
			if err != nil {
				t.Fatal(err)
			}
			err = client.Connect(ctx)
			if got := StageOf(err); err == nil || got != c.want {
				t.Errorf("stage = %q, want %q (%v)", got, c.want, err)
			}
		})
	}
}
