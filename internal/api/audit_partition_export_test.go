// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// partitionExportFake is an AuditPartitionExporter that plays a scripted partition and counts how often
// the store was asked for one: the spy behind "no partition read happens".
type partitionExportFake struct {
	pagerFake
	manifest  store.PartitionManifest
	rows      []store.AuditPartitionRow
	err       error // returned before the header
	failAfter int   // fail the stream after this many rows (0 = never)
	reads     atomic.Int32
}

func (f *partitionExportFake) ExportAuditPartition(_ context.Context, _ string, header func(store.PartitionManifest) error, row func(store.AuditPartitionRow) error) error {
	f.reads.Add(1)
	if f.err != nil {
		return f.err
	}
	if err := header(f.manifest); err != nil {
		return err
	}
	for i, r := range f.rows {
		if f.failAfter > 0 && i == f.failAfter {
			return errors.New("injected outage mid-partition")
		}
		if err := row(r); err != nil {
			return err
		}
	}
	return nil
}

func samplePartition() *partitionExportFake {
	strp := func(s string) *string { return &s }
	run := uuid.New()
	rows := []store.AuditPartitionRow{
		{Seq: 7, RecordedUS: 1759500000000001, TimeUS: 1759499999000000, ID: uuid.New(), ActorType: "human", Actor: "alice@corp.example", Action: "run.start",
			Outcome: "success", Data: strp(`{"a": 1}`), RowHash: strp(strings.Repeat("a", 64)), PrevHash: strp(strings.Repeat("9", 64)), FoldHash: strings.Repeat("a", 64)},
		{Seq: 8, RecordedUS: 1759500000000002, TimeUS: 1759500000000000, ID: uuid.New(), RunID: &run, ActorType: "system", Actor: "wardynd", Action: "run.complete",
			Outcome: "success", FoldHash: strings.Repeat("b", 64)}, // a hashless row: no row_hash, a fold hash
	}
	return &partitionExportFake{
		manifest: store.PartitionManifest{Partition: "audit_events_p202610", Rows: 2, SeqLo: 7, SeqHi: 8, RecordedLoUS: 1759500000000001, RecordedHiUS: 1759500000000002},
		rows:     rows,
	}
}

func partitionServer(t *testing.T, st store.Store) *Server {
	t.Helper()
	cfg := baseTestConfig(newHarness(t), st)
	cfg.OIDC = &oidc.Authenticator{}
	return New(cfg)
}

func ndjsonObjects(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var out []map[string]any
	dec := json.NewDecoder(bytes.NewReader(body))
	for dec.More() {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			t.Fatalf("decode ndjson line: %v (body %q)", err, body)
		}
		out = append(out, m)
	}
	return out
}

// A member (or anyone below the security tier) gets the zero-line export a member gets for a run that is
// not theirs, whatever else the request says, and the store is never asked for the partition: the answer
// cannot say whether the partition exists.
func TestAuditPartitionExport_NonOperatorGetsAnEmptyExportAndNothingIsRead(t *testing.T) {
	fake := samplePartition()
	srv := partitionServer(t, fake)
	member := ssoSession(t, "sub-member", "member@corp.example", oidc.RoleUser)
	for _, q := range []string{
		"?partition=audit_events_p202610",
		"?partition=audit_events_p202610&form=raw",
		"?partition=audit_events_p202610&form=bogus",
		"?partition=audit_events_p202610&run_id=" + uuid.NewString(),
		"?partition=audit_events_p202610&since=not-a-time",
		"?partition=does_not_exist",
		"?partition=",
	} {
		w := doSSO(t, srv, http.MethodGet, "/api/v1/audit/export"+q, member, "")
		if w.Code != http.StatusOK || w.Body.Len() != 0 || w.Header().Get("Content-Type") != "application/x-ndjson" {
			t.Errorf("member GET /audit/export%s: %d %q (%s), want the empty NDJSON 200", q, w.Code, w.Body.String(), w.Header().Get("Content-Type"))
		}
	}
	if n := fake.reads.Load(); n != 0 {
		t.Errorf("the store was asked for a partition %d time(s) on behalf of a member, want 0", n)
	}
}

