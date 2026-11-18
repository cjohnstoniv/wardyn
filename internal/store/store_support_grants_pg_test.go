// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// supportFixture is one throwaway database with the support grant store over it. Every case makes its own
// run, so the cases do not read each other's rows; the ones that need a run to end with no trigger to end its
// grant first disable the trigger for their own length.
type supportFixture struct {
	t    *testing.T
	ctx  context.Context
	pool *pgxpool.Pool
	pg   store.PG
}

func (f supportFixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
}

// run persists a RUNNING run created by owner (a fresh person when "").
func (f supportFixture) run(owner string) types.AgentRun {
	f.t.Helper()
	r := newRun(types.RunRunning)
	r.CreatedBy = owner
	if owner == "" {
		r.CreatedBy = "owner-" + uuid.NewString()
	}
	return persistRun(f.t, f.ctx, f.pool, r)
}

func (f supportFixture) request(r types.AgentRun, helper string) store.SupportGrant {
	f.t.Helper()
	g, err := f.pg.RequestSupportGrant(f.ctx, store.SupportGrant{RunID: r.ID, Helper: helper, HelperEmail: helper + "@example.com",
		Reason: "the build hangs", DoorsAsked: []string{"terminal", "ssh"}, RequestRevSeq: 4})
	if err != nil {
		f.t.Fatalf("RequestSupportGrant: %v", err)
	}
	return g
}

func (f supportFixture) accept(r types.AgentRun, g store.SupportGrant, minutes int) store.SupportGrant {
	f.t.Helper()
	out, err := f.pg.AcceptSupportGrant(f.ctx, r.ID, g.ID, r.CreatedBy, r.CreatedBy+"@example.com", []string{"terminal"}, minutes, 9)
	if err != nil {
		f.t.Fatalf("AcceptSupportGrant: %v", err)
	}
	return out
}

func (f supportFixture) noTrigger(on bool) {
	f.t.Helper()
	if on {
		f.exec(`ALTER TABLE agent_runs DISABLE TRIGGER run_support_grants_end_with_run`)
		f.t.Cleanup(func() { f.exec(`ALTER TABLE agent_runs ENABLE TRIGGER run_support_grants_end_with_run`) })
	}
}

func (f supportFixture) read(id uuid.UUID) store.SupportGrant {
	f.t.Helper()
	open, ended, err := f.pg.SupportGrantStatus(f.ctx, f.runOf(id))
	if err != nil {
		f.t.Fatalf("SupportGrantStatus: %v", err)
	}
	for _, g := range []*store.SupportGrant{open, ended} {
		if g != nil && g.ID == id {
			return *g
		}
	}
	f.t.Fatalf("grant %s is in neither the open nor the last ended slot", id)
	return store.SupportGrant{}
}

func (f supportFixture) runOf(grant uuid.UUID) uuid.UUID {
	f.t.Helper()
	var run uuid.UUID
	if err := f.pool.QueryRow(f.ctx, `SELECT run_id FROM run_support_grants WHERE id = $1`, grant).Scan(&run); err != nil {
		f.t.Fatalf("run of grant %s: %v", grant, err)
	}
	return run
}

