// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A reviewed download: a run whose desktop.download is review holds each download for a person's yes on that
// file. The Files panel asks (POST .../files/{name}/review): wardynd hashes the file once, stores nothing, and
// raises a file_download approval naming its name, size and hash and who asked, never its content. The security
// tier decides it, or a unit's file_download deciders; never the run's own people or the asker
// (fileDownloadSecondHumanSwitch), and always once. The approval releases the file once (GET ...?approval=<id>):
// wardynd copies it to a temporary file while hashing it again, sends nothing unless the bytes are the reviewed
// ones, and spends the approval before it sends. Every answer that cannot read, raise or spend an approval
// sends nothing.

// desktopReviewMaxBytes bounds one reviewed download (types.MaxReviewedDownloadBytes); a var so a test can lower it.
var desktopReviewMaxBytes int64 = types.MaxReviewedDownloadBytes

// fileDownloadSecondHumanSwitch names the second-person rule every file_download decision is held to, in its
// refusal and in the break-glass audit row the admin token's decision writes.
const fileDownloadSecondHumanSwitch = "desktop.download review"

// fileDownloadSpender is the store seam that spends a reviewed download's approval once, for that run and file
// (store.PG.SpendFileDownloadApproval). A store without it releases nothing.
type fileDownloadSpender interface {
	SpendFileDownloadApproval(ctx context.Context, id, runID uuid.UUID, want types.FileDownloadScope, jti string) (bool, error)
}

// mountDesktopReviewRoutes adds the ask to the Files panel's routes: the person who may enter the run (desktopRun).
func (s *Server) mountDesktopReviewRoutes(r chi.Router) {
	r.Post("/runs/{id}/desktop/files/{name}/review", s.handleRequestDesktopReview)
}

// auditDesktopReview is a desktop.file.transfer download row with its review step: requested, released or changed.
func (s *Server) auditDesktopReview(r *http.Request, run types.AgentRun, outcome, review string, size int64, sum string) {
	at, principal := actorFromRequest(r)
	data := map[string]any{"direction": "download", "review": review, "bytes": size}
	if sum != "" {
		data["sha256"] = sum
	}
	s.recordAudit(r.Context(), s.auditEvent(&run.ID, at, principal, "desktop.file.transfer", desktopTransferFolder, outcome, mustJSON(data)))
}

// writeReviewUnavailable answers a review whose approval could not be read, raised or spent (step): 503, with
// nothing raised and nothing sent.
func writeReviewUnavailable(w http.ResponseWriter, r *http.Request, step string, err error) {
	slog.WarnContext(r.Context(), "wardynd: reviewed download refused; the approvals could not be used", "step", step, "err", err)
	writeErrorReason(w, http.StatusServiceUnavailable, reasonDesktopReviewUnavailable,
		"the download's review could not be "+step+". Nothing was asked for or sent; try again.")
}

