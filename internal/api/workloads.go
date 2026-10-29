// Copyright 2026 The Wardyn Authors
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
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// Workloads (0.10): a named run request Wardyn keeps running for its owner (store_workloads.go keeps the
// rows). An apply checks the document through the template door and records it; it launches only while
// the stored spec is ahead of its run, through handleCreateRun as the owner, so every gate a POST /runs
// passes applies unchanged and a workload grants nothing. A new run replaces the old one only once it
// exists (create, then end); a refused launch is recorded and the running one keeps running.
//
// The two halves, normalizeWorkload with PutWorkload and launchWorkload, take no HTTP caller of their own:
// the custom-resource path records and launches through them with a request serviceLaunchRequest builds.

// The run.kill reason a workload's run ends with.
const (
	workloadKillReplaced = store.WorkloadEndReplaced // a newer launch of the workload replaced it
	workloadKillDeleted  = store.WorkloadEndDeleted  // the workload was deleted
)

const workloadNotFoundMsg = "There is no workload of that name that you may reach."

func (s *Server) mountWorkloadRoutes(r, securityOps chi.Router) {
	r.Put("/workloads/{name}", s.handlePutWorkload)
	r.Get("/workloads", s.handleListWorkloads)
	r.Get("/workloads/{name}", s.handleGetWorkload)
	r.Delete("/workloads/{name}", s.handleDeleteWorkload)
	r.Post("/workloads/{name}/restart", s.handleRestartWorkload)
	// A binding is standing authority over a service identity: the security tier's, applied directly and
	// audited, as a token move is (workload_bindings.go).
	securityOps.Post("/workload-bindings", s.handleCreateWorkloadBinding)
	securityOps.Get("/workload-bindings", s.handleListWorkloadBindings)
	securityOps.Delete("/workload-bindings/{id}", s.handleDeleteWorkloadBinding)
}

func (s *Server) workloadStore(w http.ResponseWriter) (store.WorkloadStore, bool) {
	ws, ok := s.cfg.Store.(store.WorkloadStore)
	if !ok {
		writeErrorReason(w, http.StatusNotImplemented, reasonWorkloadsUnavailable, "workloads require the Postgres store backend")
	}
	return ws, ok
}

// workloadInvalid is the 400 workload_invalid body: the sentence, and the template door's diagnostics when
// the intent is what failed.
type workloadInvalid struct {
	errorBody
	Diagnostics []client.TemplateDiagnostic `json:"diagnostics,omitempty"`
}

func (b *workloadInvalid) write(w http.ResponseWriter) {
	writeJSON(w, http.StatusBadRequest, b)
}

func invalidWorkload(msg string, diags []client.TemplateDiagnostic) *workloadInvalid {
	return &workloadInvalid{errorBody: errorBody{Error: msg, Reason: reasonWorkloadInvalid}, Diagnostics: diags}
}

// normalizeWorkload checks a workload document and returns the record half of its apply: the header as
// written, the intent as the template door normalises a `kind: Run` document's (so a YAML and a JSON copy
// of one spec store the same bytes), and the digest of that intent, run_in and restart_key. The owner,
// the source and the writer are the caller's to set. Nothing is stored on a refusal.
func normalizeWorkload(doc client.WorkloadDocument) (store.WorkloadPut, *workloadInvalid) {
	if err := doc.Validate(); err != nil {
		return store.WorkloadPut{}, invalidWorkload(err.Error(), nil)
	}
	src, err := json.Marshal(client.TemplateDocument{APIVersion: client.TemplateDocumentVersion, Kind: client.RunDocumentKind, Intent: doc.Intent})
	if err != nil {
		return store.WorkloadPut{}, invalidWorkload("the intent cannot be written as JSON", nil)
	}
	res := validateTemplateImport(src, client.TemplateFormatJSON, client.RunDocumentKind)
	if res.Document == nil {
		return store.WorkloadPut{}, invalidWorkload("the intent is not a run document Wardyn accepts; the diagnostics say where", res.Diagnostics)
	}
	intent, err := json.Marshal(res.Document.Intent)
	if err != nil {
		return store.WorkloadPut{}, invalidWorkload("the intent cannot be written as JSON", nil)
	}
	digest, err := json.Marshal(struct {
		Intent     json.RawMessage `json:"intent"`
		RunIn      string          `json:"run_in"`
		RestartKey string          `json:"restart_key"`
	}{intent, doc.RunIn, doc.RestartKey})
	if err != nil {
		return store.WorkloadPut{}, invalidWorkload("the workload cannot be written as JSON", nil)
	}
	sum := sha256.Sum256(digest)
	return store.WorkloadPut{Name: doc.Name, Description: doc.Description, RunIn: doc.RunIn, RestartKey: doc.RestartKey,
		Intent: intent, Digest: hex.EncodeToString(sum[:])}, nil
}