// TestPG_SupportGrants pins migration 0293 through the store: the request, the accept, decline and end
// statements and who may use them, the database's clock deciding live and lapsed, the sweep and its claim,
// the helper-since read, and the erasure blank.
func TestPG_SupportGrants(t *testing.T) {
	f := supportFixture{t: t, ctx: context.Background(), pool: runsPGPoolIsolated(t)}
	f.pg = store.NewPG(f.pool)

	t.Run("request, one open grant, accept", func(t *testing.T) {
		f := supportFixture{t: t, ctx: f.ctx, pool: f.pool, pg: f.pg}
		r := f.run("")
		g := f.request(r, "helper-a")
		if g.ID == uuid.Nil || g.AcceptedAt != nil || g.ExpiresAt != nil || g.Minutes != 0 || g.Doors != nil || g.EndedAt != nil || g.EndAudited ||
			g.RequestedAt.IsZero() || g.RequestRevSeq != 4 || g.Helper != "helper-a" || g.Reason != "the build hangs" {
			t.Errorf("requested row = %+v; want a requested, open row with the database's time", g)
		}
		if _, err := f.pg.RequestSupportGrant(f.ctx, store.SupportGrant{RunID: r.ID, Helper: "helper-b", Reason: "x", DoorsAsked: []string{"ssh"}}); !errors.Is(err, store.ErrSupportGrantOpen) {
			t.Errorf("a second request on an open run = %v, want ErrSupportGrantOpen", err)
		}
		other := f.run(r.CreatedBy) // the same owner, so the case below pins run_id and not the owner join
		for name, try := range map[string]func() (store.SupportGrant, error){
			"a person who is not the owner": func() (store.SupportGrant, error) {
				return f.pg.AcceptSupportGrant(f.ctx, r.ID, g.ID, "someone-else", "", []string{"ssh"}, 30, 1)
			},
			"a door that was not asked": func() (store.SupportGrant, error) {
				return f.pg.AcceptSupportGrant(f.ctx, r.ID, g.ID, r.CreatedBy, "", []string{"desktop"}, 30, 1)
			},
			"no doors": func() (store.SupportGrant, error) {
				return f.pg.AcceptSupportGrant(f.ctx, r.ID, g.ID, r.CreatedBy, "", []string{}, 30, 1)
			},
			"another run's id": func() (store.SupportGrant, error) {
				return f.pg.AcceptSupportGrant(f.ctx, other.ID, g.ID, other.CreatedBy, "", []string{"ssh"}, 30, 1)
			},
		} {
			if _, err := try(); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("accept by %s = %v, want ErrNotFound", name, err)
			}
		}
		if _, err := f.pg.AcceptSupportGrant(f.ctx, r.ID, g.ID, r.CreatedBy, "", []string{"ssh"}, 20, 1); err == nil || errors.Is(err, store.ErrNotFound) {
			t.Errorf("accept for 20 minutes = %v, want the minutes CHECK's error", err)
		}
		got := f.accept(r, g, 30)
		if got.AcceptedAt == nil || got.ExpiresAt == nil || got.ExpiresAt.Sub(*got.AcceptedAt) != 30*time.Minute || got.Minutes != 30 ||
			!slices.Equal(got.Doors, []string{"terminal"}) || got.OwnerEmail != r.CreatedBy+"@example.com" || got.AcceptRevSeq != 9 || got.EndedAt != nil {
			t.Errorf("accepted row = %+v; want 30 minutes from the database's accept time, one door, the owner's email and sequence", got)
		}
		if _, err := f.pg.AcceptSupportGrant(f.ctx, r.ID, g.ID, r.CreatedBy, "", []string{"ssh"}, 30, 1); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("a second accept = %v, want ErrNotFound", err)
		}
	})

	t.Run("accept refuses after 15 minutes and on a run that is not running", func(t *testing.T) {
		f := supportFixture{t: t, ctx: f.ctx, pool: f.pool, pg: f.pg}
		f.noTrigger(true) // so the grant is still open when the run is not
		late, fresh, lost, stopped := f.run(""), f.run(""), f.run(""), f.run("")
		for _, c := range []struct {
			r    types.AgentRun
			age  string
			want bool
		}{{late, "15 minutes", false}, {fresh, "14 minutes", true}, {lost, "1 minute", false}, {stopped, "1 minute", false}} {
			g := f.request(c.r, "helper-a")
			f.exec(`UPDATE run_support_grants SET requested_at = now() - $2::interval WHERE id = $1`, g.ID, c.age)
			if c.r.ID == lost.ID {
				f.exec(`UPDATE agent_runs SET lost_at = now(), lost_reason = 'ended' WHERE id = $1`, c.r.ID)
			}
			if c.r.ID == stopped.ID {
				f.exec(`UPDATE agent_runs SET state = 'STOPPED' WHERE id = $1`, c.r.ID)
			}
			_, err := f.pg.AcceptSupportGrant(f.ctx, c.r.ID, g.ID, c.r.CreatedBy, "", []string{"ssh"}, 15, 1)
			if (err == nil) != c.want || (err != nil && !errors.Is(err, store.ErrNotFound)) {
				t.Errorf("accept of a request %s old on a run (lost %v, stopped %v) = %v; want accepted %v", c.age, c.r.ID == lost.ID, c.r.ID == stopped.ID, err, c.want)
			}
			if !c.want && f.read(g.ID).EndedAt != nil {
				t.Errorf("a refused accept of a request %s old ended the grant; it must stay open", c.age)
			}
		}
	})

	t.Run("a request ends a lapsed request and an expired grant first", func(t *testing.T) {
		f := supportFixture{t: t, ctx: f.ctx, pool: f.pool, pg: f.pg}
		lapsed, expired := f.run(""), f.run("")
		old := f.request(lapsed, "helper-a")
		f.exec(`UPDATE run_support_grants SET requested_at = now() - interval '16 minutes' WHERE id = $1`, old.ID)
		oldGrant := f.accept(expired, f.request(expired, "helper-a"), 15)
		f.exec(`UPDATE run_support_grants SET expires_at = now() - interval '1 second' WHERE id = $1`, oldGrant.ID)
		for _, c := range []struct {
			run  types.AgentRun
			old  uuid.UUID
			want string
		}{{lapsed, old.ID, "lapsed"}, {expired, oldGrant.ID, "expired"}} {
			n := f.request(c.run, "helper-b")
			if n.ID == c.old || n.EndedAt != nil {
				t.Errorf("new request on a run whose open row was %s = %+v, want a fresh open row", c.want, n)
			}
			ended := f.read(c.old)
			if ended.EndedAt == nil || ended.EndReason != c.want || ended.EndAudited {
				t.Errorf("the old %s row = ended %v reason %q audited %v; want ended %q, unaudited for the sweep", c.want, ended.EndedAt, ended.EndReason, ended.EndAudited, c.want)
			}
		}
	})

	t.Run("live by the database's clock, ended at once when it is not", func(t *testing.T) {
		f := supportFixture{t: t, ctx: f.ctx, pool: f.pool, pg: f.pg}
		r := f.run("")
		g := f.request(r, "helper-a")
		got, found, live, err := f.pg.SupportGrantLive(f.ctx, r.ID, "helper-a", uuid.Nil, false)
		if err != nil || !found || live || got.ID != g.ID || got.EndedAt != nil {
			t.Fatalf("a request only: found %v live %v ended %v err %v; want found, not live, still open", found, live, got.EndedAt, err)
		}
		f.accept(r, g, 15)
		if _, found, _, _ := f.pg.SupportGrantLive(f.ctx, r.ID, "someone-else", uuid.Nil, false); found {
			t.Error("another helper found the grant")
		}
		if _, found, _, _ := f.pg.SupportGrantLive(f.ctx, r.ID, "helper-a", uuid.New(), false); found {
			t.Error("a grant id that is not the open grant's was found")
		}
		f.exec(`UPDATE run_support_grants SET expires_at = now() + interval '3 seconds' WHERE id = $1`, g.ID)
		if got, found, live, err := f.pg.SupportGrantLive(f.ctx, r.ID, "helper-a", g.ID, false); err != nil || !found || !live || got.LastSeenAt != nil {
			t.Errorf("just before expiry: found %v live %v seen %v err %v; want live and unstamped", found, live, got.LastSeenAt, err)
		}
		f.exec(`UPDATE run_support_grants SET expires_at = now() + interval '1 hour' WHERE id = $1`, g.ID) // a slow database cannot cross the 3 seconds below
		// Stamped once, left alone inside 30 seconds, moved after.
		first, _, _, _ := f.pg.SupportGrantLive(f.ctx, r.ID, "helper-a", uuid.Nil, true)
		if first.LastSeenAt == nil {
			t.Fatal("a live read that stamps left last_seen_at unset")
		}
		f.pg.SupportGrantLive(f.ctx, r.ID, "helper-a", uuid.Nil, true) //nolint:errcheck // the row is read back below
		if seen := f.read(g.ID).LastSeenAt; seen == nil || !seen.Equal(*first.LastSeenAt) {
			t.Errorf("a second stamp inside 30 seconds moved last_seen_at from %v to %v", first.LastSeenAt, seen)
		}
		f.exec(`UPDATE run_support_grants SET last_seen_at = now() - interval '31 seconds' WHERE id = $1`, g.ID)
		if moved, _, live, _ := f.pg.SupportGrantLive(f.ctx, r.ID, "helper-a", uuid.Nil, true); !live || moved.LastSeenAt == nil || time.Since(*moved.LastSeenAt) > time.Minute {
			t.Errorf("after 31 seconds the stamp = %v live %v, want moved to now", moved.LastSeenAt, live)
		}
		f.exec(`UPDATE run_support_grants SET expires_at = now() WHERE id = $1`, g.ID)
		ended, found, live, err := f.pg.SupportGrantLive(f.ctx, r.ID, "helper-a", uuid.Nil, true)
		if err != nil || !found || live || ended.EndedAt == nil || ended.EndReason != "expired" || ended.EndAudited {
			t.Errorf("at expiry: found %v live %v reason %q audited %v err %v; want ended 'expired', unaudited", found, live, ended.EndReason, ended.EndAudited, err)
		}
		if _, found, _, _ := f.pg.SupportGrantLive(f.ctx, r.ID, "helper-a", uuid.Nil, false); found {
			t.Error("an expired grant was still found open on the next read")
		}
	})

	t.Run("a grant on a run that is over ends as run_ended without the trigger", func(t *testing.T) {
		f := supportFixture{t: t, ctx: f.ctx, pool: f.pool, pg: f.pg}
		f.noTrigger(true)
		r := f.run("")
		g := f.accept(r, f.request(r, "helper-a"), 60)
		f.exec(`UPDATE agent_runs SET state = 'STOPPED' WHERE id = $1`, r.ID)
		got, found, live, err := f.pg.SupportGrantLive(f.ctx, r.ID, "helper-a", g.ID, false)
		if err != nil || !found || live || got.EndReason != "run_ended" || got.EndedAt == nil || got.EndAudited {
			t.Errorf("a stopped run's grant: found %v live %v reason %q audited %v err %v; want ended 'run_ended', unaudited", found, live, got.EndReason, got.EndAudited, err)
		}
	})

	t.Run("decline and end", func(t *testing.T) {
		f := supportFixture{t: t, ctx: f.ctx, pool: f.pool, pg: f.pg}
		r := f.run("")
		g := f.request(r, "helper-a")
		if _, err := f.pg.DeclineSupportGrant(f.ctx, r.ID, g.ID, "someone-else"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("decline by a non-owner = %v, want ErrNotFound", err)
		}
		d, err := f.pg.DeclineSupportGrant(f.ctx, r.ID, g.ID, r.CreatedBy)
		if err != nil || d.EndReason != "declined" || !d.EndAudited || d.EndedAt == nil {
			t.Errorf("decline = %+v, %v; want ended 'declined', audited", d, err)
		}
		a := f.accept(r, f.request(r, "helper-a"), 15)
		if _, err := f.pg.DeclineSupportGrant(f.ctx, r.ID, a.ID, r.CreatedBy); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("decline of an accepted grant = %v, want ErrNotFound", err)
		}
		if _, err := f.pg.EndSupportGrant(f.ctx, uuid.New(), a.ID, "revoked", true); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("end naming another run = %v, want ErrNotFound", err)
		}
		e, err := f.pg.EndSupportGrant(f.ctx, r.ID, a.ID, "revoked", true)
		if err != nil || e.EndReason != "revoked" || !e.EndAudited || e.EndedAt == nil {
			t.Errorf("end = %+v, %v; want ended 'revoked', audited", e, err)
		}
		if _, err := f.pg.EndSupportGrant(f.ctx, r.ID, a.ID, "ended_by_helper", false); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("a second end = %v, want ErrNotFound", err)
		}
		open, last, err := f.pg.SupportGrantStatus(f.ctx, r.ID)
		if err != nil || open != nil || last == nil || last.ID != a.ID {
			t.Errorf("status = open %v last %v, %v; want no open row and the revoked grant last", open, last, err)
		}
		fresh := f.request(r, "helper-a")
		if open, last, _ := f.pg.SupportGrantStatus(f.ctx, r.ID); open == nil || open.ID != fresh.ID || last == nil || last.ID != a.ID {
			t.Errorf("status with a new request = open %v last %v; want the request open and the revoked grant last", open, last)
		}
		if open, last, err := f.pg.SupportGrantStatus(f.ctx, uuid.New()); err != nil || open != nil || last != nil {
			t.Errorf("status of a run with none = %v, %v, %v", open, last, err)
		}
	})

	t.Run("sweep ends what is over and claims each end once", func(t *testing.T) {
		f := supportFixture{t: t, ctx: f.ctx, pool: f.pool, pg: f.pg}
		f.noTrigger(true)
		if _, err := f.pg.SweepSupportGrants(f.ctx, false); err != nil { // flush the earlier cases' ends
			t.Fatalf("SweepSupportGrants: %v", err)
		}
		lapsedRun, expiredRun, overRun, liveRun, routeRun := f.run(""), f.run(""), f.run(""), f.run(""), f.run("")
		lapsed := f.request(lapsedRun, "helper-a")
		f.exec(`UPDATE run_support_grants SET requested_at = now() - interval '20 minutes' WHERE id = $1`, lapsed.ID)
		expired := f.accept(expiredRun, f.request(expiredRun, "helper-a"), 15)
		f.exec(`UPDATE run_support_grants SET expires_at = now() - interval '1 second' WHERE id = $1`, expired.ID)
		over := f.accept(overRun, f.request(overRun, "helper-a"), 15)
		f.exec(`UPDATE agent_runs SET lost_at = now(), lost_reason = 'ended' WHERE id = $1`, overRun.ID)
		live := f.accept(liveRun, f.request(liveRun, "helper-a"), 60)
		route := f.accept(routeRun, f.request(routeRun, "helper-a"), 60)
		if _, err := f.pg.EndSupportGrant(f.ctx, routeRun.ID, route.ID, "revoked", true); err != nil {
			t.Fatal(err)
		}

		claimed, err := f.pg.SweepSupportGrants(f.ctx, false)
		if err != nil {
			t.Fatalf("SweepSupportGrants: %v", err)
		}
		got := map[uuid.UUID]store.SupportGrant{}
		for _, g := range claimed {
			got[g.ID] = g
		}
		for id, reason := range map[uuid.UUID]string{lapsed.ID: "lapsed", expired.ID: "expired", over.ID: "run_ended"} {
			if g, ok := got[id]; !ok || g.EndReason != reason || g.EndedAt == nil || !g.EndAudited {
				t.Errorf("sweep claimed %+v for a grant that should end %q", g, reason)
			}
		}
		if _, ok := got[live.ID]; ok {
			t.Error("sweep claimed the live grant")
		}
		if _, ok := got[route.ID]; ok {
			t.Error("sweep claimed a grant whose end route already audited it")
		}
		if again, err := f.pg.SweepSupportGrants(f.ctx, false); err != nil || len(again) != 0 {
			t.Errorf("a second sweep claimed %d grants, %v; want none: each end is claimed once", len(again), err)
		}
		refs, err := f.pg.LiveSupportGrants(f.ctx)
		if err != nil || !slices.Contains(refs, store.SupportGrantRef{RunID: liveRun.ID, ID: live.ID}) || slices.ContainsFunc(refs, func(r store.SupportGrantRef) bool {
			return r.ID == lapsed.ID || r.ID == expired.ID || r.ID == over.ID || r.ID == route.ID
		}) {
			t.Errorf("LiveSupportGrants = %v, %v; want the live grant and none of the ended ones", refs, err)
		}
		off, err := f.pg.SweepSupportGrants(f.ctx, true)
		if i := slices.IndexFunc(off, func(g store.SupportGrant) bool { return g.ID == live.ID }); err != nil || i < 0 || off[i].EndReason != "support_off" {
			t.Errorf("sweep with the switch off = %+v, %v; want the live grant ended 'support_off'", off, err)
		}
	})

	t.Run("the helper was accepted since a time", func(t *testing.T) {
		f := supportFixture{t: t, ctx: f.ctx, pool: f.pool, pg: f.pg}
		r, other := f.run(""), f.run("")
		g := f.request(r, "helper-a")
		since := func(run uuid.UUID, who string, at time.Time) bool {
			t.Helper()
			ok, err := f.pg.SupportHelperSince(f.ctx, run, who, at)
			if err != nil {
				t.Fatalf("SupportHelperSince: %v", err)
			}
			return ok
		}
		at := g.RequestedAt
		for name, c := range map[string]struct {
			got, want bool
		}{
			"requested only, at after":       {since(r.ID, "helper-a", at.Add(time.Hour)), false},
			"requested only, zero at":        {since(r.ID, "helper-a", time.Time{}), false},
			"requested only, at equal":       {since(r.ID, "helper-a", at), false},
			"requested only, another run":    {since(other.ID, "helper-a", time.Time{}), false},
			"requested only, another helper": {since(r.ID, "helper-b", time.Time{}), false},
			"requested only, at before":      {since(r.ID, "helper-a", at.Add(-time.Second)), false},
		} {
			if c.got != c.want {
				t.Errorf("%s = %v, want %v", name, c.got, c.want)
			}
		}
		f.accept(r, g, 15)
		for name, c := range map[string]struct {
			got, want bool
		}{
			"accepted, at before requested_at": {since(r.ID, "helper-a", at.Add(-time.Second)), false},
			"accepted, at equal":               {since(r.ID, "helper-a", at), true},
			"accepted, at after":               {since(r.ID, "helper-a", at.Add(time.Second)), true},
			"accepted, zero at":                {since(r.ID, "helper-a", time.Time{}), true},
			"accepted, another helper":         {since(r.ID, "helper-b", time.Time{}), false},
			"accepted, another run":            {since(other.ID, "helper-a", time.Time{}), false},
		} {
			if c.got != c.want {
				t.Errorf("%s = %v, want %v", name, c.got, c.want)
			}
		}
		if _, err := f.pg.EndSupportGrant(f.ctx, r.ID, g.ID, "ended_by_helper", true); err != nil {
			t.Fatal(err)
		}
		if !since(r.ID, "helper-a", at) || !since(r.ID, "helper-a", time.Time{}) {
			t.Error("an ended grant no longer counts for the helper")
		}
	})

	t.Run("erasure blanks a person's grants and no one else's", func(t *testing.T) {
		f := supportFixture{t: t, ctx: f.ctx, pool: f.pool, pg: f.pg}
		person := "erased-" + uuid.NewString()
		mine, theirs, unrelated := f.run(person), f.run(""), f.run("")
		asOwner := f.accept(mine, f.request(mine, "helper-a"), 15)
		asHelper := f.request(theirs, person)
		kept := f.request(unrelated, "helper-b")
		if _, err := f.pg.BlankRunTasks(f.ctx, person); err != nil {
			t.Fatalf("BlankRunTasks: %v", err)
		}
		for _, g := range []store.SupportGrant{asOwner, asHelper, kept} {
			var reason, helperEmail, ownerEmail string
			if err := f.pool.QueryRow(f.ctx, `SELECT reason, helper_email, owner_email FROM run_support_grants WHERE id = $1`, g.ID).Scan(&reason, &helperEmail, &ownerEmail); err != nil {
				t.Fatal(err)
			}
			blank := reason == "" && helperEmail == "" && ownerEmail == ""
			if want := g.ID != kept.ID; blank != want {
				t.Errorf("grant %s blank = %v, want %v (reason %q, emails %q %q)", g.ID, blank, want, reason, helperEmail, ownerEmail)
			}
		}
	})

	t.Run("a ticket and a lease carry the support marks", func(t *testing.T) {
		f := supportFixture{t: t, ctx: f.ctx, pool: f.pool, pg: f.pg}
		r := f.run("")
		now := time.Now().UTC().Truncate(time.Microsecond)
		grant, ends := uuid.New(), now.Add(30*time.Minute)
		for name, tk := range map[string]store.AttachTicket{
			"a support ticket": {RunID: r.ID, ActorType: types.ActorHuman, Principal: "helper-a", Role: "admin", SupportGrant: grant, SupportExpires: ends},
			"any other ticket": {RunID: r.ID, ActorType: types.ActorHuman, Principal: "owner", Role: "member"},
		} {
			token := uuid.NewString()
			if err := f.pg.MintAttachTicket(f.ctx, token, tk, now, now.Add(time.Minute)); err != nil {
				t.Fatalf("%s: mint: %v", name, err)
			}
			got, ok, err := f.pg.ConsumeAttachTicket(f.ctx, token, now)
			if err != nil || !ok || got.SupportGrant != tk.SupportGrant || !got.SupportExpires.Equal(tk.SupportExpires) {
				t.Errorf("%s consumed as grant %s expires %v (ok %v, %v); want grant %s expires %v", name, got.SupportGrant, got.SupportExpires, ok, err, tk.SupportGrant, tk.SupportExpires)
			}
		}
		support := func() bool {
			var v bool
			if err := f.pool.QueryRow(f.ctx, `SELECT support FROM run_attach_leases WHERE run_id = $1`, r.ID).Scan(&v); err != nil {
				t.Fatal(err)
			}
			return v
		}
		lease := store.AttachLease{RunID: r.ID, HolderID: uuid.New(), Replica: "r1", Principal: "helper-a", Source: "web", Since: now, Support: true}
		if ok, err := f.pg.AcquireAttachLease(f.ctx, lease, uuid.Nil, time.Minute); err != nil || !ok || !support() {
			t.Fatalf("acquire as a helper = %v, %v, support %v; want held with support true", ok, err, support())
		}
		f.exec(`UPDATE run_attach_leases SET expires_at = now() - interval '1 second' WHERE run_id = $1`, r.ID)
		owner := store.AttachLease{RunID: r.ID, HolderID: uuid.New(), Replica: "r1", Principal: "owner", Source: "web", Since: now}
		if ok, err := f.pg.AcquireAttachLease(f.ctx, owner, uuid.Nil, time.Minute); err != nil || !ok || support() {
			t.Fatalf("acquire over a lapsed support lease = %v, %v, support %v; want held with support false", ok, err, support())
		}
		f.exec(`UPDATE run_attach_leases SET expires_at = now() - interval '1 second' WHERE run_id = $1`, r.ID)
		if ok, err := f.pg.ReacquireAttachLease(f.ctx, lease, time.Minute); err != nil || !ok || !support() {
			t.Fatalf("reacquire as a helper = %v, %v, support %v; want held with support true", ok, err, support())
		}
	})

	t.Run("a role without the table's grant gets an empty sweep and list", func(t *testing.T) {
		f := supportFixture{t: t, ctx: f.ctx, pool: f.pool, pg: f.pg}
		r := f.run("")
		g := f.request(r, "helper-a")
		f.accept(r, g, 15)
		role := "wardyn_nogrant_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
		f.exec(`CREATE ROLE ` + role + ` NOLOGIN`)
		t.Cleanup(func() { _, _ = f.pool.Exec(context.Background(), `DROP ROLE IF EXISTS `+role) })
		cfg := f.pool.Config()
		cfg.ConnConfig.RuntimeParams["role"] = role // every connection of this pool runs as the role
		restricted, err := pgxpool.NewWithConfig(f.ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(restricted.Close)
		pg := store.NewPG(restricted)

		if got, err := pg.SweepSupportGrants(f.ctx, true); err != nil || len(got) != 0 {
			t.Errorf("sweep without UPDATE = %v, %v; want no rows and no error", got, err)
		}
		if f.read(g.ID).EndedAt != nil {
			t.Error("the sweep of a role without UPDATE still ended a grant (off = true)")
		}
		if got, err := pg.LiveSupportGrants(f.ctx); err != nil || len(got) != 0 {
			t.Errorf("live list without SELECT = %v, %v; want no rows and no error", got, err)
		}
		if got, err := f.pg.LiveSupportGrants(f.ctx); err != nil || len(got) != 1 {
			t.Fatalf("setup: the owner role lists %v, %v; want the one live grant", got, err)
		}
		f.exec(`GRANT SELECT ON run_support_grants TO ` + role)
		if got, err := pg.LiveSupportGrants(f.ctx); err != nil || len(got) != 1 {
			t.Errorf("live list with SELECT = %v, %v; want the one live grant", got, err)
		}
		if got, err := pg.SweepSupportGrants(f.ctx, false); err != nil || len(got) != 0 {
			t.Errorf("sweep with SELECT only = %v, %v; want still no rows and no error", got, err)
		}
	})
}
