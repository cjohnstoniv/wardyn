// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// keysetExportStore is a Pager that also pages by position, serving a newest-first feed whose row i has
// seq len(rows)-i. Its OFFSET read fails, so an export that falls back to it shows.
type keysetExportStore struct {
	pagerFake
	rows   []types.AuditEvent
	afters []int64
}

func (s *keysetExportStore) QueryAuditEventsFilteredPage(context.Context, *uuid.UUID, store.AuditFilter, store.Page) ([]types.AuditEvent, error) {
	return nil, errors.New("the OFFSET read was used")
}

func (s *keysetExportStore) QueryAuditEventsFilteredAfter(_ context.Context, _ *uuid.UUID, _ store.AuditFilter, after int64, limit int) ([]types.AuditEvent, int64, error) {
	s.afters = append(s.afters, after)
	start := 0
	if after > 0 {
		start = len(s.rows) - int(after) + 1
	}
	end := min(start+limit, len(s.rows))
	page := s.rows[start:end]
	if len(page) == 0 {
		return page, after, nil
	}
	return page, int64(len(s.rows) - (end - 1)), nil
}

// The export walks the feed by the position of the last row it sent, so a row written while it runs cannot
// move a later page; every row comes once, in the feed's order.
func TestAuditExport_PagesByPositionWhenTheStoreCan(t *testing.T) {
	st := &keysetExportStore{rows: makeEvents(2*auditExportPageSize+37, nil)}
	status, _, body, readErr := exportOverHTTP(t, st)
	if status != http.StatusOK || readErr != nil {
		t.Fatalf("status %d, read error %v, want a clean 200", status, readErr)
	}
	got := ndjsonLines(t, body)
	if len(got) != len(st.rows) {
		t.Fatalf("exported %d events, want %d", len(got), len(st.rows))
	}
	for i := range got {
		if got[i].ID != st.rows[i].ID {
			t.Fatalf("line %d = %s, want %s: the export is not the feed in order", i, got[i].ID, st.rows[i].ID)
		}
	}
	// 2037 rows: the first page ends at seq 1038, the second at seq 38.
	if want := []int64{0, 1038, 38}; !slices.Equal(st.afters, want) {
		t.Errorf("page cursors = %v, want %v", st.afters, want)
	}
}

// countingUnsealer opens every row by prefixing its target, and records how many rows each call was given.
type countingUnsealer struct{ sizes []int }

func (c *countingUnsealer) Unseal(_ context.Context, evs []types.AuditEvent) ([]types.AuditEvent, error) {
	c.sizes = append(c.sizes, len(evs))
	out := slices.Clone(evs)
	for i := range out {
		out[i].Target = "opened:" + out[i].Target
	}
	return out, nil
}

// The readable partition export opens sealed fields a page at a time: one call per row rebuilt the key and
// subject lookups for every row while the export held its snapshot.
func TestAuditPartitionExport_UnsealsAPageAtATime(t *testing.T) {
	const rows = 2*auditExportPageSize + 5
	fake := samplePartition()
	fake.rows = make([]store.AuditPartitionRow, rows)
	for i := range fake.rows {
		fake.rows[i] = store.AuditPartitionRow{Seq: int64(i + 1), RecordedUS: 1759500000000000 + int64(i), TimeUS: 1759500000000000 + int64(i),
			ID: uuid.New(), ActorType: "system", Actor: "wardynd", Action: "run.complete", Target: strconv.Itoa(i), Outcome: "success",
			FoldHash: strings.Repeat("c", 64)}
	}
	fake.manifest.Rows, fake.manifest.SeqLo, fake.manifest.SeqHi = rows, 1, rows
	wantDigest := store.NewPartitionDigest(fake.manifest)
	for _, r := range fake.rows {
		wantDigest.Add(r.FoldHash)
	}

	unsealer := &countingUnsealer{}
	cfg := baseTestConfig(newHarness(t), fake)
	cfg.OIDC = &oidc.Authenticator{}
	cfg.AuditUnsealer = unsealer
	srv := New(cfg)
	sec := ssoSession(t, "sub-sec", "sec@corp.example", oidc.RoleSecurityAdmin)

	w := doSSO(t, srv, http.MethodGet, "/api/v1/audit/export?partition=audit_events_p202610", sec, "")
	if w.Code != http.StatusOK {
		t.Fatalf("export: %d %q", w.Code, w.Body.String())
	}
	lines := ndjsonObjects(t, w.Body.Bytes())
	if len(lines) != rows+2 {
		t.Fatalf("%d lines, want manifest + %d rows + footer", len(lines), rows)
	}
	for i := range rows {
		if got := lines[i+1]["target"]; got != "opened:"+strconv.Itoa(i) || lines[i+1]["seq"] != float64(i+1) {
			t.Fatalf("row %d = %v, want seq %d opened in order", i, lines[i+1], i+1)
		}
	}
	if footer := lines[rows+1]; footer["type"] != "footer" || footer["digest"] != wantDigest.Sum() {
		t.Errorf("footer = %v, want the digest %s", footer, wantDigest.Sum())
	}
	if want := []int{auditExportPageSize, auditExportPageSize, 5}; !slices.Equal(unsealer.sizes, want) {
		t.Errorf("unseal calls were given %v rows, want %v", unsealer.sizes, want)
	}
}
