// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// workloadPutRefusal is the answer PutWorkload's refusal err gets: the status, reason and sentence the PUT
// writes. ok is false when err is not one of the store's refusals.
func workloadPutRefusal(err error) (ref client.WorkloadPreviewRefusal, ok bool) {
	switch {
	case errors.Is(err, store.ErrWorkloadSourceConflict):
		return client.WorkloadPreviewRefusal{Status: http.StatusConflict, Reason: reasonWorkloadSourceConflict,
			Error: "This workload belongs to its custom resource: change it in Git, not with an apply."}, true
	case errors.Is(err, store.ErrWorkloadCapReached):
		return client.WorkloadPreviewRefusal{Status: http.StatusUnprocessableEntity, Reason: reasonWorkloadCapReached,
			Error: fmt.Sprintf("You hold %d workloads, the most allowed. Delete one first.", store.MaxWorkloadsPerOwner)}, true
	}
	return client.WorkloadPreviewRefusal{}, false
}

// workloadDocumentFor is a workload as the API reads it at tier: the document GET /workloads/{name} shows.
func workloadDocumentFor(name, description, runIn, restartKey string, intent json.RawMessage, tier workspaceReadTier) client.WorkloadDocument {
	doc := client.WorkloadDocument{APIVersion: client.TemplateDocumentVersion, Kind: client.WorkloadDocumentKind,
		Name: name, Description: description, RunIn: runIn, RestartKey: restartKey}
	_ = json.Unmarshal(redactWorkloadIntentForRead(intent, tier), &doc.Intent) //nolint:errcheck // the door wrote it; an unreadable intent reads as none
	return doc
}

// handlePreviewWorkload is POST /workloads/{name}/preview: what the same body to PUT /workloads/{name}
// would do for the caller. It takes the PUT's own steps and stops before the first write: the strict decode
// and normalizeWorkload, the caller as the owner (there is no ?owner=), PlanWorkloadPut for the store's
// answer, and, when a launch is owed, the preflight of the request the launch would make
// (workloadCreateRequest). It stores nothing and claims no launch. The one write it can cause is
// preflight's own: a refusal at a gate that audits leaves the dry-run-marked row a refused preflight leaves.
// A store refusal is in the body, so the diff still shows; a preflight 429 or 5xx is the preview's own
// answer, since it is no prediction.
//
// The route is on the service allow-list beside the PUT, and on the delegation list nowhere, as the PUT is:
// it reaches only what the PUT reaches, the caller's own workloads and the caller's own preflight.
func (s *Server) handlePreviewWorkload(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.workloadStore(w)
	if !ok {
		return
	}
	var req client.WorkloadApplyRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	in, invalid := normalizeWorkload(client.WorkloadDocument{APIVersion: client.TemplateDocumentVersion, Kind: client.WorkloadDocumentKind,
		Name: chi.URLParam(r, "name"), Description: req.Description, RunIn: req.RunIn, RestartKey: req.RestartKey, Intent: req.Intent})
	if invalid != nil {
		invalid.write(w)
		return
	}
	owner := principalFromRequest(r)
	in.Owner, in.Source, in.By = owner, store.WorkloadSourceAPI, owner
	cur, write, err := ws.PlanWorkloadPut(r.Context(), in)
	out := client.WorkloadPreview{Name: in.Name, Owner: owner, Op: "none", Launch: client.WorkloadPreviewLaunch{State: client.WorkloadPreviewNoLaunch}}
	if ref, refused := workloadPutRefusal(err); refused {
		out.Refusal = &ref
	} else if err != nil {
		writeServerError(w, r, "plan workload", err)
		return
	}
	tier := s.workloadReadTier(r, owner)
	out.Proposed = workloadDocumentFor(in.Name, in.Description, in.RunIn, in.RestartKey, in.Intent, tier)
	if tier == workspaceReadFull { // the digest of an intent whose host axis is withheld would let a reader test guesses at it
		out.Digest = in.Digest
	}
	var stored *client.WorkloadDocument
	if cur != nil {
		doc := workloadDocumentFor(cur.Name, cur.Description, cur.RunIn, cur.RestartKey, cur.Intent, tier)
		stored = &doc
		out.Stored = stored
		if tier == workspaceReadFull {
			out.StoredDigest = cur.Digest
		}
	}
	out.Changes = client.DiffWorkloadDocuments(stored, out.Proposed)
	if out.Refusal == nil {
		out.Outcome = client.WorkloadOutcome(write)
		switch write {
		case store.WorkloadCreated:
			out.Op = "create"
		case store.WorkloadUpdated:
			out.Op = "update"
		}
		// A launch is owed to a new or changed spec, and to an unchanged one whose last launch was refused.
		if write != store.WorkloadUnchanged || cur != nil && cur.Generation > cur.CurrentGeneration {
			if !s.previewWorkloadLaunch(w, r, store.Workload{Owner: owner, Name: in.Name, Source: store.WorkloadSourceAPI,
				Description: in.Description, RunIn: in.RunIn, Intent: in.Intent}, cur, &out.Launch) {
				return
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// previewWorkloadLaunch fills launch with what launching planned would meet: the preflight of the same
// request launchClaimed makes, captured as it captures the create. ok is false when it answered w: a
// preflight 429 or 5xx is not a prediction.
func (s *Server) previewWorkloadLaunch(w http.ResponseWriter, r *http.Request, planned store.Workload, cur *store.Workload,
	launch *client.WorkloadPreviewLaunch,
) (ok bool) {
	cr, err := workloadCreateRequest(r, planned)
	if err != nil {
		writeServerError(w, r, "preview workload launch", err)
		return false
	}
	cw := newQueueRefusalCapture()
	s.handlePreflightRun(cw, cr)
	switch {
	case cw.status == http.StatusOK:
		var pf client.PreflightResult
		if err := json.Unmarshal(cw.body, &pf); err != nil {
			writeServerError(w, r, "read the launch preflight", err)
			return false
		}
		launch.State, launch.Preflight = client.WorkloadPreviewWouldLaunch, &pf
		if cur != nil {
			launch.Replaces = cur.CurrentRunID
		}
		return true
	case cw.status >= 400 && cw.status < 500 && cw.status != http.StatusTooManyRequests:
		reason, msg, _ := cw.refused()
		if reason == "" {
			reason = reasonInternalError
		}
		launch.State, launch.Reason, launch.Message = client.WorkloadPreviewWouldRefuse, reason, workloadRefusalMsg(reason, msg)
		return true
	case cw.status == 0:
		writeServerError(w, r, "preview workload launch", errors.New("the preflight answered nothing"))
		return false
	}
	for k, v := range cw.header {
		w.Header()[k] = v
	}
	w.WriteHeader(cw.status)
	_, _ = w.Write(cw.body) //nolint:errcheck // the caller went away
	return false
}
