// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

// minPartitionsAhead is the number of months of partitions ahead below which /setup/status warns: an
// insert into a month with no partition fails, and the audit spool holds the row until one exists.
const minPartitionsAhead = 3

// mountAuditRetentionRoutes registers the retention endpoints. Called with securityOps: dropping audit
// history is the security tier's duty, like reading the chain verdict beside it (Q-AR1: one security_admin
// acting alone, with the 30-day cooldown on decreases as the brake).
func (s *Server) mountAuditRetentionRoutes(securityOps chi.Router) {
	securityOps.Get("/audit/retention", s.handleGetAuditRetention)
	securityOps.Post("/audit/retention/drop", s.handleDropAuditPartition)
}

// handleGetAuditRetention reports the effective and pending retention policy, the cutover, every
// partition with its row count, state and eligibility (the drop function's own answer, so the two cannot
// disagree), and how many months ahead already have a partition. Registered on securityOps.
func (s *Server) handleGetAuditRetention(w http.ResponseWriter, r *http.Request) {
	rs, ok := s.cfg.Store.(store.AuditRetention)
	if !ok {
		writeErrorReason(w, http.StatusNotImplemented, reasonAuditRetentionStoreUnavailable, "audit retention requires the Postgres store backend")
		return
	}
	st, err := rs.AuditRetentionStatus(r.Context())
	if err != nil {
		slog.ErrorContext(r.Context(), "wardyn: audit retention status failed", slog.Any("err", err))
		writeErrorReason(w, http.StatusInternalServerError, reasonAuditRetentionReadFailed, "the audit retention status could not be read")
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// dropAuditPartitionRequest is POST /audit/retention/drop's body: the partition, and the digest of the
// export the operator checked.
type dropAuditPartitionRequest struct {
	Partition string `json:"partition"`
	Digest    string `json:"digest"`
}

// handleDropAuditPartition drops the oldest closed partition past the retention window, through the
// database function that checks the digest, writes the chained audit.retention.partition_dropped event and
// the anchor, and only then drops (one transaction, so a crash leaves all of it or none). The operator
// exports first (GET /audit/export?partition=), checks the archive, then submits its digest. A refusal is
// 409 with its own reason and an authz.denied row, so an attempt that was refused is on the record. The
// drop's own record is the chained event; the actor is the caller.
func (s *Server) handleDropAuditPartition(w http.ResponseWriter, r *http.Request) {
	rs, ok := s.cfg.Store.(store.AuditRetention)
	if !ok {
		writeErrorReason(w, http.StatusNotImplemented, reasonAuditRetentionStoreUnavailable, "audit retention requires the Postgres store backend")
		return
	}
	var body dropAuditPartitionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody)).Decode(&body); err != nil ||
		strings.TrimSpace(body.Partition) == "" {
		writeErrorReason(w, http.StatusBadRequest, reasonAuditRetentionBodyInvalid, `the body is {"partition": "<name>", "digest": "<64 hex>"}`)
		return
	}
	d, err := rs.DropAuditPartition(r.Context(), body.Partition, strings.TrimSpace(body.Digest), principalFromRequest(r))
	var refused *store.AuditRetentionRefused
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErrorReason(w, http.StatusNotFound, reasonAuditPartitionNotFound, "no such audit partition")
	case errors.As(err, &refused):
		s.refuseRetentionDrop(w, r, refused)
	case err != nil:
		slog.ErrorContext(r.Context(), "wardyn: audit retention drop failed", slog.String("partition", body.Partition), slog.Any("err", err))
		writeErrorReason(w, http.StatusInternalServerError, reasonAuditRetentionDropFailed, "the partition was not dropped")
	default:
		writeJSON(w, http.StatusOK, d)
	}
}

// refuseRetentionDrop answers a drop the database refused: one registered reason each.
func (s *Server) refuseRetentionDrop(w http.ResponseWriter, r *http.Request, e *store.AuditRetentionRefused) {
	var d authz.Decision
	switch e.Reason {
	case store.RetentionNotOldest:
		d = authz.Deny(authz.ReasonAuditRetentionNotOldest, "audit.retention", "only the oldest audit partition can be dropped")
	case store.RetentionNotClosed:
		d = authz.Deny(authz.ReasonAuditRetentionNotClosed, "audit.retention", "the partition can still receive rows, so it cannot be dropped")
	case store.RetentionInsideWindow:
		d = authz.Deny(authz.ReasonAuditRetentionInsideWindow, "audit.retention", "the partition is inside the retention window (or retention is set to forever)")
	case store.RetentionLiveRun:
		d = authz.Deny(authz.ReasonAuditRetentionLiveRun, "audit.retention", "the partition holds audit rows of a run that is still live")
	default:
		d = authz.Deny(authz.ReasonAuditRetentionDigestMismatch, "audit.retention", "the digest does not match the partition's own; export it again and recompute it")
	}
	s.refuse(w, r, d.With("partition", e.Partition))
}

// auditPartitionChecks is /setup/status's one row about the partition runway: a warning once fewer than
// minPartitionsAhead months ahead have a partition, because every insert into a month with no partition
// fails and waits in the spool. Nothing while the runway is healthy, and nothing on a store that is not
// partitioned (a test fake, or a store with no retention).
func (s *Server) auditPartitionChecks(ctx context.Context) []SetupCheck {
	rs, ok := s.cfg.Store.(store.AuditRetention)
	if !ok {
		return nil
	}
	ahead, err := rs.AuditPartitionsAhead(ctx)
	if err != nil || ahead >= minPartitionsAhead {
		return nil
	}
	return []SetupCheck{{
		ID: "audit_partitions", Label: "Audit partitions", Status: "warn",
		Detail: fmt.Sprintf("Only %d month(s) of audit partitions exist ahead of today. An audit write into a month with no partition fails and waits in the spool until one exists.", ahead),
		Fix:    "Restart wardynd, or run SELECT audit_ensure_partitions(12) as the database owner; the leader sweeper does this daily, so a warning that stays means it is not running or cannot reach the database.",
	}}
}
