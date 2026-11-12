// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auditreceipt"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

// auditReceiptRetryAfter is the Retry-After, in whole seconds, on a receipt for an event the signer has not
// covered yet: the signer's interval (Config.AuditCheckpointInterval, in the embedded AuditReceiptConfig),
// after which the next checkpoint is due; 60 when the config carries none.
func (s *Server) auditReceiptRetryAfter() string {
	secs := int(s.cfg.AuditCheckpointInterval / time.Second)
	if secs < 1 {
		secs = 60
	}
	return strconv.Itoa(secs)
}

// mountAuditChainRoutes registers the audit chain's security-tier reads on securityOps: the tamper-evidence
// sweep, and the signed checkpoint, receipt and key routes. All four stay on one tier: the receipts show other
// rows' hashes and the sweep counts every row in the deployment.
func (s *Server) mountAuditChainRoutes(securityOps chi.Router) {
	securityOps.Get("/audit/chain/verify", s.handleVerifyAuditChain)
	securityOps.Get("/audit/receipt", s.handleAuditReceipt)
	securityOps.Get("/audit/checkpoints", s.handleAuditCheckpoints)
	securityOps.Get("/audit/receipt-keys", s.handleAuditReceiptKeys)
}

// auditBundleResponse is a receipt or checkpoint export page: the bundle `wardyn audit verify` reads, and
// where the next page starts when this one ended before the data did.
type auditBundleResponse struct {
	auditreceipt.Bundle
	NextAfterSeq int64 `json:"next_after_seq,omitempty"`
}

// auditReceiptReady answers the routes' shared preconditions: signing on (503 otherwise) and a store that
// keeps checkpoints (501 otherwise).
func (s *Server) auditReceiptReady(w http.ResponseWriter) (store.AuditCheckpointer, bool) {
	if s.cfg.AuditReceiptKeys == nil || len(s.cfg.AuditReceiptKeys()) == 0 {
		writeErrorReason(w, http.StatusServiceUnavailable, reasonAuditReceiptSigningOff,
			"audit checkpoint signing is off or its key could not be loaded; there is nothing to receipt")
		return nil, false
	}
	ck, ok := s.cfg.Store.(store.AuditCheckpointer)
	if !ok {
		writeErrorReason(w, http.StatusNotImplemented, reasonAuditReceiptStoreUnavailable, "audit receipts require the Postgres store backend")
		return nil, false
	}
	return ck, true
}

// receiptRequest is a parsed GET /audit/receipt.
type receiptRequest struct {
	ids      []uuid.UUID
	runID    *uuid.UUID
	afterSeq int64
	limit    int
}

// parseReceiptRequest reads ?event=<uuid>... or ?run_id=<uuid>[&after_seq=][&limit=], writing the refusal.
func parseReceiptRequest(w http.ResponseWriter, q url.Values) (receiptRequest, bool) {
	bad := func(msg string) (receiptRequest, bool) {
		writeErrorReason(w, http.StatusBadRequest, reasonAuditReceiptRequestInvalid, msg)
		return receiptRequest{}, false
	}
	var req receiptRequest
	events, run := q["event"], q.Get("run_id")
	if (len(events) > 0) == (run != "") {
		return bad("name audit events with event=<id> (repeatable) or one run with run_id=<id>, not both and not neither")
	}
	if len(events) > auditreceipt.MaxReceiptEvents {
		writeErrorReason(w, http.StatusRequestEntityTooLarge, reasonAuditReceiptTooLarge,
			"a receipt covers at most "+strconv.Itoa(auditreceipt.MaxReceiptEvents)+" events; ask for fewer, or use run_id and page with after_seq")
		return receiptRequest{}, false
	}
	for _, e := range events {
		id, err := uuid.Parse(e)
		if err != nil {
			return bad("event is not a UUID")
		}
		if !slices.Contains(req.ids, id) {
			req.ids = append(req.ids, id)
		}
	}
	if run != "" {
		id, err := uuid.Parse(run)
		if err != nil {
			return bad("run_id is not a UUID")
		}
		req.runID = &id
	}
	paging := q.Has("after_seq") || q.Has("limit")
	if paging && req.runID == nil {
		return bad("after_seq and limit page a run's receipt: use them with run_id")
	}
	req.limit = auditreceipt.MaxReceiptEvents
	if v := q.Get("after_seq"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return bad("after_seq is not a non-negative integer")
		}
		req.afterSeq = n
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > auditreceipt.MaxReceiptEvents {
			return bad("limit is not an integer from 1 to " + strconv.Itoa(auditreceipt.MaxReceiptEvents))
		}
		req.limit = n
	}
	return req, true
}

// redactedFieldsOf is the fields a projected row's data marker says were withheld.
func redactedFieldsOf(data *string) []string {
	if data == nil {
		return nil
	}
	var m struct {
		Fields []string `json:"redacted_fields"`
	}
	_ = json.Unmarshal([]byte(*data), &m)
	return m.Fields
}

