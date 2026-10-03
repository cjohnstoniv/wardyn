// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// auditNarrowingParams are the query parameters that narrow an audit export (parseAuditFilter's and
// auditScope's). A partition export takes none of them: its footer digest covers the whole partition.
var auditNarrowingParams = []string{"run_id", "since", "until", "action", "action_prefix", "actor", "actor_type", "outcome", "origin"}

// Export forms of one partition. Both carry the same rows and the same manifest header and footer.
const (
	auditFormReadable = "readable"
	auditFormRaw      = "raw"
)

// A partition export is NDJSON: a manifest line first, one row line per row in seq order, a footer
// line last. Type says which.
type partitionManifestLine struct {
	Type string `json:"type"`
	Form string `json:"form"`
	store.PartitionManifest
}

type partitionFooterLine struct {
	Type      string `json:"type"`
	Partition string `json:"partition"`
	Rows      int64  `json:"row_count"`
	Digest    string `json:"digest"`
}

// partitionReadableLine is a row as the audit feed serves it (stored values as-is, ciphertext
// included), plus its position.
type partitionReadableLine struct {
	Type string `json:"type"`
	types.FederatedAuditEvent
	RecordedAt time.Time `json:"recorded_at"`
}

// partitionRawLine is a row as the table holds it: every column audit_row_hash covers, in the units it
// hashes them in (time as integer microseconds, data as the jsonb's own text), and the position and
// hashes. A hashless row (from before the chain began) carries no row_hash; fold_hash is then the hash
// the digest folds for it. With these an operator can recompute every row hash and the digest with no
// Wardyn code (docs/OPERATIONS.md, "Verifying an exported audit partition by hand").
type partitionRawLine struct {
	Type       string     `json:"type"`
	Seq        int64      `json:"seq"`
	RecordedUS int64      `json:"recorded_us"`
	ID         uuid.UUID  `json:"id"`
	TimeUS     int64      `json:"time_us"`
	RunID      *uuid.UUID `json:"run_id"`
	ActorType  string     `json:"actor_type"`
	Actor      string     `json:"actor"`
	Action     string     `json:"action"`
	Target     string     `json:"target"`
	Outcome    string     `json:"outcome"`
	SourceIP   string     `json:"source_ip"`
	Data       *string    `json:"data"`
	PrevHash   *string    `json:"prev_hash"`
	RowHash    *string    `json:"row_hash"`
	FoldHash   string     `json:"fold_hash,omitempty"`
}

