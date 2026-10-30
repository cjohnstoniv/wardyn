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
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// The custom-resource path of workloads (0.10). A k8s runner that lists workload namespaces reports each
// WardynWorkload it sees as a `workload` event. The org finds the namespace's binding through the
// runner's own pool membership, so the runner never names the identity or the pool, then records the
// resource as the bound service identity's workload and launches it as that identity on the binding's
// pool, through the same apply core as PUT /workloads. Its status goes back through the runner that
// reported it; the org never reads a resource's status.

const (
	// maxWorkloadReportBytes bounds one workload event: a spec at the wire's cap and the header around it.
	maxWorkloadReportBytes = runnerwire.MaxWorkloadSpecBytes + 64<<10
	// workloadStatusEvery is the status sweep's cadence; workloadStatusTimeout bounds one push.
	workloadStatusEvery   = 30 * time.Second
	workloadStatusTimeout = 5 * time.Second
)

// The `via` of a workload.apply or workload.delete row a runner's report caused.
const (
	workloadViaK8s     = "k8s"      // the resource was applied or deleted
	workloadViaK8sSync = "k8s_sync" // the runner's full list of the namespace no longer names it
)

const workloadBindingInactiveMsg = "The namespace binding no longer carries authority: its service identity is suspended or retired, " +
	"its sponsor changed since the binding was made, or the sponsor's access was revoked. Bind the namespace again."

const workloadBindingGoneMsg = "No namespace binding binds this workload's namespace, on its pool, to its service identity any more. " +
	"Bind the namespace again."

// WorkloadFromRunner is the `workload` event. It runs on the runner's read loop, so it only decodes, finds
// the binding and records; a launch, a run's end and every status push go on in the background, because
// each of them may call this same runner. A report outside the wire's shape is refused for good; a
// resource that is not a workload Wardyn accepts is acknowledged and answered by its status.
func (s *Server) WorkloadFromRunner(ctx context.Context, runnerID uuid.UUID, ev runnerwire.Event) error {
	ws, ok := s.cfg.Store.(store.WorkloadStore)
	if !ok {
		return errRunnerIngestUnavailable
	}
	var rep runnerwire.WorkloadReport
	if ev.RunID != nil || len(ev.Data) > maxWorkloadReportBytes || decodeStrictJSON(ev.Data, &rep) != nil || rep.Validate() != nil {
		return errReportShape
	}
	b, err := ws.BindingForRunner(ctx, runnerID, rep.Namespace)
	if errors.Is(err, store.ErrNotFound) {
		return s.workloadUnbound(ctx, ws, runnerID, rep)
	}
	if err != nil {
		return err
	}
	si, err := s.serviceRecord(ctx, b.Service)
	if errors.Is(err, store.ErrNotFound) {
		// Retiring an identity deletes its bindings; this report met one on its way out.
		if rep.Op == runnerwire.WorkloadOpApply {
			s.answerWorkloadReport(runnerID, rep, runnerwire.WorkloadPhaseBindingInactive, reasonWorkloadBindingInactive, workloadBindingInactiveMsg)
		}
		return nil
	}
	if err != nil {
		return err
	}
	// The rows this report writes are the bound service's, as its token's would be, with its sponsor.
	sr, cancel, err := serviceLaunchRequest(si, b.Sponsor)
	if err != nil {
		return err
	}
	defer cancel()
	switch rep.Op {
	case runnerwire.WorkloadOpApply:
		return s.applyWorkloadReport(ctx, ws, sr, runnerID, b, si, rep)
	case runnerwire.WorkloadOpDelete:
		v, err := ws.GetWorkload(ctx, types.ServicePrincipalPrefix+b.Service, rep.Name)
		// A delete of a resource the name has since been recreated as leaves the newer one's workload.
		if errors.Is(err, store.ErrNotFound) || (err == nil && (!boundK8sWorkload(v.Workload, b, rep.Namespace) || (v.K8sUID != "" && v.K8sUID != rep.UID))) {
			return nil // nothing this resource recorded
		}
		if err != nil {
			return err
		}
		return s.dropK8sWorkload(ctx, ws, sr, runnerID, b, si, v.Workload, rep.UID, workloadViaK8s)
	default:
		return s.syncWorkloadReport(ctx, ws, sr, runnerID, b, si, rep)
	}
}

