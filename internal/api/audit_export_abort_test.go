// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// exportFaultStore serves /audit/export from scripted pages. The call with
// index failAt (0-based) fails; every other call returns pages[i] (an empty
// page past the end).
type exportFaultStore struct {
	pagerFake
	pages  [][]types.AuditEvent
	failAt int
	calls  int
	lists  store.PushPathListStore
}

func (s *exportFaultStore) QueryAuditEventsFilteredPage(_ context.Context, _ *uuid.UUID, _ store.AuditFilter, _ store.Page) ([]types.AuditEvent, error) {
	i := s.calls
	s.calls++
	if i == s.failAt {
		return nil, errors.New("injected audit database outage: password=hunter2")
	}
	if i < len(s.pages) {
		return s.pages[i], nil
	}
	return nil, nil
}

// failingPathLists is a PushPathListStore whose every read fails.
type failingPathLists struct{ store.PushPathListStore }

func (failingPathLists) GetPushPathList(context.Context, uuid.UUID) (types.PushPathList, error) {
	return types.PushPathList{}, errors.New("injected path-list outage")
}

// exportStoreWithLists adds the PushPathListStore the export type-asserts.
type exportStoreWithLists struct {
	exportFaultStore
	failingPathLists
}

func fullPage() []types.AuditEvent { return makeEvents(auditExportPageSize, nil) }

// exportOverHTTP drives GET /api/v1/audit/export through the real routed
// handler on a real listener, as a client would see it.
func exportOverHTTP(t *testing.T, st store.Store) (status int, contentType string, body []byte, readErr error) {
	t.Helper()
	h := newHarness(t)
	srv := New(baseTestConfig(h, st))
	ts := httptest.NewServer(panicFails(t, srv.Handler()))
	t.Cleanup(ts.Close)
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/audit/export", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("export request: %v", err)
	}
	defer resp.Body.Close()
	body, readErr = io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Content-Type"), body, readErr
}

// TestAuthzReviewExportFailureLooksComplete is the review's reproduction,
// flipped. A store failure on page 0 used to answer 200 and an empty NDJSON
// body (an empty export that looks successful); one after a full page used to
// end cleanly with exactly 1000 rows (a truncated export that looks whole).
// Now page 0 is a 503 with a JSON body, and a later page aborts the transfer
// so the client's body read fails and cannot reach a clean end.
func TestAuthzReviewExportFailureLooksComplete(t *testing.T) {
	t.Run("first_page", func(t *testing.T) {
		st := &exportFaultStore{failAt: 0}
		status, ct, body, readErr := exportOverHTTP(t, st)
		if status != http.StatusServiceUnavailable || readErr != nil {
			t.Fatalf("status=%d readErr=%v body=%q, want a clean 503", status, readErr, body)
		}
		if strings.Contains(ct, "x-ndjson") || !strings.Contains(ct, "application/json") {
			t.Errorf("Content-Type = %q, want JSON, not NDJSON", ct)
		}
		var eb struct{ Error, Reason string }
		if err := json.Unmarshal(body, &eb); err != nil {
			t.Fatalf("body %q is not the JSON error envelope: %v", body, err)
		}
		if eb.Reason != reasonAuditExportReadFailed || eb.Error != "the audit store could not be read; nothing was exported" {
			t.Errorf("envelope = %+v", eb)
		}
		if strings.Contains(string(body), "hunter2") || strings.Contains(string(body), "outage") {
			t.Errorf("the store's error text reached the client: %q", body)
		}
	})
	// Page 1 fails after page 0's full 1000 rows were sent; an exact multiple
	// of the page size whose final (empty) read fails is the same case.
	t.Run("after_a_full_page", func(t *testing.T) {
		st := &exportFaultStore{pages: [][]types.AuditEvent{fullPage()}, failAt: 1}
		status, _, body, readErr := exportOverHTTP(t, st)
		if status != http.StatusOK {
			t.Fatalf("status = %d, want the 200 that was already committed", status)
		}
		if !errors.Is(readErr, io.ErrUnexpectedEOF) {
			t.Fatalf("body read error = %v (after %d bytes), want io.ErrUnexpectedEOF: the transfer must be aborted, not ended cleanly", readErr, len(body))
		}
	})
}

// A legitimately empty filter is still a clean, empty 200; a good export is
// unchanged.
func TestAuditExport_EmptyAndCompleteStayClean(t *testing.T) {
	status, ct, body, readErr := exportOverHTTP(t, &exportFaultStore{failAt: -1})
	if status != http.StatusOK || readErr != nil || len(body) != 0 || ct != "application/x-ndjson" {
		t.Fatalf("empty export: status=%d ct=%q body=%q err=%v", status, ct, body, readErr)
	}
	two := [][]types.AuditEvent{fullPage(), makeEvents(5, nil)}
	status, _, body, readErr = exportOverHTTP(t, &exportFaultStore{pages: two, failAt: -1})
	if status != http.StatusOK || readErr != nil || strings.Count(string(body), "\n") != auditExportPageSize+5 {
		t.Fatalf("complete export: status=%d err=%v lines=%d", status, readErr, strings.Count(string(body), "\n"))
	}
}

// A held push whose path list cannot be read is not exported as if it had no
// paths: before any byte, a 503; after, an aborted transfer.
func TestAuditExport_UnreadablePushPathListFailsTheExport(t *testing.T) {
	push := types.AuditEvent{ID: uuid.New(), Action: pushPathsAuditAction, Target: uuid.New().String(),
		ActorType: types.ActorAgent, Outcome: "success", Data: json.RawMessage(`{"paths_total":1}`)}
	t.Run("first_row", func(t *testing.T) {
		st := &exportStoreWithLists{exportFaultStore: exportFaultStore{pages: [][]types.AuditEvent{{push}}, failAt: -1}}
		status, _, body, readErr := exportOverHTTP(t, st)
		if status != http.StatusServiceUnavailable || readErr != nil {
			t.Fatalf("status=%d err=%v body=%q, want a 503", status, readErr, body)
		}
	})
	t.Run("after_rows", func(t *testing.T) {
		page := append(makeEvents(3, nil), push)
		st := &exportStoreWithLists{exportFaultStore: exportFaultStore{pages: [][]types.AuditEvent{fullPage(), page}, failAt: -1}}
		_, _, _, readErr := exportOverHTTP(t, st)
		if !errors.Is(readErr, io.ErrUnexpectedEOF) {
			t.Fatalf("body read error = %v, want io.ErrUnexpectedEOF", readErr)
		}
	})
}

// An event that cannot be encoded is a store/data fault, not a client
// hang-up: it aborts the export rather than ending it clean.
func TestAuditExport_UnencodableEventFailsTheExport(t *testing.T) {
	bad := types.AuditEvent{ID: uuid.New(), Action: "x", Data: json.RawMessage(`{not json`)}
	st := &exportFaultStore{pages: [][]types.AuditEvent{fullPage(), {bad}}, failAt: -1}
	_, _, _, readErr := exportOverHTTP(t, st)
	if !errors.Is(readErr, io.ErrUnexpectedEOF) {
		t.Fatalf("body read error = %v, want io.ErrUnexpectedEOF", readErr)
	}
}

// A non-pager store keeps its 501.
func TestAuditExport_NonPagerIs501(t *testing.T) {
	type notAPager struct{ store.Store }
	status, _, _, _ := exportOverHTTP(t, notAPager{})
	if status != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", status)
	}
}
