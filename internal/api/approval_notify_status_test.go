// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/notify"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// notifyApprovals is scopedApprovals plus an in-memory outbox reader. It answers for every id it
// holds, decided ones included, so a handler that asked about a decided row would show fields on it.
type notifyApprovals struct {
	*scopedApprovals
	esc   map[uuid.UUID]types.ApprovalEscalation
	stats []types.ApprovalNotifyChannelStat
	asked []uuid.UUID
}

func (n *notifyApprovals) ApprovalEscalations(_ context.Context, ids []uuid.UUID, _ time.Time) (map[uuid.UUID]types.ApprovalEscalation, error) {
	n.asked = append(n.asked, ids...)
	out := map[uuid.UUID]types.ApprovalEscalation{}
	for _, id := range ids {
		if e, ok := n.esc[id]; ok {
			out[id] = e
		}
	}
	return out, nil
}

func (n *notifyApprovals) ApprovalNotifyChannelStats(context.Context, time.Time) ([]types.ApprovalNotifyChannelStat, error) {
	return n.stats, nil
}

// The slack-style secret sits in the path and the query, and the URL also carries userinfo: every one
// of them must stay out of the status response.
const (
	notifyFixtureURL  = "https://bot:hunter2@hooks.example.com:8443/services/T000/B000/SECRETPATHVALUE?token=abc123"
	notifyFixtureHost = "hooks.example.com"
)

func activateNotifyConfig(t *testing.T) {
	t.Helper()
	cfg, err := notify.Parse(`{"channels":[
		{"id":"sec-oncall","type":"webhook","url":"` + notifyFixtureURL + `"},
		{"id":"plain","type":"webhook","url":"http://127.0.0.1:9/hook"},
		{"id":"mail","type":"smtp","host":"relay.example.test","port":587,"from":"wardyn@example.test","to":["ops@example.test"],"username":"smtpuser-xq","password":"smtppass-xq"}]}`)
	if err != nil {
		t.Fatalf("parse notify config: %v", err)
	}
	notify.SetActive(cfg, nil)
	t.Cleanup(func() { notify.SetActive(nil, nil) })
}

func newNotifyApprovals() *notifyApprovals {
	return &notifyApprovals{
		scopedApprovals: &scopedApprovals{fakeApprovals: newFakeApprovals(), runCreator: map[uuid.UUID]string{}},
		esc:             map[uuid.UUID]types.ApprovalEscalation{},
	}
}

// seedEscalations adds, for the run's creator, one approval per case: no tier yet, tier 1 in force,
// no further tier, and a decided one the outbox still holds rows for.
func (n *notifyApprovals) seedEscalations(t *testing.T, creator string, next time.Time) (runID uuid.UUID, noTier, tierOne, last, decided uuid.UUID) {
	t.Helper()
	runID = uuid.New()
	n.runCreator[runID] = creator
	add := func(state types.ApprovalState) uuid.UUID {
		ap, err := n.Request(context.Background(), types.ApprovalRequest{
			ID: uuid.New(), RunID: runID, Kind: types.ApprovalEgressDomain,
			RequestedScope: json.RawMessage(`{"host":"` + uuid.NewString() + `.example.com"}`), // distinct, or the fake dedups them into one row
		})
		if err != nil {
			t.Fatal(err)
		}
		ap.State = state
		n.byID[ap.ID] = ap
		return ap.ID
	}
	noTier, tierOne, last = add(types.ApprovalPending), add(types.ApprovalPending), add(types.ApprovalPending)
	decided = add(types.ApprovalApproved)
	n.esc[noTier] = types.ApprovalEscalation{NextAt: &next}
	n.esc[tierOne] = types.ApprovalEscalation{Tier: 1, NextAt: &next}
	n.esc[last] = types.ApprovalEscalation{Tier: 1}
	n.esc[decided] = types.ApprovalEscalation{Tier: 2, NextAt: &next}
	return runID, noTier, tierOne, last, decided
}

