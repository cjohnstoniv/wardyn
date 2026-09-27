// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// proxyConfigStore is dispatchTestStore with the run_proxy_configs row.
type proxyConfigStore struct {
	*dispatchTestStore
	mu     sync.Mutex
	sealed map[uuid.UUID][]byte
	putErr error
}

var _ store.RunProxyConfigs = (*proxyConfigStore)(nil)

func (s *proxyConfigStore) PutRunProxyConfig(_ context.Context, id uuid.UUID, sealed []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.putErr != nil {
		return s.putErr
	}
	s.sealed[id] = sealed
	return nil
}

func (s *proxyConfigStore) GetRunProxyConfig(_ context.Context, id uuid.UUID) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b, ok := s.sealed[id]; ok {
		return b, nil
	}
	return nil, store.ErrNotFound
}

func (s *proxyConfigStore) DeleteRunProxyConfig(_ context.Context, id uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sealed, id)
	return nil
}

func (s *proxyConfigStore) PurgeTerminalRunProxyConfigs(context.Context) (int64, error) {
	return 0, nil
}

func (s *proxyConfigStore) has(id uuid.UUID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.sealed[id]
	return ok
}

// dispatchWithProxyConfigStore is dispatchTeardownFixture over a
// proxyConfigStore, with a run config key.
func dispatchWithProxyConfigStore(t *testing.T, rn *killRaceRunner) (*Server, *proxyConfigStore, types.AgentRun) {
	t.Helper()
	srv, st, _, run := dispatchTeardownFixture(t, rn, types.RunPending)
	ps := &proxyConfigStore{dispatchTestStore: st, sealed: map[uuid.UUID][]byte{}}
	srv.cfg.Store = ps
	srv.cfg.RunConfigKey = make([]byte, kek.DEKSize)
	return srv, ps, run
}

// TestDispatch_KeepsTheProxyConfigBeforeCreatingTheSandbox (#1176): dispatch
// stores the run's rendered proxy config, sealed, before any proxy holds it,
// so a revive never has to read it back from a container. The row opens only
// as this run's config and carries its token; the sealed bytes do not.
func TestDispatch_KeepsTheProxyConfigBeforeCreatingTheSandbox(t *testing.T) {
	rn := &killRaceRunner{fakeRunner: &fakeRunner{}}
	srv, ps, run := dispatchWithProxyConfigStore(t, rn)
	stored := false
	rn.onCreate = func() { stored = ps.has(run.ID) }

	srv.dispatchRun(context.Background(), run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), dispatchParams{
		RunToken: "run-token-1176", Image: "wardyn/claude-code:latest",
		Policy: types.RunPolicySpec{MinConfinementClass: types.CC1},
	})
	if !stored {
		t.Fatal("the proxy config was not stored before CreateSandbox")
	}
	if strings.Contains(string(ps.sealed[run.ID]), "run-token-1176") {
		t.Error("the stored row holds the run token in the clear")
	}
	raw, err := srv.loadRunProxyConfig(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("loadRunProxyConfig: %v", err)
	}
	var cfg proxy.Config
	if err := json.Unmarshal(raw, &cfg); err != nil || cfg.RunToken != "run-token-1176" || cfg.RunID != run.ID {
		t.Fatalf("stored config = %s, %v; want this run's config with its token", raw, err)
	}
	other := uuid.New()
	ps.sealed[other] = ps.sealed[run.ID]
	if _, err := srv.loadRunProxyConfig(context.Background(), other); err == nil {
		t.Error("a run's sealed config opened as another run's")
	}
}

// TestDispatch_AProxyConfigThatCannotBeStoredFailsTheRun: a run whose config
// cannot be stored never gets a sandbox, so no proxy runs on a config a
// revive could not rebuild.
func TestDispatch_AProxyConfigThatCannotBeStoredFailsTheRun(t *testing.T) {
	rn := &killRaceRunner{fakeRunner: &fakeRunner{}}
	srv, ps, run := dispatchWithProxyConfigStore(t, rn)
	ps.putErr = errors.New("pgx: host=db.internal SQLSTATE 53300 too many connections")
	created := false
	rn.onCreate = func() { created = true }

	srv.dispatchRun(context.Background(), run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), dispatchParams{
		RunToken: "run-token", Image: "wardyn/claude-code:latest",
		Policy: types.RunPolicySpec{MinConfinementClass: types.CC1},
	})
	if created {
		t.Error("CreateSandbox ran for a run whose proxy config was not stored")
	}
	if got := ps.State(); got != types.RunFailed {
		t.Errorf("state = %s, want FAILED", got)
	}
	if hint := ps.FailureHint(); strings.Contains(hint, "db.internal") {
		t.Errorf("failure hint %q carries the database's error text", hint)
	}
}

// TestRevive_RebuildsFromTheStoredConfigForEveryKeptKind (#1176): a run kept
// by an outage, a reboot or its own end is revived from its stored config
// alone (the runner holds none), under a fresh token and with the same MITM
// CA, so an agent still running (outage) keeps trusting its new proxy. The
// stored row then holds the config the new proxy runs. With no stored row the
// revive is refused and nothing is replaced.
func TestRevive_RebuildsFromTheStoredConfigForEveryKeptKind(t *testing.T) {
	kinds := map[string]func(t *testing.T) *reviveFixture{
		"outage": newReviveFixture,
		"reboot": func(t *testing.T) *reviveFixture { f, _ := newRebootFixture(t); return f },
		"ended": func(t *testing.T) *reviveFixture {
			f, _ := newEndedFixture(t)
			f.now = f.now.Add(time.Hour)
			if code, body := f.extendAs(t, true, f.now.Add(48*time.Hour)); code != http.StatusOK {
				t.Fatalf("extend = %d %s", code, body)
			}
			f.run = f.st.run
			return f
		},
	}
	for name, setup := range kinds {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			if code, body := f.reviveAs(t, false); code != http.StatusOK {
				t.Fatalf("revive = %d %s, want 200", code, body)
			}
			cfg := f.newConfig(t)
			if cfg.RunToken == "" || cfg.RunToken == "old-token" {
				t.Errorf("revived token = %q; want a fresh one", cfg.RunToken)
			}
			if cfg.MITMCACertPEM != "ca-cert" || cfg.MITMCAKeyPEM != "ca-key" {
				t.Errorf("revived MITM CA = %q/%q; want the run's own CA carried over", cfg.MITMCACertPEM, cfg.MITMCAKeyPEM)
			}
			kept, err := proxy.LoadConfigBytes(f.rs.cfg)
			if err != nil || kept.RunToken != cfg.RunToken {
				t.Errorf("stored config after the revive has token %q (%v); want the new proxy's", kept.RunToken, err)
			}
		})
		t.Run(name+"/not stored", func(t *testing.T) {
			f := setup(t)
			f.rs.dropped = true
			code, body := f.reviveAs(t, false)
			if code != http.StatusConflict || !strings.Contains(body, "not stored") || len(f.rr.replaced) != 0 {
				t.Fatalf("revive with no stored config = %d %s (replaced %d); want 409 and nothing replaced", code, body, len(f.rr.replaced))
			}
			if lostAt, _ := f.st.lost(); lostAt == nil {
				t.Error("a refused revive un-lost the run")
			}
		})
	}
}
