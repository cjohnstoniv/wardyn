// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
)

const testRunPathRun = "6f1c2c7e-8d3b-4a1e-9b7f-0a2b3c4d5e6f"

// The written forms, byte for byte: an ad-hoc run keeps the path it always had, and a workload's run is
// the workload, then the run, with the owner escaped and the name as it is.
func TestRunPathWrittenForms(t *testing.T) {
	run := uuid.MustParse(testRunPathRun)
	for _, tc := range []struct {
		w    Workload
		want string
	}{
		{Workload{}, "/agent-run/" + testRunPathRun},
		{Workload{Owner: "service:ci-bot", Name: "nightly"}, "/workload/service_3Aci-bot/nightly/run/" + testRunPathRun},
		{Workload{Owner: "alice@example.com", Name: "web"}, "/workload/alice_40example_2Ecom/web/run/" + testRunPathRun},
		{Workload{Owner: "local:dev", Name: "web"}, "/workload/local_3Adev/web/run/" + testRunPathRun},
		{Workload{Owner: "admin-token", Name: "web"}, "/workload/admin-token/web/run/" + testRunPathRun},
		{Workload{Owner: "a_b", Name: "web"}, "/workload/a_5Fb/web/run/" + testRunPathRun},
		{Workload{Owner: "auth0|ab/cd", Name: "web"}, "/workload/auth0_7Cab_2Fcd/web/run/" + testRunPathRun},
		{Workload{Owner: "é", Name: "web"}, "/workload/_C3_A9/web/run/" + testRunPathRun},
	} {
		if got, err := RunPath(tc.w, run); err != nil || got != tc.want {
			t.Errorf("RunPath(%+v) = %q, %v; want %q", tc.w, got, err, tc.want)
		}
	}
	if got := (Workload{Owner: "auth0|ab/cd", Name: "web"}).String(); got != "auth0|ab/cd/web" {
		t.Errorf("String = %q, want the owner, a slash and the name", got)
	}
	if got := (Workload{}).String(); got != "" {
		t.Errorf("no workload's String = %q, want empty", got)
	}
}

// Every owner comes back from its path unchanged, and every path is a valid SPIFFE ID path: owners
// that are "." or "..", hold "/" or "_", look escaped already, or are not ASCII among them.
func TestRunPathRoundTripsAsAValidSPIFFEPath(t *testing.T) {
	run := uuid.MustParse(testRunPathRun)
	for _, owner := range []string{"service:ci-bot", "alice@example.com", ".", "..", "a/b", "/", "_", "_3A", "-", "admin-token", "é", "\x00", strings.Repeat("x.", 200)} {
		w := Workload{Owner: owner, Name: "web"}
		path, err := RunPath(w, run)
		if err != nil {
			t.Errorf("RunPath(%q): %v", owner, err)
			continue
		}
		if err := spiffeid.ValidatePath(path); err != nil {
			t.Errorf("RunPath(%q) = %q, not a SPIFFE ID path: %v", owner, path, err)
		}
		if got, gotRun, err := ParseRunPath(path); err != nil || got != w || gotRun != run {
			t.Errorf("ParseRunPath(%q) = %+v, %s, %v; want %+v, %s", path, got, gotRun, err, w, run)
		}
	}
	if got, gotRun, err := ParseRunPath("/agent-run/" + testRunPathRun); err != nil || got != (Workload{}) || gotRun != run {
		t.Errorf("an ad-hoc path = %+v, %s, %v; want no workload and the run", got, gotRun, err)
	}
}

// No two owners share a form: an owner that looks escaped is escaped again, and case is kept.
func TestRunPathOwnersNeverShareAForm(t *testing.T) {
	run := uuid.MustParse(testRunPathRun)
	seen := map[string]string{}
	for _, owner := range []string{"a:", "a_3A", "a_", "a_5F", "a.b", "a_2Eb", "a/b", "a_2Fb", "A", "a", "_", "_5F"} {
		path, err := RunPath(Workload{Owner: owner, Name: "web"}, run)
		if err != nil {
			t.Fatalf("RunPath(%q): %v", owner, err)
		}
		if prev, dup := seen[path]; dup {
			t.Errorf("owners %q and %q share the path %q", prev, owner, path)
		}
		seen[path] = owner
	}
}

// RunPath refuses a workload with no owner or with a name that is not a workload name, rather than
// write a path ParseRunPath would refuse.
func TestRunPathRefusesAnInvalidWorkload(t *testing.T) {
	run := uuid.MustParse(testRunPathRun)
	for _, w := range []Workload{{Name: "web"}, {Owner: "alice"}, {Owner: "alice", Name: "Web"}, {Owner: "alice", Name: "a/b"}, {Owner: "alice", Name: "a_b"}, {Owner: "alice", Name: strings.Repeat("a", 64)}} {
		if got, err := RunPath(w, run); err == nil {
			t.Errorf("RunPath(%+v) = %q, want a refusal", w, got)
		}
	}
}

// ParseRunPath takes only what RunPath writes.
func TestParseRunPathRefusesAnyOtherForm(t *testing.T) {
	run := testRunPathRun
	for name, path := range map[string]string{
		"empty":                                  "",
		"no leading slash":                       "agent-run/" + run,
		"no run":                                 "/agent-run/",
		"a segment after an ad-hoc run":          "/agent-run/" + run + "/x",
		"another first segment":                  "/agent/" + run,
		"no run segment":                         "/workload/alice/web/" + run,
		"a segment after a workload's run":       "/workload/alice/web/run/" + run + "/x",
		"the run segment misspelt":               "/workload/alice/web/runs/" + run,
		"no owner":                               "/workload//web/run/" + run,
		"a name in capitals":                     "/workload/alice/Web/run/" + run,
		"a name with an escape":                  "/workload/alice/a_5Fb/run/" + run,
		"an owner in lowercase hex":              "/workload/service_3aci-bot/web/run/" + run,
		"an owner escaping a byte kept as it is": "/workload/_61lice/web/run/" + run,
		"an owner with an unescaped dot":         "/workload/alice.b/web/run/" + run,
		"an owner with a cut-off escape":         "/workload/alice_4/web/run/" + run,
		"an owner with a non-hex escape":         "/workload/alice_ZZ/web/run/" + run,
		"an ad-hoc run id in capitals":           "/agent-run/" + strings.ToUpper(run),
		"an ad-hoc run id without dashes":        "/agent-run/" + strings.ReplaceAll(run, "-", ""),
		"a workload's run id in capitals":        "/workload/alice/web/run/" + strings.ToUpper(run),
	} {
		if w, got, err := ParseRunPath(path); err == nil {
			t.Errorf("%s: ParseRunPath(%q) = %+v, %s; want a refusal", name, path, w, got)
		}
	}
}
