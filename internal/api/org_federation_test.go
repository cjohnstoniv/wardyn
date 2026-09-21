// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/version"
)

// TestHealthzOrgFederation_GoldenWhenOff pins the anonymous /healthz body of a
// daemon with no WARDYN_ORG_URL byte for byte: hybrid enrolment must not change
// a single byte a non-hybrid deployment already serves. The build's version is
// the one field replaced before comparing, so a release bump does not rot it.
// Regenerate with: WARDYN_UPDATE_GOLDEN=1 go test ./internal/api/ -run TestHealthzOrgFederation
func TestHealthzOrgFederation_GoldenWhenOff(t *testing.T) {
	const path = "testdata/healthz_no_org_golden.json"
	w := do(t, newHarness(t).srv, http.MethodGet, "/healthz", "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("healthz code = %d", w.Code)
	}
	got := bytes.ReplaceAll(w.Body.Bytes(), []byte(`"version":"`+version.Version+`"`), []byte(`"version":"<version>"`))
	if goldenUpdate() {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("/healthz with no org URL changed:\n--- golden ---\n%s\n--- now ---\n%s", want, got)
	}
}

// TestHealthzOrgFederation_BlockNeverNamesTheDevice pins what a hybrid laptop's
// anonymous /healthz adds: lag and enrolled, and neither the device id nor
// anything that could carry the org URL.
func TestHealthzOrgFederation_BlockNeverNamesTheDevice(t *testing.T) {
	h := newHarness(t)
	id := uuid.New()
	st := federation.Status{DeviceID: id, AckedSeq: 40, HeadSeq: 52, LastError: "dial tcp org.example.test:443: refused"}
	h.srv.cfg.OrgFederation = func() federation.Status { return st }
	body := do(t, h.srv, http.MethodGet, "/healthz", "", "").Body.String()
	var got struct {
		OrgFederation map[string]any `json:"org_federation"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.OrgFederation) != 2 || got.OrgFederation["enrolled"] != true || got.OrgFederation["lag"] != float64(12) {
		t.Fatalf("org_federation = %v", got.OrgFederation)
	}
	for _, leak := range []string{id.String(), "org.example.test"} {
		if strings.Contains(body, leak) {
			t.Errorf("/healthz discloses %q: %s", leak, body)
		}
	}
	if m := do(t, h.srv, http.MethodGet, "/metrics", adminToken, "").Body.String(); !strings.Contains(m, "\nwardyn_org_federation_lag 12\n") {
		t.Errorf("/metrics lacks the lag gauge:\n%s", m)
	}
	st.Revoked = true
	body = do(t, h.srv, http.MethodGet, "/healthz", "", "").Body.String()
	if !strings.Contains(body, `"org_federation":{"enrolled":false,"lag":12}`) {
		t.Errorf("revoked device still reads enrolled: %s", body)
	}
}

// TestCreateRunRefusedWhenRevoked: once the organisation has revoked this
// device, POST /runs answers 503 naming re-enrolment before it reads the body.
func TestCreateRunRefusedWhenRevoked(t *testing.T) {
	h := newHarness(t)
	revoked := false
	h.srv.cfg.OrgFederation = func() federation.Status { return federation.Status{Revoked: revoked} }
	if w := do(t, h.srv, http.MethodPost, "/api/v1/runs", adminToken, `{`); w.Code != http.StatusBadRequest {
		t.Fatalf("enrolled: code = %d, want the ordinary 400 for a bad body: %s", w.Code, w.Body)
	}
	revoked = true
	w := do(t, h.srv, http.MethodPost, "/api/v1/runs", adminToken, `{"agent":"claude-code","task":"x"}`)
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "re-enrolled") {
		t.Fatalf("revoked: code = %d body = %s, want 503 naming re-enrolment", w.Code, w.Body)
	}
}