// decodeStrictJSON decodes data into v, refusing an unknown field and anything after the one value.
func decodeStrictJSON(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("trailing data after the value")
	}
	return nil
}

// boundK8sWorkload reports whether wl is the workload a resource of namespace recorded under binding b.
func boundK8sWorkload(wl store.Workload, b store.WorkloadBinding, namespace string) bool {
	return wl.Source == store.WorkloadSourceK8s && wl.SourcePoolID != nil && *wl.SourcePoolID == b.PoolID && wl.SourceNamespace == namespace
}

// workloadUnbound answers a report from a namespace no pool of this runner binds. A namespace bound on
// another pool belongs to that pool's runners, so this runner's resource gets no answer here; an applied
// resource in a namespace nothing binds reads NotBound.
func (s *Server) workloadUnbound(ctx context.Context, ws store.WorkloadStore, runnerID uuid.UUID, rep runnerwire.WorkloadReport) error {
	if rep.Op != runnerwire.WorkloadOpApply {
		return nil
	}
	bindings, err := ws.ListWorkloadBindings(ctx, store.Page{})
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(bindings, func(b store.WorkloadBinding) bool { return b.Namespace == rep.Namespace }) {
		s.answerWorkloadReport(runnerID, rep, runnerwire.WorkloadPhaseNotBound, "",
			"No Wardyn workload binding names this namespace on this runner's pools. A security admin binds it to a service identity.")
	}
	return nil
}

