// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// desktopApprovalsFake is the approval service the desktop viewer's read goes through: it lists the PENDING rows
// of the runs asked for, in insertion order, and counts any decision made on it. The rest of the service is nil, so
// a call to it fails the test loudly.
type desktopApprovalsFake struct {
	ApprovalService
	rows    []types.ApprovalRequest
	listErr error
	decides int
}

func (f *desktopApprovalsFake) ListPendingApprovalsForRuns(_ context.Context, runIDs []uuid.UUID) ([]types.ApprovalRequest, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := []types.ApprovalRequest{}
	for _, ap := range f.rows {
		if ap.State != types.ApprovalPending {
			continue
		}
		for _, id := range runIDs {
			if ap.RunID == id {
				out = append(out, ap)
				break
			}
		}
	}
	return out, nil
}

func (f *desktopApprovalsFake) Decide(context.Context, uuid.UUID, types.ActorType, types.ApprovalDecision) (types.ApprovalRequest, error) {
	f.decides++
	return types.ApprovalRequest{}, errors.New("a gateway route decided an approval")
}

// desktopApprovalsNoPager is an approval service without the per-run pending read.
type desktopApprovalsNoPager struct{ ApprovalService }

func egressRow(runID uuid.UUID, host, mode string, at time.Time) types.ApprovalRequest {
	scope, _ := json.Marshal(map[string]any{"host": host, "mode": mode, "hold_seconds": 30})
	return types.ApprovalRequest{
		ID: uuid.New(), RunID: runID, Kind: types.ApprovalEgressDomain, RequestedScope: scope,
		State: types.ApprovalPending, RequestedAt: at,
	}
}

// getDesktopApprovals is GET <desktop>/__approvals with cookie c.
func getDesktopApprovals(h *uiHarness, c *http.Cookie) *httptest.ResponseRecorder {
	return uiGet(h, desktopPath(h, uiDesktopApprovals), c)
}

func openDesktopSession(t *testing.T, h *uiHarness) *http.Cookie {
	t.Helper()
	return desktopCookie(t, h.enterDesktop(h.ticket(h.run.ID, h.owner, oidc.RoleUser)))
}

type desktopApprovalsBody struct {
	Approvals []map[string]any `json:"approvals"`
}

func decodeDesktopApprovals(t *testing.T, rec *httptest.ResponseRecorder) desktopApprovalsBody {
	t.Helper()
	var body desktopApprovalsBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not the approvals list: %v: %s", err, rec.Body.String())
	}
	return body
}

// No route on the gateway decides an approval: any other method on the list's path, and any path below it, is the
// gateway's own 404, and the waiting approval is still waiting.
func TestDesktopApprovals_GatewayHasNoDecideDoor(t *testing.T) {
	h, _ := newDesktopHarness(t)
	row := egressRow(h.run.ID, "example.com", "wait_for_review", h.clock.now())
	fake := &desktopApprovalsFake{rows: []types.ApprovalRequest{row}}
	h.srv.cfg.Approvals = fake
	c := openDesktopSession(t, h)

	id := row.ID.String()
	for _, tc := range []struct{ method, rest string }{
		{http.MethodPost, uiDesktopApprovals},
		{http.MethodPut, uiDesktopApprovals},
		{http.MethodDelete, uiDesktopApprovals},
		{http.MethodGet, uiDesktopApprovals + "/" + id},
		{http.MethodPost, uiDesktopApprovals + "/" + id},
		{http.MethodGet, uiDesktopApprovals + "/" + id + "/approve"},
		{http.MethodPost, uiDesktopApprovals + "/" + id + "/approve"},
		{http.MethodPost, uiDesktopApprovals + "/" + id + "/deny"},
	} {
		req := httptest.NewRequest(tc.method, desktopPath(h, tc.rest), strings.NewReader(`{"state":"APPROVED"}`))
		req.AddCookie(c)
		rec := httptest.NewRecorder()
		h.gateway.ServeHTTP(rec, req)
		var body errorBody
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != http.StatusNotFound || body.Reason != reasonUIGatewayNotFound {
			t.Errorf("%s %s = %d %s, want 404 %s", tc.method, tc.rest, rec.Code, rec.Body.String(), reasonUIGatewayNotFound)
		}
	}
	if fake.decides != 0 {
		t.Fatalf("the gateway decided %d approvals, want none", fake.decides)
	}
	if got := fake.rows[0]; got.State != types.ApprovalPending || got.DecidedBy != "" || got.DecidedAt != nil {
		t.Fatalf("the approval is %s decided by %q, want PENDING and undecided", got.State, got.DecidedBy)
	}
	if got := decodeDesktopApprovals(t, getDesktopApprovals(h, c)); len(got.Approvals) != 1 {
		t.Fatalf("after the refused calls the list has %d rows, want the one still waiting", len(got.Approvals))
	}
}