// writeWorkloadPutError answers a refused PutWorkload. ok is false when err was answered.
func writeWorkloadPutError(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, store.ErrWorkloadSourceConflict):
		writeErrorReason(w, http.StatusConflict, reasonWorkloadSourceConflict,
			"This workload belongs to its custom resource: change it in Git, not with an apply.")
	case errors.Is(err, store.ErrWorkloadCapReached):
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonWorkloadCapReached,
			fmt.Sprintf("You hold %d workloads, the most allowed. Delete one first.", store.MaxWorkloadsPerOwner))
	default:
		writeServerError(w, r, "record workload", err)
	}
	return false
}

// handlePutWorkload is PUT /workloads/{name}: apply the caller's own workload of that name. 201 when it is
// new, 200 otherwise; a refused launch is a 200 too, because the spec was stored.
func (s *Server) handlePutWorkload(w http.ResponseWriter, r *http.Request) {
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
	ctx := r.Context()
	owner := principalFromRequest(r)
	in.Owner, in.Source, in.By = owner, store.WorkloadSourceAPI, owner
	wl, write, err := ws.PutWorkload(ctx, in)
	if !writeWorkloadPutError(w, r, err) {
		return
	}
	if write != store.WorkloadUnchanged {
		actorType, actor := actorFromRequest(r)
		s.recordAudit(ctx, s.auditEvent(nil, actorType, actor, "workload.apply", workloadTarget(wl), "success", mustJSON(map[string]any{
			"name": wl.Name, "owner": wl.Owner, "source": wl.Source, "generation": wl.Generation, "digest": wl.Digest, "outcome": write,
		})))
	}
	launch, err := s.launchWorkload(r, ws, wl)
	if err != nil {
		writeServerError(w, r, "launch workload", err)
		return
	}
	status := http.StatusOK
	if write == store.WorkloadCreated {
		status = http.StatusCreated
	}
	s.answerWorkload(w, r, ws, wl, client.WorkloadOutcome(write), launch, status)
}

// answerWorkload writes an apply's or a restart's answer: the workload as it reads after the launch.
func (s *Server) answerWorkload(w http.ResponseWriter, r *http.Request, ws store.WorkloadStore, wl store.Workload, outcome client.WorkloadOutcome,
	launch client.WorkloadLaunch, status int,
) {
	v, err := ws.GetWorkload(r.Context(), wl.Owner, wl.Name)
	if err != nil {
		v = store.WorkloadView{Workload: wl} // deleted under the apply: answer what was recorded
	}
	writeJSON(w, status, client.WorkloadApplyResult{Workload: workloadOut(v), Outcome: outcome, Launch: launch})
}

func workloadTarget(wl store.Workload) string { return wl.Owner + "/" + wl.Name }

// workloadOut is a stored workload as the API reports it.
func workloadOut(v store.WorkloadView) client.Workload {
	out := client.Workload{Name: v.Name, Owner: v.Owner, Source: client.WorkloadSource(v.Source), Namespace: v.SourceNamespace,
		Description: v.Description, RunIn: v.RunIn, RestartKey: v.RestartKey, Generation: v.Generation,
		CurrentGeneration: v.CurrentGeneration, RunID: v.CurrentRunID, State: string(v.RunState), CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt}
	if v.RunState != "" && v.ServiceHealth != "" {
		out.State += "/" + v.ServiceHealth
	}
	_ = json.Unmarshal(v.Intent, &out.Intent) //nolint:errcheck // the door wrote it; an unreadable intent reads as none
	if f := v.LastRefusal; f != nil {
		out.LastRefusal = &client.WorkloadRefusal{Generation: f.Generation, Reason: f.Reason, Message: f.Message, At: f.At}
	}
	return out
}