// A security_admin (and the admin) receive the stream: manifest, rows in order, footer with the fold.
func TestAuditPartitionExport_SecurityTierReceivesTheStream(t *testing.T) {
	fake := samplePartition()
	srv := partitionServer(t, fake)
	wantDigest := store.NewPartitionDigest(fake.manifest)
	for _, r := range fake.rows {
		wantDigest.Add(r.FoldHash)
	}
	for who, cookie := range map[string]*http.Cookie{
		"security_admin": ssoSession(t, "sub-sec", "sec@corp.example", oidc.RoleSecurityAdmin),
		"admin":          ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin),
	} {
		for _, form := range []string{"", "&form=readable", "&form=raw"} {
			w := doSSO(t, srv, http.MethodGet, "/api/v1/audit/export?partition=audit_events_p202610"+form, cookie, "")
			if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/x-ndjson" {
				t.Fatalf("%s %q: %d %q", who, form, w.Code, w.Body.String())
			}
			lines := ndjsonObjects(t, w.Body.Bytes())
			if len(lines) != 4 {
				t.Fatalf("%s %q: %d lines, want manifest + 2 rows + footer", who, form, len(lines))
			}
			wantForm := "readable"
			if form == "&form=raw" {
				wantForm = "raw"
			}
			if lines[0]["type"] != "manifest" || lines[0]["form"] != wantForm || lines[0]["partition"] != "audit_events_p202610" || lines[0]["row_count"] != float64(2) {
				t.Errorf("%s %q: manifest line %v", who, form, lines[0])
			}
			if lines[1]["seq"] != float64(7) || lines[2]["seq"] != float64(8) || lines[1]["type"] != "row" {
				t.Errorf("%s %q: rows %v %v are not seq 7, 8 in order", who, form, lines[1], lines[2])
			}
			if lines[3]["type"] != "footer" || lines[3]["digest"] != wantDigest.Sum() || lines[3]["row_count"] != float64(2) {
				t.Errorf("%s %q: footer %v, want digest %s over 2 rows", who, form, lines[3], wantDigest.Sum())
			}
			if wantForm == "raw" {
				// Stored values, in the units the row hash is computed in.
				if lines[1]["time_us"] != float64(1759499999000000) || lines[1]["data"] != `{"a": 1}` || lines[1]["row_hash"] != strings.Repeat("a", 64) {
					t.Errorf("%s raw row 1 = %v", who, lines[1])
				}
				if lines[2]["row_hash"] != nil || lines[2]["fold_hash"] != strings.Repeat("b", 64) || lines[2]["data"] != nil {
					t.Errorf("%s raw hashless row = %v, want no row_hash, a fold_hash, no data", who, lines[2])
				}
			} else if lines[1]["actor"] != "alice@corp.example" || lines[1]["row_hash"] != strings.Repeat("a", 64) {
				t.Errorf("%s readable row 1 = %v", who, lines[1])
			}
		}
	}
}

// A partition export takes no other filter, so the footer digest always covers the whole partition.
func TestAuditPartitionExport_OtherFiltersAreRefused(t *testing.T) {
	fake := samplePartition()
	srv := partitionServer(t, fake)
	sec := ssoSession(t, "sub-sec", "sec@corp.example", oidc.RoleSecurityAdmin)
	for _, p := range auditNarrowingParams {
		w := doSSO(t, srv, http.MethodGet, "/api/v1/audit/export?partition=audit_events_p202610&"+p+"=x", sec, "")
		var eb struct{ Reason string }
		_ = json.Unmarshal(w.Body.Bytes(), &eb)
		if w.Code != http.StatusBadRequest || eb.Reason != "audit_export_partition_filter" {
			t.Errorf("?partition=&%s=: %d %s, want 400 audit_export_partition_filter", p, w.Code, w.Body.String())
		}
	}
	// The literal case from the spec: an empty partition value beside run_id.
	w := doSSO(t, srv, http.MethodGet, "/api/v1/audit/export?partition=&run_id="+uuid.NewString(), sec, "")
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "audit_export_partition_filter") {
		t.Errorf("?partition=&run_id=: %d %s, want the filter refusal", w.Code, w.Body.String())
	}
	if n := fake.reads.Load(); n != 0 {
		t.Errorf("a refused request read the partition %d time(s)", n)
	}
}

