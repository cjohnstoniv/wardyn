// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// limitRunnerStore serves several runner rows and counts site-config reads.
type limitRunnerStore struct {
	streamRunnerStore
	rows        map[uuid.UUID]types.Runner
	configReads *atomic.Int32
}

func (s limitRunnerStore) GetSiteConfig(ctx context.Context) (types.SiteConfig, error) {
	s.configReads.Add(1)
	return s.streamRunnerStore.GetSiteConfig(ctx)
}

func (s limitRunnerStore) GetRunner(_ context.Context, id uuid.UUID) (types.Runner, error) {
	if row, ok := s.rows[id]; ok {
		return row, nil
	}
	return types.Runner{}, store.ErrNotFound
}

type streamLimitHarness struct {
	url         string
	srv         *Server
	keys        map[uuid.UUID]ed25519.PrivateKey
	handled     chan struct{}
	configReads atomic.Int32
}

// newStreamLimitHarness serves n unclaimed runners, which a verified handshake
// answers with AUTH_OK then GOAWAY, so no admission is involved. Every client
// dials from the same loopback address and the limiter clock never advances.
func newStreamLimitHarness(t *testing.T, ctx context.Context, n int, trusted ...netip.Prefix) *streamLimitHarness {
	t.Helper()
	h := &streamLimitHarness{keys: map[uuid.UUID]ed25519.PrivateKey{}, handled: make(chan struct{}, 64)}
	rows := map[uuid.UUID]types.Runner{}
	for range n {
		pub, key, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		row := types.Runner{ID: uuid.New(), PublicKey: pub, OrgURLSHA256: federation.OrgURLSHA256("https://org.example"),
			State: types.RunnerUnclaimed, CreatedAt: time.Now()}
		rows[row.ID], h.keys[row.ID] = row, key
	}
	s := New(Config{RunnerOrgURL: "https://org.example", BaseCtx: ctx, Now: func() time.Time { return time.Unix(100, 0) }, TrustedProxies: trusted,
		Store: limitRunnerStore{streamRunnerStore: streamRunnerStore{settings: &types.RunnerSettings{Enabled: true}}, rows: rows, configReads: &h.configReads}})
	handler := panicFails(t, s.Handler())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r)
		h.handled <- struct{}{}
	}))
	t.Cleanup(server.Close)
	h.url, h.srv = "ws"+strings.TrimPrefix(server.URL, "http"), s
	return h
}

// dial runs one handshake and waits until the server finished the request, so
// every limiter charge has landed before the next dial. It returns the HTTP
// status (101 once upgraded) and the handshake result.
func (h *streamLimitHarness) dial(t *testing.T, ctx context.Context, id uuid.UUID, key ed25519.PrivateKey) (int, error) {
	t.Helper()
	return h.dialFrom(t, ctx, id, key, "")
}

// dialFrom is dial with an X-Forwarded-For header, when xff is not empty.
func (h *streamLimitHarness) dialFrom(t *testing.T, ctx context.Context, id uuid.UUID, key ed25519.PrivateKey, xff string) (int, error) {
	t.Helper()
	opts := &websocket.DialOptions{HTTPHeader: http.Header{}}
	if xff != "" {
		opts.HTTPHeader.Set("X-Forwarded-For", xff)
	}
	conn, resp, err := websocket.Dial(ctx, h.url+"/api/v1/runners/"+id.String()+"/stream", opts)
	status := 0
	if resp != nil {
		status = resp.StatusCode
	}
	if err == nil {
		_, err = runnerwire.AuthenticateOrg(ctx, runnerwire.NewWebSocket(conn), id.String(), "https://org.example", key, nil)
		conn.CloseNow()
	}
	select {
	case <-h.handled:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	return status, err
}

func TestRunnerStreamFailedHandshakesFromOneAddressAreLimited(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	h := newStreamLimitHarness(t, ctx, 1)
	_, wrong, _ := ed25519.GenerateKey(rand.Reader)
	for id := range h.keys {
		for i := range int(enrolBurst) {
			if status, err := h.dial(t, ctx, id, wrong); status != http.StatusSwitchingProtocols || err == nil {
				t.Fatalf("failed handshake %d: status %d, err %v", i, status, err)
			}
		}
		if status, _ := h.dial(t, ctx, id, wrong); status != http.StatusTooManyRequests {
			t.Fatalf("failed handshake past the burst: status %d, want 429", status)
		}
	}
}

func TestRunnerStreamLimitsOneRunnerReconnectingInALoop(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	h := newStreamLimitHarness(t, ctx, 1)
	for id, key := range h.keys {
		for i := range int(enrolBurst) {
			if status, err := h.dial(t, ctx, id, key); status != http.StatusSwitchingProtocols || err != nil {
				t.Fatalf("connect %d: status %d, err %v", i, status, err)
			}
		}
		if _, err := h.dial(t, ctx, id, key); err == nil {
			t.Fatal("runner reconnecting past the burst was not limited")
		}
	}
}

func TestRunnerStreamDistinctRunnersBehindOneAddressAllConnect(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	h := newStreamLimitHarness(t, ctx, int(enrolBurst)+2)
	for id, key := range h.keys {
		if status, err := h.dial(t, ctx, id, key); status != http.StatusSwitchingProtocols || err != nil {
			t.Fatalf("runner %s: status %d, err %v", id, status, err)
		}
	}
}

func TestRunnerStreamAddressLimitPrecedesSiteConfigRead(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	h := newStreamLimitHarness(t, ctx, 0)
	for range int(enrolBurst) {
		_, _ = h.dial(t, ctx, uuid.New(), nil)
	}
	reads := h.configReads.Load()
	if status, _ := h.dial(t, ctx, uuid.New(), nil); status != http.StatusTooManyRequests || h.configReads.Load() != reads {
		t.Fatalf("refused connection: status %d, site-config reads %d -> %d", status, reads, h.configReads.Load())
	}
}

// Connections held open without AUTH never finish their handler, so the address
// bucket must be charged when each one arrives, not when it ends.
func TestRunnerStreamBoundsUnauthenticatedConnectionsHeldOpenFromOneAddress(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	h := newStreamLimitHarness(t, ctx, 1)
	for id := range h.keys {
		for i := range int(enrolBurst) + 5 {
			conn, resp, err := websocket.Dial(ctx, h.url+"/api/v1/runners/"+id.String()+"/stream", nil)
			if err == nil {
				defer conn.CloseNow()
			}
			status := 0
			if resp != nil {
				status = resp.StatusCode
			}
			past := i >= int(enrolBurst)
			if past && status != http.StatusTooManyRequests {
				t.Fatalf("held connection %d past the burst: status %d; want 429", i, status)
			}
			if !past && err != nil {
				t.Fatalf("held connection %d within the burst: %v", i, err)
			}
		}
	}
	if reads := h.configReads.Load(); reads > int32(enrolBurst) {
		t.Fatalf("site-config reads %d, want at most %d", reads, int(enrolBurst))
	}
}