// exportAuditPartition serves GET /audit/export?partition=<name>: one closed partition, in seq order,
// with the manifest first and the digest fold in a footer (store.PartitionDigest, the same fold
// audit_partition_digest runs in the database). ?form=raw gives the stored, hash-covered values;
// the default readable form gives the audit feed's own event shape.
//
// Only a security operator is served. Any other session gets the zero-line NDJSON export auditScope
// gives a member, and the partition is never read, so the response says nothing about which partitions
// exist. A partition export carries no other filter: the footer digest is always the whole partition.
// A failure after the first byte aborts the transfer, as the feed export does.
func (s *Server) exportAuditPartition(w http.ResponseWriter, r *http.Request) {
	if !s.isSecurityOperator(r.Context()) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		return
	}
	q := r.URL.Query()
	for p := range q {
		if slices.Contains(auditNarrowingParams, p) {
			s.refuse(w, r, authz.Deny(authz.ReasonAuditExportPartitionFilter, "audit.export",
				"a partition export covers the whole partition; remove the other filter parameters"))
			return
		}
	}
	form := q.Get("form")
	switch form {
	case "":
		form = auditFormReadable
	case auditFormReadable, auditFormRaw:
	default:
		writeErrorReason(w, http.StatusBadRequest, reasonAuditInvalidExportForm, "invalid form (want readable or raw)")
		return
	}
	exp, ok := s.cfg.Store.(store.AuditPartitionExporter)
	if !ok {
		writeErrorReason(w, http.StatusNotImplemented, reasonAuditExportStoreUnavailable, "partition export requires the Postgres store backend")
		return
	}

	wrote := false // a byte has reached the response: the 200 is committed
	var rowsSent int64
	var digest *store.PartitionDigest
	var manifest store.PartitionManifest
	send := func(v any) error {
		line, err := json.Marshal(v)
		if err != nil {
			return err
		}
		wrote = true
		_, err = w.Write(append(line, '\n'))
		return err
	}
	err := exp.ExportAuditPartition(r.Context(), q.Get("partition"),
		func(m store.PartitionManifest) error {
			manifest, digest = m, store.NewPartitionDigest(m)
			w.Header().Set("Content-Type", "application/x-ndjson")
			return send(partitionManifestLine{Type: "manifest", Form: form, PartitionManifest: m})
		},
		func(row store.AuditPartitionRow) error {
			digest.Add(row.FoldHash)
			rowsSent++
			var line any = readablePartitionLine(row)
			if form == auditFormRaw {
				line = rawPartitionLine(row)
			}
			if err := send(line); err != nil {
				return err
			}
			if fl, ok := w.(http.Flusher); ok && rowsSent%auditExportPageSize == 0 {
				fl.Flush()
			}
			return nil
		})
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErrorReason(w, http.StatusNotFound, reasonAuditPartitionNotFound, "no such audit partition")
	case errors.Is(err, store.ErrAuditPartitionOpen):
		writeErrorReason(w, http.StatusConflict, reasonAuditPartitionOpen, "the partition can still receive rows and has no digest yet")
	case err != nil:
		s.failAuditExport(w, r, "partition read", int(rowsSent), wrote, err)
	case rowsSent != manifest.Rows:
		s.failAuditExport(w, r, "partition read", int(rowsSent), wrote,
			errors.New("the partition's rows do not match its manifest"))
	default:
		if err := send(partitionFooterLine{Type: "footer", Partition: manifest.Partition, Rows: rowsSent, Digest: digest.Sum()}); err != nil {
			s.failAuditExport(w, r, "footer write", int(rowsSent), wrote, err)
		}
	}
}

func rawPartitionLine(row store.AuditPartitionRow) partitionRawLine {
	l := partitionRawLine{
		Type: "row", Seq: row.Seq, RecordedUS: row.RecordedUS, ID: row.ID, TimeUS: row.TimeUS, RunID: row.RunID,
		ActorType: row.ActorType, Actor: row.Actor, Action: row.Action, Target: row.Target, Outcome: row.Outcome,
		SourceIP: row.SourceIP, Data: row.Data, PrevHash: row.PrevHash, RowHash: row.RowHash,
	}
	if row.RowHash == nil {
		l.FoldHash = row.FoldHash
	}
	return l
}

func readablePartitionLine(row store.AuditPartitionRow) partitionReadableLine {
	ev := types.AuditEvent{
		ID: row.ID, Time: time.UnixMicro(row.TimeUS).UTC(), RunID: row.RunID, ActorType: types.ActorType(row.ActorType),
		Actor: row.Actor, Action: row.Action, Target: row.Target, Outcome: row.Outcome, SourceIP: row.SourceIP,
	}
	if row.Data != nil && *row.Data != "null" {
		ev.Data = json.RawMessage(*row.Data)
	}
	if row.PrevHash != nil {
		ev.PrevHash = *row.PrevHash
	}
	if row.RowHash != nil {
		ev.RowHash = *row.RowHash
	}
	ev.DeviceID = store.FederatedDeviceID(ev)
	return partitionReadableLine{Type: "row", FederatedAuditEvent: types.FederatedAuditEvent{AuditEvent: ev, Seq: row.Seq}, RecordedAt: time.UnixMicro(row.RecordedUS).UTC()}
}