// receiptEvent is a stored row as the reader is served it: the hash columns as stored, the data as
// partitionProjector serves it (hostViewOf below a super admin, marked with redacted_fields), sealed fields
// still ciphertext because that is what was hashed.
func receiptEvent(row store.AuditPartitionRow, project func(string, []byte) ([]byte, bool), redacted *int64) auditreceipt.Event {
	before := *redacted
	l := projectedRawLine(row, project)
	ev := auditreceipt.Event{Row: auditreceipt.Row{
		Seq: l.Seq, ID: l.ID, TimeUS: l.TimeUS, RunID: l.RunID, ActorType: l.ActorType, Actor: l.Actor, Action: l.Action,
		Target: &l.Target, Outcome: l.Outcome, SourceIP: &l.SourceIP, Data: l.Data, PrevHash: l.PrevHash, RowHash: l.RowHash,
	}}
	if *redacted > before {
		ev.RedactedFields = redactedFieldsOf(l.Data)
	}
	return ev
}

func signedCheckpoints(cps []store.AuditCheckpoint) []auditreceipt.SignedCheckpoint {
	out := make([]auditreceipt.SignedCheckpoint, len(cps))
	for i, c := range cps {
		out[i] = auditreceipt.SignedCheckpoint{SeqLo: c.SeqLo, SeqHi: c.SeqHi, Statement: c.Statement, Signature: c.Signature}
	}
	return out
}

// handleAuditReceipt serves GET /audit/receipt?event=<id>[&event=<id>...] or ?run_id=<id>[&after_seq=N][&limit=N]:
// the rows, the hash list of each checkpoint holding one, and the signed statement of every checkpoint from
// the first to the last, so `wardyn audit verify` can show offline that each row is in the log unaltered.
// Security tier only (routes.go), the tier of the partition export it is projected like: a security admin gets
// each row without its host detail, marked redacted_fields, and verify reports it as withheld.
//
// Caps: 1,000 events, 10 checkpoints with hash lists and 500 header-only checkpoints between them. Events past
// them are 413 audit_receipt_too_large; a run's receipt is cut there instead and next_after_seq names where to
// continue. An event newer than the last checkpoint is 409 audit_receipt_pending with Retry-After; one in no
// checkpoint at all is 409 audit_receipt_unchained.
func (s *Server) handleAuditReceipt(w http.ResponseWriter, r *http.Request) {
	ck, ok := s.auditReceiptReady(w)
	if !ok {
		return
	}
	req, ok := parseReceiptRequest(w, r.URL.Query())
	if !ok {
		return
	}
	ctx := r.Context()
	rows, err := ck.AuditReceiptRows(ctx, req.ids, req.runID, req.afterSeq, max(len(req.ids), req.limit+1))
	if err != nil {
		slog.ErrorContext(ctx, "wardyn: audit receipt rows", slog.Any("err", err))
		writeErrorReason(w, http.StatusInternalServerError, reasonAuditReceiptReadFailed, "the audit rows could not be read")
		return
	}
	if len(rows) == 0 || (req.runID == nil && len(rows) < len(req.ids)) {
		writeErrorReason(w, http.StatusNotFound, reasonAuditReceiptNotFound, "no such audit event, or no audit rows for that run")
		return
	}
	more := req.runID != nil && len(rows) > req.limit
	if more {
		rows = rows[:req.limit]
	}
	var redacted int64
	project := partitionProjector(s.isOperator(ctx), &redacted)
	events := make([]auditreceipt.Event, len(rows))
	for i, row := range rows {
		events[i] = receiptEvent(row, project, &redacted)
	}
	cps, err := ck.AuditCheckpoints(ctx, events[0].Row.Seq-1, auditreceipt.CheckpointLookahead)
	if err != nil {
		slog.ErrorContext(ctx, "wardyn: audit receipt checkpoints", slog.Any("err", err))
		writeErrorReason(w, http.StatusInternalServerError, reasonAuditReceiptReadFailed, "the audit checkpoints could not be read")
		return
	}
	asm, err := auditreceipt.Assemble(auditreceipt.AssembleInput{
		Events: events, Checkpoints: signedCheckpoints(cps), Page: req.runID != nil,
		Hashes: func(lo, hi int64) ([]auditreceipt.SeqHash, error) {
			list, err := ck.AuditCheckpointRowHashes(ctx, lo, hi)
			out := make([]auditreceipt.SeqHash, len(list))
			for i, e := range list {
				out[i] = auditreceipt.SeqHash{Seq: e.Seq, RowHash: e.RowHash}
			}
			return out, err
		},
	})
	switch {
	case errors.Is(err, auditreceipt.ErrPending):
		w.Header().Set("Retry-After", s.auditReceiptRetryAfter())
		writeErrorReason(w, http.StatusConflict, reasonAuditReceiptPending,
			"the event is newer than the last signed checkpoint; ask again after the next one is signed")
		return
	case errors.Is(err, auditreceipt.ErrUnchained):
		writeErrorReason(w, http.StatusConflict, reasonAuditReceiptUnchained,
			"the event is in no signed checkpoint and never will be: it carries no row hash, or it was written before signing began")
		return
	case errors.Is(err, auditreceipt.ErrTooLarge):
		writeErrorReason(w, http.StatusRequestEntityTooLarge, reasonAuditReceiptTooLarge,
			"the events span more checkpoints than one receipt carries; ask for fewer, or for a run and page with after_seq")
		return
	case err != nil:
		slog.ErrorContext(ctx, "wardyn: audit receipt assembly", slog.Any("err", err))
		writeErrorReason(w, http.StatusInternalServerError, reasonAuditReceiptReadFailed, "the receipt could not be assembled")
		return
	}
	resp := auditBundleResponse{Bundle: asm.Bundle}
	if more || asm.Included < len(events) {
		resp.NextAfterSeq = events[asm.Included-1].Row.Seq
	}
	target, runData := "events:"+strconv.Itoa(asm.Included), ""
	if req.runID != nil {
		target, runData = "run:"+req.runID.String(), req.runID.String()
	} else if asm.Included == 1 {
		target = events[0].Row.ID.String()
	}
	s.recordAudit(ctx, s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r), "audit.receipt.create", target, "success",
		mustJSON(map[string]any{"events": asm.Included, "run_id": runData, "checkpoints": len(asm.Bundle.Checkpoints), "redacted": redacted})))
	writeJSON(w, http.StatusOK, resp)
}

