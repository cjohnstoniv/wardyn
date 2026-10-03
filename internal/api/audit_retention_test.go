// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// retentionFake is a store.AuditRetention that answers from script and remembers the last drop it was asked for.
type retentionFake struct {
	pagerFake
	status    types.AuditRetentionStatus
	statusErr error
	dropErr   error
	ahead     int
	aheadErr  error

	dropPartition, dropDigest, dropActor string
	drops                                int
}

func (f *retentionFake) AuditRetentionStatus(context.Context) (types.AuditRetentionStatus, error) {
	return f.status, f.statusErr
}

func (f *retentionFake) DropAuditPartition(_ context.Context, partition, digest, actor string) (types.AuditRetentionDrop, error) {
	f.drops++
	f.dropPartition, f.dropDigest, f.dropActor = partition, digest, actor
	if f.dropErr != nil {
		return types.AuditRetentionDrop{}, f.dropErr
	}
	return types.AuditRetentionDrop{Partition: partition, Rows: 4, SeqLo: 1, SeqHi: 4, Digest: digest, EventSeq: 9}, nil
}

func (f *retentionFake) AutodropAuditPartition(context.Context) (types.AuditRetentionDrop, bool, error) {
	return types.AuditRetentionDrop{}, false, nil
}

func (f *retentionFake) SetAuditRetentionPolicy(context.Context, int) (types.AuditRetentionPolicyChange, error) {
	return types.AuditRetentionPolicyChange{}, nil
}

func (f *retentionFake) EnsureAuditPartitions(context.Context, int) (int, error) { return 0, nil }

func (f *retentionFake) AuditPartitionsAhead(context.Context) (int, error) {
	return f.ahead, f.aheadErr
}

func retentionServer(t *testing.T, st store.Store) (*Server, *recRecorder) {
	t.Helper()
	h := newHarness(t)
	cfg := baseTestConfig(h, st)
	cfg.OIDC = &oidc.Authenticator{}
	return New(cfg), h.audit
}

// Only the security tier reaches either route; a member is refused before anything is read or dropped.
func TestAuditRetention_OnlyTheSecurityTierReachesIt(t *testing.T) {
	fake := &retentionFake{}
	srv, _ := retentionServer(t, fake)
	member := ssoSession(t, "sub-member", "member@corp.example", oidc.RoleUser)
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/audit/retention", ""},
		{http.MethodPost, "/api/v1/audit/retention/drop", `{"partition":"audit_events_legacy","digest":"abc"}`},
	} {
		if w := doSSO(t, srv, c.method, c.path, member, c.body); w.Code != http.StatusForbidden {
			t.Errorf("member %s %s: %d, want 403", c.method, c.path, w.Code)
		}
	}
	if fake.drops != 0 {
		t.Errorf("a member's drop reached the store %d time(s)", fake.drops)
	}
}

func TestAuditRetention_StatusIsServedToSecurityAdmins(t *testing.T) {
	pending := 90
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	fake := &retentionFake{status: types.AuditRetentionStatus{
		Policy:      types.AuditRetentionPolicy{Days: 0, EffectiveDays: 0, PendingDays: &pending, PendingEffectiveAt: &at},
		Cutover:     time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		MonthsAhead: 12,
		Partitions:  []types.AuditRetentionPartition{{Name: "audit_events_legacy", Rows: 5, State: "closed", Refusal: store.RetentionInsideWindow}},
	}}
	srv, _ := retentionServer(t, fake)
	sec := ssoSession(t, "sub-sec", "sec@corp.example", oidc.RoleSecurityAdmin)
	w := doSSO(t, srv, http.MethodGet, "/api/v1/audit/retention", sec, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /audit/retention: %d %s", w.Code, w.Body.String())
	}
	var got types.AuditRetentionStatus
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.MonthsAhead != 12 || got.Policy.PendingDays == nil || *got.Policy.PendingDays != 90 || len(got.Partitions) != 1 ||
		got.Partitions[0].Refusal != store.RetentionInsideWindow {
		t.Errorf("status = %+v", got)
	}

	fake.statusErr = errors.New("password=hunter2")
	w = doSSO(t, srv, http.MethodGet, "/api/v1/audit/retention", sec, "")
	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "audit_retention_read_failed") || strings.Contains(w.Body.String(), "hunter2") {
		t.Errorf("a failing read: %d %s, want a 500 audit_retention_read_failed that leaks nothing", w.Code, w.Body.String())
	}
	w = doSSO(t, partitionOnlyServer(t), http.MethodGet, "/api/v1/audit/retention", sec, "")
	if w.Code != http.StatusNotImplemented {
		t.Errorf("a store without partitions: %d, want 501", w.Code)
	}
}