// handleRequestDesktopReview is POST /runs/{id}/desktop/files/{name}/review: 202 with the PENDING file_download row.
func (s *Server) handleRequestDesktopReview(w http.ResponseWriter, r *http.Request) {
	run, rules, ok := s.desktopRun(w, r)
	if !ok {
		return
	}
	name, nameOK := desktopFileNameParam(r)
	switch {
	case !nameOK || !validDesktopFileName(name):
		writeErrorReason(w, http.StatusBadRequest, reasonDesktopTransferBadRequest, "a file name is a plain name: no folder, no leading dot")
		return
	case rules.Download == types.TransferAllowed:
		writeErrorReason(w, http.StatusConflict, reasonDesktopReviewNotHeld, "this run's desktop rules allow downloads; download the file directly")
		return
	case rules.Download != types.TransferReview:
		s.auditDesktopTransfer(r, run, "download", "denied", -1, "")
		writeErrorReason(w, http.StatusForbidden, reasonDesktopTransferBlocked, "this run's desktop rules block downloading files. Nothing was sent.")
		return
	case s.cfg.Approvals == nil:
		writeReviewUnavailable(w, r, "raised", errors.New("no approval service"))
		return
	}
	_, principal := actorFromRequest(r)
	run, start, ok := s.desktopAdmit(w, r, run, true)
	if !ok {
		return
	}
	defer start.release()
	sum := sha256.New()
	var n int64
	code, err := s.desktopExec(r.Context(), run, start, desktopDownloadScript, nil, func(o io.Reader) error {
		var cerr error
		n, cerr = io.Copy(sum, io.LimitReader(o, desktopReviewMaxBytes+1))
		return cerr
	}, name)
	switch {
	case code == 3:
		writeErrorReason(w, http.StatusNotFound, reasonDesktopTransferNotFound, "no file of that name in "+desktopTransferFolder)
		return
	case err != nil || code != 0:
		writeErrorReason(w, http.StatusBadGateway, reasonDesktopTransferFailed, fmt.Sprintf("could not read the file in the desktop (exit %d)", code))
		return
	case n > desktopReviewMaxBytes:
		s.auditDesktopReview(r, run, "denied", "requested", n, "")
		writeErrorReason(w, http.StatusRequestEntityTooLarge, reasonDesktopTransferTooLarge,
			fmt.Sprintf("the file is over the %d MiB limit for a reviewed download. Nothing was asked.", desktopReviewMaxBytes>>20))
		return
	}
	scope := types.FileDownloadScope{Name: name, Bytes: n, SHA256: hex.EncodeToString(sum.Sum(nil)), RequestedBy: principal}
	if !scope.Valid() {
		writeErrorReason(w, http.StatusBadRequest, reasonDesktopTransferBadRequest, "this download can't be asked for: no person is named as asking")
		return
	}
	// The per-run cap the sandbox's own raises are held to, failing closed on a count that cannot be read.
	count, cerr := s.cfg.Approvals.CountForRun(r.Context(), run.ID)
	if cerr != nil {
		writeReviewUnavailable(w, r, "raised", cerr)
		return
	}
	if count >= maxApprovalsPerRun {
		writeErrorReason(w, http.StatusTooManyRequests, reasonInternalApprovalCapReached, "this run has raised too many approvals; no more will be accepted")
		return
	}
	raw, _ := json.Marshal(scope)
	req := types.ApprovalRequest{ID: uuid.New(), RunID: run.ID, Kind: types.ApprovalFileDownload, RequestedScope: raw}
	created, err := s.cfg.Approvals.Request(r.Context(), req)
	if errors.Is(err, types.ErrDuplicatePendingApproval) || (err == nil && created.ID != req.ID) {
		writeErrorReason(w, http.StatusConflict, reasonDesktopReviewPending, "a review of this file is already waiting; it is released once a person approves it")
		return
	}
	if err != nil {
		writeReviewUnavailable(w, r, "raised", err)
		return
	}
	s.auditDesktopReview(r, run, "success", "requested", scope.Bytes, scope.SHA256)
	writeJSON(w, http.StatusAccepted, created)
}

