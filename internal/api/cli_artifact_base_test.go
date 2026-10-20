// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestValidateCLIArtifactBase pins WARDYN_CLI_ARTIFACT_BASE's boot gate. The
// value is published on the anonymous /healthz, so each refusal below is a way
// a credential or a plaintext fetch could ride along.
func TestValidateCLIArtifactBase(t *testing.T) {
	cases := []struct {
		name, raw string
		ok        bool
		want      string
	}{
		{"unset -> empty, nothing published", "", true, ""},
		{"https mirror", "https://mirror.example/wardyn", true, "https://mirror.example/wardyn"},
		{"trailing slash trimmed", "https://mirror.example/wardyn/", true, "https://mirror.example/wardyn"},
		{"http refused", "http://mirror.example", false, ""},
		{"user name refused", "https://user@mirror.example", false, ""},
		{"user name and password refused", "https://user:pass@mirror.example", false, ""},
		{"query refused", "https://mirror.example?x=1", false, ""},
		{"fragment refused", "https://mirror.example#x", false, ""},
		{"empty host refused", "https:///path", false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ValidateCLIArtifactBase(c.raw)
			if c.ok {
				if err != nil || got != c.want {
					t.Fatalf("got (%q, %v), want (%q, nil)", got, err, c.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected a refusal, got %q", got)
			}
			if !strings.Contains(err.Error(), "WARDYN_CLI_ARTIFACT_BASE") {
				t.Errorf("refusal %q does not name the setting", err)
			}
			if got != "" {
				t.Errorf("a refused value must return %q, got %q", "", got)
			}
		})
	}
}

func healthzBody(t *testing.T, cfg Config) map[string]any {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	r.RemoteAddr = "127.0.0.1:54321"
	w := httptest.NewRecorder()
	panicFails(t, New(cfg).Handler()).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("healthz code = %d", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body
}

// TestHealthz_CLIArtifactBase: absent (not "") while unset, so a deployment
// that names no mirror keeps its body; the configured value round-trips.
func TestHealthz_CLIArtifactBase(t *testing.T) {
	if v, present := healthzBody(t, Config{})["cli_artifact_base"]; present {
		t.Errorf("cli_artifact_base = %v with the setting unset, want the key absent", v)
	}
	const mirror = "https://mirror.example/wardyn"
	if got := healthzBody(t, Config{CLIArtifactBase: mirror})["cli_artifact_base"]; got != mirror {
		t.Errorf("cli_artifact_base = %v, want %q", got, mirror)
	}
}