// launchWorkload launches wl's spec when a launch is owed and this replica takes the claim, as the
// identity r carries: the owner, on the apply's own request or on one serviceLaunchRequest built. The
// launch is handleCreateRun on a POST /api/v1/runs made of the stored intent, so a refusal is the create
// path's own reason and sentence. A launched run becomes the workload's and then the run it replaces is
// ended; a refused one is recorded, and the current run keeps running. An error leaves the claim to lapse.
func (s *Server) launchWorkload(r *http.Request, ws store.WorkloadStore, wl store.Workload) (client.WorkloadLaunch, error) {
	none := client.WorkloadLaunch{State: client.WorkloadNoLaunch}
	if wl.CurrentGeneration >= wl.Generation {
		return none, nil // a live or ended run of this spec stands: an ended one is restarted, never relaunched
	}
	ctx := r.Context()
	claimed, ok, err := ws.ClaimWorkloadLaunch(ctx, wl.ID)
	if err != nil || !ok {
		return none, err
	}
	if claimed.Source == store.WorkloadSourceK8s && claimed.SourcePoolID == nil {
		// A custom resource's runs go to its binding's pool only; with the pool gone there is nowhere to go.
		gone := &runnerpool.Refusal{Reason: runnerpool.ReasonNotFound}
		return s.refuseWorkloadLaunch(r, ws, claimed, string(gone.Reason), gone.Message())
	}
	cr, err := workloadCreateRequest(r, claimed)
	if err != nil {
		return none, err
	}
	cw := newQueueRefusalCapture()
	s.handleCreateRun(cw, cr)
	if cw.status != http.StatusCreated {
		reason, msg, _ := cw.refused()
		if reason == "" {
			reason = reasonInternalError
		}
		return s.refuseWorkloadLaunch(r, ws, claimed, reason, msg)
	}
	var created createRunResponse
	if err := json.Unmarshal(cw.body, &created); err != nil {
		return none, fmt.Errorf("read the launched run: %w", err)
	}
	prev, err := ws.SetWorkloadRun(ctx, claimed.ID, claimed.ClaimToken, claimed.Generation, created.ID)
	if err != nil {
		// The run is not the workload's: another launch was recorded first, or the workload is gone.
		why := workloadKillReplaced
		if errors.Is(err, store.ErrNotFound) {
			why = workloadKillDeleted
		}
		s.endUntakenWorkloadRun(r, ws, created.ID, claimed, why)
		if errors.Is(err, store.ErrWorkloadSuperseded) || errors.Is(err, store.ErrNotFound) {
			return none, nil
		}
		return none, err
	}
	actorType, actor := actorFromRequest(r)
	s.recordAudit(ctx, s.auditEvent(&created.ID, actorType, actor, "workload.launch", workloadTarget(claimed), "success", mustJSON(map[string]any{
		"name": claimed.Name, "owner": claimed.Owner, "generation": claimed.Generation, "run_id": created.ID,
	})))
	if prev != nil && *prev != created.ID {
		s.endWorkloadRun(r, *prev, claimed, workloadKillReplaced)
		s.recordAudit(ctx, s.auditEvent(&created.ID, actorType, actor, "workload.replace", workloadTarget(claimed), "success", mustJSON(map[string]any{
			"name": claimed.Name, "owner": claimed.Owner, "generation": claimed.Generation, "old_run_id": *prev, "new_run_id": created.ID,
		})))
	}
	return client.WorkloadLaunch{State: client.WorkloadLaunched, RunID: &created.ID, Warnings: created.Warnings}, nil
}

