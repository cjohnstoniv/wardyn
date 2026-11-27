// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestProxyOutage_NilForDefault(t *testing.T) {
	if got := proxyOutage(types.DefaultOutageTerms()); got != nil {
		t.Fatalf("proxyOutage(default) = %+v, want nil", got)
	}
	got := proxyOutage(types.OutageTerms{AllowanceSec: 1800, Strict: true})
	if got == nil || got.AllowanceS != 1800 || !got.Strict {
		t.Fatalf("proxyOutage(1800 strict) = %+v", got)
	}
	if got := proxyOutage(types.OutageTerms{AllowanceSec: 7200}); got == nil || got.AllowanceS != 7200 || got.Strict {
		t.Fatalf("proxyOutage(7200) = %+v", got)
	}
}

func TestProxyOutage_NilForUnattendedOnly(t *testing.T) {
	if got := proxyOutage(types.OutageTerms{AllowanceSec: 3600, EndUnattended: true}); got != nil {
		t.Fatalf("proxyOutage(default allowance, end unattended) = %+v, want nil: the choice is the control plane's alone", got)
	}
}

func TestSidecarPolicyDropsOutage(t *testing.T) {
	in := types.RunPolicySpec{Outage: &types.OutageControls{AllowanceMinutes: 30, Strict: true}}
	if got := sidecarPolicy(in, dispatchParams{}); got.Outage != nil {
		t.Fatalf("sidecarPolicy kept outage %+v", got.Outage)
	}
	if in.Outage == nil {
		t.Fatal("sidecarPolicy cleared the caller's outage")
	}
}

func TestValidatePolicySpec_OutageBounds(t *testing.T) {
	for _, tc := range []struct {
		name string
		o    types.OutageControls
		ok   bool
	}{
		{"29", types.OutageControls{AllowanceMinutes: 29}, false},
		{"30", types.OutageControls{AllowanceMinutes: 30}, true},
		{"1440", types.OutageControls{AllowanceMinutes: 1440, Strict: true, Unattended: types.OutageUnattendedEnd}, true},
		{"1441", types.OutageControls{AllowanceMinutes: 1441}, false},
		{"unattended x", types.OutageControls{Unattended: "x"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := tc.o
			err := validatePolicySpec(types.RunPolicySpec{MinConfinementClass: types.CC2, Outage: &o})
			if (err == nil) != tc.ok {
				t.Fatalf("validatePolicySpec(outage %+v) = %v, want ok %v", tc.o, err, tc.ok)
			}
		})
	}
}

func TestValidateSiteConfig_OutageAllowanceBounds(t *testing.T) {
	if err := validateSiteConfig(types.SiteConfig{}); err != nil {
		t.Fatalf("validateSiteConfig(no outage_allowance_minutes) = %v, want ok", err)
	}
	for _, tc := range []struct {
		minutes int
		ok      bool
	}{{0, true}, {59, false}, {60, true}, {1440, true}, {1441, false}} {
		err := validateSiteConfig(types.SiteConfig{OutageAllowanceMinutes: ptr(tc.minutes)})
		if (err == nil) != tc.ok {
			t.Fatalf("validateSiteConfig(outage_allowance_minutes %d) = %v, want ok %v", tc.minutes, err, tc.ok)
		}
	}
}

func TestRecordOutageTerms_FailedSiteReadTakesDefault(t *testing.T) {
	h := newHarness(t)
	fake := &killCASStore{casApplied: true}
	cfg := baseTestConfig(h, fake)
	cfg.Broker = h.broker
	srv := New(cfg)
	run := types.AgentRun{ID: uuid.New(), State: types.RunStarting}
	got, ok := srv.recordOutageTerms(context.Background(), run, types.SiteConfig{OutageAllowanceMinutes: ptr(240)}, errors.New("site read failed"), types.RunPolicySpec{})
	if !ok || got != types.DefaultOutageTerms() {
		t.Fatalf("recordOutageTerms after a failed site read = %+v %v, want the default terms and the run kept", got, ok)
	}
	if revoked(h.broker.revoked, run.ID) {
		t.Fatal("a run with default terms was failed")
	}
}

func TestRecordOutageTerms_UnrecordableNonDefaultRefusesRun(t *testing.T) {
	h := newHarness(t)
	fake := &killCASStore{casApplied: true}
	cfg := baseTestConfig(h, fake)
	cfg.Broker = h.broker
	srv := New(cfg)
	run := types.AgentRun{ID: uuid.New(), State: types.RunStarting}
	policy := types.RunPolicySpec{Outage: &types.OutageControls{AllowanceMinutes: 30, Strict: true}}
	got, ok := srv.recordOutageTerms(context.Background(), run, types.SiteConfig{}, nil, policy)
	if ok {
		t.Fatalf("recordOutageTerms on a store that cannot record terms = %+v, kept; want the run refused", got)
	}
	if !revoked(h.broker.revoked, run.ID) {
		t.Fatalf("the refused run was not failed and revoked; broker.revoked=%v", h.broker.revoked)
	}
	var found bool
	for _, ev := range h.audit.snapshot() {
		if ev.Action == "run.create" && ev.Outcome == "failure" && strings.Contains(string(ev.Data), "outage terms") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no run.create failure row naming the outage terms; events=%+v", h.audit.snapshot())
	}
}

func TestSiteConfigPut_OutageAllowanceCarriedWhenUnnamed(t *testing.T) {
	fake := &fakeSiteConfigStore{cfg: types.SiteConfig{OutageAllowanceMinutes: ptr(240)}}
	srv, audit := newSiteConfigHarness(t, fake)
	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"unnamed keeps the stored allowance", `{}`, 240},
		{"a null keeps the stored allowance", `{"outage_allowance_minutes": null}`, 240},
		{"an explicit 0 resets it", `{"outage_allowance_minutes": 0}`, 0},
		{"a named value sets it", `{"outage_allowance_minutes": 120}`, 120},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, tc.body)
			if w.Code != http.StatusOK {
				t.Fatalf("PUT %s = %d; body=%s", tc.body, w.Code, w.Body.String())
			}
			if fake.putSeen == nil || fake.putSeen.OutageAllowanceMinutes == nil || *fake.putSeen.OutageAllowanceMinutes != tc.want {
				t.Fatalf("PUT %s stored %+v, want outage_allowance_minutes %d", tc.body, fake.putSeen, tc.want)
			}
			var last *types.AuditEvent
			events := audit.snapshot()
			for i, ev := range events {
				if ev.Action == "site_config.write" {
					last = &events[i]
				}
			}
			if last == nil {
				t.Fatal("no site_config.write row")
			}
			var datum map[string]any
			if err := json.Unmarshal(last.Data, &datum); err != nil {
				t.Fatalf("decode datum: %v; raw=%s", err, last.Data)
			}
			if got, ok := datum["outage_allowance_minutes"].(float64); !ok || int(got) != tc.want {
				t.Fatalf("site_config.write datum outage_allowance_minutes = %v, want %d", datum["outage_allowance_minutes"], tc.want)
			}
		})
	}
}
