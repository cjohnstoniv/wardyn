// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// /healthz.org_ca_fingerprints round-trips WARDYN_ORG_CA_FINGERPRINTS as the
// normalized list, and is absent while it is unset.
func TestHealthz_OrgCAFingerprints(t *testing.T) {
	a, b := strings.Repeat("ab", 32), strings.Repeat("0c", 32)
	parsed, err := ParseOrgCAFingerprints(" sha256:" + strings.ToUpper(a) + ", SHA256:" + strings.Repeat("0c:", 31) + "0c ,")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"sha256:" + a, "sha256:" + b}
	if !reflect.DeepEqual(parsed, want) {
		t.Fatalf("parsed %v, want %v", parsed, want)
	}
	for _, tc := range []struct {
		fps  []string
		want any
	}{{parsed, []any{want[0], want[1]}}, {nil, nil}} {
		srv := New(Config{OrgCAFingerprints: tc.fps})
		r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		r.RemoteAddr = "127.0.0.1:54321"
		w := httptest.NewRecorder()
		panicFails(t, srv.Handler()).ServeHTTP(w, r)
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("healthz body: %v", err)
		}
		got, present := body["org_ca_fingerprints"]
		if tc.want == nil && present {
			t.Errorf("unset: org_ca_fingerprints present as %v", got)
		}
		if tc.want != nil && !reflect.DeepEqual(got, tc.want) {
			t.Errorf("org_ca_fingerprints = %v, want %v", got, tc.want)
		}
	}
}

func TestHealthz_OrgCAFingerprintsRefusesMalformed(t *testing.T) {
	for _, raw := range []string{"abc", "sha256:" + strings.Repeat("ab", 31), "sha1:" + strings.Repeat("ab", 32)} {
		if _, err := ParseOrgCAFingerprints(raw); err == nil || !strings.Contains(err.Error(), "WARDYN_ORG_CA_FINGERPRINTS") {
			t.Errorf("%q: %v, want a refusal naming the setting", raw, err)
		}
	}
	if fps, err := ParseOrgCAFingerprints(""); err != nil || fps != nil {
		t.Errorf("empty: %v, %v", fps, err)
	}
}