// applyWorkloadReport records an applied resource as the bound service's workload and, when its spec is
// ahead of its run, launches it in the background. A spec Wardyn does not accept, one that names another
// pool, or one the store refuses is answered by the resource's status and changes nothing stored.
func (s *Server) applyWorkloadReport(ctx context.Context, ws store.WorkloadStore, sr *http.Request, runnerID uuid.UUID, b store.WorkloadBinding,
	si types.ServiceIdentity, rep runnerwire.WorkloadReport,
) error {
	var spec client.WorkloadCRSpec
	if err := decodeStrictJSON(rep.Spec, &spec); err != nil {
		s.answerWorkloadReport(runnerID, rep, runnerwire.WorkloadPhaseInvalid, reasonWorkloadInvalid, "The spec is not a WardynWorkload spec: "+err.Error())
		return nil
	}
	in, invalid := normalizeWorkload(client.WorkloadDocument{APIVersion: client.TemplateDocumentVersion, Kind: client.WorkloadDocumentKind,
		Name: rep.Name, Description: spec.Description, RunIn: spec.RunIn, RestartKey: spec.RestartKey, Intent: spec.Intent})
	if invalid != nil {
		msg := invalid.Error
		if len(invalid.Diagnostics) > 0 {
			d := invalid.Diagnostics[0]
			msg += fmt.Sprintf(" (%s: %s)", d.Path, d.Message)
		}
		s.answerWorkloadReport(runnerID, rep, runnerwire.WorkloadPhaseInvalid, reasonWorkloadInvalid, msg)
		return nil
	}
	// The binding chooses the pool. A resource may name it, but only as the binding's.
	req, err := client.RunDocumentRequest(spec.Intent)
	if err != nil {
		s.answerWorkloadReport(runnerID, rep, runnerwire.WorkloadPhaseInvalid, reasonWorkloadInvalid, "The spec's intent is not a run request: "+err.Error())
		return nil
	}
	if named, perr := uuid.Parse(req.RunnerPoolID); req.RunnerPoolID != "" && (perr != nil || named != b.PoolID) {
		s.answerWorkloadReport(runnerID, rep, runnerwire.WorkloadPhaseRefused, reasonWorkloadPoolMismatch,
			"The spec names a runner pool other than the one this namespace is bound on. Remove runner_pool_id, or name the bound pool.")
		return nil
	}
	owner, generation := types.ServicePrincipalPrefix+b.Service, rep.Generation
	in.Owner, in.Source, in.By = owner, store.WorkloadSourceK8s, owner
	in.SourcePoolID, in.SourceNamespace, in.SourceRunnerID, in.K8sGeneration, in.K8sUID = &b.PoolID, rep.Namespace, &runnerID, &generation, rep.UID
	wl, write, err := ws.PutWorkload(ctx, in)
	switch {
	case errors.Is(err, store.ErrWorkloadSourceConflict):
		s.answerWorkloadReport(runnerID, rep, runnerwire.WorkloadPhaseRefused, reasonWorkloadSourceConflict,
			"The service identity already holds a workload of this name, applied through the API or from another namespace or pool.")
		return nil
	case errors.Is(err, store.ErrWorkloadCapReached):
		s.answerWorkloadReport(runnerID, rep, runnerwire.WorkloadPhaseRefused, reasonWorkloadCapReached,
			fmt.Sprintf("The service identity holds %d workloads, the most allowed.", store.MaxWorkloadsPerOwner))
		return nil
	case err != nil:
		return err
	}
	if write != store.WorkloadUnchanged {
		actorType, actor := actorFromRequest(sr)
		s.recordAudit(sr.Context(), s.auditEvent(nil, actorType, actor, "workload.apply", workloadTarget(wl), "success", mustJSON(map[string]any{
			"name": wl.Name, "owner": wl.Owner, "source": wl.Source, "generation": wl.Generation, "digest": wl.Digest, "outcome": write,
			"via": workloadViaK8s, "runner_id": runnerID, "pool_id": b.PoolID, "namespace": rep.Namespace, "uid": rep.UID,
		})))
	}
	if wl.CurrentGeneration < wl.Generation {
		s.goBackground(func() { s.launchBoundWorkload(ws, b, true, si, wl) })
	}
	return nil
}

// launchBoundWorkload launches a recorded resource's owed generation as the bound service, once the
// binding still carries authority: the identity usable, its sponsor the one the binding was made under,
// and the sponsor's access not revoked since (a sign-out that keeps tokens does not count). These are the
// rules that end its tokens. A binding that no longer carries it is recorded as the launch's refusal, and the current run keeps running. Either way the
// resource's status is pushed at once. found is false when the binding was deleted since (launchUnderBinding).
func (s *Server) launchBoundWorkload(ws store.WorkloadStore, b store.WorkloadBinding, found bool, si types.ServiceIdentity, wl store.Workload) {
	sponsor := si.Sponsor
	if found {
		sponsor = b.Sponsor
	}
	sr, cancel, err := serviceLaunchRequest(si, sponsor)
	if err != nil {
		slog.Error("wardynd: launch a custom resource's workload", slog.String("workload", workloadTarget(wl)), slog.Any("err", err))
		return
	}
	defer cancel()
	ctx := sr.Context()
	// Unknown standing launches nothing; the runner's next report of the resource tries again.
	if _, err := s.launchUnderBinding(sr, ws, b, found, si, wl); err != nil {
		slog.ErrorContext(ctx, "wardynd: launch a custom resource's workload", slog.String("workload", workloadTarget(wl)), slog.Any("err", err))
	}
	if v, err := ws.GetWorkload(ctx, wl.Owner, wl.Name); err == nil {
		s.pushWorkloadView(ctx, ws, v)
	}
}

