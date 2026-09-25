// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// /healthz says whether every proxy reaches this daemon over the pinned TLS
// hop (internal/hoptls) — true exactly when there is an internal CA to hand
// out, false only on a loopback-http local install.
func TestHealthz_ProxyHopTLS(t *testing.T) {
	for _, tc := range []struct {
		ca   string
		want bool
	}{{"-----BEGIN CERTIFICATE-----\nx\n-----END CERTIFICATE-----\n", true}, {"", false}} {
		srv := New(Config{ControlPlaneCAPEM: tc.ca})
		r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		r.RemoteAddr = "127.0.0.1:54321"
		w := httptest.NewRecorder()
		panicFails(t, srv.Handler()).ServeHTTP(w, r)
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("healthz body: %v", err)
		}
		if got, ok := body["proxy_hop_tls"].(bool); !ok || got != tc.want {
			t.Errorf("CA set=%v: proxy_hop_tls = %v, want %v", tc.ca != "", body["proxy_hop_tls"], tc.want)
		}
	}
}

// Dispatch hands the internal CA to the run's proxy beside the URL it dials.
func TestDispatch_ProxyConfigCarriesControlPlaneCA(t *testing.T) {
	fr := &fakeRunner{}
	srv, _ := pgHarnessWithRunner(t, fr)
	const ca = "-----BEGIN CERTIFICATE-----\ninternal\n-----END CERTIFICATE-----\n"
	srv.cfg.ControlPlaneURL = "https://wardynd:8443"
	srv.cfg.ControlPlaneCAPEM = ca

	w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, `{"agent":"claude-code","repo":"acme/widgets","task":"do the thing"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create run: code = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	fr.waitForSandbox(t)
	if got := fr.lastSpec.ProxyConfig; got.ControlPlaneURL != "https://wardynd:8443" || got.ControlPlaneCAPEM != ca {
		t.Fatalf("ProxyConfig = {url %q, ca %q}, want the https URL and the internal CA", got.ControlPlaneURL, got.ControlPlaneCAPEM)
	}
}