// The list needs this run's open desktop session: no cookie, another run's desktop cookie, a relayed app's cookie
// and a closed session are all refused.
func TestDesktopApprovals_NeedsThisRunsDesktopSession(t *testing.T) {
	h, _ := newDesktopHarness(t)
	h.srv.cfg.Approvals = &desktopApprovalsFake{rows: []types.ApprovalRequest{egressRow(h.run.ID, "example.com", "wait_for_review", h.clock.now())}}

	mine := openDesktopSession(t, h)
	if got := getDesktopApprovals(h, mine); got.Code != http.StatusOK {
		t.Fatalf("this run's own desktop session = %d %s, want 200", got.Code, got.Body.String())
	}

	other := h.run
	other.ID = uuid.New()
	h.store.putRun(other)
	h.store.putEffectivePolicy(other.ID, types.RunPolicySpec{MinConfinementClass: types.CC2})
	first := h.run
	h.run = other
	otherCookie := openDesktopSession(t, h)
	h.run = first

	appRec := h.enter(url.Values{"run": {h.run.ID.String()}, "app": {"code"}, "ticket": {h.ticket(h.run.ID, h.owner, oidc.RoleUser)}})
	appCookie := desktopCookie(t, appRec)

	refused := func(name string, rec *httptest.ResponseRecorder) {
		t.Helper()
		var body errorBody
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != http.StatusForbidden || body.Reason != reasonUIGatewayNoSession || strings.Contains(rec.Body.String(), "example.com") {
			t.Errorf("%s = %d %s, want 403 %s and no list", name, rec.Code, rec.Body.String(), reasonUIGatewayNoSession)
		}
	}
	refused("no cookie", getDesktopApprovals(h, nil))
	refused("another run's desktop cookie", getDesktopApprovals(h, otherCookie))
	refused("a relayed app's cookie", getDesktopApprovals(h, appCookie))

	if rec := postDesktopClose(h.gateway, h, mine); rec.Code != http.StatusNoContent {
		t.Fatalf("close = %d, want 204", rec.Code)
	}
	if got := getDesktopApprovals(h, mine); got.Code == http.StatusOK || strings.Contains(got.Body.String(), "example.com") {
		t.Errorf("a closed session read the list: %d %s", got.Code, got.Body.String())
	}
}

// The list carries this run's PENDING egress_domain rows and nothing of any other kind, run or state, and only the
// five fields.
func TestDesktopApprovals_ListsOnlyThisRunsWaitingSites(t *testing.T) {
	h, _ := newDesktopHarness(t)
	now := h.clock.now()
	other := uuid.New()

	held := egressRow(h.run.ID, " Held.Example.COM ", "wait_for_review", now.Add(-7*time.Second))
	passive := egressRow(h.run.ID, "passive.example.com", "deny_with_review", now.Add(-5*time.Second))
	toolCall := egressRow(h.run.ID, "tool.example.com", "wait_for_review", now)
	toolCall.Kind = types.ApprovalToolCall
	decided := egressRow(h.run.ID, "decided.example.com", "wait_for_review", now)
	decided.State, decided.DecidedBy = types.ApprovalApproved, "someone"
	elsewhere := egressRow(other, "elsewhere.example.com", "wait_for_review", now)
	h.srv.cfg.Approvals = &desktopApprovalsFake{rows: []types.ApprovalRequest{held, toolCall, decided, elsewhere, passive}}
	c := openDesktopSession(t, h)

	rec := getDesktopApprovals(h, c)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d %s, want 200", rec.Code, rec.Body.String())
	}
	got := decodeDesktopApprovals(t, rec).Approvals
	if len(got) != 2 {
		t.Fatalf("list has %d rows, want the held and the passive egress row only: %s", len(got), rec.Body.String())
	}
	if got[0]["id"] != held.ID.String() || got[0]["host"] != "held.example.com" || got[0]["held"] != true {
		t.Errorf("first row = %v, want the held row with its lowercased host", got[0])
	}
	wantUntil := held.RequestedAt.Add(types.HoldWindowEgress)
	if until, err := time.Parse(time.RFC3339Nano, got[0]["held_until"].(string)); err != nil || !until.Equal(wantUntil) {
		t.Errorf("held_until = %v, want %v", got[0]["held_until"], wantUntil)
	}
	if got[1]["id"] != passive.ID.String() || got[1]["host"] != "passive.example.com" {
		t.Errorf("second row = %v, want the passive row", got[1])
	}
	if _, ok := got[1]["held"]; ok {
		t.Errorf("an unheld row carries held: %v", got[1])
	}
	for _, row := range got {
		keys := []string{}
		for k := range row {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			switch k {
			case "id", "host", "held", "held_until", "requested_at":
			default:
				t.Errorf("row carries %q beyond the five fields: %v", k, row)
			}
		}
	}
	if strings.Contains(rec.Body.String(), "elsewhere") || strings.Contains(rec.Body.String(), "tool.example.com") || strings.Contains(rec.Body.String(), "decided.example.com") {
		t.Errorf("list leaks another run's, kind's or state's row: %s", rec.Body.String())
	}

	// What one poll costs on this harness (a fake store, never a production figure).
	const polls = 50
	took := make([]time.Duration, 0, polls)
	for i := 0; i < polls; i++ {
		start := time.Now()
		if r := getDesktopApprovals(h, c); r.Code != http.StatusOK {
			t.Fatalf("poll %d = %d", i, r.Code)
		}
		took = append(took, time.Since(start))
	}
	sort.Slice(took, func(i, j int) bool { return took[i] < took[j] })
	t.Logf("approvals-arm: %d GETs, median %s, max %s", polls, took[polls/2], took[polls-1])
}

