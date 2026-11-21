// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// pgDownAudit writes to the real audit table, or refuses every event while down is set.
type pgDownAudit struct {
	rec  store.Recorder
	down bool
}

func (p *pgDownAudit) Record(ctx context.Context, ev types.AuditEvent) error {
	if p.down {
		return errors.New("the audit store is down")
	}
	return p.rec.Record(ctx, ev)
}

// The switch commits and the process stops before either audit row is delivered: the operator, the confirmed
// session list and the events' own ids and time are in the database. A second server over the same database
// delivers both rows, once, with those values; replaying them again, as after a crash between the append and
// the mark, adds no row; and a second activation neither changes the switch nor writes a row.
func TestPortalIssuersActivate_CommittedMarkDeliversItsAuditAfterACrash_PG(t *testing.T) {
	ctx := context.Background()
	e := newActivateEnv(t)
	e.client("old-wardynd")
	e.warm()
	down := &pgDownAudit{rec: store.Recorder{Pool: e.pool}, down: true}
	e.srv.cfg.Audit = down
	if code, body := e.activate(`{"old_servers_stopped":true,"confirm_listed_sessions":true}`); code != http.StatusOK || !e.on() {
		t.Fatalf("activate with the audit store down = %d %s, want 200 and the portals on", code, body)
	}
	count := func(action string) (n int) {
		t.Helper()
		if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action = $1`, action).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if a, o := count("portal_issuers.activate"), count("portal_issuers.activate_override"); a != 0 || o != 0 {
		t.Fatalf("rows with the audit store down: activate %d, override %d, want 0", a, o)
	}
	rec, owed, err := e.st.PendingPortalActivation(ctx)
	if err != nil || !owed || rec.Actor == "" || len(rec.Sessions) != 1 || !strings.HasPrefix(rec.Sessions[0], "old-wardynd@") {
		t.Fatalf("the committed record = %+v owed %v, %v; want the actor and the one overridden session", rec, owed, err)
	}

	// The process is gone. A second server, with a working audit store, finds the owed record on its tick.
	other := New(baseTestConfig(newHarness(t), e.st))
	other.cfg.Now = func() time.Time { return e.now }
	other.cfg.Audit = &pgDownAudit{rec: store.Recorder{Pool: e.pool}}
	if on, err := other.portalMonitorStep(ctx, e.now); err != nil || !on {
		t.Fatalf("the recovering tick = %v, %v", on, err)
	}
	check := func(when string) {
		t.Helper()
		for _, c := range []struct {
			action string
			id     string
		}{{"portal_issuers.activate", rec.ActivateEventID.String()}, {"portal_issuers.activate_override", rec.OverrideEventID.String()}} {
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
				t.Errorf("%s: %s rows %d actor %s/%s sameTime %v; want 1 row with the committed id, time and actor %s/%s", when, c.action, n, actorType, actor, sameTime, rec.ActorType, rec.Actor)
			}
			if c.action == "portal_issuers.activate_override" && !strings.Contains(data, rec.Sessions[0]) {
				t.Errorf("%s: the override row's data %s does not name %s", when, data, rec.Sessions[0])
			}
		}
		if a, o := count("portal_issuers.activate"), count("portal_issuers.activate_override"); a != 1 || o != 1 {
			t.Errorf("%s: activate %d, override %d rows, want 1 each", when, a, o)
		}
	}
	check("after the recovery")
	if _, owed, err := e.st.PendingPortalActivation(ctx); err != nil || owed {
		t.Fatalf("after the recovery: owed %v, %v; want delivered", owed, err)
	}

	// A crash between the append and the mark: the record is owed again, its rows are already there.
	if _, err := e.pool.Exec(ctx, `UPDATE site_config SET config = jsonb_set(config, ARRAY[$1::text, 'audited_at'], 'null'::jsonb)`, store.PortalActivationKey); err != nil {
		t.Fatal(err)
	}
	if _, owed, _ := e.st.PendingPortalActivation(ctx); !owed {
		t.Fatal("the record was not owed again after its mark was cleared")
	}
	if _, err := other.portalMonitorStep(ctx, e.now); err != nil {
		t.Fatal(err)
	}
	check("after the second recovery")

	// One effective activation: another call is told it is already on and writes nothing.
	if code, body := e.activate(`{"old_servers_stopped":true}`); code != http.StatusOK || !strings.Contains(body, `"already_activated":true`) {
		t.Fatalf("a second activation = %d %s, want 200 already_activated", code, body)
	}
	if won, err := e.st.ActivatePortalIssuersAs(ctx, store.PortalActivation{ActorType: "human", Actor: "someone-else"}); err != nil || won {
		t.Fatalf("a second store activation = %v, %v; want not the winner", won, err)
	}
	if again, _, _ := e.st.PendingPortalActivation(ctx); again.Actor != "" {
		t.Errorf("the record changed to %+v", again)
	}
	check("after the second activation")
}