// launchUnderBinding launches wl's owed generation on sr, a request acting as the bound service si, while b,
// its namespace binding (found: it still exists), carries authority. A binding deleted since, or one that
// carries none, records the launch refused workload_binding_inactive, and the current run keeps running.
// A standing that cannot be read launches nothing and is the error.
func (s *Server) launchUnderBinding(sr *http.Request, ws store.WorkloadStore, b store.WorkloadBinding, found bool, si types.ServiceIdentity,
	wl store.Workload,
) (client.WorkloadLaunch, error) {
	live := false
	if found {
		var err error
		if live, err = s.bindingLive(sr.Context(), b, si); err != nil {
			return client.WorkloadLaunch{}, err
		}
	}
	if live {
		return s.launchWorkload(sr, ws, wl)
	}
	msg := workloadBindingInactiveMsg
	if !found {
		msg = workloadBindingGoneMsg
	}
	claimed, ok, err := ws.ClaimWorkloadLaunch(sr.Context(), wl.ID)
	if err != nil || !ok {
		return client.WorkloadLaunch{State: client.WorkloadNoLaunch}, err
	}
	return s.refuseWorkloadLaunch(sr, ws, claimed, reasonWorkloadBindingInactive, msg)
}

// workloadBindingOf finds, in bindings, the one wl, a custom resource's workload, launches under now: the
// binding of its namespace, on its pool, to its owner, as the runner's next report of the resource would
// find it. None when that binding was deleted since or the pool row is gone.
func workloadBindingOf(bindings []store.WorkloadBinding, wl store.Workload) (store.WorkloadBinding, bool) {
	i := slices.IndexFunc(bindings, func(b store.WorkloadBinding) bool {
		return wl.SourcePoolID != nil && b.PoolID == *wl.SourcePoolID && b.Namespace == wl.SourceNamespace &&
			types.ServicePrincipalPrefix+b.Service == wl.Owner
	})
	if i < 0 {
		return store.WorkloadBinding{}, false
	}
	return bindings[i], true
}

// syncWorkloadReport deletes the workloads the namespace's resources recorded on the binding's pool that
// the runner's full list of the namespace no longer names: resources deleted while no report could say so.
func (s *Server) syncWorkloadReport(ctx context.Context, ws store.WorkloadStore, sr *http.Request, runnerID uuid.UUID, b store.WorkloadBinding,
	si types.ServiceIdentity, rep runnerwire.WorkloadReport,
) error {
	if live, err := s.bindingLive(ctx, b, si); err != nil || !live {
		skipDeleteOfInactiveBinding(ctx, runnerID, b, rep.Namespace, err)
		return err
	}
	bound, err := ws.ListBoundK8sWorkloads(ctx, b.PoolID, rep.Namespace)
	if err != nil {
		return err
	}
	for _, wl := range bound {
		if slices.Contains(rep.Names, wl.Name) {
			continue
		}
		if err := s.dropK8sWorkload(ctx, ws, sr, runnerID, b, si, wl, "", workloadViaK8sSync); err != nil {
			return err
		}
	}
	return nil
}

// bindingLive reports whether b still carries authority for the identity si: the rules that end its tokens
// (queueOwnerLive), as of the binding's making.
func (s *Server) bindingLive(ctx context.Context, b store.WorkloadBinding, si types.ServiceIdentity) (bool, error) {
	return s.queueOwnerLive(ctx, serviceAuthority(si, b.Sponsor), b.CreatedAt)
}

// skipDeleteOfInactiveBinding logs a delete a binding without authority did not make (err is the standing
// check's own failure, when it failed).
func skipDeleteOfInactiveBinding(ctx context.Context, runnerID uuid.UUID, b store.WorkloadBinding, namespace string, err error) {
	slog.WarnContext(ctx, "wardynd: a binding without authority deletes no workload", slog.String("runner_id", runnerID.String()),
		slog.String("pool_id", b.PoolID.String()), slog.String("namespace", namespace), slog.String("service", b.Service), slog.Any("err", err))
}