func TestAuditPartitionExport_Refusals(t *testing.T) {
	sec := ssoSession(t, "sub-sec", "sec@corp.example", oidc.RoleSecurityAdmin)
	for name, tc := range map[string]struct {
		fake       func() *partitionExportFake
		query      string
		wantCode   int
		wantReason string
	}{
		"unknown partition": {func() *partitionExportFake { f := samplePartition(); f.err = store.ErrNotFound; return f },
			"?partition=nope", http.StatusNotFound, "audit_partition_not_found"},
		"open partition": {func() *partitionExportFake { f := samplePartition(); f.err = store.ErrAuditPartitionOpen; return f },
			"?partition=audit_events_p202610", http.StatusConflict, "audit_partition_open"},
		"unreadable store": {func() *partitionExportFake { f := samplePartition(); f.err = errors.New("password=hunter2"); return f },
			"?partition=audit_events_p202610", http.StatusServiceUnavailable, "audit_export_read_failed"},
		"invalid form": {samplePartition, "?partition=audit_events_p202610&form=xml", http.StatusBadRequest, "audit_invalid_export_form"},
	} {
		t.Run(name, func(t *testing.T) {
			w := doSSO(t, partitionServer(t, tc.fake()), http.MethodGet, "/api/v1/audit/export"+tc.query, sec, "")
			var eb struct{ Reason string }
			_ = json.Unmarshal(w.Body.Bytes(), &eb)
			if w.Code != tc.wantCode || eb.Reason != tc.wantReason || strings.Contains(w.Body.String(), "hunter2") {
				t.Errorf("%d %s, want %d %s", w.Code, w.Body.String(), tc.wantCode, tc.wantReason)
			}
		})
	}
	t.Run("a store without partitions", func(t *testing.T) {
		w := doSSO(t, partitionServer(t, &pagerFake{}), http.MethodGet, "/api/v1/audit/export?partition=x", sec, "")
		if w.Code != http.StatusNotImplemented {
			t.Errorf("%d %s, want 501", w.Code, w.Body.String())
		}
	})
}

// An export that cannot be finished never looks finished: a failure after the first byte aborts the
// transfer, so no client reaches a clean end of stream without the footer.
func TestAuditPartitionExport_AFailureMidStreamAbortsTheTransfer(t *testing.T) {
	// A full page is flushed before the failure, so the 200 is committed (as in the feed export's own test).
	fake := samplePartition()
	one := fake.rows[0]
	fake.rows = make([]store.AuditPartitionRow, auditExportPageSize+2)
	for i := range fake.rows {
		fake.rows[i] = one
	}
	fake.manifest.Rows = int64(len(fake.rows))
	fake.failAfter = auditExportPageSize + 1
	h := newHarness(t)
	srv := New(baseTestConfig(h, fake))
	ts := httptest.NewServer(panicFails(t, srv.Handler()))
	t.Cleanup(ts.Close)
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/audit/export?partition=audit_events_p202610", nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !errors.Is(readErr, io.ErrUnexpectedEOF) {
		t.Fatalf("status %d, read error %v (after %d bytes), want a committed 200 and an aborted body", resp.StatusCode, readErr, len(body))
	}
	if strings.Contains(string(body), `"footer"`) {
		t.Error("an aborted export carried a footer")
	}
}