// serveReviewedDownload is the download of a run whose desktop.download is review: only the approval named, an
// APPROVED file_download of this run for this file, releases it, once, and only with the reviewed bytes.
func (s *Server) serveReviewedDownload(w http.ResponseWriter, r *http.Request, run types.AgentRun, name string) {
	ctx := r.Context()
	q := r.URL.Query().Get("approval")
	if q == "" {
		s.auditDesktopTransfer(r, run, "download", "denied", -1, "")
		writeErrorReason(w, http.StatusForbidden, reasonDesktopReviewRequired,
			"this run's desktop rules hold each download for a person's review: ask for one, and download it once it is approved. Nothing was sent.")
		return
	}
	id, err := uuid.Parse(q)
	if err != nil {
		writeErrorReason(w, http.StatusBadRequest, reasonDesktopTransferBadRequest, "approval is an approval id")
		return
	}
	if s.cfg.Approvals == nil {
		writeReviewUnavailable(w, r, "read", errors.New("no approval service"))
		return
	}
	ap, err := s.cfg.Approvals.Get(ctx, id)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		writeReviewUnavailable(w, r, "read", err)
		return
	}
	want, scopeOK := types.ParseFileDownloadScope(ap.RequestedScope)
	if err != nil || ap.RunID != run.ID || ap.Kind != types.ApprovalFileDownload || !scopeOK || want.Name != name || ap.State != types.ApprovalApproved {
		writeErrorReason(w, http.StatusForbidden, reasonDesktopReviewNotApproved, "that approval is not an approved review of this file on this run. Nothing was sent.")
		return
	}
	if ap.MintedJTI != "" {
		writeErrorReason(w, http.StatusConflict, reasonDesktopReviewSpent, "that approval already released its file; ask for another review. Nothing was sent.")
		return
	}
	spender, ok := s.cfg.Store.(fileDownloadSpender)
	if !ok {
		writeReviewUnavailable(w, r, "spent", errors.New("the store has no review spend"))
		return
	}
	run, start, ok := s.desktopAdmit(w, r, run, true)
	if !ok {
		return
	}
	defer start.release()
	tmp, err := os.CreateTemp("", "wardyn-review-*")
	if err != nil {
		writeErrorReason(w, http.StatusBadGateway, reasonDesktopTransferFailed, "could not hold the file to check it; nothing was sent")
		return
	}
	defer func() { _ = tmp.Close(); _ = os.Remove(tmp.Name()) }()
	sum := sha256.New()
	var n int64
	code, err := s.desktopExec(ctx, run, start, desktopDownloadScript, nil, func(o io.Reader) error {
		var cerr error
		n, cerr = io.Copy(io.MultiWriter(tmp, sum), io.LimitReader(o, desktopReviewMaxBytes+1))
		return cerr
	}, name)
	got := hex.EncodeToString(sum.Sum(nil))
	switch {
	case code == 3:
		writeErrorReason(w, http.StatusNotFound, reasonDesktopTransferNotFound, "no file of that name in "+desktopTransferFolder)
		return
	case err != nil || code != 0:
		if s.refuseReviewUnavailable(w, r, run, err) {
			return
		}
		s.auditDesktopTransfer(r, run, "download", "failure", n, "")
		writeErrorReason(w, http.StatusBadGateway, reasonDesktopTransferFailed, fmt.Sprintf("could not read the file in the desktop (exit %d)", code))
		return
	case n != want.Bytes || got != want.SHA256:
		s.auditDesktopReview(r, run, "denied", "changed", n, got)
		writeErrorReason(w, http.StatusConflict, reasonFileChangedSinceReview,
			"the file changed after it was reviewed: its bytes are not the ones approved. Nothing was sent; ask for another review.")
		return
	}
	spent, err := spender.SpendFileDownloadApproval(ctx, ap.ID, run.ID, want, "review:"+uuid.NewString())
	if err != nil {
		writeReviewUnavailable(w, r, "spent", err)
		return
	}
	if !spent {
		writeErrorReason(w, http.StatusConflict, reasonDesktopReviewSpent, "that approval already released its file; ask for another review. Nothing was sent.")
		return
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		writeErrorReason(w, http.StatusBadGateway, reasonDesktopTransferFailed, "could not read back the checked file; ask for another review")
		return
	}
	desktopDownloadHeaders(w, name)
	if _, err := io.Copy(w, tmp); err != nil {
		s.auditDesktopReview(r, run, "failure", "released", n, got)
		return
	}
	s.auditDesktopReview(r, run, "success", "released", n, got)
}

// fileDownloadDecisionRule is decide's scope rule for a file_download row: it releases one file, once, so a
// bodyless decision or "once" stores "once" and any wider scope is refused. isFD reports whether the row is one;
// ok=false means the refusal is written.
func fileDownloadDecisionRule(w http.ResponseWriter, ap types.ApprovalRequest, scope types.ApprovalScope) (types.ApprovalScope, bool, bool) {
	if ap.Kind != types.ApprovalFileDownload {
		return scope, false, true
	}
	if scope == "" || scope == types.ScopeOnce {
		return types.ScopeOnce, true, true
	}
	writeErrorReason(w, http.StatusBadRequest, reasonDecisionScopeInvalidForKind,
		`A download's approval releases that file once: send no decision_scope, or "once"`)
	return scope, true, false
}

// askedForDownload reports whether principal asked for ap's download: the asker never decides it, whoever they are.
func askedForDownload(ap types.ApprovalRequest, principal string) bool {
	if ap.Kind != types.ApprovalFileDownload {
		return false
	}
	sc, ok := types.ParseFileDownloadScope(ap.RequestedScope)
	return !ok || sc.RequestedBy == principal // a scope that cannot be read names nobody who may decide
}
