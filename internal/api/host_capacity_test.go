// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/hostcapacity"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// hostAt is a guard with both limits set over a host reading avail MiB free
// and load1 load: 4096/150 refuses on both, 16384/1 admits.
func hostAt(avail int, load1 float64) *hostcapacity.Guard {
	return hostcapacity.New(hostcapacity.Limits{MinAvailableMiB: 8192, MaxLoad1: 100},
		func() (int, float64, error) { return avail, load1, nil })
}

// hostCapacityRefusals returns the host_capacity.refuse rows, failing on any
// that is not a daemon-authored denial.
func hostCapacityRefusals(t *testing.T, rec *recRecorder) []types.AuditEvent {
	t.Helper()
	var rows []types.AuditEvent
	for _, ev := range rec.snapshot() {
		if ev.Action != "host_capacity.refuse" {
			continue
		}
		if ev.Outcome != "denied" || ev.ActorType != types.ActorSystem || ev.Actor != "wardynd" || ev.RunID != nil {
			t.Fatalf("host_capacity.refuse row = %+v, want a wardynd denial with no run id", ev)
		}
		rows = append(rows, ev)
	}
	return rows
}

// hostCapacityStore records every state write a launcher could make before its
// run row, so a refusal can be shown to have made none of them.
type hostCapacityStore struct {
	*runWarnStore
	claims int
}

func (s *hostCapacityStore) ClaimSourceActiveRun(context.Context, uuid.UUID, uuid.UUID) error {
	s.claims++
	return nil
}

// TestHostCapacityCreateRun drives the real POST /runs: guard off and guard
// within limits both create the run as before; over the limits the answer is
// 503 host_capacity_refused with the reason and a Retry-After, and no row.
func TestHostCapacityCreateRun(t *testing.T) {
	h := newHarness(t)
	st := &runWarnStore{capStore: &capStore{}}
	cfg := baseTestConfig(h, st)
	cfg.DefaultPolicy = types.RunPolicySpec{MinConfinementClass: types.CC2, AllowedDomains: []string{"api.anthropic.com"}}
	srv := New(cfg)
	const body = `{"agent":"claude-code","task":"t"}`
	for _, g := range []*hostcapacity.Guard{nil, hostAt(16384, 1)} {
		srv.cfg.HostCapacity = g
		st.created = types.AgentRun{}
		if w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, body); w.Code != http.StatusCreated || st.created.ID == uuid.Nil {
			t.Fatalf("guard %v: code = %d, want 201 with a row: %s", g, w.Code, w.Body)
		}
	}
	srv.cfg.HostCapacity = hostAt(4096, 150)
	st.created = types.AgentRun{}
	w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, body)
	var got map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != http.StatusServiceUnavailable || got["error"] != "host_capacity_refused" ||
		got["reason"] != "mem_available_mib=4096 < 8192, load1=150.00 > 100" || w.Header().Get("Retry-After") != "30" {
		t.Fatalf("over limits: code = %d body = %s Retry-After = %q", w.Code, w.Body, w.Header().Get("Retry-After"))
	}
	if st.created.ID != uuid.Nil {
		t.Fatal("over limits: a run row was written")
	}
	rows := hostCapacityRefusals(t, h.audit)
	var data map[string]string
	if len(rows) == 1 {
		_ = json.Unmarshal(rows[0].Data, &data)
	}
	if len(rows) != 1 || rows[0].Target != "runs" ||
		data["reason"] != "mem_available_mib=4096 < 8192, load1=150.00 > 100" || data["requested_by"] != adminTokenPrincipal {
		t.Fatalf("over limits: host_capacity.refuse rows = %+v, want one for runs", rows)
	}
}

// TestHostCapacityLaunchersRefuseBeforeState drives the four server-authored
// launchers over a saturated host: each returns the refusal before claiming,
// minting or writing a run row.
func TestHostCapacityLaunchersRefuseBeforeState(t *testing.T) {
	h := newHarness(t)
	st := &hostCapacityStore{runWarnStore: &runWarnStore{capStore: &capStore{}}}
	cfg := baseTestConfig(h, st)
	cfg.Runner = &fakeRunner{}
	cfg.DefaultPolicy = types.RunPolicySpec{MinConfinementClass: types.CC2, AllowedDomains: []string{"api.anthropic.com"}}
	cfg.HostCapacity = hostAt(4096, 150)
	srv := New(cfg)
	ctx := context.Background()
	hl, ok := agentHarnessLogin(awsSSOAgent)
	if !ok {
		t.Fatal("aws-sso harness login convention missing")
	}
	ws := types.Workspace{ID: uuid.New(), Kind: types.WorkspaceKindLocalDir, Source: "/w", Status: types.WorkspaceScanned}
	for door, launch := range map[string]func() error{
		"source_scan": func() error {
			_, err := srv.launchSourceScanRun(ctx, "alice@example.com", types.Source{ID: uuid.New(), Kind: types.SourceRepo, Locator: "https://github.com/acme/widgets.git"})
			return err
		},
		"harness_login": func() error {
			_, _, err := srv.launchHarnessLoginRun(ctx, "alice@example.com", hl, loginTarget{startURL: perUserPortal})
			return err
		},
		"record": func() error {
			_, _, err := srv.launchRecordRun(ctx, "alice@example.com", ws, "build", "build", false)
			return err
		},
		"site_config_probe": func() error {
			_, _, err := srv.runSiteConfigProbe(ctx, "alice@example.com", "true", nil, nil, nil)
			return err
		},
	} {
		t.Run(door, func(t *testing.T) {
			before := len(hostCapacityRefusals(t, h.audit))
			var refused hostcapacity.ErrRefused
			if err := launch(); !errors.As(err, &refused) {
				t.Fatalf("err = %v, want hostcapacity.ErrRefused", err)
			}
			if rows := hostCapacityRefusals(t, h.audit)[before:]; len(rows) != 1 || rows[0].Target != door ||
				!strings.Contains(string(rows[0].Data), `"requested_by":"alice@example.com"`) {
				t.Fatalf("host_capacity.refuse rows = %+v, want one for %s", rows, door)
			}
			if st.created.ID != uuid.Nil || st.claims != 0 || len(h.broker.revoked) != 0 {
				t.Fatalf("refused launch touched state: row = %v, claims = %d, revoked = %v", st.created.ID, st.claims, h.broker.revoked)
			}
		})
	}
}

// TestHostCapacityWriteServerError: the refusal reaches the caller as its 503
// through writeServerError, where every launcher's failure path already ends,
// wrapped or not; any other error keeps its 500.
func TestHostCapacityWriteServerError(t *testing.T) {
	refusal := fmt.Errorf("create scan run: %w", hostcapacity.ErrRefused{Reason: "load1=150.00 > 100"})
	w := httptest.NewRecorder()
	writeServerError(w, httptest.NewRequest(http.MethodPost, "/", nil), "launch scan run", refusal)
	var got map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "30" ||
		got["error"] != "host_capacity_refused" || got["reason"] != "load1=150.00 > 100" {
		t.Fatalf("refusal: code = %d body = %s Retry-After = %q", w.Code, w.Body, w.Header().Get("Retry-After"))
	}
	w = httptest.NewRecorder()
	writeServerError(w, httptest.NewRequest(http.MethodPost, "/", nil), "launch scan run", errors.New("db down"))
	if w.Code != http.StatusInternalServerError || w.Header().Get("Retry-After") != "" {
		t.Fatalf("other error: code = %d, want an unchanged 500", w.Code)
	}
}