// handleAuditCheckpoints serves GET /audit/checkpoints?after_seq=N&limit=N: the signed checkpoints covering rows
// above after_seq, oldest first, at most 1,000 a page, with no row and no hash list. Verifying it checks
// signatures and links, never inclusion. next_after_seq is set when a page was full.
func (s *Server) handleAuditCheckpoints(w http.ResponseWriter, r *http.Request) {
	ck, ok := s.auditReceiptReady(w)
	if !ok {
		return
	}
	q := r.URL.Query()
	var after int64
	limit := auditreceipt.MaxCheckpointPage
	if v := q.Get("after_seq"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeErrorReason(w, http.StatusBadRequest, reasonAuditReceiptRequestInvalid, "after_seq is not a non-negative integer")
			return
		}
		after = n
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > auditreceipt.MaxCheckpointPage {
			writeErrorReason(w, http.StatusBadRequest, reasonAuditReceiptRequestInvalid, "limit is not an integer from 1 to "+strconv.Itoa(auditreceipt.MaxCheckpointPage))
			return
		}
		limit = n
	}
	cps, err := ck.AuditCheckpoints(r.Context(), after, limit)
	if err != nil {
		slog.ErrorContext(r.Context(), "wardyn: audit checkpoints", slog.Any("err", err))
		writeErrorReason(w, http.StatusInternalServerError, reasonAuditReceiptReadFailed, "the audit checkpoints could not be read")
		return
	}
	resp := auditBundleResponse{Bundle: auditreceipt.CheckpointExport(signedCheckpoints(cps))}
	if len(cps) == limit {
		resp.NextAfterSeq = cps[len(cps)-1].SeqHi
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleAuditReceiptKeys serves GET /audit/receipt-keys: the public keys checkpoints are signed with, current
// and previous, retired ones marked. The keys are public; the route is on the security tier to keep the four
// audit-chain routes on one. An operator saves the answer once, where this server cannot change it, and pins
// it with `wardyn audit verify --keys`.
func (s *Server) handleAuditReceiptKeys(w http.ResponseWriter, _ *http.Request) {
	if s.cfg.AuditReceiptKeys == nil || len(s.cfg.AuditReceiptKeys()) == 0 {
		writeErrorReason(w, http.StatusServiceUnavailable, reasonAuditReceiptSigningOff,
			"audit checkpoint signing is off or its key could not be loaded")
		return
	}
	keys := auditreceipt.Keys{Format: auditreceipt.KeysFormat}
	for _, k := range s.cfg.AuditReceiptKeys() {
		if len(k.PublicKey) != ed25519.PublicKeySize {
			continue
		}
		k.Kid, k.Fingerprint = auditreceipt.KeyID(k.PublicKey), auditreceipt.Fingerprint(k.PublicKey)
		if der, err := x509.MarshalPKIXPublicKey(ed25519.PublicKey(k.PublicKey)); err == nil {
			k.PublicKeyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
		}
		keys.Keys = append(keys.Keys, k)
	}
	writeJSON(w, http.StatusOK, keys)
}
