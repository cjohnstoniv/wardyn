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
		Data: json.RawMessage(`{"decision":"APPROVED","device_origin":{"device_id":"forged"}}`),
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
		if g.Actor == "ceo@corp.example" {
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

	const q = `
		SELECT e.seq, e.source_ip, e.data->'device_origin'->>'source_ip',
		       audit_row_hash(e.data->'device_origin'->>'prev_hash', e.id, e.time, e.run_id, e.actor_type, e.actor,
		                      e.action, e.target, e.outcome, e.data->'device_origin'->>'source_ip', e.data - 'device_origin')
		         = e.data->'device_origin'->>'row_hash',
		       e.row_hash = audit_row_hash(e.prev_hash, e.id, e.time, e.run_id, e.actor_type, e.actor,
		                                   e.action, e.target, e.outcome, e.source_ip, e.data),
		       e.prev_hash = (SELECT p.row_hash FROM audit_events p WHERE p.seq < e.seq ORDER BY p.seq DESC LIMIT 1)
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
