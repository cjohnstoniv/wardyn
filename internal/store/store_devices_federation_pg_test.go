// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// What a device may NOT decide about the rows it forwards, and what one device
// may not do to the organisation's own audit writers. Guarded by
// WARDYN_TEST_PG like the rest of this package's Postgres tests.
package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A device can compute a valid hash for ANY claim, so the hash check proves
// only that the device signed what it sent. A row naming one of the
// organisation's own runs is refused outright — phase one federates audit
// rows, never run records — so a laptop cannot file "ceo@corp.example
// approved" into an org run's evidence trail.
func TestPG_Devices_IngestRefusesARowUnderAnOrgRun(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	run := persistRun(t, ctx, pool, newRun(types.RunRunning))
	d, err := st.CreateDevice(ctx, types.Device{ID: uuid.New(), Name: "laptop"}, "wdd_"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	ev := types.AuditEvent{
		ID: uuid.New(), Time: time.Now().UTC().Truncate(time.Microsecond), RunID: &run.ID,
		ActorType: types.ActorHuman, Actor: "ceo@corp.example", Action: "approval.decide",
		Target: "approval:" + uuid.NewString(), Outcome: "success", SourceIP: "10.0.0.1",
		Data: json.RawMessage(`{"decision":"APPROVED"}`),
	}
	ev.RowHash = auditRowHash(t, pool, "", ev)
	_, err = st.IngestDeviceAudit(ctx, d.ID, testPeer, []types.FederatedAuditEvent{{AuditEvent: ev, Seq: 1}})
	if !errors.Is(err, store.ErrFederatedOrgRun) {
		t.Fatalf("ingest of a row under org run %s: err = %v, want ErrFederatedOrgRun", run.ID, err)
	}
	got, err := st.QueryAuditEvents(ctx, run.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range got {
		if strings.HasSuffix(g.Actor, "ceo@corp.example") {
			t.Errorf("a device-forwarded row sits under org run %s as %s %q, action %q, source_ip %q", run.ID, g.ActorType, g.Actor, g.Action, g.SourceIP)
		}
	}
	if devs, _ := st.ListDevices(ctx); len(devs) > 0 {
		for _, dv := range devs {
			if dv.ID == d.ID && dv.LastSeq != 0 {
				t.Errorf("refused batch moved the device cursor to %d", dv.LastSeq)
			}
		}
	}
}

// The source_ip column records the peer the organisation saw — routes.go
// refuses to take it from any caller — and the device's claim moves into
// data.device_origin. Both chains still verify: the device's claim recomputes
// from the stored row, and the organisation's own chain links through it.
func TestPG_Devices_IngestRecordsTheObservedPeerAndBothChainsVerify(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	d, err := st.CreateDevice(ctx, types.Device{ID: uuid.New(), Name: "laptop"}, "wdd_"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	laptopRun := uuid.New() // the laptop's own run: not in this organisation's agent_runs
	var rows []types.FederatedAuditEvent
	prev := ""
	for seq := int64(1); seq <= 3; seq++ {
		ev := types.AuditEvent{
			ID: uuid.New(), Time: time.Now().UTC().Truncate(time.Microsecond), RunID: &laptopRun,
			ActorType: types.ActorHuman, Actor: "dev@corp.example", Action: "run.create", Outcome: "success",
			SourceIP: "10.9.9.9", PrevHash: prev,
			Data: json.RawMessage(`{"big":9007199254740993,"nested":{"b":1,"a":[1,2]},"s":"x"}`),
		}
		ev.RowHash = auditRowHash(t, pool, prev, ev)
		prev = ev.RowHash
		rows = append(rows, types.FederatedAuditEvent{AuditEvent: ev, Seq: seq})
	}
	res, err := st.IngestDeviceAudit(ctx, d.ID, testPeer, rows)
	if err != nil || res.Accepted != 3 {
		t.Fatalf("ingest: %+v %v", res, err)
	}

	q := `
		SELECT e.seq, e.source_ip, e.data->'device_origin'->>'source_ip',
		       ` + store.FederatedClaimHashSQL + ` = e.data->'device_origin'->>'row_hash',
		       e.row_hash = audit_row_hash(e.prev_hash, e.id, e.time, e.run_id, e.actor_type, e.actor,
		                                   e.action, e.target, e.outcome, e.source_ip, e.data),
		       COALESCE(e.prev_hash, '') = COALESCE((SELECT p.row_hash FROM audit_events p
		                                             WHERE p.seq < e.seq AND p.row_hash IS NOT NULL ORDER BY p.seq DESC LIMIT 1), '')
		FROM audit_events e
		WHERE e.data->'device_origin'->>'device_id' = $1
		ORDER BY e.seq`
	dbRows, err := pool.Query(ctx, q, d.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	defer dbRows.Close()
	n := 0
	for dbRows.Next() {
		var seq int64
		var sourceIP, claimed string
		var deviceHashOK, orgHashOK, orgLinkOK bool
		if err := dbRows.Scan(&seq, &sourceIP, &claimed, &deviceHashOK, &orgHashOK, &orgLinkOK); err != nil {
			t.Fatal(err)
		}
		n++
		if sourceIP != testPeer || claimed != "10.9.9.9" {
			t.Errorf("seq %d: source_ip %q, device_origin.source_ip %q; want the observed peer %q and the claim 10.9.9.9", seq, sourceIP, claimed, testPeer)
		}
		if !deviceHashOK {
			t.Errorf("seq %d: the device's claimed row_hash does not recompute from the stored row", seq)
		}
		if !orgHashOK || !orgLinkOK {
			t.Errorf("seq %d: organisation chain broken (row hash ok=%v, link ok=%v)", seq, orgHashOK, orgLinkOK)
		}
	}
	if err := dbRows.Err(); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("found %d federated rows, want 3", n)
	}
	status, err := st.VerifyAuditChain(ctx)
	if err != nil || !status.OK {
		t.Fatalf("organisation chain verify after ingest: %+v %v", status, err)
	}
}

// A 500-row batch whose last hash is wrong is refused WITHOUT the chain lock:
// the recompute runs before any lock, so neither the refusal nor a replay of
// it can make the organisation's own writers wait. The deterministic half
// holds the chain lock in another session and expects the refusal well
// inside the lock timeout; the load half replays the batch from many workers
// while the organisation writes its own rows (WARDYN_TEST_INGEST_WORKERS,
// default 16).
func TestPG_Devices_RefusedBatchReplayDoesNotStarveOrgAuditWriters(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	d, err := st.CreateDevice(ctx, types.Device{ID: uuid.New(), Name: "laptop"}, "wdd_"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	pad := strings.Repeat("x", 16000)
	batch := make([]types.FederatedAuditEvent, 0, 500)
	prev := ""
	for i := 1; i <= 500; i++ {
		ev := types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC().Truncate(time.Microsecond), ActorType: types.ActorSystem,
			Actor: "device", Action: "x", Outcome: "success", Data: json.RawMessage(`{"pad":"` + pad + `"}`), PrevHash: prev}
		ev.RowHash = auditRowHash(t, pool, prev, ev)
		prev = ev.RowHash
		batch = append(batch, types.FederatedAuditEvent{AuditEvent: ev, Seq: int64(i)})
	}
	batch[499].RowHash = "deadbeef"

	holder, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_lock($1)`, db.AuditChainLockKey); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = st.IngestDeviceAudit(ctx, d.ID, testPeer, batch)
	single := time.Since(start)
	_, _ = holder.Exec(ctx, `SELECT pg_advisory_unlock($1)`, db.AuditChainLockKey)
	holder.Release()
	t.Logf("one 500-row x 16 KiB batch, refused on row 500, chain lock held elsewhere: %s (err=%v)", single, err)
	if !errors.Is(err, store.ErrConflict) || single >= db.AuditChainLockTimeout {
		t.Fatalf("refused batch: err=%v after %s — want ErrConflict without waiting on the chain lock", err, single)
	}

	workers := 16
	if n, _ := strconv.Atoi(os.Getenv("WARDYN_TEST_INGEST_WORKERS")); n > 0 {
		workers = n
	}
	const window = 15 * time.Second
	deadline := time.Now().Add(window)
	var replays atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				_, _ = st.IngestDeviceAudit(ctx, d.ID, testPeer, batch)
				replays.Add(1)
			}
		}()
	}
	var writes, failures int
	var maxLat time.Duration
	for time.Now().Before(deadline) {
		ev := types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorSystem, Actor: "wardyn/test",
			Action: "run.create", Outcome: "success", Data: json.RawMessage(`{}`)}
		s := time.Now()
		err := store.InsertAuditEvent(ctx, pool, &ev)
		maxLat = max(maxLat, time.Since(s))
		writes++
		if err != nil {
			failures++
			if failures <= 3 {
				t.Logf("org writer error: %v", err)
			}
		}
	}
	wg.Wait()
	t.Logf("%d workers replaying for %s: %d replays; org writer: %d attempts, %d failed, max latency %s",
		workers, window, replays.Load(), writes, failures, maxLat)
	if failures > 0 || maxLat > 5*time.Second {
		t.Errorf("org audit writes failed %d times (max latency %s) while one device replayed a refused batch", failures, maxLat)
	}
}

// A device revoked after its request was authenticated is ErrDeviceRevoked —
// never ErrConflict, which the handler records as a broken chain.
func TestPG_Devices_IngestForARevokedDeviceIsErrDeviceRevoked(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	d, err := st.CreateDevice(ctx, types.Device{ID: uuid.New(), Name: "laptop"}, "wdd_"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RevokeDevice(ctx, d.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	r1 := chainedRow(t, pool, "", 1, "run.create")
	if _, err := st.IngestDeviceAudit(ctx, d.ID, testPeer, []types.FederatedAuditEvent{r1}); !errors.Is(err, store.ErrDeviceRevoked) || errors.Is(err, store.ErrConflict) {
		t.Fatalf("ingest for a revoked device: err = %v, want ErrDeviceRevoked and not ErrConflict", err)
	}
}

// ─── every accepted row re-checks; everything else is refused ────────────────

// storedFederation is one stored federated row as a verifier sees it: the
// device's claim recomputed with store.FederatedClaimHashSQL, and the
// organisation's own chain link and hash.
type storedFederation struct {
	seq                             int64
	target, data                    string
	deviceHashOK, orgHashOK, linkOK bool
}

func storedFederated(t *testing.T, pool *pgxpool.Pool, deviceID uuid.UUID) []storedFederation {
	t.Helper()
	q := `
		SELECT e.seq, e.target, e.data::text,
		       ` + store.FederatedClaimHashSQL + ` = e.data->'device_origin'->>'row_hash',
		       e.row_hash = audit_row_hash(e.prev_hash, e.id, e.time, e.run_id, e.actor_type, e.actor,
		                                   e.action, e.target, e.outcome, e.source_ip, e.data),
		       COALESCE(e.prev_hash, '') = COALESCE((SELECT p.row_hash FROM audit_events p
		                                             WHERE p.seq < e.seq AND p.row_hash IS NOT NULL ORDER BY p.seq DESC LIMIT 1), '')
		FROM audit_events e WHERE e.actor = $1 ORDER BY e.seq`
	rows, err := pool.Query(context.Background(), q, store.FederatedActor(deviceID, "federation:"+deviceID.String()))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []storedFederation
	for rows.Next() {
		var s storedFederation
		if err := rows.Scan(&s.seq, &s.target, &s.data, &s.deviceHashOK, &s.orgHashOK, &s.linkOK); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func federationDevice(t *testing.T, st store.PG) types.Device {
	t.Helper()
	d, err := st.CreateDevice(context.Background(), types.Device{ID: uuid.New(), Name: "laptop"}, "wdd_"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// federationRow is one claim with a genuine hash over exactly what it says;
// data nil is an absent (SQL NULL) value.
func federationRow(t *testing.T, pool *pgxpool.Pool, d types.Device, prev string, seq int64, target string, data json.RawMessage) types.FederatedAuditEvent {
	t.Helper()
	ev := types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC().Truncate(time.Microsecond), ActorType: types.ActorHuman,
		Actor: "federation:" + d.ID.String(), Action: "run.create", Target: target, Outcome: "success",
		SourceIP: "10.9.9.9", PrevHash: prev, Data: data}
	ev.RowHash = auditRowHash(t, pool, prev, ev)
	return types.FederatedAuditEvent{AuditEvent: ev, Seq: seq}
}

// Every shape a laptop really stores is accepted, and each accepted row's
// claim recomputes from the stored row alone — JSON null (a row written with
// no data) and an absent value (the boot canary's insert names no data column)
// included, which is what device_origin.data_null exists for.
func TestPG_Devices_EveryAcceptedRowReChecksFromTheStoredRow(t *testing.T) {
	pool := runsPGPool(t)
	st := store.NewPG(pool)
	laptopCapped := strings.Repeat("t", store.MaxAuditTargetLen) + store.AuditTargetTruncatedMarker
	for _, c := range []struct {
		name, target string
		data         json.RawMessage
	}{
		{"object", "t", json.RawMessage(`{"a":1}`)},
		{"empty object", "t", json.RawMessage(`{}`)},
		{"JSON null", "t", json.RawMessage(`null`)},
		{"absent", "t", nil},
		{"duplicate keys, big numbers, escapes", "t", json.RawMessage(`{"a":1,"a":2,"n":{"k":1,"k":2},"A":"\u00e9","big":123456789012345678901234567890,"f":1.0}`)},
		{"an object holding a data key", "t", json.RawMessage(`{"data":[1]}`)},
		{"a target the laptop already capped", laptopCapped, json.RawMessage(`{"a":1}`)},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := federationDevice(t, st)
			res, err := st.IngestDeviceAudit(context.Background(), d.ID, testPeer,
				[]types.FederatedAuditEvent{federationRow(t, pool, d, "", 1, c.target, c.data)})
			if err != nil || res.Accepted != 1 {
				t.Fatalf("ingest: %+v %v", res, err)
			}
			got := storedFederated(t, pool, d.ID)
			if len(got) != 1 {
				t.Fatalf("stored %d rows, want 1", len(got))
			}
			if !got[0].deviceHashOK {
				t.Errorf("the device's claim does not recompute from the stored row: data=%s", got[0].data)
			}
			if !got[0].orgHashOK || !got[0].linkOK {
				t.Errorf("organisation chain broken (hash ok=%v, link ok=%v)", got[0].orgHashOK, got[0].linkOK)
			}
		})
	}
}

// A claim the organisation could only store by changing it — so that the
// device's hash would no longer recompute — is refused whole, before any
// database work: another device_origin (this organisation's marker), data that
// is not an object, a target no laptop's own insert could have produced.
func TestPG_Devices_UnreCheckableClaimsAreRefused(t *testing.T) {
	pool := runsPGPool(t)
	st := store.NewPG(pool)
	for _, c := range []struct {
		name, target string
		data         json.RawMessage
	}{
		{"a device_origin key", "t", json.RawMessage(`{"device_origin":{"device_id":"` + uuid.NewString() + `"},"x":1}`)},
		{"an array", "t", json.RawMessage(`[1,2]`)},
		{"a string", "t", json.RawMessage(`"str"`)},
		{"a number", "t", json.RawMessage(`42`)},
		{"a boolean", "t", json.RawMessage(`true`)},
		{"a target over the cap", strings.Repeat("t", store.MaxAuditTargetLen+50), json.RawMessage(`{"a":1}`)},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := federationDevice(t, st)
			_, err := st.IngestDeviceAudit(context.Background(), d.ID, testPeer,
				[]types.FederatedAuditEvent{federationRow(t, pool, d, "", 1, c.target, c.data)})
			if !errors.Is(err, store.ErrFederatedRowInvalid) {
				t.Fatalf("err = %v, want ErrFederatedRowInvalid", err)
			}
			if got := storedFederated(t, pool, d.ID); len(got) != 0 {
				t.Fatalf("a refused claim stored %d rows", len(got))
			}
		})
	}
}

// A claimed value Postgres cannot represent — a \u0000 escape in data, a NUL
// in text — is the device's fault. ErrFederatedRowInvalid (a 4xx the
// forwarder stops on), never a store error it would retry forever.
func TestPG_Devices_UnstorableClaimedValueIsErrFederatedRowInvalid(t *testing.T) {
	pool := runsPGPool(t)
	st := store.NewPG(pool)
	for _, c := range []struct {
		name  string
		actor string
		data  json.RawMessage
	}{
		{"a \\u0000 escape in data", "dev", json.RawMessage(`{"s":"a\u0000b"}`)},
		{"a NUL in a text column", "dev\x00ice", json.RawMessage(`{"a":1}`)},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := federationDevice(t, st)
			ev := types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorHuman, Actor: c.actor,
				Action: "run.create", Outcome: "success", Data: c.data, RowHash: "claimed"}
			_, err := st.IngestDeviceAudit(context.Background(), d.ID, testPeer, []types.FederatedAuditEvent{{AuditEvent: ev, Seq: 1}})
			if !errors.Is(err, store.ErrFederatedRowInvalid) {
				t.Fatalf("err = %v, want ErrFederatedRowInvalid", err)
			}
		})
	}
}

// A row chained on a head a reset has since superseded is refused: each
// recomputed hash covers the row's own claimed prev_hash, and the link to the
// recorded head is checked under the device row's lock.
func TestPG_Devices_RowChainedOnASupersededHeadIsRefused(t *testing.T) {
	pool := runsPGPool(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	d := federationDevice(t, st)
	a1 := federationRow(t, pool, d, "", 1, "a1", json.RawMessage(`{}`))
	if _, err := st.IngestDeviceAudit(ctx, d.ID, testPeer, []types.FederatedAuditEvent{a1}); err != nil {
		t.Fatal(err)
	}
	g5 := federationRow(t, pool, d, "", 5, "g5", json.RawMessage(`{}`))
	if res, err := st.IngestDeviceAudit(ctx, d.ID, testPeer, []types.FederatedAuditEvent{g5}); err != nil || !res.Reset {
		t.Fatalf("reset: %+v %v", res, err)
	}
	b6 := federationRow(t, pool, d, a1.RowHash, 6, "b6", json.RawMessage(`{}`))
	if _, err := st.IngestDeviceAudit(ctx, d.ID, testPeer, []types.FederatedAuditEvent{b6}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a row chained on a superseded head: err = %v, want ErrConflict", err)
	}
}

// A retry whose already-ingested prefix was edited is refused: every claimed
// hash is recomputed, the skipped prefix included.
func TestPG_Devices_RetryWithAnEditedIngestedPrefixIsRefused(t *testing.T) {
	pool := runsPGPool(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	d := federationDevice(t, st)
	r1 := federationRow(t, pool, d, "", 1, "t", json.RawMessage(`{"a":1}`))
	if _, err := st.IngestDeviceAudit(ctx, d.ID, testPeer, []types.FederatedAuditEvent{r1}); err != nil {
		t.Fatal(err)
	}
	r2 := federationRow(t, pool, d, r1.RowHash, 2, "t", json.RawMessage(`{"a":2}`))
	edited := r1
	edited.Data = json.RawMessage(`{"a":"edited"}`)
	if _, err := st.IngestDeviceAudit(ctx, d.ID, testPeer, []types.FederatedAuditEvent{edited, r2}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("retry with an edited prefix: err = %v, want ErrConflict", err)
	}
}

// Concurrent pushes from one device, the organisation's own writers and
// TouchDevice together: no deadlock and no lock timeout (the device row is
// always taken before the chain lock, and no chain-lock holder touches a
// device row), and the chain verifies afterwards.
func TestPG_Devices_ConcurrentPushesNeitherDeadlockNorBreakTheChain(t *testing.T) {
	pool := runsPGPool(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	d := federationDevice(t, st)
	deadline := time.Now().Add(4 * time.Second)
	var mu sync.Mutex
	var errs []string
	prev, seq := "", int64(0)
	next := func() []types.FederatedAuditEvent {
		mu.Lock()
		defer mu.Unlock()
		seq++
		r := federationRow(t, pool, d, prev, seq, "m", json.RawMessage(`{"m":1}`))
		prev = r.RowHash
		return []types.FederatedAuditEvent{r}
	}
	var pushes, writes atomic.Int64
	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				if _, err := st.IngestDeviceAudit(ctx, d.ID, testPeer, next()); err != nil && !errors.Is(err, store.ErrConflict) {
					mu.Lock()
					errs = append(errs, err.Error())
					mu.Unlock()
				}
				pushes.Add(1)
			}
		}()
	}
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				ev := types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorSystem,
					Actor: "wardyn/test", Action: "run.create", Outcome: "success", Data: json.RawMessage(`{}`)}
				if err := store.InsertAuditEvent(ctx, pool, &ev); err != nil {
					mu.Lock()
					errs = append(errs, "org: "+err.Error())
					mu.Unlock()
				}
				writes.Add(1)
				_ = st.TouchDevice(ctx, d.ID, time.Now().UTC())
			}
		}()
	}
	wg.Wait()
	// ErrConflict is the harness's own seq race (two pushers built successive
	// rows out of order), not a store failure; anything else is.
	t.Logf("%d pushes, %d org writes, %d errors other than the harness's seq race", pushes.Load(), writes.Load(), len(errs))
	for _, e := range errs {
		t.Errorf("unexpected error under concurrent load: %s", e)
	}
	if status, err := st.VerifyAuditChain(ctx); err != nil || !status.OK {
		t.Fatalf("verify after load: %+v %v", status, err)
	}
}