func decodeApprovals(t *testing.T, body []byte) map[uuid.UUID]types.ApprovalRequest {
	t.Helper()
	var rows []types.ApprovalRequest
	if err := json.Unmarshal(body, &rows); err != nil {
		t.Fatalf("decode approvals: %v (body=%s)", err, body)
	}
	out := map[uuid.UUID]types.ApprovalRequest{}
	for _, r := range rows {
		out[r.ID] = r
	}
	return out
}

func assertEscalationFields(t *testing.T, label string, got map[uuid.UUID]types.ApprovalRequest, next time.Time, noTier, tierOne, last, decided uuid.UUID) {
	t.Helper()
	if g := got[noTier]; g.EscalationTier != 0 || g.SLADueAt == nil || !g.SLADueAt.Equal(next) {
		t.Errorf("%s: no tier yet = tier %d due %v, want no tier and the next time", label, g.EscalationTier, g.SLADueAt)
	}
	if g := got[tierOne]; g.EscalationTier != 1 || g.SLADueAt == nil || !g.SLADueAt.Equal(next) {
		t.Errorf("%s: tier 1 in force = tier %d due %v, want tier 1 and the next time", label, g.EscalationTier, g.SLADueAt)
	}
	if g := got[last]; g.EscalationTier != 1 || g.SLADueAt != nil {
		t.Errorf("%s: no further tier = tier %d due %v, want tier 1 and no next time", label, g.EscalationTier, g.SLADueAt)
	}
	if g, ok := got[decided]; !ok || g.EscalationTier != 0 || g.SLADueAt != nil {
		t.Errorf("%s: decided row present=%v tier %d due %v, want present and carrying neither field", label, ok, g.EscalationTier, g.SLADueAt)
	}
}

// TestApprovalEscalationProjection_EveryListRead: the fields ride every list read (the DB-paged
// default, the run-scoped fetch-all and the member's own-runs page), and a decided row carries
// neither — the projection never even asks about it.
func TestApprovalEscalationProjection_EveryListRead(t *testing.T) {
	activateNotifyConfig(t)
	next := time.Now().UTC().Add(40 * time.Minute).Truncate(time.Second)
	n := newNotifyApprovals()
	runID, noTier, tierOne, last, decided := n.seedEscalations(t, "sub-owner", next)

	h := newHarness(t)
	cfg := baseTestConfig(h, r3PlainStore{})
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Approvals = n
	srv := New(cfg)

	for label, tc := range map[string]struct{ path, sub, role string }{
		"admin paged list":     {"/api/v1/approvals", "sub-sec", oidc.RoleSecurityAdmin},
		"admin run-scoped":     {"/api/v1/approvals?run_id=" + runID.String(), "sub-sec", oidc.RoleSecurityAdmin},
		"member own-runs page": {"/api/v1/approvals", "sub-owner", oidc.RoleUser},
	} {
		n.asked = nil
		w := doSSO(t, srv, http.MethodGet, tc.path, ssoSession(t, tc.sub, tc.sub+"@corp.example", tc.role), "")
		if w.Code != http.StatusOK {
			t.Fatalf("%s: code = %d; body=%s", label, w.Code, w.Body.String())
		}
		assertEscalationFields(t, label, decodeApprovals(t, w.Body.Bytes()), next, noTier, tierOne, last, decided)
		for _, id := range n.asked {
			if id == decided {
				t.Errorf("%s: the projection asked about the decided approval", label)
			}
		}
	}
}

