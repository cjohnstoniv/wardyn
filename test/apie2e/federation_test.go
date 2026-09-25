// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package apie2e

// ONE AUDIT STREAM, END TO END: a laptop's own chained audit rows land on the
// organisation's table with their chain evidence intact, a row edited after the
// laptop chained it is refused, and a purge of the laptop's table is visible at
// the organisation rather than silent (docs/design/0.8/PLAN.md, "Hybrid
// enrolment and audit federation").
//
// Both ends are real: the organisation is the real server over httptest on its
// own throwaway database, and the laptop is a second throwaway database with the
// real federation.Forwarder pointed at the organisation's URL. Nothing between
// them is faked — the rows travel as JSON over HTTP, through deviceAuth and
// handleDeviceAuditIngest into store.PG.IngestDeviceAudit.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/testfloor"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// fedRows crosses both the forwarder's 500-row batch and the verify sweep's
// 1,000-row page (store.AuditChainPageSize), so neither boundary goes untested.
const fedRows = 1200

// fedSkipMarker is internal/store's storeProbeSkipMarker. The rule below is a
// copy of that package's storeProbeMustNotSkip/storeSkipOrFatal, because Go test
// helpers cannot cross a package boundary; the words and the derivation are kept
// identical so the lanes cannot drift into different ideas of a hidden failure.
const fedSkipMarker = "WARDYN_TEST_PG_SUPERUSER"

// errProbeOnly rolls back the trigger-bypass probe once it has succeeded.
var errProbeOnly = errors.New("probe only")

// fedMustNotSkip reports whether a skip from here on would be HIDING a failure
// rather than reporting an unmet precondition.
func fedMustNotSkip(pool *pgxpool.Pool) bool {
	if os.Getenv(fedSkipMarker) == "1" {
		return true
	}
	u, err := url.Parse(os.Getenv("WARDYN_TEST_PG"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return false
	}
	var super bool
	if err := pool.QueryRow(context.Background(),
		`SELECT rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&super); err != nil {
		return false
	}
	return super
}

// throwawayDSN creates a fresh database on the WARDYN_TEST_PG server, dropped on
// cleanup, and returns its DSN. Neither end runs on the shared database: the
// verify sweep walks the WHOLE table, so a row another test left behind would
// decide this test's verdict.
func throwawayDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("WARDYN_TEST_PG")
	if dsn == "" {
		t.Skip("WARDYN_TEST_PG not set; skipping apie2e federation test")
	}
	ctx := context.Background()
	admin, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	name := "wardyn_fed_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		admin.Close()
		t.Fatalf("create throwaway database %s: %v", name, err)
	}
	// Registered before any pool on it, so LIFO cleanup closes those pools first
	// (a database with a connected client cannot be dropped).
	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = admin.Exec(cctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=$1 AND pid <> pg_backend_pid()`, name)
		_, _ = admin.Exec(cctx, `DROP DATABASE IF EXISTS `+name)
		admin.Close()
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse WARDYN_TEST_PG: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}

// asSuperuser runs fn in one transaction with every ordinary trigger on the
// laptop's tables off — the DB-admin bypass the chain exists to make visible.
func asSuperuser(t *testing.T, pool *pgxpool.Pool, fn func(tx pgx.Tx) error) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // best-effort on the failure path
	if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
		t.Fatalf("superuser step: %v", err)
	}
	if err := fn(tx); err != nil {
		t.Fatalf("superuser step: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("superuser step: commit: %v", err)
	}
}