// dropK8sWorkload deletes wl, the workload of a resource that is gone, and ends its run in the background
// as the bound service (run.kill reason workload_deleted). uid is the resource's, when the report named it.
// The delete holds only while the row is still the one wl was read as: an apply of a recreated resource
// that moved it to its own uid in between keeps the row and its run, and nothing is audited.
func (s *Server) dropK8sWorkload(ctx context.Context, ws store.WorkloadStore, sr *http.Request, runnerID uuid.UUID, b store.WorkloadBinding,
	si types.ServiceIdentity, wl store.Workload, uid, via string,
) error {
	// A binding that lost its authority deletes nothing: the rows and the run stay as they are, as an apply
	// launches nothing under it (launchBoundWorkload).
	if live, err := s.bindingLive(ctx, b, si); err != nil || !live {
		skipDeleteOfInactiveBinding(ctx, runnerID, b, wl.SourceNamespace, err)
		return err
	}
	removed, err := ws.DeleteK8sWorkload(ctx, wl)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	data := map[string]any{"name": removed.Name, "owner": removed.Owner, "source": removed.Source, "via": via,
		"runner_id": runnerID, "pool_id": b.PoolID, "namespace": removed.SourceNamespace}
	if uid != "" {
		data["uid"] = uid
	}
	if id := removed.CurrentRunID; id != nil {
		data["ended_run_id"] = *id
		s.goBackground(func() {
			er, cancel, err := serviceLaunchRequest(si, b.Sponsor)
			if err != nil {
				slog.Error("wardynd: end a deleted custom resource's run", slog.String("run_id", id.String()), slog.Any("err", err))
				return
			}
			defer cancel()
			s.endWorkloadRun(er, *id, removed, workloadKillDeleted)
		})
	}
	actorType, actor := actorFromRequest(sr)
	s.recordAudit(sr.Context(), s.auditEvent(removed.CurrentRunID, actorType, actor, "workload.delete", workloadTarget(removed), "success", mustJSON(data)))
	return nil
}

// answerWorkloadReport sends, in the background, the status of a resource whose report recorded nothing:
// no stored workload carries this answer, so the status sweep never repeats it, and the runner's next
// report of the resource earns it again.
func (s *Server) answerWorkloadReport(runnerID uuid.UUID, rep runnerwire.WorkloadReport, phase, reason, msg string) {
	a := runnerwire.WorkloadStatusArgs{Namespace: rep.Namespace, Name: rep.Name, UID: rep.UID, ObservedGeneration: rep.Generation, Phase: phase,
		Reason: reason, Message: clipUTF8(msg, runnerwire.MaxWorkloadMessage)}
	s.goBackground(func() {
		ctx, cancel := context.WithTimeout(context.Background(), workloadStatusTimeout)
		defer cancel()
		if err := s.runnerEndpoint(runnerID).sub.WorkloadStatus(ctx, a); err != nil {
			slog.WarnContext(ctx, "wardynd: a custom resource's status was not sent", slog.String("runner_id", runnerID.String()),
				slog.String("resource", rep.Namespace+"/"+rep.Name), slog.String("phase", phase), slog.Any("err", err))
		}
	})
}