// TestApprovalEscalationProjection_MemberSeesOnlyOwn: a member's list is their own runs' approvals,
// so the fields appear on those and a foreign approval does not appear at all.
func TestApprovalEscalationProjection_MemberSeesOnlyOwn(t *testing.T) {
	activateNotifyConfig(t)
	next := time.Now().UTC().Add(time.Hour)
	n := newNotifyApprovals()
	_, _, mineTier, _, _ := n.seedEscalations(t, "sub-owner", next)
	_, _, foreignTier, _, _ := n.seedEscalations(t, "sub-someone-else", next)

	h := newHarness(t)
	cfg := baseTestConfig(h, r3PlainStore{})
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Approvals = n
	w := doSSO(t, New(cfg), http.MethodGet, "/api/v1/approvals", ssoSession(t, "sub-owner", "owner@corp.example", oidc.RoleUser), "")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d; body=%s", w.Code, w.Body.String())
	}
	got := decodeApprovals(t, w.Body.Bytes())
	if g, ok := got[mineTier]; !ok || g.EscalationTier != 1 {
		t.Errorf("own approval present=%v tier %d, want tier 1", ok, g.EscalationTier)
	}
	if _, ok := got[foreignTier]; ok {
		t.Errorf("a member's list carries another owner's approval")
	}
	for _, id := range n.asked {
		if id == foreignTier {
			t.Errorf("the projection read a foreign approval's outbox rows")
		}
	}
}

// TestApprovalEscalationProjection_OffLeavesRowsAlone: with notifications unconfigured the outbox is
// never read, so rows left by an earlier config cannot put a chip on an approval.
func TestApprovalEscalationProjection_OffLeavesRowsAlone(t *testing.T) {
	notify.SetActive(nil, nil)
	n := newNotifyApprovals()
	runID, noTier, tierOne, _, _ := n.seedEscalations(t, "sub-owner", time.Now().Add(time.Hour))
	h := newHarness(t)
	cfg := baseTestConfig(h, r3PlainStore{})
	cfg.Approvals = n
	w := do(t, New(cfg), http.MethodGet, "/api/v1/approvals?run_id="+runID.String(), adminToken, "")
	got := decodeApprovals(t, w.Body.Bytes())
	if got[noTier].SLADueAt != nil || got[tierOne].EscalationTier != 0 || len(n.asked) != 0 {
		t.Errorf("notify off: rows %+v, asked %v; want no fields and no outbox read", got, n.asked)
	}
}

func statusServer(t *testing.T, n *notifyApprovals) *Server {
	t.Helper()
	h := newHarness(t)
	cfg := baseTestConfig(h, r3PlainStore{})
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Approvals = n
	return New(cfg)
}

