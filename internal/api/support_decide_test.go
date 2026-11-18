// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	sdHelper = "sub-helper-sd"
	sdOwner  = "sub-owner-sd"
)

// sdCase is one hold and one caller: the hold's kind, when it was raised against the helper's request, the
// grant's state, the second-human switch and whether the grant record answers. The caller is a security admin,
// who decides any kind on any run, so a refusal can only be the helper rule.
type sdCase struct {
	name        string
	kind        types.ApprovalKind
	raised      string // "after": the grant was requested before the hold; "before": requested after it; "" no grant
	other       bool   // the caller is a different admin than the helper
	ended       bool
	secondHuman bool
	readFails   bool
	verbs       []string
}

// sdScope is a valid request scope for each kind a case names.
func sdScope(t *testing.T, kind types.ApprovalKind) json.RawMessage {
	t.Helper()
	switch kind {
	case types.ApprovalEgressDomain:
		return json.RawMessage(`{"host":"registry.npmjs.org"}`)
	case types.ApprovalFileDownload:
		raw, err := json.Marshal(types.FileDownloadScope{Name: "report.csv", Bytes: 17,
			SHA256: strings.Repeat("ab", 32), RequestedBy: sdOwner})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	return json.RawMessage(`{}`)
}

// build seeds the hold on a foreign run and the helper's grant on the support backend, and returns the caller's
// context and a closure that fires the real POST with the same identity.
func (c sdCase) build(t *testing.T) (*mdFixture, context.Context, func(string) *httptest.ResponseRecorder) {
	var b *supportBackend
	if c.secondHuman {
		t.Setenv(envEgressSecondHuman, "1")
	}
	f := newMDFixture(t, c.kind, sdScope(t, c.kind), nil, sdOwner, nil, nil, func(cfg *Config) {
		ast := cfg.Store.(*authzStore)
		b = &supportBackend{authzStore: ast, fakeSupportGrants: newFakeSupportGrants(func(id uuid.UUID) (types.AgentRun, bool) {
			ast.mu.Lock()
			defer ast.mu.Unlock()
			r, ok := ast.runs[id]
			return r, ok
		})}
		cfg.Store = b
	})
	if c.raised != "" {
		requested := f.ap.RequestedAt.Add(-time.Minute) // the hold came after the request
		if c.raised == "before" {
			requested = f.ap.RequestedAt.Add(time.Minute)
		}
		accepted := requested.Add(time.Second)
		g := store.SupportGrant{ID: uuid.New(), RunID: f.ap.RunID, Helper: sdHelper, RequestedAt: requested, AcceptedAt: &accepted}
		if c.ended {
			end := accepted.Add(time.Minute)
			g.EndedAt, g.EndReason = &end, "expired"
		}
		b.fakeSupportGrants.list = append(b.fakeSupportGrants.list, g)
	}
	if c.readFails {
		b.failOn("SupportHelperSince", errors.New("the grant record is down"))
	}
	sub := sdHelper
	if c.other {
		sub = "sub-other-admin-sd"
	}
	sess := ssoSession(t, sub, "admin@corp.example", oidc.RoleAdmin)
	ctx := withOIDCRole(withOIDCEmail(withOIDCHuman(context.Background(), sub), "admin@corp.example"), oidc.RoleAdmin)
	return f, ctx, func(verb string) *httptest.ResponseRecorder {
		return doSSO(t, f.srv, http.MethodPost, f.path(verb), sess, `{"reason":"t"}`)
	}
}

// supportDecideRows is the value list: every kind a helper could meet, each raised after and before the
// request, with the grant open and ended.
func supportDecideRows() []sdCase {
	var rows []sdCase
	for _, k := range []struct {
		kind  types.ApprovalKind
		verbs []string // the verbs that resolve a hold of that kind in this harness when nothing refuses
	}{
		{types.ApprovalCredential, nil},
		{types.ApprovalPushContent, nil},
		{types.ApprovalEgressDomain, nil},
		{types.ApprovalFileDownload, []string{"deny"}},
	} {
		n := string(k.kind)
		rows = append(rows,
			sdCase{name: n + " raised after the request, grant open: refused both ways", kind: k.kind, raised: "after"},
			sdCase{name: n + " raised after the request, grant ended: refused both ways", kind: k.kind, raised: "after", ended: true},
			sdCase{name: n + " raised before the request: as before", kind: k.kind, raised: "before", verbs: k.verbs},
			sdCase{name: n + " raised after the request, a different admin decides: as before", kind: k.kind, raised: "after", other: true, verbs: k.verbs},
		)
	}
	rows = append(rows,
		sdCase{name: "no grant at all: as before", kind: types.ApprovalCredential, verbs: []string{"deny"}},
		sdCase{name: "the grant record cannot be read: refused both ways", kind: types.ApprovalCredential, raised: "before", readFails: true},
		sdCase{name: "second_human on, egress raised before the request: refused as the second human", kind: types.ApprovalEgressDomain, raised: "before", secondHuman: true},
		sdCase{name: "second_human on, egress raised before the request, grant ended: refused as the second human", kind: types.ApprovalEgressDomain, raised: "before", ended: true, secondHuman: true},
		sdCase{name: "second_human on, egress, a different admin decides: allowed", kind: types.ApprovalEgressDomain, raised: "before", other: true, secondHuman: true, verbs: []string{"deny"}},
	)
	return rows
}

// supportDecideMDCases are TestMayDecideAgreesWithDecide's rows for a helper: mayDecide and a real POST give
// the same verdict.
func supportDecideMDCases() []mdCase {
	var out []mdCase
	for _, c := range supportDecideRows() {
		out = append(out, mdCase{name: "support helper: " + c.name, build: c.build, verbs: c.verbs})
	}
	return out
}

// TestSupportHelperDecisionRefusal pins each refusal's status, reason and the row left PENDING, so a mayDecide
// that agrees with a POST that refused for another reason cannot pass for the rule.
func TestSupportHelperDecisionRefusal(t *testing.T) {
	for _, c := range []struct {
		sdCase
		status int
		reason string
	}{
		{sdCase{name: "after, open", kind: types.ApprovalCredential, raised: "after"}, http.StatusForbidden, "support_helper_decision"},
		{sdCase{name: "after, ended", kind: types.ApprovalPushContent, raised: "after", ended: true}, http.StatusForbidden, "support_helper_decision"},
		{sdCase{name: "file download after", kind: types.ApprovalFileDownload, raised: "after"}, http.StatusForbidden, "support_helper_decision"},
		{sdCase{name: "read fails", kind: types.ApprovalEgressDomain, raised: "before", readFails: true}, http.StatusForbidden, "support_helper_decision"},
		{sdCase{name: "second human", kind: types.ApprovalEgressDomain, raised: "before", secondHuman: true}, http.StatusForbidden, "second_human_required"},
		{sdCase{name: "second human, ended", kind: types.ApprovalFileDownload, raised: "before", ended: true}, http.StatusForbidden, "second_human_required"},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, verb := range []string{"approve", "deny"} {
				f, _, fire := c.build(t)
				w := fire(verb)
				if w.Code != c.status || !strings.Contains(w.Body.String(), `"reason":"`+c.reason+`"`) {
					t.Fatalf("%s: status %d body %s, want %d %s", verb, w.Code, w.Body.String(), c.status, c.reason)
				}
				if c.reason == "support_helper_decision" && !strings.Contains(w.Body.String(), SupportHelperDecisionBody) {
					t.Fatalf("%s: body %s lacks the helper sentence", verb, w.Body.String())
				}
				if c.reason == "second_human_required" && !strings.Contains(w.Body.String(), "you are helping in this run, so someone else approves or denies it") {
					t.Fatalf("%s: body %s lacks the second-human helper sentence", verb, w.Body.String())
				}
				if got, _ := f.srv.cfg.Approvals.Get(context.Background(), f.ap.ID); got.State != types.ApprovalPending {
					t.Fatalf("%s: the hold is %s after a refusal, want PENDING", verb, got.State)
				}
			}
		})
	}
}