func partitionOnlyServer(t *testing.T) *Server {
	t.Helper()
	srv, _ := retentionServer(t, &pagerFake{})
	return srv
}

// A drop passes the caller as the actor and the digest through untouched; each refusal is its own 409
// reason with an authz.denied row naming the partition, and nothing about an internal error leaks.
func TestAuditRetention_DropAndItsRefusals(t *testing.T) {
	sec := ssoSession(t, "sub-sec", "sec@corp.example", oidc.RoleSecurityAdmin)
	body := `{"partition":"audit_events_legacy","digest":"abc"}`

	t.Run("a drop", func(t *testing.T) {
		fake := &retentionFake{}
		srv, _ := retentionServer(t, fake)
		w := doSSO(t, srv, http.MethodPost, "/api/v1/audit/retention/drop", sec, body)
		var d types.AuditRetentionDrop
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &d) != nil || d.Partition != "audit_events_legacy" || d.EventSeq != 9 {
			t.Fatalf("drop: %d %s", w.Code, w.Body.String())
		}
		if fake.dropPartition != "audit_events_legacy" || fake.dropDigest != "abc" || fake.dropActor != "sub-sec" {
			t.Errorf("the store saw %q %q as %q, want the partition, the digest and the caller", fake.dropPartition, fake.dropDigest, fake.dropActor)
		}
	})

	for reason, wantReason := range map[string]string{
		store.RetentionNotOldest:      "audit_retention_not_oldest",
		store.RetentionNotClosed:      "audit_retention_not_closed",
		store.RetentionInsideWindow:   "audit_retention_inside_window",
		store.RetentionLiveRun:        "audit_retention_live_run",
		store.RetentionDigestMismatch: "audit_retention_digest_mismatch",
	} {
		t.Run(reason, func(t *testing.T) {
			fake := &retentionFake{dropErr: &store.AuditRetentionRefused{Reason: reason, Partition: "audit_events_legacy"}}
			srv, rec := retentionServer(t, fake)
			w := doSSO(t, srv, http.MethodPost, "/api/v1/audit/retention/drop", sec, body)
			var eb struct{ Reason string }
			_ = json.Unmarshal(w.Body.Bytes(), &eb)
			if w.Code != http.StatusConflict || eb.Reason != wantReason {
				t.Fatalf("%d %s, want 409 %s", w.Code, w.Body.String(), wantReason)
			}
			var denied *types.AuditEvent
			for _, ev := range rec.snapshot() {
				if ev.Action == "authz.denied" {
					denied = &ev
				}
			}
			if denied == nil || !strings.Contains(string(denied.Data), wantReason) || !strings.Contains(string(denied.Data), "audit_events_legacy") {
				t.Errorf("no authz.denied row naming %s and the partition (have %+v)", wantReason, rec.snapshot())
			}
		})
	}

	t.Run("an unknown partition", func(t *testing.T) {
		srv, _ := retentionServer(t, &retentionFake{dropErr: store.ErrNotFound})
		if w := doSSO(t, srv, http.MethodPost, "/api/v1/audit/retention/drop", sec, body); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "audit_partition_not_found") {
			t.Errorf("%d %s, want 404 audit_partition_not_found", w.Code, w.Body.String())
		}
	})
	t.Run("an internal failure", func(t *testing.T) {
		srv, _ := retentionServer(t, &retentionFake{dropErr: errors.New("password=hunter2")})
		w := doSSO(t, srv, http.MethodPost, "/api/v1/audit/retention/drop", sec, body)
		if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "audit_retention_drop_failed") || strings.Contains(w.Body.String(), "hunter2") {
			t.Errorf("%d %s, want a 500 audit_retention_drop_failed that leaks nothing", w.Code, w.Body.String())
		}
	})
	t.Run("a malformed body", func(t *testing.T) {
		fake := &retentionFake{}
		srv, _ := retentionServer(t, fake)
		for _, b := range []string{``, `not json`, `{}`, `{"partition":"  ","digest":"abc"}`} {
			w := doSSO(t, srv, http.MethodPost, "/api/v1/audit/retention/drop", sec, b)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "audit_retention_body_invalid") {
				t.Errorf("body %q: %d %s, want 400 audit_retention_body_invalid", b, w.Code, w.Body.String())
			}
		}
		if fake.drops != 0 {
			t.Errorf("a malformed body reached the store %d time(s)", fake.drops)
		}
	})
}