// workloadCreateRequest is the POST /api/v1/runs a launch of wl makes on r's identity: the stored intent
// as a Run document's request (its inline_policy laid on as the overlay), named for the workload, with the
// header's description and run_in, and fenced to the binding's pool for a custom resource.
func workloadCreateRequest(r *http.Request, wl store.Workload) (*http.Request, error) {
	var intent client.TemplateIntent
	if err := json.Unmarshal(wl.Intent, &intent); err != nil {
		return nil, fmt.Errorf("read the workload intent: %w", err)
	}
	req, err := client.RunDocumentRequest(intent)
	if err != nil {
		return nil, err
	}
	req.Title, req.Description, req.RunIn = wl.Name, wl.Description, wl.RunIn
	if wl.Source == store.WorkloadSourceK8s && wl.SourcePoolID != nil {
		req.RunnerPoolID = wl.SourcePoolID.String()
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	cr := r.Clone(r.Context())
	cr.Method, cr.URL, cr.RequestURI = http.MethodPost, &url.URL{Path: "/api/v1/runs"}, ""
	cr.Body, cr.ContentLength, cr.GetBody = io.NopCloser(bytes.NewReader(body)), int64(len(body)), nil
	cr.Header.Set("Content-Type", "application/json")
	return cr, nil
}

// refuseWorkloadLaunch records the claimed launch's refusal, which leaves the current run running, and
// answers it. A claim that lapsed and was taken by another launch records nothing.
func (s *Server) refuseWorkloadLaunch(r *http.Request, ws store.WorkloadStore, wl store.Workload, reason, msg string) (client.WorkloadLaunch, error) {
	ctx := r.Context()
	err := ws.RecordWorkloadRefusal(ctx, wl.ID, wl.ClaimToken, store.WorkloadRefusal{Generation: wl.Generation, Reason: reason, Message: msg, At: s.cfg.Now().UTC()})
	switch {
	case errors.Is(err, store.ErrWorkloadSuperseded) || errors.Is(err, store.ErrNotFound):
		return client.WorkloadLaunch{State: client.WorkloadNoLaunch}, nil
	case err != nil:
		return client.WorkloadLaunch{}, err
	}
	actorType, actor := actorFromRequest(r)
	s.recordAudit(ctx, s.auditEvent(nil, actorType, actor, "workload.launch_refused", workloadTarget(wl), "denied", mustJSON(map[string]any{
		"name": wl.Name, "owner": wl.Owner, "generation": wl.Generation, "reason": reason,
	})))
	return client.WorkloadLaunch{State: client.WorkloadRefused, Reason: reason, Message: msg}, nil
}

// endWorkloadRun ends runID, a run that is no longer wl's own, as the caller of r: the run a newer launch
// replaced, the current run of a deleted workload, or a run whose record another launch won. The run.kill
// row says why. A run that has ended already is left as it is. The run is owed its end in
// workload_run_ends, written with the delete or the record that let it go, and the end works detached from
// r: a caller who goes away does not cut it short, and an end that does not finish here is finished by
// sweepWorkloadRunEnds.
func (s *Server) endWorkloadRun(r *http.Request, runID uuid.UUID, wl store.Workload, why string) {
	killerType, killer := actorFromRequest(r)
	s.endOwedWorkloadRun(r.Context(), store.WorkloadRunEnd{RunID: runID, Owner: wl.Owner, Name: wl.Name, Generation: wl.Generation, Reason: why},
		killerType, killer)
}

// endUntakenWorkloadRun ends runID, a run a launch of wl started that the workload did not take. Nothing
// else remembers that run, so it is recorded as owed its end first.
func (s *Server) endUntakenWorkloadRun(r *http.Request, ws store.WorkloadStore, runID uuid.UUID, wl store.Workload, why string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), killCascadeTimeout)
	defer cancel()
	if err := ws.OweWorkloadRunEnd(ctx, store.WorkloadRunEnd{RunID: runID, Owner: wl.Owner, Name: wl.Name, Generation: wl.Generation, Reason: why}); err != nil {
		slog.ErrorContext(ctx, "wardynd: record a workload's run as owed its end", slog.String("run_id", runID.String()),
			slog.String("workload", workloadTarget(wl)), slog.Any("err", err))
	}
	s.endWorkloadRun(r, runID, wl, why)
}

