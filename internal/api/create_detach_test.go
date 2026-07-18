// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// This file pins the CLIENT-DISCONNECT ISOLATION invariant on handleCreateRun's
// image-build block — the same one dispatchWithVerify and handleKillRun
// (C4) already detach for, and which the build block that runs BEFORE dispatch's
// detach point never got. A build is a multi-minute docker pull+build that
// honours cancellation, so a client Ctrl-C / closed tab / LB read timeout aborts
// it AND takes its FAILED-compensator down with it: failAndRevoke's CAS runs on
// the same dead ctx, so the run strands PENDING with no state write, no revoke,
// and no audit until the next daemon boot reconciles it.

// createDetachStore models PG's ACTUAL ctx behaviour, which is the whole point
// of the test: PG.UpdateRunStateIf executes Pool.Exec(ctx, ...), so a cancelled
// ctx returns (false, wrapped context.Canceled) — never (true, nil). A fake that
// ignored ctx would make the compensator look like it worked.
type createDetachStore struct {
	store.Store
	mu   sync.Mutex
	runs map[uuid.UUID]types.AgentRun
}

func newCreateDetachStore() *createDetachStore {
	return &createDetachStore{runs: map[uuid.UUID]types.AgentRun{}}
}

func (s *createDetachStore) CreateRun(_ context.Context, run types.AgentRun) (types.AgentRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runs[run.ID] = run
	return run, nil
}

func (s *createDetachStore) GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	if err := ctx.Err(); err != nil {
		return types.AgentRun{}, fmt.Errorf("get run: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	run, ok := s.runs[id]
	if !ok {
		return types.AgentRun{}, store.ErrNotFound
	}
	return run, nil
}

func (s *createDetachStore) ListRuns(context.Context) ([]types.AgentRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]types.AgentRun, 0, len(s.runs))
	for _, r := range s.runs {
		out = append(out, r)
	}
	return out, nil
}

func (s *createDetachStore) UpdateRunStateIf(ctx context.Context, id uuid.UUID, from, to types.RunState) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, fmt.Errorf("update run state: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	run, ok := s.runs[id]
	if !ok || run.State != from {
		return false, nil
	}
	run.State = to
	s.runs[id] = run
	return true, nil
}

func (s *createDetachStore) SetRunImage(context.Context, uuid.UUID, string) error { return nil }
func (s *createDetachStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return types.SiteConfig{}, nil
}

func (s *createDetachStore) state(id uuid.UUID) types.RunState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runs[id].State
}

// disconnectBuilder models the real hazard: the client goes away mid-build, so
// the build's ctx dies and the build errors out — envbuild's FinalizeBase threads
// ctx into docker API calls that honour cancellation.
type disconnectBuilder struct {
	disconnect func()
	err        error
	buildCtx   context.Context
	mu         sync.Mutex
}

func (b *disconnectBuilder) FinalizeBase(ctx context.Context, _, _ string) (string, error) {
	b.mu.Lock()
	b.buildCtx = ctx
	b.mu.Unlock()
	b.disconnect() // the client hangs up while the image pulls
	return "", b.err
}

func (b *disconnectBuilder) BuildDevcontainer(context.Context, string, string, string) (string, error) {
	return "", nil
}

func (b *disconnectBuilder) BuildFromDevcontainerFiles(context.Context, map[string]string, string) (string, error) {
	return "", nil
}

func (b *disconnectBuilder) observedCtx() context.Context {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buildCtx
}

// TestCreateRun_ClientDisconnectDuringBuild_StillCompensates is the
// counterfactual: a BYOI build fails while the client is already gone. The run
// must still land FAILED with its credentials revoked — the compensator exists
// precisely for the build-failure case, and the single most likely producer of a
// build failure (a disconnect) must not be the one case that silently disables
// it. Counterfactual: on the request ctx, failAndRevoke's CAS returns
// (false, context.Canceled), which the old code collapsed into "someone else
// won" — so the run stays PENDING, nothing is revoked, and no audit is written.
func TestCreateRun_ClientDisconnectDuringBuild_StillCompensates(t *testing.T) {
	h := newHarness(t)
	st := newCreateDetachStore()
	brk := &raceBroker{}
	cfg := baseTestConfig(h, st)
	cfg.Broker = brk
	audit := &recRecorder{}
	cfg.Audit = audit
	// No Runner: the build block under test runs before dispatch, and a nil runner
	// keeps the run at its post-build state so we can read the compensator's write.
	srv := New(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	builder := &disconnectBuilder{disconnect: cancel, err: errors.New("docker: pull: context canceled")}
	srv.cfg.ImageBuilder = builder

	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs",
		strings.NewReader(`{"agent":"claude-code","repo":"acme/widgets","task":"do the thing","image":"ubuntu:24.04"}`))
	r.Header.Set("Authorization", "Bearer "+adminToken)
	r.Host = "127.0.0.1"
	r.RemoteAddr = "127.0.0.1:54321"
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r.WithContext(ctx))

	if len(st.runs) != 1 {
		t.Fatalf("expected exactly one run row, got %d", len(st.runs))
	}
	var runID uuid.UUID
	for id := range st.runs {
		runID = id
	}

	// The build ctx must be detached from the client's, and carry its own deadline
	// rather than letting the connection be the de-facto build timeout.
	bctx := builder.observedCtx()
	if bctx == nil {
		t.Fatal("the builder was never invoked")
	}
	if dl, ok := bctx.Deadline(); !ok {
		t.Error("an image build must carry its own explicit deadline, not inherit the client's connection as one")
	} else if until := time.Until(dl); until <= 0 || until > imageBuildTimeout+time.Minute {
		t.Errorf("build deadline = %v out, want ~%v", until, imageBuildTimeout)
	}

	if got := st.state(runID); got != types.RunFailed {
		t.Fatalf("a build failure must land the run FAILED even when the client disconnected mid-build; state = %q — a PENDING run here is stranded until the next daemon boot, with no audit and no revoke", got)
	}
	if revs := brk.revocations(runID); revs != 1 {
		t.Errorf("the FAILED run's credentials must be revoked exactly once (cascade-on-every-stop); got %d", revs)
	}
	if ev := findAudit(audit.events, runID, "run.build", "failure"); ev == nil {
		t.Errorf("the build failure must be audited; events=%s", auditDump(audit.events, runID))
	}
}
