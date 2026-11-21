// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// spoolReplica is a replica whose store write fails for the held event ids, whose real local spool takes
// them, and which answers as production does: the spooling recorder's error wrapping audit.ErrSpooled, or
// the sealing recorder's nil for a row it appended to the spool alone.
type spoolReplica struct {
	inner      audit.Recorder
	spool      *AuditSpool
	holdAction string
	nilBack    bool
}

func (r *spoolReplica) Record(ctx context.Context, ev types.AuditEvent) error {
	if ev.Action != r.holdAction {
		return r.inner.Record(ctx, ev)
	}
	if err := r.spool.Append(ev); err != nil {
		return err
	}
	if r.nilBack {
		return nil
	}
	return fmt.Errorf("%w: %w", audit.ErrSpooled, errors.New("the audit store is down"))
}

// sealKeys serves one fixed key to every subject.
type sealKeys struct{}

func (sealKeys) Current(context.Context, string, string) (uuid.UUID, []byte, error) {
	return uuid.MustParse("00000000-0000-0000-0000-0000000000aa"), make([]byte, 32), nil
}
func (sealKeys) Key(context.Context, string, uuid.UUID) ([]byte, error) { return make([]byte, 32), nil }

// sealRecorder seals before it hands the row to inner, as the daemon's sealing recorder does under full sealing.
type sealRecorder struct {
	sealer *audit.Sealer
	inner  audit.Recorder
}

func (r sealRecorder) Record(ctx context.Context, ev types.AuditEvent) error {
	sealed, pending, err := r.sealer.Seal(ctx, ev)
	if err != nil || pending {
		return fmt.Errorf("seal: pending %v: %w", pending, err)
	}
	return r.inner.Record(ctx, sealed)
}