// endOwedWorkloadRun ends e's run as killer and then drops what was owed. The reads and the kill run on a
// context detached from ctx and bounded, as killRunCascade's own are. Once the KILLED transition is won the
// end is durable: a teardown cut short is the sandbox sweep's to finish (recoverAbandonedKillTail). A run
// that is not ended here stays owed.
func (s *Server) endOwedWorkloadRun(ctx context.Context, e store.WorkloadRunEnd, killerType types.ActorType, killer string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), killCascadeTimeout)
	defer cancel()
	extra := map[string]any{"reason": e.Reason, "workload": e.Name, "generation": e.Generation}
	run, err := s.cfg.Store.GetRun(ctx, e.RunID)
	ended := err == nil && isTerminalRunState(run.State)
	// A lost CAS is a move between live states (a dispatch claim, the sandbox coming up): re-read and kill
	// from the state the run now holds, as killLiveRun does.
	for attempt := 1; err == nil && !ended; attempt++ {
		applied, killData, kerr := s.killRunCascade(ctx, run, killerType, killer, extra)
		if kerr != nil || applied || attempt == killCASAttempts {
			ended = applied
			if err = kerr; err == nil && len(killData) > 0 {
				err = fmt.Errorf("teardown steps failed: %v", killData)
			}
			break
		}
		run, err = s.cfg.Store.GetRun(ctx, e.RunID)
		ended = err == nil && isTerminalRunState(run.State)
	}
	if errors.Is(err, store.ErrNotFound) {
		ended, err = true, nil // the run row is gone, and its owed end with it
	}
	target := e.Owner + "/" + e.Name
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: end a workload's run", slog.String("run_id", e.RunID.String()), slog.String("workload", target), slog.Any("err", err))
	}
	ws, ok := s.cfg.Store.(store.WorkloadStore)
	if !ended || !ok {
		return
	}
	if err := ws.DropWorkloadRunEnd(ctx, e.RunID); err != nil {
		slog.ErrorContext(ctx, "wardynd: drop a workload run's owed end", slog.String("run_id", e.RunID.String()), slog.String("workload", target), slog.Any("err", err))
	}
}

// workloadRunEndGrace is how old an owed end must be before the sweep takes it: longer than an end in
// flight takes (its reads share one killCascadeTimeout and its won kill has its own), so the sweep does not
// race the request that let the run go.
const workloadRunEndGrace = 2*killCascadeTimeout + 30*time.Second

// workloadRunEndBatch bounds the owed ends one sweep pass takes; the rest wait for the next pass.
const workloadRunEndBatch = 100

// sweepWorkloadRunEnds ends the runs that workloads let go of and whose end was cut short: the request
// went away, a read failed, or the process died first. It runs on the workload status sweeper's cadence,
// on one replica, as system/wardynd. No other sweep reaches such a run: the sandbox sweep takes terminal
// runs only, and the lease sweep only a run past its end or its lifetime cap.
func (s *Server) sweepWorkloadRunEnds(ctx context.Context) error {
	ws, ok := s.cfg.Store.(store.WorkloadStore)
	if !ok {
		return nil
	}
	ctx, end, ok := s.beginLeaderSweep(ctx)
	if !ok {
		return nil
	}
	defer end()
	owed, err := ws.ListWorkloadRunEnds(ctx, workloadRunEndGrace, workloadRunEndBatch)
	if err != nil {
		return err
	}
	for _, e := range owed {
		if ctx.Err() != nil {
			return ctx.Err() // the rest wait for the next pass
		}
		s.endOwedWorkloadRun(ctx, e, types.ActorSystem, "wardynd")
	}
	return nil
}

// workloadOwnerOf is the owner a workload route acts for: ?owner= when given, else the caller. ok is false
// when the caller may not read, restart or delete that owner's workloads. The owner may, and so may the
// current sponsor of a service-identity owner and the security tier, the reach a run's read and stop have
// (ownsRunOrSponsorOrAdmin).
func (s *Server) workloadOwnerOf(r *http.Request) (string, bool) {
	caller := principalFromRequest(r)
	owner := r.URL.Query().Get("owner")
	if owner == "" || owner == caller {
		return caller, caller != ""
	}
	return owner, s.isSecurityOperator(r.Context()) || s.sponsorsWorkloadOwner(r, owner)
}

// sponsorsWorkloadOwner reports whether the caller of r is the current sponsor of the service identity
// owner names. A service and a portal's delegated token never are, as for a run (sponsorsRun).
func (s *Server) sponsorsWorkloadOwner(r *http.Request, owner string) bool {
	name, ok := strings.CutPrefix(owner, types.ServicePrincipalPrefix)
	caller := principalFromRequest(r)
	if !ok || caller == "" || isServicePrincipal(caller) || delegatedRequest(r.Context()) {
		return false
	}
	si, err := s.serviceRecord(r.Context(), name)
	return err == nil && si.Sponsor == caller
}

