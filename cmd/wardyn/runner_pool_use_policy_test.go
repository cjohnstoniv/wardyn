// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

var ciSubjects = []sdk.RunnerPoolSubject{
	{SubjectType: types.CapabilitySubjectGroup, Subject: "platform-eng"},
	{SubjectType: types.CapabilitySubjectUserType, Subject: "ci-bot"},
	{SubjectType: types.CapabilitySubjectUser, Subject: "dana@example.com"},
}

// use-policy get, set and clear say what the design's command-line frames say, a set names the pool
// revision it read, and a user pool is turned away before any use-policy request.
func TestRunnerPoolUsePolicyCommands(t *testing.T) {
	ci, general, laptop := uuid.New(), uuid.New(), uuid.New()
	at := time.Date(2026, 10, 9, 14, 2, 11, 0, time.UTC)
	srv := newPoolRoutes(t, map[string]any{
		"GET /api/v1/runner-pools/" + ci.String():                      sdk.RunnerPool{ID: ci, Name: "CI jobs", Scope: sdk.RunnerPoolOrganization, Revision: 9},
		"GET /api/v1/runner-pools/" + general.String():                 sdk.RunnerPool{ID: general, Name: "General", Scope: sdk.RunnerPoolOrganization, Revision: 6},
		"GET /api/v1/runner-pools/" + laptop.String():                  sdk.RunnerPool{ID: laptop, Name: "Laptop", Scope: sdk.RunnerPoolUser, Revision: 4},
		"GET /api/v1/runner-pools/" + ci.String() + "/use-policy":      sdk.RunnerPoolUsePolicy{PoolID: ci, Subjects: ciSubjects, Revision: 9, UpdatedAt: at},
		"GET /api/v1/runner-pools/" + general.String() + "/use-policy": sdk.RunnerPoolUsePolicy{PoolID: general, Subjects: []sdk.RunnerPoolSubject{}, Revision: 6},
		"PUT /api/v1/runner-pools/" + ci.String() + "/use-policy":      sdk.RunnerPoolUsePolicy{PoolID: ci, Revision: 10, UpdatedAt: at},
		"DELETE /api/v1/runner-pools/" + ci.String() + "/use-policy":   nil,
	})
	run := func(args ...string) (string, error) {
		return execCmdCapture(t, append(append([]string{"runner", "pool", "use-policy"}, args...), "--url", srv.URL, "--token", "t")...)
	}
	out, err := run("get", ci.String())
	if want := "revision 9, updated 2026-10-09T14:02:11Z\nSUBJECT TYPE|SUBJECT\ngroup|platform-eng\nuser_type|ci-bot\nuser|dana@example.com"; err != nil || squeeze(out) != want {
		t.Errorf("get = %v\n%s\nwant\n%s", err, squeeze(out), want)
	}
	if out, err := run("get", general.String()); err != nil || out != "no use policy: everyone who may launch runs on organization pools may use General\n" {
		t.Errorf("get with no policy = %v\n%q", err, out)
	}
	out, err = run("set", ci.String(), "--user", "lee@example.com", "--group", "platform-eng", "--role", "ci-bot")
	if err != nil || out != "use policy set on CI jobs, revision 10\n" {
		t.Errorf("set = %v\n%q", err, out)
	}
	var sent sdk.RunnerPoolUsePolicyRequest
	for _, r := range srv.reqs {
		if r.method == http.MethodPut {
			if err := json.Unmarshal(r.body, &sent); err != nil {
				t.Fatal(err)
			}
		}
	}
	wantSubjects := []sdk.RunnerPoolSubject{{SubjectType: "group", Subject: "platform-eng"}, {SubjectType: "user_type", Subject: "ci-bot"}, {SubjectType: "user", Subject: "lee@example.com"}}
	if sent.Revision == nil || *sent.Revision != 9 || len(sent.Subjects) != 3 || sent.Subjects[0] != wantSubjects[0] || sent.Subjects[1] != wantSubjects[1] || sent.Subjects[2] != wantSubjects[2] {
		t.Errorf("set sent %+v, want revision 9 and %v", sent, wantSubjects)
	}
	if out, err := run("clear", ci.String()); err != nil || out != "use policy cleared on CI jobs: everyone who may launch runs on organization pools may use it\n" ||
		!srv.sent(http.MethodDelete, "/api/v1/runner-pools/"+ci.String()+"/use-policy") {
		t.Errorf("clear = %v\n%q", err, out)
	}
	before := len(srv.reqs)
	for _, verb := range [][]string{{"get", laptop.String()}, {"set", laptop.String(), "--user", "lee@example.com"}, {"clear", laptop.String()}} {
		if _, err := run(verb...); err == nil || err.Error() != "Laptop is a user pool. Only its owner can use it; a use policy narrows organization pools only" {
			t.Errorf("%v on a user pool = %v", verb, err)
		}
	}
	for _, r := range srv.reqs[before:] {
		if strings.HasSuffix(r.path, "/use-policy") {
			t.Errorf("a user pool's use policy was asked for: %s %s", r.method, r.path)
		}
	}
}