// workloadStatusOf is the status a resource's stored workload reads as: the refusal of its current spec's
// launch when there is one, else its run's state with the run's service health.
func workloadStatusOf(v store.WorkloadView) runnerwire.WorkloadStatusArgs {
	a := runnerwire.WorkloadStatusArgs{Namespace: v.SourceNamespace, Name: v.Name, UID: v.K8sUID}
	if v.K8sGeneration != nil {
		a.ObservedGeneration = *v.K8sGeneration
	}
	if v.CurrentRunID != nil {
		a.RunID = v.CurrentRunID.String()
	}
	if f := v.LastRefusal; f != nil && f.Generation == v.Generation && v.CurrentGeneration < v.Generation {
		a.Phase = runnerwire.WorkloadPhaseRefused
		if f.Reason == reasonWorkloadBindingInactive {
			a.Phase = runnerwire.WorkloadPhaseBindingInactive
		}
		a.Reason, a.Message = clipUTF8(f.Reason, runnerwire.MaxRefusalReason), clipUTF8(f.Message, runnerwire.MaxWorkloadMessage)
		return a
	}
	switch {
	case v.CurrentGeneration < v.Generation || v.RunState == "":
		a.Phase = runnerwire.WorkloadPhasePending // the current spec has no run yet
	case v.RunState == types.RunQueued:
		a.Phase = runnerwire.WorkloadPhaseQueued
	case v.RunState == types.RunPending || v.RunState == types.RunStarting:
		a.Phase = runnerwire.WorkloadPhasePending
	case v.RunState == types.RunCompleted:
		a.Phase = runnerwire.WorkloadPhaseCompleted
	case v.RunState.IsTerminal():
		a.Phase = runnerwire.WorkloadPhaseEnded
	case v.ServiceHealth == string(types.ServiceHealthUnhealthy):
		a.Phase = runnerwire.WorkloadPhaseUnhealthy
	default:
		a.Phase = runnerwire.WorkloadPhaseRunning
	}
	return a
}

// workloadStatusHash is what workloads.status_pushed holds for a status sent.
func workloadStatusHash(a runnerwire.WorkloadStatusArgs) string {
	b, _ := json.Marshal(a) //nolint:errcheck // a struct of strings and an int always marshals
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// clipUTF8 cuts s to at most n bytes without splitting a character.
func clipUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}