// workloadFor reads the workload a {name} route names, for the owner workloadOwnerOf resolves. A missing
// workload and one the caller may not reach get the same 404.
func (s *Server) workloadFor(w http.ResponseWriter, r *http.Request, ws store.WorkloadStore) (store.WorkloadView, bool) {
	var v store.WorkloadView
	err := store.ErrNotFound
	if owner, ok := s.workloadOwnerOf(r); ok {
		v, err = ws.GetWorkload(r.Context(), owner, chi.URLParam(r, "name"))
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErrorReason(w, http.StatusNotFound, reasonWorkloadNotFound, workloadNotFoundMsg)
		return v, false
	case err != nil:
		writeServerError(w, r, "read workload", err)
		return v, false
	}
	return v, true
}

// handleGetWorkload is GET /workloads/{name}[?owner=]: the normalised document with its run's state.
func (s *Server) handleGetWorkload(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.workloadStore(w)
	if !ok {
		return
	}
	if v, ok := s.workloadFor(w, r, ws); ok {
		writeJSON(w, http.StatusOK, workloadOut(v))
	}
}

// handleListWorkloads is GET /workloads[?owner=]: the caller's own and those of the service identities
// they sponsor; every owner's for the security tier; one owner's with ?owner=, or none when the caller may
// not reach that owner.
func (s *Server) handleListWorkloads(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.workloadStore(w)
	if !ok {
		return
	}
	page, ok := parseListPage(w, r, defaultListLimit)
	if !ok {
		return
	}
	ctx := r.Context()
	owners, all, err := s.workloadListOwners(r)
	if err != nil {
		writeServerError(w, r, "list workloads", err)
		return
	}
	servePage(w, r, page, func(p store.Page) ([]client.Workload, error) {
		var rows []store.WorkloadView
		var lerr error
		if all {
			rows, lerr = ws.ListWorkloads(ctx, p)
		} else {
			rows, lerr = ws.ListWorkloadsOf(ctx, owners, p)
		}
		out := make([]client.Workload, 0, len(rows))
		for _, v := range rows {
			out = append(out, workloadOut(v))
		}
		return out, lerr
	}, nil)
}

// workloadListOwners is whose workloads GET /workloads lists: all, or the owners named.
func (s *Server) workloadListOwners(r *http.Request) (owners []string, all bool, err error) {
	ctx := r.Context()
	if r.URL.Query().Get("owner") != "" {
		if owner, ok := s.workloadOwnerOf(r); ok {
			return []string{owner}, false, nil
		}
		return nil, false, nil
	}
	if s.isSecurityOperator(ctx) {
		return nil, true, nil
	}
	caller := principalFromRequest(r)
	owners = []string{caller}
	ss, ok := s.cfg.Store.(store.ServiceIdentityStore)
	if !ok || isServicePrincipal(caller) || delegatedRequest(ctx) {
		return owners, false, nil
	}
	sponsored, err := ss.ListServiceIdentities(ctx, caller, store.Page{})
	for _, si := range sponsored {
		owners = append(owners, types.ServicePrincipalPrefix+si.Name)
	}
	return owners, false, err
}

// handleDeleteWorkload is DELETE /workloads/{name}[?owner=]: the row goes, then its run is ended. A custom
// resource's workload is deleted in Git; through the API only the security tier may, and the resource
// writes it back unless it is gone too.
func (s *Server) handleDeleteWorkload(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.workloadStore(w)
	if !ok {
		return
	}
	v, ok := s.workloadFor(w, r, ws)
	if !ok {
		return
	}
	ctx := r.Context()
	if v.Source == store.WorkloadSourceK8s && !s.isSecurityOperator(ctx) {
		writeErrorReason(w, http.StatusConflict, reasonWorkloadManagedByK8s,
			"This workload belongs to its custom resource: delete the resource in Git, or ask an admin.")
		return
	}
	removed, err := ws.DeleteWorkload(ctx, v.Owner, v.Name)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErrorReason(w, http.StatusNotFound, reasonWorkloadNotFound, workloadNotFoundMsg)
		return
	case err != nil:
		writeServerError(w, r, "delete workload", err)
		return
	}
	data := map[string]any{"name": removed.Name, "owner": removed.Owner, "source": removed.Source, "via": "api"}
	if id := removed.CurrentRunID; id != nil {
		s.endWorkloadRun(r, *id, removed, workloadKillDeleted)
		data["ended_run_id"] = *id
	}
	actorType, actor := actorFromRequest(r)
	s.recordAudit(ctx, s.auditEvent(removed.CurrentRunID, actorType, actor, "workload.delete", workloadTarget(removed), "success", mustJSON(data)))
	w.WriteHeader(http.StatusNoContent)
}