// /setup/status warns when the partition runway is short, and says nothing while it is healthy.
func TestAuditPartitionChecks(t *testing.T) {
	for _, c := range []struct {
		name    string
		fake    *retentionFake
		wantRow bool
	}{
		{"healthy", &retentionFake{ahead: 12}, false},
		{"exactly three months", &retentionFake{ahead: minPartitionsAhead}, false},
		{"two months", &retentionFake{ahead: minPartitionsAhead - 1}, true},
		{"none", &retentionFake{ahead: 0}, true},
		{"unreadable", &retentionFake{aheadErr: errors.New("down")}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv, _ := retentionServer(t, c.fake)
			rows := srv.auditPartitionChecks(context.Background())
			if (len(rows) == 1) != c.wantRow {
				t.Fatalf("rows = %+v, want a warning: %v", rows, c.wantRow)
			}
			if c.wantRow && (rows[0].ID != "audit_partitions" || rows[0].Status != "warn" || rows[0].Blocking) {
				t.Errorf("row = %+v, want a non-blocking audit_partitions warning", rows[0])
			}
		})
	}
	if rows := partitionOnlyServer(t).auditPartitionChecks(context.Background()); len(rows) != 0 {
		t.Errorf("a store without partitions got %+v", rows)
	}
}

func (f *retentionFake) Ping(context.Context) error { return nil }

// LatestAuditEventByAction is the sensor-heartbeat read /metrics makes; none was ever written.
func (f *retentionFake) LatestAuditEventByAction(context.Context, string) (types.AuditEvent, error) {
	return types.AuditEvent{}, store.ErrNotFound
}

// wardyn_audit_partitions_ahead is on the gated scrape when the store has partitions, and absent when
// the read fails.
func TestMetrics_AuditPartitionsAhead(t *testing.T) {
	scrape := func(f store.Store) string {
		srv, _ := retentionServer(t, f)
		w := do(t, srv, http.MethodGet, "/metrics", adminToken, "")
		if w.Code != http.StatusOK {
			t.Fatalf("/metrics = %d", w.Code)
		}
		return w.Body.String()
	}
	if body := scrape(&retentionFake{ahead: 12}); !strings.Contains(body, "# TYPE wardyn_audit_partitions_ahead gauge\nwardyn_audit_partitions_ahead 12\n") {
		t.Errorf("/metrics lacks the gauge at 12:\n%s", body)
	}
	if body := scrape(&retentionFake{aheadErr: errors.New("down")}); strings.Contains(body, "wardyn_audit_partitions_ahead") {
		t.Errorf("/metrics carries the gauge although the read failed:\n%s", body)
	}
}