// pushWorkloadView sends v's status to the runner that last reported its resource and records the hash
// of what was sent. A push that fails records nothing, so the next sweep sends it again. A row with no
// resource uid yet (recorded before the column) waits for its resource's next report to name it.
func (s *Server) pushWorkloadView(ctx context.Context, ws store.WorkloadStore, v store.WorkloadView) {
	if v.SourceRunnerID == nil || v.K8sUID == "" {
		return
	}
	// The sweep and a launch may push one workload at once; one at a time, so the hash stored last is
	// the status written last.
	mu, _ := workloadPushMu.LoadOrStore(v.ID, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()
	a := workloadStatusOf(v)
	pctx, cancel := context.WithTimeout(ctx, workloadStatusTimeout)
	err := s.runnerEndpoint(*v.SourceRunnerID).sub.WorkloadStatus(pctx, a)
	cancel()
	if err == nil {
		err = ws.SetWorkloadStatusPushed(ctx, v.ID, workloadStatusHash(a))
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		slog.WarnContext(ctx, "wardynd: a custom resource's status was not sent", slog.String("workload", workloadTarget(v.Workload)),
			slog.String("runner_id", v.SourceRunnerID.String()), slog.Any("err", err))
	}
}

// workloadPushMu holds a *sync.Mutex per workload id (pushWorkloadView), one per custom-resource
// workload this process has pushed.
var workloadPushMu sync.Map

// pushWorkloadStatuses pushes the status of every custom-resource workload for which push says so.
func (s *Server) pushWorkloadStatuses(ctx context.Context, push func(store.WorkloadView) bool) error {
	ws, ok := s.cfg.Store.(store.WorkloadStore)
	if !ok {
		return nil
	}
	views, err := ws.ListK8sWorkloadStatuses(ctx)
	if err != nil {
		return err
	}
	for _, v := range views {
		if ctx.Err() != nil {
			return ctx.Err() // the rest wait for the next pass
		}
		if v.SourceRunnerID != nil && push(v) {
			s.pushWorkloadView(ctx, ws, v)
		}
	}
	return nil
}

// sweepWorkloadStatuses is the status sweep: each workload whose status moved since it was last sent,
// pushed by the replica that holds its runner's session.
func (s *Server) sweepWorkloadStatuses(ctx context.Context) error {
	return s.pushWorkloadStatuses(ctx, func(v store.WorkloadView) bool {
		return s.runnerOnline(*v.SourceRunnerID) && workloadStatusHash(workloadStatusOf(v)) != v.StatusPushed
	})
}

// pushRunnerWorkloadStatuses re-pushes every status of runnerID's resources, once its session is READY:
// a status sent while it was away, or one its last session never wrote, is written now.
func (s *Server) pushRunnerWorkloadStatuses(ctx context.Context, runnerID uuid.UUID) {
	if err := s.pushWorkloadStatuses(ctx, func(v store.WorkloadView) bool { return *v.SourceRunnerID == runnerID }); err != nil {
		slog.WarnContext(ctx, "wardynd: a READY runner's workload statuses were not pushed", slog.String("runner_id", runnerID.String()), slog.Any("err", err))
	}
}

// workloadLaunchRetries bounds the lapsed launches one pass of sweepWorkloadLaunches retries.
const workloadLaunchRetries = 50

// sweepWorkloadLaunches retries, in the background, the launch of each custom resource's workload whose
// launch claim lapsed, rather than leave it to the runner's next full report of the resource. The retry is
// that report's own launch (launchBoundWorkload): as the bound service, under the namespace binding that
// holds now, so a binding deleted since, or one without authority, records the launch refused. A workload
// whose service identity is gone is left. Each retry takes a fresh claim, so one workload is retried at
// most once a store.WorkloadLaunchClaim, and a refused launch waits for its resource's next report.
func (s *Server) sweepWorkloadLaunches(ctx context.Context) error {
	ws, ok := s.cfg.Store.(store.WorkloadStore)
	if !ok {
		return nil
	}
	lapsed, err := ws.ListLapsedK8sWorkloadLaunches(ctx, workloadLaunchRetries)
	if err != nil || len(lapsed) == 0 {
		return err
	}
	bindings, err := ws.ListWorkloadBindings(ctx, store.Page{})
	if err != nil {
		return err
	}
	for _, wl := range lapsed {
		name, ok := strings.CutPrefix(wl.Owner, types.ServicePrincipalPrefix)
		if !ok {
			continue
		}
		si, err := s.serviceRecord(ctx, name)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		b, found := workloadBindingOf(bindings, wl)
		s.goBackground(func() { s.launchBoundWorkload(ws, b, found, si, wl) })
	}
	return nil
}

// startWorkloadStatusSweeper starts the status sweep, and with it the sweep of workload runs still owed
// their end (sweepWorkloadRunEnds) and of lapsed launches (sweepWorkloadLaunches), for the life of the
// daemon, on a store that keeps workloads.
func (s *Server) startWorkloadStatusSweeper() {
	if _, ok := s.cfg.Store.(store.WorkloadStore); ok && s.watcherBaseCtx() != nil {
		s.goBackground(func() { s.workloadStatusSweeper(s.watcherBaseCtx(), workloadStatusEvery) })
	}
}

// workloadStatusSweeper runs the status sweep every `every` until ctx ends. A pass is bounded by the
// cadence, and a panicking pass is contained so the next one runs.
func (s *Server) workloadStatusSweeper(ctx context.Context, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			func() {
				defer func() {
					if r := recover(); r != nil {
						slog.ErrorContext(ctx, "wardynd: PANIC in workload status sweep (contained; the sweep continues next tick)", slog.Any("panic", r))
					}
				}()
				pctx, cancel := context.WithTimeout(ctx, every)
				defer cancel()
				if err := s.sweepWorkloadStatuses(pctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
					slog.WarnContext(ctx, "wardynd: workload status sweep", slog.Any("err", err))
				}
				if err := s.sweepWorkloadRunEnds(pctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
					slog.WarnContext(ctx, "wardynd: workload run end sweep", slog.Any("err", err))
				}
				if err := s.sweepWorkloadLaunches(pctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
					slog.WarnContext(ctx, "wardynd: workload launch sweep", slog.Any("err", err))
				}
			}()
		}
	}
}