// handleRestartWorkload is POST /workloads/{name}/restart[?owner=]: a fresh run of the same spec, then the
// old one ends; a refused one leaves the old one running. The launch is the owner's: the owner's own
// request, or for a service identity's workload its authority as its token would carry it. A person's
// workload restarted by someone else is recorded and starts at the owner's next apply, because nothing
// launches as a person without their session.
func (s *Server) handleRestartWorkload(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.workloadStore(w)
	if !ok {
		return
	}
	v, ok := s.workloadFor(w, r, ws)
	if !ok {
		return
	}
	ctx := r.Context()
	wl, err := ws.RestartWorkload(ctx, v.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErrorReason(w, http.StatusNotFound, reasonWorkloadNotFound, workloadNotFoundMsg)
		return
	case err != nil:
		writeServerError(w, r, "restart workload", err)
		return
	}
	actorType, actor := actorFromRequest(r)
	s.recordAudit(ctx, s.auditEvent(nil, actorType, actor, "workload.restart", workloadTarget(wl), "success", mustJSON(map[string]any{
		"name": wl.Name, "owner": wl.Owner, "generation": wl.Generation,
	})))
	launch, err := s.restartLaunch(r, ws, wl)
	if err != nil {
		writeServerError(w, r, "launch workload", err)
		return
	}
	s.answerWorkload(w, r, ws, wl, client.WorkloadUnchanged, launch, http.StatusOK)
}

// restartLaunch launches a restarted workload as its owner (handleRestartWorkload says who that can be).
func (s *Server) restartLaunch(r *http.Request, ws store.WorkloadStore, wl store.Workload) (client.WorkloadLaunch, error) {
	if principalFromRequest(r) == wl.Owner {
		return s.launchWorkload(r, ws, wl)
	}
	name, ok := strings.CutPrefix(wl.Owner, types.ServicePrincipalPrefix)
	if !ok {
		return client.WorkloadLaunch{State: client.WorkloadNoLaunch, Message: "The restart is recorded: the owner's next apply starts the new run."}, nil
	}
	ctx := r.Context()
	si, err := s.serviceRecord(ctx, name)
	inactive := client.WorkloadLaunch{State: client.WorkloadRefused, Reason: reasonServiceIdentityInactive,
		Message: "The service identity is suspended or retired, or its sponsor's session is not live, so nothing launches as it."}
	switch {
	case errors.Is(err, store.ErrNotFound):
		return inactive, nil
	case err != nil:
		return client.WorkloadLaunch{}, err
	}
	// The service's standing now, as its tokens are checked on every request: usable, its sponsor live.
	sr, cancel, err := serviceLaunchRequest(si, si.Sponsor)
	if err != nil {
		return client.WorkloadLaunch{}, err
	}
	defer cancel()
	if live, err := s.queueOwnerLive(sr.Context(), serviceAuthority(si, si.Sponsor), s.cfg.Now()); err != nil || !live {
		return inactive, err
	}
	return s.launchWorkload(sr, ws, wl)
}

// serviceAuthority is the authority a launch acting as service identity si under sponsor binds: the
// identity a token of si carries (serviceTokenAuth), at member reach with no groups and si's user type.
func serviceAuthority(si types.ServiceIdentity, sponsor string) queueAuthority {
	return queueAuthority{Sub: types.ServicePrincipalPrefix + si.Name, Role: oidc.RoleUser, UserType: si.UserType, Groups: []string{},
		Service: si.Name, Sponsor: sponsor}
}

// serviceLaunchRequest is the request a launch acting as si under sponsor is made on: si's authority bound
// (bindAuthority) on a fresh context that carries nothing of whoever asked, so the create path resolves the
// ceiling, the capability rows and the pool for the service alone. cancel releases it.
func serviceLaunchRequest(si types.ServiceIdentity, sponsor string) (*http.Request, context.CancelFunc, error) {
	ctx, cancel := context.WithTimeout(context.Background(), queueStartPrepTimeout)
	r, err := http.NewRequestWithContext(bindAuthority(ctx, serviceAuthority(si, sponsor), contextChoice{}), http.MethodPost, "/api/v1/runs", http.NoBody)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	return r, cancel, nil
}
