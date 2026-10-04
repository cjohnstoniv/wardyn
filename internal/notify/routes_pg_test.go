// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package notify_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/notify"
)

// bodies collects the JSON bodies a test receiver got.
type bodies struct {
	mu   sync.Mutex
	list []map[string]any
}

func (b *bodies) handler() http.Handler {
	return http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r.Body)
		var m map[string]any
		_ = json.Unmarshal(buf.Bytes(), &m)
		b.mu.Lock()
		b.list = append(b.list, m)
		b.mu.Unlock()
	})
}

func (b *bodies) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.list)
}

func (b *bodies) last() map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.list[len(b.list)-1]
}

// routedHarness is a harness on a config with the given channels and routes JSON.
func routedHarness(t *testing.T, channels, routes string) *harness {
	t.Helper()
	pool := isolatedPool(t)
	cfg, err := notify.Parse(`{"console_url":"https://wardyn.example.com","channels":[` + channels + `],"routes":` + routes + `}`)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	rec := &recorder{}
	notify.SetActive(cfg, rec)
	t.Cleanup(func() { notify.SetActive(nil, nil) })
	return &harness{pool: pool, cfg: cfg, rec: rec}
}

type tierRow struct {
	Tier  int
	Chan  string
	State string
	Due   time.Duration // due_at - requested_at
}