// TestPG_AuditPartitionExportMatchesTheDatabaseDigest runs the whole path against Postgres: rows written
// by the real writer into a month that is then closed, exported by the real handler as a security_admin
// in both forms, and folded from the raw archive's own hash columns with nothing but sha256. The footer,
// that fold and audit_partition_digest must be one value; a member sees nothing of it.
func TestPG_AuditPartitionExportMatchesTheDatabaseDigest(t *testing.T) {
	pool := throwawayPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	const rows = 25
	for i := 0; i < rows; i++ {
		ev := types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorHuman, Actor: "alice@corp.example",
			Action: "export.partition.probe", Outcome: "success", Data: json.RawMessage(`{"z": 1, "a": {"n": 2}}`)}
		if err := store.InsertAuditEvent(ctx, pool, &ev); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	part := func() string {
		var name string
		if err := pool.QueryRow(ctx, `SELECT tableoid::regclass::text FROM audit_events WHERE action = 'export.partition.probe' LIMIT 1`).Scan(&name); err != nil {
			t.Fatalf("find the live partition: %v", err)
		}
		return name
	}()
	srv := partitionServer(t, pg)
	sec := ssoSession(t, "sub-sec", "sec@corp.example", oidc.RoleSecurityAdmin)
	member := ssoSession(t, "sub-member", "member@corp.example", oidc.RoleUser)

	// Open: refused until the high-water mark passes the month.
	if w := doSSO(t, srv, http.MethodGet, "/api/v1/audit/export?partition="+part, sec, ""); w.Code != http.StatusConflict {
		t.Fatalf("export of the open partition %s: %d %s, want 409", part, w.Code, w.Body.String())
	}
	if _, err := pool.Exec(ctx, `UPDATE audit_partition_meta SET hw_recorded_at = '2200-01-01'`); err != nil {
		t.Fatalf("close the partition: %v", err)
	}
	var dbDigest string
	if err := pool.QueryRow(ctx, `SELECT audit_partition_digest($1)`, part).Scan(&dbDigest); err != nil {
		t.Fatalf("audit_partition_digest(%s): %v", part, err)
	}

	for _, form := range []string{"readable", "raw"} {
		w := doSSO(t, srv, http.MethodGet, "/api/v1/audit/export?form="+form+"&partition="+part, sec, "")
		if w.Code != http.StatusOK {
			t.Fatalf("%s export: %d %s", form, w.Code, w.Body.String())
		}
		lines := ndjsonObjects(t, w.Body.Bytes())
		if len(lines) != rows+2 {
			t.Fatalf("%s export has %d lines, want manifest + %d rows + footer", form, len(lines), rows)
		}
		if got := lines[len(lines)-1]["digest"]; got != dbDigest {
			t.Errorf("%s export footer digest = %v, audit_partition_digest = %s", form, got, dbDigest)
		}
		if form == "raw" {
			m := lines[0]
			hdr := func(v any) string { return fmt.Sprintf("%.0f", v.(float64)) }
			header := fmt.Sprintf(`["%s", "%s", "%s", "%s", "%s", "%s"]`, m["partition"], hdr(m["seq_lo"]), hdr(m["seq_hi"]),
				hdr(m["recorded_lo_us"]), hdr(m["recorded_hi_us"]), hdr(m["row_count"]))
			sum := sha256.Sum256([]byte(header))
			d := hex.EncodeToString(sum[:])
			for _, r := range lines[1 : len(lines)-1] {
				h, _ := r["row_hash"].(string)
				if h == "" {
					h, _ = r["fold_hash"].(string)
				}
				sum = sha256.Sum256([]byte(d + h))
				d = hex.EncodeToString(sum[:])
			}
			if d != dbDigest {
				t.Errorf("a fold of the raw archive with nothing but sha256 = %s, audit_partition_digest = %s", d, dbDigest)
			}
			if lines[1]["data"] != `{"a": {"n": 2}, "z": 1}` {
				t.Errorf("raw data = %q, want the jsonb's own text", lines[1]["data"])
			}
		}
	}
	if w := doSSO(t, srv, http.MethodGet, "/api/v1/audit/export?partition="+part, member, ""); w.Code != http.StatusOK || w.Body.Len() != 0 {
		t.Errorf("member export of an existing partition: %d %q, want the empty 200", w.Code, w.Body.String())
	}
	if w := doSSO(t, srv, http.MethodGet, "/api/v1/audit/export?partition=audit_events_p199001", sec, ""); w.Code != http.StatusNotFound {
		t.Errorf("security export of an unknown partition: %d, want 404", w.Code)
	}
}
