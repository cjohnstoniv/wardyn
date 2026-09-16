// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/identity/embedded"
	"github.com/cjohnstoniv/wardyn/internal/identity/identitytest"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// createRunFailingStore is the real PG store with ONE method poisoned, so
// everything POST /runs does before the insert (policy resolution, quota, the
// workspace reads) behaves exactly as in production and only the insert fails.
type createRunFailingStore struct {
	store.Store
	err error
}

func (s createRunFailingStore) CreateRun(context.Context, types.AgentRun) (types.AgentRun, error) {
	return types.AgentRun{}, s.err
}

// TestCreateRunServerErrorDoesNotLeakDriverText (W6-S2) pins the highest-traffic
// member door against the leak class the release says it closed.
//
// `POST /api/v1/runs` is plain-tier — every member reaches it — and its insert
// failure wrote `err.Error()` straight into the 500. A Postgres blip therefore
// answered the member with the deployment's database host, port, user and
// database name. Fifteen sibling sites on the same member-reachable routes
// (/runs/{id}/{files,grants,profile}, /me/tokens, the inline-policy resolvers)
// did the same; they all move to the writeServerError chokepoint with this one.
func TestCreateRunServerErrorDoesNotLeakDriverText(t *testing.T) {
	dsn := os.Getenv("WARDYN_TEST_PG")
	if dsn == "" {
		t.Skip("WARDYN_TEST_PG not set; skipping Postgres-backed create-run test")
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		pool.Close()
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(pool.Close)

	const secret = "host=10.0.0.5 port=5432 user=wardyn database=wardyn"
	audit := &recRecorder{}
	idp, err := embedded.New(nil, "wardyn.local", identitytest.NewMemRevocationStore(), audit)
	if err != nil {
		t.Fatalf("embedded.New: %v", err)
	}
	srv := New(Config{
		Store:       createRunFailingStore{Store: store.NewPG(pool), err: errors.New("pgx: dial: " + secret)},
		Identity:    idp,
		Approvals:   newFakeApprovals(),
		Broker:      &fakeBroker{},
		Audit:       audit,
		Runner:      &fakeRunner{},
		AdminToken:  adminToken,
		TrustDomain: "wardyn.local",
		DefaultPolicy: types.RunPolicySpec{
			AllowedDomains:      []string{"api.anthropic.com"},
			MinConfinementClass: types.CC2,
			AutoStopAfterSec:    -1,
		},
		ControlPlaneURL: "http://wardynd:8080",
	})

	var logged bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, `{"agent":"claude-code","task":"echo hi"}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), secret) {
		t.Errorf("POST /runs leaked the driver's connection string to a member:\n%s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "create run") {
		t.Errorf("the 500 no longer names the action that failed: %s", w.Body.String())
	}
	if !strings.Contains(logged.String(), secret) {
		t.Errorf("the driver error was dropped instead of logged — the operator now has nothing:\n%s", logged.String())
	}
}
