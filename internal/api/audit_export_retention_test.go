// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// retentionExportStore is the keyset feed with a retention mark that moves when a drop commits. dropAtRead
// is the page read after which the drop lands (1 is the first), 0 never.
type retentionExportStore struct {
	keysetExportStore
	mark       int64
	reads      int
	dropAtRead int
	markErr    error
}

func (s *retentionExportStore) QueryAuditEventsFilteredAfter(ctx context.Context, runID *uuid.UUID, f store.AuditFilter, after int64, limit int) ([]types.AuditEvent, int64, error) {
	page, next, err := s.keysetExportStore.QueryAuditEventsFilteredAfter(ctx, runID, f, after, limit)
	s.reads++
	if s.reads == s.dropAtRead {
		s.mark++ // the drop commits right after this page was read
	}
	return page, next, err
}

func (s *retentionExportStore) AuditRetentionMark(context.Context) (int64, error) {
	return s.mark, s.markErr
}

// A retention drop that commits between two pages ends the export as incomplete: the client's read fails and the
// stream cannot look finished, even though the page after the drop would have been served short.
func TestAuditExport_RetentionDropBetweenPagesAbortsTheExport(t *testing.T) {
	st := &retentionExportStore{keysetExportStore: keysetExportStore{rows: makeEvents(2*auditExportPageSize+37, nil)}, dropAtRead: 2}
	status, _, _, readErr := exportOverHTTP(t, st)
	if status != http.StatusOK || !errors.Is(readErr, io.ErrUnexpectedEOF) {
		t.Fatalf("status %d, read error %v, want a 200 whose body read ends unexpectedly", status, readErr)
	}
}

// A drop committed after the very first read, before any byte, is a 503 and nothing is exported.
func TestAuditExport_RetentionDropBeforeTheFirstByteIs503(t *testing.T) {
	st := &retentionExportStore{keysetExportStore: keysetExportStore{rows: makeEvents(10, nil)}, dropAtRead: 1}
	status, _, body, readErr := exportOverHTTP(t, st)
	if status != http.StatusServiceUnavailable || readErr != nil || len(body) == 0 {
		t.Fatalf("status %d, read error %v, want a 503 with its JSON body", status, readErr)
	}
}

// No drop: the export is whole and clean, and a mark that cannot be read is not waved through.
func TestAuditExport_StableRetentionMarkLeavesTheExportClean(t *testing.T) {
	st := &retentionExportStore{keysetExportStore: keysetExportStore{rows: makeEvents(2*auditExportPageSize+37, nil)}}
	status, _, body, readErr := exportOverHTTP(t, st)
	if status != http.StatusOK || readErr != nil || len(ndjsonLines(t, body)) != len(st.rows) {
		t.Fatalf("status %d, read error %v, lines %d, want a clean full export", status, readErr, len(ndjsonLines(t, body)))
	}
	st = &retentionExportStore{keysetExportStore: keysetExportStore{rows: makeEvents(10, nil)}, markErr: errors.New("anchors unreadable")}
	if status, _, _, _ := exportOverHTTP(t, st); status != http.StatusServiceUnavailable {
		t.Fatalf("an unreadable retention mark gave status %d, want 503", status)
	}
}