func portalAuditRows(t *testing.T, e *activateEnv, action string) (n int) {
	t.Helper()
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE action = $1`, action).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func activationDoc(t *testing.T, e *activateEnv) string {
	t.Helper()
	var doc string
	if err := e.pool.QueryRow(context.Background(), `SELECT config -> $1::text FROM site_config WHERE singleton`, store.PortalActivationKey).Scan(&doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

// The switch commits; the first replica's activate row reaches the shared audit table and its override row
// only reaches its local spool, whether the recorder says ErrSpooled or nil. That replica is lost with its
// spool. The record stays owed, so another replica sends the missing row with the record's id, time, actor and
// session, and only then settles; nothing writes a second row.
func TestPortalIssuersActivate_LostReplicaSpoolIsRedeliveredBeforeSettling_PG(t *testing.T) {
	for _, nilBack := range []bool{false, true} {
		t.Run(fmt.Sprintf("nilBack=%v", nilBack), func(t *testing.T) {
			ctx := context.Background()
			e := newActivateEnv(t)
			e.client("old-wardynd")
			e.warm()
			spool, err := NewAuditSpool(filepath.Join(t.TempDir(), "spool.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			e.srv.cfg.Audit = &spoolReplica{inner: store.Recorder{Pool: e.pool}, spool: spool, holdAction: "portal_issuers.activate_override", nilBack: nilBack}
			if code, body := e.activate(`{"old_servers_stopped":true,"confirm_listed_sessions":true}`); code != http.StatusOK || !e.on() {
				t.Fatalf("activate = %d %s, want 200 and the portals on", code, body)
			}
			if a, o := portalAuditRows(t, e, "portal_issuers.activate"), portalAuditRows(t, e, "portal_issuers.activate_override"); a != 1 || o != 0 || spool.Appends() != 1 {
				t.Fatalf("after the first replica: activate rows %d, override rows %d, spool lines %d; want 1, 0, 1", a, o, spool.Appends())
			}
			rec, owed, err := e.st.PendingPortalActivation(ctx)
			if err != nil || !owed || len(rec.Sessions) != 1 || !strings.HasPrefix(rec.Sessions[0], "old-wardynd@") {
				t.Fatalf("the record after the first replica = %+v owed %v, %v; want owed with the one overridden session", rec, owed, err)
			}

			// The first replica is replaced and its spool is gone. Another replica's tick finishes the delivery.
			other := New(baseTestConfig(newHarness(t), e.st))
			other.cfg.Now = func() time.Time { return e.now }
			other.cfg.Audit = store.Recorder{Pool: e.pool}
			if on, err := other.portalMonitorStep(ctx, e.now); err != nil || !on {
				t.Fatalf("the recovering tick = %v, %v", on, err)
			}
			for _, c := range []struct {
				action string
				id     uuid.UUID
			}{{"portal_issuers.activate", rec.ActivateEventID}, {"portal_issuers.activate_override", rec.OverrideEventID}} {
				var n int
				var actor, actorType, data string
				var sameTime bool
				err := e.pool.QueryRow(ctx, `
					SELECT count(*), COALESCE(max(actor), ''), COALESCE(max(actor_type), ''), COALESCE(max(data::text), ''), COALESCE(bool_and("time" = $3), false)
					  FROM audit_events WHERE action = $1 AND id = $2`, c.action, c.id, rec.EventAt).Scan(&n, &actor, &actorType, &data, &sameTime)
				if err != nil {
					t.Fatal(err)
				}
				if n != 1 || actor != rec.Actor || actorType != rec.ActorType || !sameTime {
					t.Errorf("%s: rows %d actor %s/%s sameTime %v; want 1 row with the committed id, time and actor %s/%s", c.action, n, actorType, actor, sameTime, rec.ActorType, rec.Actor)
				}
				if c.action == "portal_issuers.activate_override" && !strings.Contains(data, rec.Sessions[0]) {
					t.Errorf("the override row's data %s does not name %s", data, rec.Sessions[0])
				}
			}
			if _, owed, err := e.st.PendingPortalActivation(ctx); err != nil || owed {
				t.Fatalf("after the recovery: owed %v, %v; want settled", owed, err)
			}
			if doc := activationDoc(t, e); strings.Contains(doc, rec.Actor) || strings.Contains(doc, "sessions") || strings.Contains(doc, rec.ActivateEventID.String()) {
				t.Errorf("the settled document %s still holds the operator, the sessions or an event id", doc)
			}

			// Another tick, and another activation: one effective switch, no second row.
			if _, err := other.portalMonitorStep(ctx, e.now); err != nil {
				t.Fatal(err)
			}
			if code, body := e.activate(`{"old_servers_stopped":true}`); code != http.StatusOK || !strings.Contains(body, `"already_activated":true`) {
				t.Fatalf("a second activation = %d %s, want 200 already_activated", code, body)
			}
			if a, o := portalAuditRows(t, e, "portal_issuers.activate"), portalAuditRows(t, e, "portal_issuers.activate_override"); a != 1 || o != 1 {
				t.Errorf("rows at the end: activate %d, override %d, want 1 each", a, o)
			}
		})
	}
}

// Under full audit sealing a named operator's actor is stored as their subject, which their erasure key
// protects. The activation record must not keep their principal beside the event ids: once settled, the
// document holds the delivery time only, with no operator, no sessions and no event id, while the audit rows
// carry the sealed subject.
func TestPortalIssuersActivate_SettledRecordKeepsNoOperatorUnderFullSealing_PG(t *testing.T) {
	ctx := context.Background()
	e := newActivateEnv(t)
	const operator = "ops@example.com"
	if _, err := e.st.UpsertLoginIdentity(ctx, store.LoginIdentity{Principal: operator, Issuer: "https://login.example/" + uuid.NewString() + "/v2.0",
		TenantID: uuid.NewString(), ObjectID: uuid.NewString()}, e.now); err != nil {
		t.Fatal(err)
	}
	e.srv.cfg.Audit = sealRecorder{sealer: &audit.Sealer{Keys: sealKeys{}, Subjects: e.st, SealActor: true}, inner: store.Recorder{Pool: e.pool}}
	won, err := e.st.ActivatePortalIssuersAs(ctx, store.PortalActivation{ActorType: string(types.ActorHuman), Actor: operator, Sessions: []string{"billing-job@10.0.0.9"}})
	if err != nil || !won {
		t.Fatalf("activate = %v, %v", won, err)
	}
	rec, owed, err := e.st.PendingPortalActivation(ctx)
	if err != nil || !owed || rec.Actor != operator {
		t.Fatalf("the record before delivery = %+v owed %v, %v; want the named operator owed", rec, owed, err)
	}
	if doc := activationDoc(t, e); !strings.Contains(doc, operator) {
		t.Fatalf("the unsettled document %s lacks the operator it must replay", doc)
	}
	if _, err := e.srv.portalMonitorStep(ctx, e.now); err != nil {
		t.Fatal(err)
	}
	if _, owed, err := e.st.PendingPortalActivation(ctx); err != nil || owed {
		t.Fatalf("after delivery: owed %v, %v; want settled", owed, err)
	}
	var actor string
	if err := e.pool.QueryRow(ctx, `SELECT actor FROM audit_events WHERE id = $1`, rec.ActivateEventID).Scan(&actor); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(actor, audit.SubjectActorPrefix) {
		t.Fatalf("the audit row's actor = %q, want the sealed subject (full sealing was not in force)", actor)
	}
	doc := activationDoc(t, e)
	for _, leak := range []string{operator, "billing-job", "sessions", "actor", rec.ActivateEventID.String(), rec.OverrideEventID.String()} {
		if strings.Contains(doc, leak) {
			t.Errorf("the settled document %s still holds %q", doc, leak)
		}
	}
	if !strings.Contains(doc, "audited_at") {
		t.Errorf("the settled document %s lacks its delivery time", doc)
	}
	if !e.on() {
		t.Error("settling switched the portals off")
	}
}
