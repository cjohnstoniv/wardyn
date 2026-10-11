// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ?target=<person> is translated as ?actor= is: the name and the subject the
// person resolves to, and a person with none keeps the name alone.
func TestParseAuditFilterForTranslatesATargetToTheirSubject(t *testing.T) {
	srv := &Server{cfg: Config{AuditUnsealer: actorTranslatorStub{subjects: map[string]string{"alice": "subject:0f0e"}}}}
	parse := func(query string) [4]string {
		f, ok := srv.parseAuditFilterFor(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/audit"+query, nil))
		if !ok {
			t.Fatalf("parse %q refused", query)
		}
		return [4]string{f.Actor, f.ActorAlt, f.Target, f.TargetAlt}
	}
	for query, want := range map[string][4]string{
		"?target=alice":              {"", "", "alice", "subject:0f0e"},
		"?target=nobody":             {"", "", "nobody", ""},
		"?actor=alice&target=alice":  {"alice", "subject:0f0e", "alice", "subject:0f0e"},
		"?actor=nobody&target=alice": {"nobody", "", "alice", "subject:0f0e"},
		"":                           {},
	} {
		if got := parse(query); got != want {
			t.Errorf("%q = %v, want %v", query, got, want)
		}
	}
	if f, _ := (&Server{}).parseAuditFilterFor(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/audit?target=alice", nil)); f.Target != "alice" || f.TargetAlt != "" {
		t.Errorf("with no unsealer the filter is %+v, want the name alone", f)
	}
}

// A target filter only ever narrows what auditScope already allowed: a member's
// ?target= never reaches a run they do not own, nor the global feed, on either
// door, in either stored form.
func TestAuditQuery_TargetFilterNeverWidensAMember(t *testing.T) {
	h := newHarness(t)
	mine, theirs := uuid.New(), uuid.New()
	const subject = "subject:0f0e"
	withTarget := func(evs []types.AuditEvent, targets ...string) []types.AuditEvent {
		for i := range evs {
			evs[i].Target = targets[i%len(targets)]
		}
		return evs
	}
	st := &auditScopeStore{runs: map[uuid.UUID]types.AgentRun{
		mine:   {ID: mine, CreatedBy: auditMemberSub},
		theirs: {ID: theirs, CreatedBy: "sub-somebody-else"},
	}}
	st.auditByRun = map[uuid.UUID][]types.AuditEvent{
		mine:   withTarget(makeEvents(4, &mine), "alice", subject, "bob", "carol"),
		theirs: withTarget(makeEvents(3, &theirs), "alice"),
	}
	st.recentAudit = withTarget(makeEvents(8, nil), "alice", subject)

	cfg := baseTestConfig(h, st)
	cfg.OIDC = &oidc.Authenticator{}
	cfg.AuditUnsealer = actorTranslatorStub{subjects: map[string]string{"alice": subject}}
	srv := New(cfg)
	member := ssoSession(t, auditMemberSub, "member@corp.example", oidc.RoleUser)
	admin := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)

	for _, c := range []struct {
		name   string
		query  string
		cookie *http.Cookie
		want   int
	}{
		{"member, no run_id, the global feed is not theirs", "?target=alice", member, 0},
		{"member, another's run", "?run_id=" + theirs.String() + "&target=alice", member, 0},
		{"member, own run, both stored forms", "?run_id=" + mine.String() + "&target=alice", member, 2},
		{"member, own run, a target nobody has", "?run_id=" + mine.String() + "&target=nobody", member, 0},
		{"admin, global feed, both stored forms", "?target=alice", admin, 8},
		{"admin, another's run", "?run_id=" + theirs.String() + "&target=alice", admin, 3},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := doSSO(t, srv, http.MethodGet, "/api/v1/audit"+c.query, c.cookie, "")
			if w.Code != http.StatusOK {
				t.Fatalf("GET /audit%s = %d (body=%s)", c.query, w.Code, w.Body.String())
			}
			var got []types.AuditEvent
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			x := doSSO(t, srv, http.MethodGet, "/api/v1/audit/export"+c.query, c.cookie, "")
			if len(got) != c.want || len(ndjsonLines(t, x.Body.Bytes())) != c.want {
				t.Errorf("GET /audit%s: %d events, /audit/export %d, want %d", c.query, len(got), len(ndjsonLines(t, x.Body.Bytes())), c.want)
			}
		})
	}
}