// writeLaptopRows appends n rows through the laptop's real audit writer, so its
// own trigger chains them; returns the laptop's head seq afterwards.
func writeLaptopRows(t *testing.T, pool *pgxpool.Pool, n int, tag string) int64 {
	t.Helper()
	ctx := context.Background()
	rec := store.Recorder{Pool: pool}
	for i := range n {
		if err := rec.Record(ctx, types.AuditEvent{
			ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorHuman, Actor: "dev@laptop.example",
			Action: "run.create", Target: fmt.Sprintf("%s-%d", tag, i), Outcome: "success", SourceIP: "10.0.0.7",
			Data: json.RawMessage(fmt.Sprintf(`{"i":%d,"tag":%q}`, i, tag)),
		}); err != nil {
			t.Fatalf("laptop audit write %d: %v", i, err)
		}
	}
	head, err := store.NewPG(pool).AuditHeadSeq(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return head
}

// forward runs a FRESH forwarder — a wardynd (re)start: it loads the durable
// cursor, and a halt from an earlier refusal does not carry over — until done
// holds for its status, then stops it.
func forward(t *testing.T, orgURL string, laptop *pgxpool.Pool, cred federation.Credential,
	done func(federation.Status) bool) federation.Status {
	t.Helper()
	fwd := federation.NewForwarder(federation.NewClient(orgURL), store.NewPG(laptop), cred, store.Recorder{Pool: laptop})
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { fwd.Run(ctx); close(stopped) }()
	ok := waitFor(t, 60*time.Second, func() bool { return done(fwd.Status()) })
	cancel()
	<-stopped
	st := fwd.Status()
	if !ok {
		t.Fatalf("forwarder never reached the expected state: %+v", st)
	}
	return st
}

// originRow is one federated row as the organisation stored it.
type originRow struct {
	seq      int64
	rowHash  string
	prevHash string
}

// orgOrigins returns the device's rows on the organisation's table, in the
// organisation's own seq order, failing if any row's stored claim no longer
// recomputes to the hash the laptop chained it to.
func orgOrigins(t *testing.T, org *pgxpool.Pool, deviceID uuid.UUID) []originRow {
	t.Helper()
	rows, err := org.Query(context.Background(), `
		SELECT (e.data->'device_origin'->>'seq')::bigint, e.data->'device_origin'->>'row_hash',
		       e.data->'device_origin'->>'prev_hash', `+store.FederatedClaimHashSQL+`
		FROM audit_events e
		WHERE e.data->'device_origin'->>'device_id' = $1
		ORDER BY e.seq`, deviceID.String())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []originRow
	for rows.Next() {
		var r originRow
		var recomputed string
		if err := rows.Scan(&r.seq, &r.rowHash, &r.prevHash, &recomputed); err != nil {
			t.Fatal(err)
		}
		if recomputed != r.rowHash {
			t.Fatalf("org row for laptop seq %d: its stored claim recomputes to %s, the laptop chained %s", r.seq, recomputed, r.rowHash)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// laptopChain returns the laptop's own (seq, row_hash, prev_hash), oldest first.
func laptopChain(t *testing.T, laptop *pgxpool.Pool) []originRow {
	t.Helper()
	rows, err := laptop.Query(context.Background(),
		`SELECT seq, row_hash, COALESCE(prev_hash,'') FROM audit_events ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (originRow, error) {
		var o originRow
		return o, r.Scan(&o.seq, &o.rowHash, &o.prevHash)
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func requireChainOK(t *testing.T, which string, pool *pgxpool.Pool, wantChecked int64) {
	t.Helper()
	st, err := store.NewPG(pool).VerifyAuditChain(context.Background())
	if err != nil {
		t.Fatalf("%s: verify chain: %v", which, err)
	}
	if !st.OK || st.Checked < wantChecked {
		t.Fatalf("%s: chain verification = %+v, want OK over at least %d rows", which, st, wantChecked)
	}
}

// orgAuditCount counts the organisation's rows for one action on this device,
// optionally narrowed to a data->>'reason'.
func orgAuditCount(t *testing.T, org *pgxpool.Pool, action string, deviceID uuid.UUID, outcome, reason string) int {
	t.Helper()
	var n int
	if err := org.QueryRow(context.Background(), `
		SELECT count(*) FROM audit_events
		WHERE action = $1 AND target = $2 AND outcome = $3 AND ($4 = '' OR data->>'reason' = $4)`,
		action, deviceID.String(), outcome, reason).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func orgDevice(t *testing.T, h *harness, id uuid.UUID) types.Device {
	t.Helper()
	devs, err := h.sdk.ListDevices(context.Background())
	if err != nil {
		t.Fatalf("list devices: %v", err)
	}
	for _, d := range devs {
		if d.ID == id {
			return d
		}
	}
	t.Fatalf("device %s is not in the organisation's inventory", id)
	return types.Device{}
}

func laptopCursor(t *testing.T, laptop *pgxpool.Pool) int64 {
	t.Helper()
	c, err := store.NewPG(laptop).GetFederationCursor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestFederation_OneAuditStream(t *testing.T) {
	// the one-audit-stream proof (#104), falsifiable only with a database it can
	// create and a role that can edit and purge the laptop's table.
	testfloor.Mark(t, "pg")
	ctx := context.Background()
	orgDSN, laptopDSN := throwawayDSN(t), throwawayDSN(t)

	laptop, err := db.Connect(ctx, laptopDSN)
	if err != nil {
		t.Fatalf("connect laptop: %v", err)
	}
	t.Cleanup(laptop.Close)
	if err := db.Migrate(ctx, laptop); err != nil {
		t.Fatalf("migrate laptop: %v", err)
	}
	// The edit and the purge are a superuser's; probe that up front, at the top
	// level, so a lane that cannot do them is a visible skip of THIS test (the
	// pg skip floor reads top-level outcomes) — or a failure where it could.
	if err := pgx.BeginFunc(ctx, laptop, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`)
		return cmp.Or(err, errProbeOnly)
	}); !errors.Is(err, errProbeOnly) {
		if fedMustNotSkip(laptop) {
			t.Fatalf("this lane can satisfy this precondition, so a skip here would hide a failure ("+
				fedSkipMarker+"=1, or a trigger-bypass-capable role over a URL-form DSN): this role cannot disable triggers: %v", err)
		}
		t.Skipf("this role cannot disable triggers (needs superuser): %v", err)
	}

	// newHarness boots on WARDYN_TEST_PG; point it at the organisation's own database.
	t.Setenv("WARDYN_TEST_PG", orgDSN)
	h := newHarness(t, harnessOpts{})
	org := h.pool

	var cred federation.Credential
	var head int64

	if !t.Run("enrol through the SDK and forward 1200 rows", func(t *testing.T) {
		minted, err := h.sdk.MintDeviceEnrolmentToken(ctx, "laptop-104")
		if err != nil {
			t.Fatalf("mint enrolment token: %v", err)
		}
		enrolled, err := federation.NewClient(h.srv.URL).Enrol(ctx, minted.Token)
		if err != nil {
			t.Fatalf("enrol: %v", err)
		}
		cred = federation.Credential{DeviceID: enrolled.DeviceID, Token: enrolled.Token,
			EnrolmentTokenSHA256: federation.TokenSHA256(minted.Token)}

		head = writeLaptopRows(t, laptop, fedRows, "first")
		st := forward(t, h.srv.URL, laptop, cred, func(s federation.Status) bool {
			return s.HeadSeq == head && s.AckedSeq == head && s.LastError == ""
		})
		if st.Lag() != 0 {
			t.Fatalf("forwarder idle with lag %d", st.Lag())
		}

		want, got := laptopChain(t, laptop), orgOrigins(t, org, cred.DeviceID)
		if len(want) != fedRows || len(got) != fedRows {
			t.Fatalf("laptop holds %d rows, organisation holds %d for the device; want %d at both ends", len(want), len(got), fedRows)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("row %d: organisation holds %+v, laptop chained %+v (origin seqs out of order or evidence lost)", i, got[i], want[i])
			}
		}
		if d := orgDevice(t, h, cred.DeviceID); d.LastSeq != head || d.LastRowHash != want[len(want)-1].rowHash {
			t.Fatalf("organisation's recorded cursor = (%d, %s), want (%d, %s)", d.LastSeq, d.LastRowHash, head, want[len(want)-1].rowHash)
		}
		requireChainOK(t, "laptop", laptop, fedRows)
		requireChainOK(t, "organisation", org, fedRows)
	}) {
		return
	}

	if !t.Run("a second pass from a rewound cursor adds nothing", func(t *testing.T) {
		if err := store.NewPG(laptop).SetFederationCursor(ctx, 0); err != nil {
			t.Fatal(err)
		}
		forward(t, h.srv.URL, laptop, cred, func(s federation.Status) bool {
			return s.AckedSeq == head && s.LastError == ""
		})
		if n := len(orgOrigins(t, org, cred.DeviceID)); n != fedRows {
			t.Fatalf("organisation holds %d rows for the device after a full re-send, want %d", n, fedRows)
		}
		if n := orgAuditCount(t, org, "device.chain.reset", cred.DeviceID, "success", ""); n != 0 {
			t.Fatalf("a re-send recorded %d chain resets", n)
		}
		requireChainOK(t, "organisation", org, fedRows)
	}) {
		return
	}

	if !t.Run("an edited laptop row is refused and the cursor holds", func(t *testing.T) {
		writeLaptopRows(t, laptop, 10, "tail")
		// Mid-batch, and by position: seq has gaps (0056 allocates it in the trigger).
		edited := laptopChain(t, laptop)[fedRows+5].seq
		asSuperuser(t, laptop, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `UPDATE audit_events SET data = '{"i":5,"tag":"rewritten"}' WHERE seq = $1`, edited)
			if err == nil && tag.RowsAffected() != 1 {
				err = errors.New("edited no row")
			}
			return err
		})
		if st, err := store.NewPG(laptop).VerifyAuditChain(ctx); err != nil || st.OK || st.BrokenSeq != edited {
			t.Fatalf("precondition: the laptop's own sweep should break at seq %d: %+v %v", edited, st, err)
		}

		st := forward(t, h.srv.URL, laptop, cred, func(s federation.Status) bool {
			return s.LastError != "" || s.AckedSeq > head // refused, or (wrongly) accepted
		})
		if !strings.Contains(st.LastError, "422") || st.AckedSeq != head {
			t.Fatalf("forwarder after the edit: %+v, want a 422 refusal with the cursor held at %d", st, head)
		}
		if !waitFor(t, 10*time.Second, func() bool {
			return orgAuditCount(t, org, "device.audit.ingest", cred.DeviceID, "failure", "chain_mismatch") > 0
		}) {
			t.Fatal("no device.audit.ingest chain_mismatch failure row on the organisation")
		}
		if c := laptopCursor(t, laptop); c != head {
			t.Fatalf("laptop cursor moved to %d, want %d", c, head)
		}
		if d := orgDevice(t, h, cred.DeviceID); d.LastSeq != head {
			t.Fatalf("organisation's recorded cursor moved to %d, want %d", d.LastSeq, head)
		}
		if n := len(orgOrigins(t, org, cred.DeviceID)); n != fedRows {
			t.Fatalf("organisation holds %d rows for the device, want %d: part of the refused batch landed", n, fedRows)
		}
	}) {
		return
	}

	// purge is a superuser's reset of the laptop's audit table followed by five
	// new rows: the organisation must record it as a chain reset and hold every
	// new row, and both cursors must name the new head. reusesSeqs is the case
	// where the reset restarts seq, so the new rows reuse seqs the organisation
	// already recorded and matching on seq alone would drop them as re-sends.
	purge := func(t *testing.T, stmt string, reusesSeqs bool, resets, heldBefore int) {
		t.Helper()
		cursorBefore := laptopCursor(t, laptop)
		asSuperuser(t, laptop, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, stmt)
			return err
		})
		newHead := writeLaptopRows(t, laptop, 5, "after-purge")
		want := laptopChain(t, laptop)
		if len(want) != 5 || want[0].prevHash != "" || (newHead < cursorBefore) != reusesSeqs {
			t.Fatalf("precondition: 5 rows from a genesis, head %d vs cursor %d reusing seqs=%v: %+v",
				newHead, cursorBefore, reusesSeqs, want)
		}
		forward(t, h.srv.URL, laptop, cred, func(s federation.Status) bool {
			return s.AckedSeq == newHead && s.LastError == ""
		})

		if n := orgAuditCount(t, org, "device.chain.reset", cred.DeviceID, "success", ""); n != resets {
			t.Fatalf("organisation recorded %d chain resets, want %d", n, resets)
		}
		got := orgOrigins(t, org, cred.DeviceID)
		if len(got) != heldBefore+5 {
			t.Fatalf("organisation holds %d rows for the device, want %d", len(got), heldBefore+5)
		}
		for i, w := range want {
			if got[heldBefore+i] != w {
				t.Fatalf("post-purge row %d: organisation holds %+v, laptop chained %+v", i, got[heldBefore+i], w)
			}
		}
		if c := laptopCursor(t, laptop); c != newHead {
			t.Fatalf("laptop cursor = %d, want %d", c, newHead)
		}
		if d := orgDevice(t, h, cred.DeviceID); d.LastSeq != newHead || d.LastRowHash != want[4].rowHash {
			t.Fatalf("organisation's recorded cursor = (%d, %s), want (%d, %s)", d.LastSeq, d.LastRowHash, newHead, want[4].rowHash)
		}
		requireChainOK(t, "laptop", laptop, 5)
		requireChainOK(t, "organisation", org, int64(heldBefore+5))
	}

	if !t.Run("a truncated laptop table is a visible chain reset and ingest resumes", func(t *testing.T) {
		purge(t, `TRUNCATE audit_events`, false, 1, fedRows)
	}) {
		return
	}
	t.Run("a truncate that restarts the laptop's seq is a chain reset, never rows dropped as re-sends", func(t *testing.T) {
		purge(t, `TRUNCATE audit_events RESTART IDENTITY`, true, 2, fedRows+5)
	})
}
