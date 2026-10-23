// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// A sign-in token pasted before its sandbox is RUNNING (#3449), against a real
// Postgres: the launch is parked at the state the paste lands in, and the
// post-capture kill's swap is made to lose to the launch's next move. The run
// must still end KILLED with no sandbox left running.
// Guarded by WARDYN_TEST_PG (throwawayPGPool); skipped cleanly when unset.

package api

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// captureRaceStore is the real PG store with one seam. The launch's own move
// out of `hold` parks until the post-capture kill tries its swap from `hold`;
// that swap then waits for the launch's move to land, so it runs against a
// state it did not read and loses.
type captureRaceStore struct {
	store.PG
	site     types.SiteConfig
	audit    *memAudit
	hold     types.RunState
	parked   chan struct{} // closed when the launch is parked in `hold`
	release  chan struct{} // closed by the kill's swap
	moved    chan struct{} // closed once the launch's move out of `hold` has landed
	parkOnce sync.Once
	raceOnce sync.Once
	raced    bool // the raced swap ran and lost, so the test forced what it claims
}

func (s *captureRaceStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return s.site, nil
}

// QueryAuditEvents serves the run's harness.login.start stamp from the audit
// sink, as signInStore does.
func (s *captureRaceStore) QueryAuditEvents(_ context.Context, runID uuid.UUID, _ int) ([]types.AuditEvent, error) {
	var out []types.AuditEvent
	for _, ev := range s.audit.find("harness.login.start") {
		if ev.RunID != nil && *ev.RunID == runID {
			out = append(out, ev)
		}
	}
	return out, nil
}

func (s *captureRaceStore) UpdateRunStateIf(ctx context.Context, id uuid.UUID, from, to types.RunState) (bool, error) {
	switch {
	case from == s.hold && to != types.RunKilled:
		held := false
		s.parkOnce.Do(func() {
			held = true
			close(s.parked)
			<-s.release
		})
		applied, err := s.PG.UpdateRunStateIf(ctx, id, from, to)
		if held {
			close(s.moved)
		}
		return applied, err
	case from == s.hold:
		first := false
		s.raceOnce.Do(func() {
			first = true
			close(s.release)
			<-s.moved
		})
		applied, err := s.PG.UpdateRunStateIf(ctx, id, from, to)
		if first {
			s.raced = err == nil && !applied
		}
		return applied, err
	}
	return s.PG.UpdateRunStateIf(ctx, id, from, to)
}

// liveSandboxRunner is fakeRunner keeping the set of sandboxes created and not
// yet stopped or killed.
type liveSandboxRunner struct {
	*fakeRunner
	mu   sync.Mutex
	live map[string]bool
}

func (r *liveSandboxRunner) CreateSandbox(ctx context.Context, spec runner.SandboxSpec) (runner.Sandbox, error) {
	sb, err := r.fakeRunner.CreateSandbox(ctx, spec)
	if err == nil {
		r.mu.Lock()
		r.live[sb.Ref] = true
		r.mu.Unlock()
	}
	return sb, err
}

func (r *liveSandboxRunner) StopSandbox(_ context.Context, ref string) error {
	r.mu.Lock()
	delete(r.live, ref)
	r.mu.Unlock()
	return nil
}

func (r *liveSandboxRunner) KillSandbox(ctx context.Context, ref string) error {
	return r.StopSandbox(ctx, ref)
}

func TestPG_SignInCapturedBeforeRunningIsKilled(t *testing.T) {
	const token = "sk-ant-oat01-member-own-claude-sign-in"
	site := credentialSite(subProvider("claude"))
	p := site.ModelProviders.Providers[0]
	member := ssoSession(t, "sub-member", "member@corp.example", oidc.RoleUser)

	for _, hold := range []types.RunState{types.RunPending, types.RunStarting} {
		t.Run(fmt.Sprintf("stored while %s", hold), func(t *testing.T) {
			audit := &memAudit{}
			st := &captureRaceStore{
				PG: store.NewPG(throwawayPGPool(t)), site: site, audit: audit, hold: hold,
				parked: make(chan struct{}), release: make(chan struct{}), moved: make(chan struct{}),
			}
			rn := &liveSandboxRunner{fakeRunner: &fakeRunner{}, live: map[string]bool{}}
			h := newHarness(t)
			cfg := baseTestConfig(h, st)
			cfg.Audit = audit
			cfg.OIDC = &oidc.Authenticator{}
			cfg.Runner = rn
			cfg.Secrets = &memSecrets{m: map[string][]byte{}, owned: map[string]map[string][]byte{}}
			cfg.MaskRegistry = secretmask.NewRegistry()
			cfg.AgentImages = map[string]string{"claude-code": "wardyn/agent-claude-code:local"}
			cfg.DefaultPolicy = govDeployment()
			srv := New(cfg)
			srv.signInCaptureKillGrace = 0

			code, body := signIn(t, srv, member, p.ID)
			if code != http.StatusOK {
				t.Fatalf("sign-in = %d %s", code, body)
			}
			runID := signInRunID(t, body)
			select {
			case <-st.parked:
			case <-time.After(30 * time.Second):
				t.Fatalf("the sign-in launch never reached %s", hold)
			}
			if got, err := st.GetRun(context.Background(), runID); err != nil || got.State != hold {
				t.Fatalf("sign-in run before the capture = %s (%v), want %s", got.State, err, hold)
			}

			w := doSSO(t, srv, http.MethodPut, "/api/v1/model-providers/"+p.ID+"/sign-in", member,
				fmt.Sprintf(`{"run_id":%q,"token":%q}`, runID, token))
			if w.Code != http.StatusNoContent {
				t.Fatalf("capture = %d %s, want 204", w.Code, w.Body.String())
			}
			srv.WaitBackground()
			if !st.raced {
				t.Fatalf("the kill's swap from %s did not lose to the launch: the race was not forced", hold)
			}

			got, err := st.GetRun(context.Background(), runID)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != types.RunKilled {
				t.Fatalf("sign-in run is %s after a capture stored while %s, want KILLED", got.State, hold)
			}
			rn.mu.Lock()
			live := slices.Sorted(maps.Keys(rn.live))
			rn.mu.Unlock()
			if len(live) != 0 {
				t.Fatalf("sign-in sandboxes still running after a capture stored while %s: %v", hold, live)
			}
			if data := killData(t, killRow(t, audit, runID.String())); data["reason"] != signInCapturedReason {
				t.Errorf("run.kill reason = %v, want %q", data["reason"], signInCapturedReason)
			}
		})
	}
}
