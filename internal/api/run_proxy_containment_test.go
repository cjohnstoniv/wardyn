// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func (m *memProxyStarts) RecordProxyContainment(_ context.Context, runID uuid.UUID, gen int, stopErr string) error {
	now := m.tick()
	m.pmu.Lock()
	defer m.pmu.Unlock()
	p := m.starts[runID][gen]
	if p == nil || p.State != types.ProxyStartFailed || p.ContainedAt != nil {
		return nil
	}
	p.ContainmentError = stopErr
	if stopErr == "" {
		p.ContainedAt = &now
	}
	return nil
}

func (f *readbackFixture) setProxyStopErr(err error) {
	f.lr.mu.Lock()
	f.lr.proxyErr = err
	f.lr.mu.Unlock()
}

// failWithUnstoppableProxy revives the run, makes the proxy stop fail, and posts a report of other
// bytes: the start fails with its proxy stop unresolved. It answers the start's generation.
func (f *readbackFixture) failWithUnstoppableProxy(t *testing.T) int {
	t.Helper()
	if code := f.revive(t); code != http.StatusOK {
		t.Fatalf("revive: code %d, want 200", code)
	}
	_, _, jti := f.lastHandover(t)
	f.setProxyStopErr(errors.New("daemon down"))
	if code, state := postLoaded(t, f.srv, f.run.ID, jti, hashHex([]byte("other bytes"))); code != http.StatusOK || state != string(types.ProxyStartFailed) {
		t.Fatalf("mismatched report: %d %q, want 200 failed", code, state)
	}
	p := f.active(t)
	if p.State != types.ProxyStartFailed || p.ContainedAt != nil || !strings.Contains(p.ContainmentError, "daemon down") {
		t.Fatalf("start = %s (contained %v, error %q), want failed and unresolved", p.State, p.ContainedAt, p.ContainmentError)
	}
	return p.Generation
}

// A mismatch whose proxy stop fails stays unresolved, on every retry that fails too, until a stop
// really succeeds; then it is contained, and asking again stops nothing more.
func TestReadback_MismatchContainmentUnresolvedUntilTheStopSucceeds(t *testing.T) {
	f := newReadbackFixture(t)
	gen := f.failWithUnstoppableProxy(t)
	if c := f.srv.containProxyStart(context.Background(), f.run.ID, gen); c.Contained || c.Superseded || c.Err == nil {
		t.Fatalf("retry with the stop still failing = %+v, want unresolved", c)
	}
	if p := f.active(t); p.ContainedAt != nil {
		t.Fatal("a failed stop must not record the start contained")
	}
	f.setProxyStopErr(nil)
	if err := f.srv.sweepOverdueProxyStarts(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p := f.active(t); p.ContainedAt == nil || p.ContainmentError != "" {
		t.Fatalf("after a stop that succeeds: contained %v, error %q; want contained", p.ContainedAt, p.ContainmentError)
	}
	stops := f.lr.proxyStopCount()
	if c := f.srv.containProxyStart(context.Background(), f.run.ID, gen); !c.Contained || c.Err != nil {
		t.Fatalf("a contained start answers %+v, want contained", c)
	}
	if f.lr.proxyStopCount() != stops {
		t.Error("a contained start must not be stopped again")
	}
}

// Containment is fenced on its generation: once a newer start is the run's active one, retrying the
// older failed start's stop stops nothing, from a direct call or the sweep.
func TestReadback_ContainmentNeverStopsANewerStart(t *testing.T) {
	f := newReadbackFixture(t)
	gen := f.failWithUnstoppableProxy(t)
	if _, err := f.rb.BeginProxyStart(context.Background(), f.run.ID, gen+1, "newer-jti", store.ProxyStartExpected); err != nil {
		t.Fatal(err)
	}
	f.setProxyStopErr(nil)
	stops, sandboxStops := f.lr.proxyStopCount(), f.lr.stopCount()
	if c := f.srv.containProxyStart(context.Background(), f.run.ID, gen); !c.Superseded || c.Contained {
		t.Fatalf("an older start's containment = %+v, want superseded", c)
	}
	if err := f.srv.sweepOverdueProxyStarts(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.lr.proxyStopCount() != stops || f.lr.stopCount() != sandboxStops {
		t.Error("an older start's containment must never stop the newer start's proxy")
	}
	if p := f.rb.get(f.run.ID, gen); p.ContainedAt != nil {
		t.Error("an older start whose stop was never confirmed must not be recorded contained")
	}
}