func (h *harness) tierRows(t *testing.T, approvalID uuid.UUID) []tierRow {
	t.Helper()
	rows, err := h.pool.Query(context.Background(), `
		SELECT n.tier, n.channel, n.state, extract(epoch FROM n.due_at - a.requested_at)
		  FROM approval_notifications n JOIN approvals a ON a.id = n.approval_id
		 WHERE n.approval_id = $1 ORDER BY n.tier, n.channel`, approvalID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []tierRow
	for rows.Next() {
		var r tierRow
		var secs float64
		if err := rows.Scan(&r.Tier, &r.Chan, &r.State, &secs); err != nil {
			t.Fatal(err)
		}
		r.Due = time.Duration(secs * float64(time.Second)).Round(time.Second)
		out = append(out, r)
	}
	return out
}

func (h *harness) tierState(t *testing.T, approvalID uuid.UUID, tier int) string {
	t.Helper()
	var s string
	if err := h.pool.QueryRow(context.Background(),
		`SELECT state FROM approval_notifications WHERE approval_id = $1 AND tier = $2`, approvalID, tier).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// dueNow moves one tier's row to now, as if its `after` had passed.
func (h *harness) dueNow(t *testing.T, approvalID uuid.UUID, tier int) {
	t.Helper()
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE approval_notifications SET due_at = now() - interval '1 second', next_attempt_at = now() - interval '1 second'
		  WHERE approval_id = $1 AND tier = $2`, approvalID, tier); err != nil {
		t.Fatal(err)
	}
}

const twoTierRoute = `[{"tiers":[{"after":"0s","channels":["hook"]},{"after":"30m","channels":["hook","mail"],"notify":["profile_contact","run_owner"]}]}]`

func twoChannels(u string) string {
	return chanJSON(u, "") + `,{"id":"mail","type":"webhook","url":` + strconv.Quote(u) + `}`
}

// TestRoutes_TiersAreEnqueuedUpFrontAndSentOnlyWhenDueAndPending: both tiers' rows exist at raise time
// with the right due_at; a tier-1 row is not sent before it is due, is sent after while pending, and
// is cancelled when the approval was decided first.
func TestRoutes_TiersAreEnqueuedUpFrontAndSentOnlyWhenDueAndPending(t *testing.T) {
	var got bodies
	srv := httptest.NewServer(got.handler())
	defer srv.Close()
	h := routedHarness(t, twoChannels(srv.URL), twoTierRoute)
	ctx := context.Background()

	pending := h.raise(t, h.run(t), `{"host":"a.example"}`, "")
	want := []tierRow{{0, "hook", "pending", 0}, {1, "hook", "pending", 30 * time.Minute}, {1, "mail", "pending", 30 * time.Minute}}
	rows := h.tierRows(t, pending.ID)
	if len(rows) != len(want) {
		t.Fatalf("rows = %+v, want %+v", rows, want)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Fatalf("row %d = %+v, want %+v", i, rows[i], want[i])
		}
	}

	// Before due: only the tier-0 row is sent.
	w := h.worker(nil, nil)
	if n, err := w.Tick(ctx); err != nil || n != 1 {
		t.Fatalf("tick before due: %d, %v; want exactly the tier-0 row", n, err)
	}
	if got.count() != 1 || got.last()["event"] != "raised" {
		t.Fatalf("receiver got %d bodies, last %v", got.count(), got.last())
	}
	// The tier-0 announcement names when the next tier is due.
	var nextDue time.Time
	if err := h.pool.QueryRow(ctx, `SELECT min(due_at) FROM approval_notifications WHERE approval_id = $1 AND tier = 1`, pending.ID).Scan(&nextDue); err != nil {
		t.Fatal(err)
	}
	if ap, _ := got.last()["approval"].(map[string]any); ap["sla_due_at"] != nextDue.UTC().Format(time.RFC3339) {
		t.Fatalf("tier-0 approval = %v, want sla_due_at %s", ap, nextDue.UTC().Format(time.RFC3339))
	}
	if s := h.tierState(t, pending.ID, 1); s != "pending" {
		t.Fatalf("tier-1 state before due = %s, want pending", s)
	}

	// After due and still pending: both tier-1 rows are sent as escalations.
	h.dueNow(t, pending.ID, 1)
	if n, err := w.Tick(ctx); err != nil || n != 2 {
		t.Fatalf("tick after due: %d, %v; want both tier-1 rows", n, err)
	}
	if got.count() != 3 || got.last()["event"] != "escalated" || got.last()["tier"] != float64(1) {
		t.Fatalf("receiver got %d bodies, last %v", got.count(), got.last())
	}
	// No later tier is scheduled, so the last tier carries no deadline.
	if ap, _ := got.last()["approval"].(map[string]any); ap["sla_due_at"] != nil {
		t.Fatalf("last-tier approval = %v, want no sla_due_at", ap)
	}
	if s := h.tierState(t, pending.ID, 1); s != "sent" {
		t.Fatalf("tier-1 state after send = %s, want sent", s)
	}

	// Decided before its tier is due: the escalation is cancelled, nothing sent.
	decided := h.raise(t, h.run(t), `{"host":"b.example"}`, "")
	if _, err := h.pool.Exec(ctx, `UPDATE approvals SET state = 'APPROVED' WHERE id = $1`, decided.ID); err != nil {
		t.Fatal(err)
	}
	h.dueNow(t, decided.ID, 1)
	before := got.count()
	if _, err := w.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	// the tier-0 row of `decided` is cancelled too, so nothing at all is sent
	if got.count() != before {
		t.Fatalf("a notification was sent for an approval decided first")
	}
	if s := h.tierState(t, decided.ID, 1); s != "cancelled" {
		t.Fatalf("tier-1 state = %s, want cancelled", s)
	}
}

// TestRoutes_ProfileContactResolvesFromTheLeafProfileAndDropsWhatProjectDrops: profile_contact uses
// the leaf profile's stored contact through policyref.Project; run_owner uses the owner's address; a
// stored contact email that no longer validates is skipped, not sent.
func TestRoutes_ProfileContactResolvesFromTheLeafProfileAndDropsWhatProjectDrops(t *testing.T) {
	var got bodies
	srv := httptest.NewServer(got.handler())
	defer srv.Close()
	h := routedHarness(t, twoChannels(srv.URL), twoTierRoute)
	ctx := context.Background()
	if _, err := h.pool.Exec(ctx, `INSERT INTO people (principal, email, created_by) VALUES ('alice', 'alice@example.com', 'admin')`); err != nil {
		t.Fatal(err)
	}
	good, bad := uuid.New(), uuid.New()
	for _, p := range []struct {
		id      uuid.UUID
		name    string
		contact string
	}{
		{good, "payments", `{"owner":"Payments","email":"payments-owner@example.com"}`},
		// a direct write that the write path would have refused: a display name and a query
		{bad, "legacy", `{"owner":"Legacy","email":"Evil <x@example.com>?cc=y@example.com"}`},
	} {
		if _, err := h.pool.Exec(ctx, `INSERT INTO governance_profiles (id, name, ceiling, contact) VALUES ($1, $2, '{}', $3)`, p.id, p.name, p.contact); err != nil {
			t.Fatal(err)
		}
	}

	send := func(profile uuid.UUID) []any {
		runID := h.run(t)
		if _, err := h.pool.Exec(ctx, `UPDATE agent_runs SET governance_profile_id = $1 WHERE id = $2`, profile, runID); err != nil {
			t.Fatal(err)
		}
		a := h.raise(t, runID, `{"host":"`+uuid.NewString()+`.example"}`, "")
		w := h.worker(nil, nil)
		if _, err := w.Tick(ctx); err != nil { // tier 0 first, so the last delivery below is tier 1
			t.Fatal(err)
		}
		h.dueNow(t, a.ID, 1)
		if _, err := w.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		// the last delivery is a tier-1 row
		list, _ := got.last()["recipients"].([]any)
		return list
	}

	list := send(good)
	if len(list) != 2 {
		t.Fatalf("recipients = %v, want profile_contact and run_owner", list)
	}
	first, second := list[0].(map[string]any), list[1].(map[string]any)
	if first["role"] != "profile_contact" || first["email"] != "payments-owner@example.com" ||
		second["role"] != "run_owner" || second["email"] != "alice@example.com" {
		t.Fatalf("recipients = %v", list)
	}

	list = send(bad)
	if len(list) != 1 || list[0].(map[string]any)["role"] != "run_owner" {
		t.Fatalf("recipients = %v, want only run_owner: the dropped contact must be skipped", list)
	}
}

// TestRoutes_RedactRequesterOmitsTheRequester: a channel with redact_requester sends no requester.
func TestRoutes_RedactRequesterOmitsTheRequester(t *testing.T) {
	var got bodies
	srv := httptest.NewServer(got.handler())
	defer srv.Close()
	ch := `{"id":"hook","type":"webhook","url":` + strconv.Quote(srv.URL) + `,"redact_requester":true}`
	h := routedHarness(t, ch, `[{"tiers":[{"after":"0s","channels":["hook"]}]}]`)
	if _, err := h.pool.Exec(context.Background(), `INSERT INTO people (principal, email, created_by) VALUES ('alice', 'alice@example.com', 'admin')`); err != nil {
		t.Fatal(err)
	}
	h.raise(t, h.run(t), `{"host":"a.example"}`, "")
	if _, err := h.worker(nil, nil).Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got.count() != 1 {
		t.Fatalf("receiver got %d bodies", got.count())
	}
	if _, has := got.last()["requester"]; has {
		t.Fatalf("body carries the requester on a redacting channel: %v", got.last())
	}
}