// Where a second person has to approve, a set and a clear are held: the command says so in the words
// every held write uses, claims nothing was set or cleared, and exits 0.
func TestRunnerPoolUsePolicyHeldForASecondPerson(t *testing.T) {
	ci := uuid.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(sdk.RunnerPool{ID: ci, Name: "CI jobs", Scope: sdk.RunnerPoolOrganization, Revision: 9})
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"pending_change":{"id":"` + uuid.NewString() + `","state":"pending","kind":"runner_pool_use_policy"}}`))
	}))
	t.Cleanup(srv.Close)
	const held = "pending approval: 1 change(s) stored, not applied; a second approver runs `wardyn governance changes approve <id>`\n"
	for _, args := range [][]string{{"set", ci.String(), "--group", "platform-eng"}, {"clear", ci.String()}} {
		out, err := execCmdCapture(t, append(append([]string{"runner", "pool", "use-policy"}, args...), "--url", srv.URL, "--token", "t")...)
		if err != nil || out != held {
			t.Errorf("%v held = %v\n%q", args, err, out)
		}
	}
}

// show says who may use an organization pool from its use policy: everyone, the listed subjects with
// the policy's revision, or nobody once everyone listed has left; a caller who may not read the policy
// gets no row.
func TestRunnerPoolShowWhoMayUseAnOrganizationPool(t *testing.T) {
	id := uuid.New()
	pool := sdk.RunnerPool{ID: id, Name: "CI jobs", Scope: sdk.RunnerPoolOrganization, State: types.RunnerPoolActive, Revision: 9}
	at := time.Date(2026, 10, 9, 14, 2, 11, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		policy any
		want   string
	}{
		{"listed", sdk.RunnerPoolUsePolicy{PoolID: id, Subjects: ciSubjects, Revision: 9, UpdatedAt: at}, "WHO MAY USE|only: group platform-eng, role ci-bot, person dana@example.com|(use policy, revision 9)"},
		{"no policy", sdk.RunnerPoolUsePolicy{PoolID: id, Subjects: []sdk.RunnerPoolSubject{}, Revision: 9}, "WHO MAY USE|everyone who may launch runs on organization pools"},
		{"everyone left", sdk.RunnerPoolUsePolicy{PoolID: id, Subjects: []sdk.RunnerPoolSubject{}, Revision: 9, UpdatedAt: at}, "WHO MAY USE|Nobody can use this pool. Everyone it listed has left.|(use policy, revision 9)"},
	} {
		srv := newPoolRoutes(t, map[string]any{
			"GET /api/v1/runner-pools/" + id.String():                 pool,
			"GET /api/v1/runner-pools/" + id.String() + "/members":    []sdk.RunnerPoolMemberView{},
			"GET /api/v1/runner-pools/" + id.String() + "/use-policy": tc.policy,
		})
		out, err := execCmdCapture(t, "runner", "pool", "show", id.String(), "--url", srv.URL, "--token", "t")
		if err != nil || !strings.HasSuffix(squeeze(out), "\n"+tc.want) || !strings.Contains(squeeze(out), "TYPE|organization pool") || strings.Contains(out, "OWNER") {
			t.Errorf("%s: show = %v\n%s\nwant last row %q", tc.name, err, squeeze(out), tc.want)
		}
	}
	// A person who may use the pool may not read its policy: the server says 403 and the row is left out.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/use-policy"):
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"security admin or admin only"}`))
		case strings.HasSuffix(r.URL.Path, "/members"):
			_, _ = w.Write([]byte(`[]`))
		default:
			_ = json.NewEncoder(w).Encode(pool)
		}
	}))
	t.Cleanup(srv.Close)
	out, err := execCmdCapture(t, "runner", "pool", "show", id.String(), "--url", srv.URL, "--token", "t")
	if err != nil || strings.Contains(out, "WHO MAY USE") || !strings.Contains(out, "POOL LIMITS") {
		t.Errorf("show without the policy read = %v\n%s", err, out)
	}
}