// desktopApproval has exactly the five json names the viewer reads.
func TestDesktopApprovals_FieldSet(t *testing.T) {
	typ := reflect.TypeOf(desktopApproval{})
	got := []string{}
	for i := 0; i < typ.NumField(); i++ {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		got = append(got, name)
	}
	sort.Strings(got)
	want := []string{"held", "held_until", "host", "id", "requested_at"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("desktopApproval json names = %v, want %v", got, want)
	}
}

// A read that fails, or a service that cannot make it, answers 503 with no list: never an empty one.
func TestDesktopApprovals_ReadErrorIs503(t *testing.T) {
	h, _ := newDesktopHarness(t)
	c := openDesktopSession(t, h)
	for name, svc := range map[string]ApprovalService{
		"a store error":      &desktopApprovalsFake{listErr: errors.New("database unreachable")},
		"a service no pager": &desktopApprovalsNoPager{},
	} {
		h.srv.cfg.Approvals = svc
		rec := getDesktopApprovals(h, c)
		var body errorBody
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != http.StatusServiceUnavailable || body.Reason != reasonStoreError || strings.Contains(rec.Body.String(), `"approvals"`) {
			t.Errorf("%s = %d %s, want 503 %s and no list", name, rec.Code, rec.Body.String(), reasonStoreError)
		}
	}
}

// The list is never cached, and the viewer page keeps its content security policy.
func TestDesktopApprovals_NoStoreAndCSPUnchanged(t *testing.T) {
	h, _ := newDesktopHarness(t)
	h.srv.cfg.Approvals = &desktopApprovalsFake{}
	c := openDesktopSession(t, h)
	if rec := getDesktopApprovals(h, c); rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("list = %d with Cache-Control %q, want 200 and no-store", rec.Code, rec.Header().Get("Cache-Control"))
	}
	const csp = "default-src 'self'; connect-src 'self'; img-src 'self' data: blob:; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"
	if rec := uiGet(h, desktopPath(h, "/"), c); rec.Code != http.StatusOK || rec.Header().Get("Content-Security-Policy") != csp {
		t.Fatalf("viewer = %d with CSP %q, want %q", rec.Code, rec.Header().Get("Content-Security-Policy"), csp)
	}
}

// The open viewer's boot data names the list's path; a refusal, which anyone holding the link is shown, does not.
func TestDesktopViewerBootNamesTheApprovalsPath(t *testing.T) {
	h, _ := newDesktopHarness(t)
	c := openDesktopSession(t, h)
	want := `&#34;approvals&#34;:&#34;` + desktopPath(h, uiDesktopApprovals) + `&#34;`
	if got := uiGet(h, desktopPath(h, "/"), c); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), want) {
		t.Fatalf("viewer = %d %s, want %s in its boot data", got.Code, got.Body.String(), want)
	}
	if got := uiGet(h, desktopPath(h, "/"), nil); got.Code != http.StatusForbidden || strings.Contains(got.Body.String(), "approvals") {
		t.Fatalf("refusal = %d %s, want 403 without the approvals path", got.Code, got.Body.String())
	}
}
