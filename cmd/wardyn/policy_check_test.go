// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

const (
	checkEvaluated = `{"allowed_domains":["pypi.org","*.corp.example"],"first_use_approval":"always_deny","min_confinement_class":"CC2"}`
	checkNarrower  = `{"allowed_domains":["pypi.org"],"first_use_approval":"always_deny","min_confinement_class":"CC3"}`
	checkWider     = `{"allowed_domains":["pypi.org","api.vendor.example"],"first_use_approval":"always_deny","min_confinement_class":"CC1"}`
)

// runPolicyCheck runs `wardyn policy check` with args and returns its stdout and its error.
func runPolicyCheck(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := rootCmd()
	out := &strings.Builder{}
	root.SetArgs(append([]string{"policy", "check"}, args...))
	root.SetIn(strings.NewReader(""))
	root.SetOut(out)
	root.SetErr(&strings.Builder{})
	err := root.Execute()
	return out.String(), err
}

// checkFiles writes the evaluated policy and one candidate, and returns their paths.
func checkFiles(t *testing.T, candidate string) (evaluated, cand string) {
	t.Helper()
	dir := t.TempDir()
	evaluated, cand = dir+"/evaluated.json", dir+"/candidate.json"
	writeFile(t, evaluated, checkEvaluated)
	writeFile(t, cand, candidate)
	return evaluated, cand
}

func TestPolicyCheck_ANarrowerCandidateIsWithinAndExitsZero(t *testing.T) {
	evaluated, cand := checkFiles(t, checkNarrower)
	out, err := runPolicyCheck(t, "--within", evaluated, "-f", cand)
	if err != nil {
		t.Fatalf("policy check: %v\n%s", err, out)
	}
	if !strings.Contains(out, "is within") {
		t.Errorf("output = %q, want the within line", out)
	}
}

// The two sides are not interchangeable: the evaluated policy is wider than the narrower candidate,
// so the same two files the other way round must fail.
func TestPolicyCheck_AWiderCandidateExitsOneAndNamesEachField(t *testing.T) {
	evaluated, cand := checkFiles(t, checkWider)
	out, err := runPolicyCheck(t, "--within", evaluated, "-f", cand)
	if err == nil || exitCodeFor(err) != 1 {
		t.Fatalf("policy check = %v (exit %d), want exit 1\n%s", err, exitCodeFor(err), out)
	}
	for _, want := range []string{"  allowed_domains: api.vendor.example\n", "  min_confinement_class\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("output = %q, want it to hold %q", out, want)
		}
	}
	if strings.Contains(out, "first_use_approval") {
		t.Errorf("output = %q, names a field that did not widen", out)
	}

	evaluated, cand = checkFiles(t, checkNarrower)
	if out, err := runPolicyCheck(t, "--within", cand, "-f", evaluated); err == nil || exitCodeFor(err) != 1 {
		t.Errorf("the evaluated policy within the narrower one = %v, want exit 1\n%s", err, out)
	}
}

func TestPolicyCheck_JSONCarriesTheVerdictAndTheFields(t *testing.T) {
	evaluated, cand := checkFiles(t, checkWider)
	out, err := runPolicyCheck(t, "--within", evaluated, "-f", cand, "--json")
	if exitCodeFor(err) != 1 {
		t.Fatalf("policy check --json = %v, want exit 1", err)
	}
	var got struct {
		Within    *bool `json:"within"`
		Widenings []struct {
			Field   string   `json:"field"`
			Entries []string `json:"entries"`
		} `json:"widenings"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if got.Within == nil || *got.Within || len(got.Widenings) != 2 ||
		got.Widenings[0].Field != "allowed_domains" || len(got.Widenings[0].Entries) != 1 ||
		got.Widenings[1].Field != "min_confinement_class" {
		t.Errorf("verdict = %s", out)
	}
}

// A stored policy is read as the server wrote it. A field this build does not know refuses the
// check: decoded loosely it would be dropped, and the candidate would pass for lacking a
// restriction it never saw.
func TestPolicyCheck_WithinAStoredPolicyRefusesAFieldThisBuildDoesNotKnow(t *testing.T) {
	_, cand := checkFiles(t, checkNarrower)
	id := uuid.New()
	known := newCmdServer(t, http.StatusOK, map[string]any{"id": id, "name": "evaluated", "spec": json.RawMessage(checkEvaluated)})
	out, err := runPolicyCheck(t, "--within", id.String(), "-f", cand, "--url", known.URL, "--token", "tok")
	if err != nil {
		t.Fatalf("policy check within a stored policy: %v\n%s", err, out)
	}
	if got := known.last(); got.method != http.MethodGet || got.path != "/api/v1/policies/"+id.String() {
		t.Errorf("got %s %s, want GET /api/v1/policies/%s", got.method, got.path, id)
	}

	newer := newCmdServer(t, http.StatusOK, map[string]any{"id": id, "name": "evaluated", "spec": map[string]any{
		"allowed_domains": []string{"pypi.org"}, "field_from_a_newer_server": true,
	}})
	out, err = runPolicyCheck(t, "--within", id.String(), "-f", cand, "--url", newer.URL, "--token", "tok")
	if err == nil || !strings.Contains(err.Error(), "field_from_a_newer_server") {
		t.Errorf("policy check = %v, want the unknown field refused\n%s", err, out)
	}
}

func TestPolicyCheck_AnUnknownPolicyIDSaysATemplateCannotBeASide(t *testing.T) {
	_, cand := checkFiles(t, checkNarrower)
	srv := newCmdServer(t, http.StatusNotFound, map[string]string{"error": "not found"})
	_, err := runPolicyCheck(t, "--within", uuid.NewString(), "-f", cand, "--url", srv.URL, "--token", "tok")
	if err == nil || exitCodeFor(err) != 3 || !strings.Contains(err.Error(), "template") {
		t.Errorf("policy check = %v (exit %d), want exit 3 and the template sentence", err, exitCodeFor(err))
	}
}

// An empty file read as an empty policy would be within anything, so a gate would pass on a file
// that was never written.
func TestPolicyCheck_AnEmptyDocumentIsRefusedOnEitherSide(t *testing.T) {
	evaluated, cand := checkFiles(t, "")
	if out, err := runPolicyCheck(t, "--within", evaluated, "-f", cand); err == nil || !strings.Contains(err.Error(), "holds no policy") {
		t.Errorf("an empty candidate = %v, want it refused\n%s", err, out)
	}
	if out, err := runPolicyCheck(t, "--within", cand, "-f", evaluated); err == nil || !strings.Contains(err.Error(), "holds no policy") {
		t.Errorf("an empty --within file = %v, want it refused\n%s", err, out)
	}
}
