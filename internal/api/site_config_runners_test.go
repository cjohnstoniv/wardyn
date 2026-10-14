// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestRunnerAdmissionReadsCurrentEnablement(t *testing.T) {
	fake := &fakeSiteConfigStore{}
	srv, _ := newSiteConfigHarness(t, fake)
	for _, tc := range []struct {
		name string
		cfg  *types.RunnerSettings
		err  error
		want int
	}{
		{"absent", nil, nil, 403},
		{"enabled", &types.RunnerSettings{Enabled: true}, nil, 200},
		{"disabled again", &types.RunnerSettings{}, nil, 403},
		{"read error", &types.RunnerSettings{Enabled: true}, errors.New("config failed"), 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake.cfg.Runners = tc.cfg
			fake.getErr = tc.err
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/runners/test", nil)
			allowed := srv.requireRunnersEnabled(w, r)
			if allowed != (tc.want == 200) || w.Code != tc.want {
				t.Fatalf("allowed=%v status=%d", allowed, w.Code)
			}
		})
	}
}

func TestSiteConfigCannotChangeRunnerEnablement(t *testing.T) {
	for _, stored := range []*types.RunnerSettings{nil, {}, {Enabled: true}} {
		for _, body := range []string{`{}`, `{"runners":null}`, `{"runners":{"enabled":false}}`, `{"runners":{"enabled":true}}`} {
			fake := &fakeSiteConfigStore{cfg: types.SiteConfig{Runners: stored}}
			srv, audit := newSiteConfigHarness(t, fake)
			w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, body)
			named := strings.Contains(body, `"enabled":true`) != (stored != nil && stored.Enabled) && strings.Contains(body, `"enabled"`)
			if named {
				if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), reasonSiteConfigRunnersViaOwnRoute) || fake.putSeen != nil {
					t.Fatalf("stored=%+v body=%s: a changed runners.enabled was not refused: %d %s", stored, body, w.Code, w.Body.String())
				}
				continue
			}
			if w.Code != http.StatusOK {
				t.Fatalf("PUT status=%d body=%s", w.Code, w.Body.String())
			}
			if fake.putSeen == nil || !reflect.DeepEqual(fake.putSeen.Runners, stored) {
				t.Fatalf("stored=%+v body=%s: generic PUT changed runner enablement", stored, body)
			}
			for _, ev := range audit.snapshot() {
				if ev.Action == "runners.enabled.set" {
					t.Fatal("generic PUT wrote the switch's audit row")
				}
			}
		}
	}
}

func TestRunnerAdmissionNoStore(t *testing.T) {
	srv, _ := newSiteConfigHarness(t, &fakeSiteConfigStore{})
	srv.cfg.Store = nil
	w := httptest.NewRecorder()
	if srv.requireRunnersEnabled(w, httptest.NewRequest(http.MethodGet, "/runners/test", nil)) || w.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing store status=%d", w.Code)
	}
}

// A captured baseline (get with runners ON, applied after a reset) and a managed file re-applied on
// every boot both go through the SDK, which must not send a runners block the server would refuse.
func TestSiteConfigSDKRoundTripSurvivesRunnersSwitch(t *testing.T) {
	fake := &fakeSiteConfigStore{cfg: types.SiteConfig{Runners: &types.RunnerSettings{Enabled: true}, UpstreamProxyURL: "http://proxy.corp:3128"}}
	srv, _ := newSiteConfigHarness(t, fake)
	ts := httptest.NewServer(panicFails(t, srv.Handler()))
	t.Cleanup(ts.Close)
	c := client.New(ts.URL, adminToken)

	var baseline types.SiteConfig
	w := do(t, srv, http.MethodGet, "/api/v1/site-config", adminToken, "")
	if err := json.Unmarshal(w.Body.Bytes(), &baseline); err != nil || baseline.Runners == nil || !baseline.Runners.Enabled {
		t.Fatalf("GET must carry runners ON for this test to mean anything: %v %s", err, w.Body.String())
	}

	// reset: the stored runners block is gone (or turned off), the captured baseline still says ON
	for name, stored := range map[string]*types.RunnerSettings{"after reset": nil, "turned off since": {}} {
		fake.cfg = types.SiteConfig{Runners: stored}
		if _, err := c.PutSiteConfigResult(context.Background(), baseline); err != nil {
			t.Fatalf("%s: applying the captured baseline failed: %v", name, err)
		}
		if runnersEnabled(fake.cfg) {
			t.Fatalf("%s: applying a baseline turned runners on", name)
		}
		if fake.cfg.UpstreamProxyURL != "http://proxy.corp:3128" {
			t.Fatalf("%s: the rest of the baseline was not applied", name)
		}
	}

	// the managed desktop file, re-applied on every boot, names a value other than the stored one
	fake.cfg = types.SiteConfig{Runners: &types.RunnerSettings{Enabled: true}}
	managed := types.SiteConfig{Runners: &types.RunnerSettings{Enabled: false}, UpstreamProxyURL: "http://proxy.corp:3128"}
	if _, err := c.PutSiteConfigResult(context.Background(), managed); err != nil || !runnersEnabled(fake.cfg) {
		t.Fatalf("a managed re-apply failed or moved the switch: %v enabled=%v", err, runnersEnabled(fake.cfg))
	}
}
