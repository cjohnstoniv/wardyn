// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// templateIDServer answers an id that names no stored policy, as the server does, and answers the
// template check route for that id with status and body. Each check's request body is sent on the
// returned channel.
func templateIDServer(t *testing.T, id uuid.UUID, status int, body string) (*httptest.Server, <-chan []byte) {
	t.Helper()
	sent := make(chan []byte, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/policies/"+id.String():
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"policy not found","reason":"policy_not_found"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/templates/"+id.String()+"/policy-check":
			var req struct {
				Candidate json.RawMessage `json:"candidate"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			sent <- req.Candidate
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, sent
}

// A UUID that names no stored policy is checked as a run template, by the server: the candidate goes
// to the template's check route and the server's verdict decides the exit code.
func TestPolicyCheck_ATemplateIDIsCheckedByTheServer(t *testing.T) {
	_, cand := checkFiles(t, checkWider)
	id := uuid.New()
	srv, sent := templateIDServer(t, id, http.StatusOK, `{"within":false,"widenings":[{"field":"allowed_domains","entries":["api.vendor.example"]},{"field":"min_confinement_class"}]}`)
	out, err := runPolicyCheck(t, "--within", id.String(), "-f", cand, "--url", srv.URL, "--token", "tok")
	if err == nil || exitCodeFor(err) != 1 {
		t.Fatalf("policy check = %v (exit %d), want exit 1\n%s", err, exitCodeFor(err), out)
	}
	for _, want := range []string{"  allowed_domains: api.vendor.example\n", "  min_confinement_class\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("output = %q, want it to hold %q", out, want)
		}
	}
	if got := <-sent; !strings.Contains(string(got), `"api.vendor.example"`) {
		t.Errorf("the check sent candidate %s, want the candidate file's policy", got)
	}

	srv, _ = templateIDServer(t, id, http.StatusOK, `{"within":true,"widenings":[]}`)
	if out, err := runPolicyCheck(t, "--within", id.String(), "-f", cand, "--url", srv.URL, "--token", "tok"); err != nil {
		t.Errorf("a within verdict = %v, want exit 0\n%s", err, out)
	}
}

// A verdict that says within and still names a field is read as not within: the gate fails closed
// on an answer that contradicts itself.
func TestPolicyCheck_ATemplateVerdictThatNamesAFieldIsNotWithin(t *testing.T) {
	_, cand := checkFiles(t, checkNarrower)
	id := uuid.New()
	srv, _ := templateIDServer(t, id, http.StatusOK, `{"within":true,"widenings":[{"field":"allowed_domains","entries":["api.vendor.example"]}]}`)
	if out, err := runPolicyCheck(t, "--within", id.String(), "-f", cand, "--url", srv.URL, "--token", "tok"); err == nil || exitCodeFor(err) != 1 {
		t.Errorf("policy check = %v (exit %d), want exit 1\n%s", err, exitCodeFor(err), out)
	}
}

// An id that names neither a policy nor a template the caller can read says so. A server that
// cannot check against a template (one older than the route, or one that keeps no templates) says
// that instead, and tells the caller what to pass.
func TestPolicyCheck_ATemplateIDTheServerCannotCheckSaysWhy(t *testing.T) {
	_, cand := checkFiles(t, checkNarrower)
	id := uuid.New()
	for _, row := range []struct {
		status     int
		body, want string
		exit       int
	}{
		{http.StatusNotFound, `{"error":"That template does not exist, or you cannot see it.","reason":"template_not_found"}`, "no policy or run template you can read", 3},
		{http.StatusNotFound, "404 page not found\n", "cannot check against a run template", 3},
		{http.StatusMethodNotAllowed, "", "cannot check against a run template", 3},
		{http.StatusNotImplemented, `{"error":"Templates are not available on this server yet.","reason":"templates_unavailable"}`, "cannot check against a run template", 4},
	} {
		srv, _ := templateIDServer(t, id, row.status, row.body)
		_, err := runPolicyCheck(t, "--within", id.String(), "-f", cand, "--url", srv.URL, "--token", "tok")
		if err == nil || exitCodeFor(err) != row.exit || !strings.Contains(err.Error(), row.want) {
			t.Errorf("%d %s: policy check = %v (exit %d), want exit %d and %q", row.status, row.body, err, exitCodeFor(err), row.exit, row.want)
		}
	}
}