// TestApprovalNotifyStatus_RefusedToMember: a member, and a caller with no session, are refused; the
// security tier reads it.
func TestApprovalNotifyStatus_RefusedToMember(t *testing.T) {
	activateNotifyConfig(t)
	srv := statusServer(t, newNotifyApprovals())
	const path = "/api/v1/approval-notify/status"
	if w := doSSO(t, srv, http.MethodGet, path, ssoSession(t, "sub-m", "m@corp.example", oidc.RoleUser), ""); w.Code != http.StatusForbidden {
		t.Errorf("member = %d, want 403; body=%s", w.Code, w.Body.String())
	}
	if w := doSSO(t, srv, http.MethodGet, path, nil, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("no session = %d, want 401", w.Code)
	}
	if w := doSSO(t, srv, http.MethodGet, path, ssoSession(t, "sub-s", "s@corp.example", oidc.RoleSecurityAdmin), ""); w.Code != http.StatusOK {
		t.Errorf("security admin = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// TestApprovalNotifyStatus_HostOnlyNeverTheURL: the destination is the parsed hostname. The fixture
// URL carries userinfo, a port, a long secret path and a query, and none of it may reach the body,
// which is also scanned for the whole URL's pieces after the channel list is decoded.
func TestApprovalNotifyStatus_HostOnlyNeverTheURL(t *testing.T) {
	activateNotifyConfig(t)
	now := time.Now().UTC().Truncate(time.Second)
	n := newNotifyApprovals()
	n.stats = []types.ApprovalNotifyChannelStat{{
		Channel: "sec-oncall", LastSentAt: &now, LastError: "http_status:503", LastErrorAt: &now, FailedLastHour: 4,
	}, {
		// A value the worker would never write: the guard keeps free text out of the response.
		Channel: "plain", LastError: "Post " + notifyFixtureURL + ": dial tcp", LastErrorAt: &now,
	}}
	w := doSSO(t, statusServer(t, n), http.MethodGet, "/api/v1/approval-notify/status",
		ssoSession(t, "sub-s", "s@corp.example", oidc.RoleSecurityAdmin), "")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d; body=%s", w.Code, w.Body.String())
	}
	for _, leak := range []string{"hunter2", "bot:", "SECRETPATHVALUE", "services/T000", "token=", "abc123", "https://", "8443", "?"} {
		if strings.Contains(w.Body.String(), leak) {
			t.Errorf("status body contains %q: %s", leak, w.Body.String())
		}
	}
	var resp approvalNotifyStatusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Channels) != 3 {
		t.Fatalf("channels = %+v, want all configured channels", resp.Channels)
	}
	c := resp.Channels[0]
	if c.ID != "sec-oncall" || c.Type != "webhook" || c.DestinationHost != notifyFixtureHost || c.LastError != "http_status:503" || c.FailedLastHour != 4 || c.LastSuccessAt == nil {
		t.Errorf("channel 0 = %+v", c)
	}
	if p := resp.Channels[1]; p.DestinationHost != "127.0.0.1" || p.LastError != "" {
		t.Errorf("channel 1 = %+v, want host 127.0.0.1 and the unsafe error class dropped", p)
	}
	if m := resp.Channels[2]; m.ID != "mail" || m.Type != "smtp" || m.DestinationHost != "relay.example.test" {
		t.Errorf("channel 2 = %+v, want the smtp relay host", m)
	}
	for _, leak := range []string{"smtpuser-xq", "smtppass-xq"} {
		if strings.Contains(w.Body.String(), leak) {
			t.Errorf("status body contains %q: %s", leak, w.Body.String())
		}
	}
}

// TestApprovalNotifyStatus_Unconfigured: no channels, an empty list rather than null.
func TestApprovalNotifyStatus_Unconfigured(t *testing.T) {
	notify.SetActive(nil, nil)
	w := doSSO(t, statusServer(t, newNotifyApprovals()), http.MethodGet, "/api/v1/approval-notify/status",
		ssoSession(t, "sub-s", "s@corp.example", oidc.RoleSecurityAdmin), "")
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"channels":[]}` {
		t.Errorf("unconfigured = %d %s, want 200 and an empty channel list", w.Code, w.Body.String())
	}
}

// TestSetupStatus_ApprovalNotifyRow: the row is absent unconfigured, ok with a clean hour, and warn
// after a dead row in the last hour.
func TestSetupStatus_ApprovalNotifyRow(t *testing.T) {
	find := func(t *testing.T, n *notifyApprovals) (SetupCheck, bool) {
		t.Helper()
		t.Setenv("HOME", t.TempDir())
		code, st := decodeSetup(t, New(Config{AdminToken: adminToken, Approvals: n}), adminToken)
		if code != http.StatusOK {
			t.Fatalf("GET /setup/status = %d", code)
		}
		for _, c := range st.Checks {
			if c.ID == "approval_notify" {
				return c, true
			}
		}
		return SetupCheck{}, false
	}

	notify.SetActive(nil, nil)
	if c, ok := find(t, newNotifyApprovals()); ok {
		t.Errorf("unconfigured: row present %+v, want absent", c)
	}

	activateNotifyConfig(t)
	c, ok := find(t, newNotifyApprovals())
	if !ok || c.Status != "ok" || c.Blocking || c.Detail != "No channel has failed in the last hour." {
		t.Errorf("configured, clean: %+v present=%v, want a non-blocking ok", c, ok)
	}

	n := newNotifyApprovals()
	n.stats = []types.ApprovalNotifyChannelStat{{Channel: "sec-oncall", FailedLastHour: 3}, {Channel: "plain", FailedLastHour: 1}}
	c, ok = find(t, n)
	want := "4 approval notifications failed in the last hour. The approvals still wait in the console."
	if !ok || c.Status != "warn" || c.Blocking || c.Detail != want || c.Label != "Approval notifications" ||
		c.Fix != "See Settings → Approval notifications for each channel's last error." {
		t.Errorf("after dead rows: %+v present=%v, want a non-blocking warn with %q", c, ok, want)
	}
}
