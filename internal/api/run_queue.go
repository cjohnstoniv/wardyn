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
	"log/slog"
	"maps"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The pool queue (docs/design/0.9/pool-queue.md): a launch into a pool at its concurrent-run cap waits
// instead of failing. This file is the enqueue half: the QUEUED row and what the start will need, held
// with nothing a running run holds. run_queue_promote.go moves waiting runs to PENDING and
// run_queue_start.go starts them.

const (
	// queueMaxPerOwner is how many runs one person may have waiting for one pool.
	queueMaxPerOwner = 100
	// queueMaxWaitSec bounds max_wait_sec: seven days.
	queueMaxWaitSec = 7 * 24 * 60 * 60
	// queueBackgroundWaitSec and queueInteractiveWaitSec are the waits an untouched run gets.
	queueBackgroundWaitSec  = 24 * 60 * 60
	queueInteractiveWaitSec = 60 * 60
	// queueEnqueueBound bounds the promotion attempt a create makes after its commit; on the bound the
	// transaction rolls back, the run stays QUEUED and the leader sweep retries.
	queueEnqueueBound = 2 * time.Second
	// queueSweepBound bounds one pool's promotion in a sweep tick or a kick; queueScanLimit is how many
	// waiting runs a pass reads at a time. A pass promotes at most as many runs as the pool has free slots.
	queueSweepBound = 5 * time.Second
	queueScanLimit  = 50
)

// queueStore is the PG-only surface the pool queue needs. The api layer type-asserts it, as it does for
// the watcher lease: a store without it (a test double) never queues.
type queueStore interface {
	CreateQueuedRun(ctx context.Context, r types.AgentRun, e store.RunQueueEntry, maxPerOwner int) (types.AgentRun, error)
	PromoteRunQueue(ctx context.Context, pass store.PromotePass, admit store.QueueAdmit) ([]store.PromotedRun, []store.QueueHeld, error)
	ExpiredQueueEntries(ctx context.Context, now time.Time, limit int) ([]store.RunQueueEntry, error)
	QueuePoolIDs(ctx context.Context) ([]uuid.UUID, error)
	QueueOwnerEntries(ctx context.Context, owner string) ([]store.RunQueueEntry, error)
	QueueEntriesOfRevokedRunners(ctx context.Context, limit int) ([]store.RunQueueEntry, error)
	GetRunQueueEntry(ctx context.Context, id uuid.UUID) (store.RunQueueEntry, error)
	RunQueuePosition(ctx context.Context, id uuid.UUID) (int, error)
	QueueDepth(ctx context.Context) (map[uuid.UUID]int, error)
	PoolSlotsUsed(ctx context.Context) (map[uuid.UUID]int, error)
	StalePromotedRuns(ctx context.Context, staleAfter time.Duration) ([]types.AgentRun, error)
	StartQueuedRun(ctx context.Context, id uuid.UUID, st store.QueueStart) (types.AgentRun, bool, error)
	SetQueueWait(ctx context.Context, id uuid.UUID, detail string) error
	SetPromotedStatusDetail(ctx context.Context, id uuid.UUID, detail string) error
	ExitQueuedRun(ctx context.Context, id uuid.UUID, to types.RunState, detail, hint string) (bool, error)
}

var _ queueStore = store.PG{}

func (s *Server) queueStore() (queueStore, bool) {
	qs, ok := s.cfg.Store.(queueStore)
	return qs, ok
}

// queuedLaunch is run_queue.launch: the request as the fold first saw it and the few facts the start
// compares or promotion needs. Ids, names and references only; no secret value is ever in a request.
type queuedLaunch struct {
	// Request is the decoded, validated request before the fold changed it (a workspace's base image or
	// repo seeded onto it, say), so the start folds it afresh as the owner.
	Request json.RawMessage `json:"request"`
	// ProfileID is the governance profile the owner resolved to at launch; nil for none.
	ProfileID *uuid.UUID `json:"profile_id,omitempty"`
	// ProviderID is the model provider the launch chose; the start re-resolves this one, never another.
	ProviderID string `json:"provider_id,omitempty"`
	// ConfinementDefault says who chose the class of a launch that named none (the organisation or the
	// server); the start replays that class as the run's own, so a later change of the organisation's
	// default never ends a run that already waited under one.
	ConfinementDefault runnerpool.BarrierDefaultSource `json:"confinement_default,omitempty"`
	// Repos are the run's repository locators, for the owner's provider door at the start.
	Repos []string `json:"repos,omitempty"`
	// Resources and LifetimeCapSec are what promotion's quota fit check asks about.
	Resources      *types.ResourceLimits `json:"resources,omitempty"`
	LifetimeCapSec int                   `json:"lifetime_cap_sec,omitempty"`
	// MaxWaitSec is the wait the run was given, after the cut, for the audit row.
	MaxWaitSec int `json:"max_wait_sec"`
	// Unbound is the size the run gets before a runner's per-run limit bounds it (runPlacement.unbound):
	// the promotion sizes the run for each runner by it. Absent on a launch queued before it existed,
	// which the promotion holds to the size it joined with.
	Unbound *types.RunnerAmounts `json:"unbound,omitempty"`
	// Barrier is the class the run was queued at and Drives whether it has drives: the queue's verdict
	// holds each runner to them as the start's fold does, so it never offers a runner the start cannot
	// keep. A launch queued before they existed is held to neither.
	Barrier types.ConfinementClass `json:"barrier,omitempty"`
	Drives  bool                   `json:"drives,omitempty"`
	// NoLocal says the launch waits on the pool's remote-hosted runners and cannot run on the person's
	// own (runPlacement.noLocal): the verdict and the start then keep it off them.
	NoLocal bool `json:"no_local,omitempty"`
}

// queueWaitSec is the wait a launch gets: max_wait_sec when given, cut at seven days with a warning,
// else 24 hours for a Background run and 1 hour for an Interactive one.
func queueWaitSec(req createRunRequest, interactive bool) (sec int, warning string) {
	asked := 0
	if req.MaxWaitSec != nil {
		asked = *req.MaxWaitSec
	}
	switch {
	case asked > queueMaxWaitSec:
		return queueMaxWaitSec, fmt.Sprintf("max_wait_sec %d is past the longest wait of %d seconds (7 days), so this run waits at most that long", asked, queueMaxWaitSec)
	case asked > 0:
		return asked, ""
	case interactive:
		return queueInteractiveWaitSec, ""
	}
	return queueBackgroundWaitSec, ""
}

// queuePolicyRef is run_queue.policy_ref: what the run's policy was when it was reviewed, so a start can
// tell it changed (Q7-A). A stored policy is its id and the SHA-256 of its spec; a launch preset is its
// name, version and the SHA-256 of its request. An inline or default policy is the request's own, so
// nothing can change under it. A policy or preset that has gone is recorded as gone.
func (s *Server) queuePolicyRef(ctx context.Context, req createRunRequest) (json.RawMessage, error) {
	ref := map[string]any{}
	if req.PolicyID != nil {
		ref["policy_id"] = req.PolicyID.String()
		p, err := s.cfg.Store.GetPolicy(ctx, *req.PolicyID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			ref["gone"] = true
		case err != nil:
			return nil, fmt.Errorf("read the stored policy: %w", err)
		default:
			ref["spec_sha256"] = queueSHA256JSON(p.Spec)
		}
	}
	if req.Preset != "" {
		ref["preset"], ref["preset_version"] = req.Preset, req.PresetVersion
		p, err := s.cfg.Store.GetLaunchPreset(ctx, req.Preset)
		switch {
		case errors.Is(err, store.ErrNotFound):
			ref["preset_gone"] = true
		case err != nil:
			return nil, fmt.Errorf("read the launch preset: %w", err)
		default:
			ref["request_sha256"] = queueSHA256(p.Request)
		}
	}
	return json.Marshal(ref)
}

func queueSHA256JSON(v any) string {
	raw, _ := json.Marshal(v) //nolint:errcheck // a policy spec always marshals
	return queueSHA256(raw)
}

func queueSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// queuedCreate is a create that waits: the door's facts, handed to answer in place of the mint.
// answer is a method on this type, not on the Server, so the structural guard on the create/preflight
// parity (preflight_launch_gate_parity_test.go) sees the handler's gates and not this branch.
type queuedCreate struct {
	run           types.AgentRun
	req           createRunRequest
	original      json.RawMessage // the request before the fold, as decoded and validated
	f             runFold
	reqCC         types.ConfinementClass
	createdByType types.ActorType
	taskWarning   string
	workspacePath string
	intent        launchIntent // run.LaunchIntent carries its document
}

// answer inserts the QUEUED run and its queue row in one transaction, audits it, makes one bounded
// promotion attempt, and answers 201: QUEUED with the wait sentence, or PENDING if a slot was free.
func (q queuedCreate) answer(s *Server, w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	qs, ok := s.queueStore()
	if !ok {
		writeServerError(w, r, "queue run", errors.New("this store cannot hold a queue"))
		return
	}
	pool := q.f.place.pool
	wait, waitWarn := queueWaitSec(q.req, runTypeForEnd(q.run.Experience) == types.RunnerPoolRunInteractive || (q.run.Experience == "" && q.run.Interactive))
	entry, err := q.entry(s, ctx, wait)
	if err != nil {
		writeServerError(w, r, "record run queue facts", err)
		return
	}
	q.run.State = types.RunQueued
	// The snapshots revive, extend and the run.create row read commit with the queue row: a creator
	// that dies after the commit leaves no promotable run without its authority.
	entry.Components, entry.Harnesses = q.f.comps.snapshotRows(), q.f.harnesses
	created, err := s.createQueuedRun(ctx, q.run, entry)
	switch {
	case errors.Is(err, store.ErrRunQueueFull):
		writePoolRefusal(w, runnerpool.Refusal{Reason: runnerpool.ReasonQueueFull, Name: pool.Name, Count: queueMaxPerOwner})
		return
	case errors.Is(err, store.ErrRunnerPoolStale):
		writePoolRefusal(w, runnerpool.Refusal{Reason: runnerpool.ReasonStale, Name: pool.Name})
		return
	case err != nil:
		writeServerError(w, r, "create run", err)
		return
	}
	s.recordPolicyApplyAudits(ctx, q.f.source)
	if rows := entry.Components; len(rows) > 0 {
		err := errors.New("api: the store cannot record run components")
		if st, ok := s.cfg.Store.(store.ComponentStore); ok {
			err = fenceErasedComponents(ctx, st, created.ID, rows)
		}
		if err != nil {
			s.abortQueuedRun(ctx, created.ID, createRunComponentsAbortHint)
			writeServerError(w, r, "record run components", err)
			return
		}
	}
	s.recordCreateFolds(ctx, created.ID, q.f.reqEvents)
	position, err := qs.RunQueuePosition(ctx, created.ID)
	if err != nil {
		position = -1 // the run left the queue already: a promoter beat the answer
	}
	s.auditQueuedCreate(ctx, q, created, entry, wait, position)
	capMax := 0
	if row := q.f.place.row; row.Limits != nil && row.Limits.MaxConcurrentRuns != nil {
		capMax = *row.Limits.MaxConcurrentRuns
	}
	if err := qs.SetQueueWait(ctx, created.ID, statusDetailQueuePrefix+string(q.waitReason())); err != nil {
		slog.WarnContext(ctx, "wardynd: record queue wait", slog.String("run_id", created.ID.String()), slog.Any("err", err))
	}
	// One bounded promotion attempt for this pool: a free slot starts the run now. On the bound it rolls
	// back, the run stays QUEUED, and the sweep retries.
	_ = s.promoteQueue(ctx, *created.RunnerPoolID, queueEnqueueBound) // logged there; the sweep retries and reports it
	warnings := q.warnings(s, r, created, waitWarn)
	if got, gerr := s.cfg.Store.GetRun(ctx, created.ID); gerr == nil {
		created = got
	}
	if created.State == types.RunQueued {
		sentence := runnerpool.WaitAtCapacityMsg(pool.Name, capMax)
		if q.f.place.waitsForRoom {
			sentence = runnerpool.WaitRunnersAtCapacityMsg(pool.Name)
		}
		warnings = append(warnings, sentence)
		runs := []types.AgentRun{created}
		s.projectStatusDetailFor(r, runs) // the 201 carries the same wait reason and place a GET does
		created = runs[0]
	}
	w.Header().Set("Location", s.cfg.BasePath+"/api/v1/runs/"+created.ID.String())
	writeJSON(w, http.StatusCreated, createRunResponse{AgentRun: created, Warnings: warnings, ResourcesApplied: &q.f.place.applied})
}

// waitReason is why the launch waits: the pool is at its own cap, or (a pool with none) every runner
// that could take the run is at one of its limits.
func (q queuedCreate) waitReason() runnerpool.WaitReason {
	if q.f.place.waitsForRoom {
		return runnerpool.WaitRunnersAtCapacity
	}
	return runnerpool.WaitAtCapacity
}

// entry is the run_queue row a launch is stored with.
func (q queuedCreate) entry(s *Server, ctx context.Context, waitSec int) (store.RunQueueEntry, error) {
	ref, err := s.queuePolicyRef(ctx, q.req)
	if err != nil {
		return store.RunQueueEntry{}, err
	}
	auth, err := json.Marshal(captureQueueAuthority(ctx))
	if err != nil {
		return store.RunQueueEntry{}, err
	}
	launch := queuedLaunch{
		Request: q.original, ProviderID: q.f.mpChoice.provider.ID, MaxWaitSec: waitSec, ConfinementDefault: q.f.confinementDefault,
		Repos: runRepos(types.AgentRun{Repo: q.req.Repo}, nil), LifetimeCapSec: q.f.end.capSec,
		Resources: s.runFitSpec(q.f.spec, q.f.mainDrive).Resources, Unbound: q.f.place.unbound,
		Barrier: q.f.enforced, Drives: len(q.f.drives) > 0, NoLocal: q.f.place.noLocal,
	}
	launch.Repos = append(launch.Repos, repoLocatorsOf(q.f.spec.WorkspaceRepos)...)
	if q.f.ceiling.Profile != nil {
		id := q.f.ceiling.Profile.ID
		launch.ProfileID = &id
	}
	raw, err := json.Marshal(launch)
	if err != nil {
		return store.RunQueueEntry{}, err
	}
	now := q.run.CreatedAt
	return store.RunQueueEntry{
		PoolID: *q.run.RunnerPoolID, Owner: q.run.CreatedBy, QueuedAt: now, DeadlineAt: now.Add(time.Duration(waitSec) * time.Second),
		PolicyRef: ref, Authority: auth, Launch: raw,
	}, nil
}

// auditQueuedCreate is the run.create row of a launch that waits: the queue facts and no jti, since
// nothing is minted.
func (s *Server) auditQueuedCreate(ctx context.Context, q queuedCreate, created types.AgentRun, e store.RunQueueEntry, waitSec, position int) {
	// The same advisory the 201 carries (warnings): a below-floor credential run says so in its audit row too.
	_, belowFloor := appendCredentialConfinementAdvisory(nil, q.f.spec, q.f.enforced, q.f.modelCred.Kind)
	createData := createRunAuditData(q.req, q.f.policyID, created.ConfinementClass, q.reqCC, "", q.f.policyWarns, q.f.autonomy, belowFloor, q.f.mpChoice)
	delete(createData, "jti") // nothing is minted while the run waits
	createData["policy_source"] = q.f.source
	stampConfinementDefault(createData, q.reqCC, q.f.confinementDefault)
	createData["end"] = q.f.end.audit(created)
	q.intent.stamp(createData, created.LaunchIntent != nil)
	stampDriveAudit(createData, q.f.drives)
	stampHarnessSet(createData, q.f.harnesses, q.f.image)
	createData["queue"] = map[string]any{
		"pool_id": e.PoolID, "position": position, "max_wait_sec": waitSec, "deadline_at": e.DeadlineAt, "capped": !q.f.place.waitsForRoom,
		"reason": string(q.waitReason()),
	}
	q.f.comps.stampRunCreate(createData)
	maps.Copy(createData, q.f.place.auditFields(created, s.runnerNameOf(ctx, created)))
	s.markGovernanceExempt(ctx, createData)
	s.stampBreakGlass(ctx, created.ID, q.createdByType, created.CreatedBy, createData)
	s.recordAudit(ctx, s.auditEvent(&created.ID, q.createdByType, created.CreatedBy, "run.create",
		created.ID.String(), "success", mustJSON(withRunUserType(ctx, created.UserType, createData))))
	s.auditRunComponents(ctx, created.ID, q.createdByType, created.CreatedBy, q.f.comps)
}

// warnings are the 201's advisories for a waiting launch: the ones create would carry that do not
// depend on a minted identity or a built image.
func (q queuedCreate) warnings(s *Server, r *http.Request, created types.AgentRun, waitWarn string) []string {
	ctx := r.Context()
	warnings := withUnpublishedImageWarning(append(append([]string(nil), q.f.policyWarns...), s.warnWorkspaceCollision(r, created.ID, q.workspacePath)...), q.req.Agent, s.cfg.AgentImages)
	for _, w := range []string{q.taskWarning, waitWarn} {
		if w != "" {
			warnings = append(warnings, w)
		}
	}
	warnings = append(warnings, q.f.autonomyWarns...)
	warnings = append(warnings, s.managedSettingsUndeliveredWarning(ctx, managedSettingsRun{req: &q.req, spec: q.f.spec, harnesses: q.f.harnesses, level: q.f.autonomy.Level,
		locked: ceilingForDispatch(q.f.ceiling, q.f.adoGrade, q.f.bedrockGrade).agentGuardrailLocks, class: q.f.enforced, place: q.f.place}, nil)...)
	warnings = append(warnings, s.repoSourceWarnings(ctx, created.ID, q.f.spec, q.req)...)
	warnings = append(warnings, q.f.fitWarnings...)
	warnings = append(warnings, q.f.end.warnings(created)...)
	warnings, _ = appendCredentialConfinementAdvisory(warnings, q.f.spec, q.f.enforced, q.f.modelCred.Kind)
	return append(warnings, noModelAccessWarning(q.req, q.f.mpChoice)...)
}

// abortQueuedRun fails a run that was queued but could not be recorded in full, so it never waits as a
// half-built row. The hint is the one line under the FAILED badge. A promoter that took the run before
// the create could finish recording it does not get to start it: the run is killed there, so the error
// the create answers is true.
func (s *Server) abortQueuedRun(ctx context.Context, runID uuid.UUID, hint string) {
	ctx = context.WithoutCancel(ctx)
	if applied, _ := s.exitQueued(ctx, runID, types.RunFailed, "", hint); applied {
		return
	}
	run, err := s.cfg.Store.GetRun(ctx, runID)
	if err == nil {
		_, _, err = s.killRunCascade(ctx, run, types.ActorSystem, "wardynd", map[string]any{"reason": queueCancelCreateAborted})
	}
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: end a promoted run whose create failed", slog.String("run_id", runID.String()), slog.Any("err", err))
	}
}

// exitQueued ends a run that is still QUEUED: the state, its queue row and the reason in one statement
// (store.ExitQueuedRun), then the counters and the live-event feed casRunState keeps for every other
// transition. false with a nil error means another exit (a kill, a promotion) won first; an error is a
// write that failed, so the run still waits.
func (s *Server) exitQueued(ctx context.Context, runID uuid.UUID, to types.RunState, detail, hint string) (bool, error) {
	qs, ok := s.queueStore()
	if !ok {
		return false, nil
	}
	applied, err := qs.ExitQueuedRun(ctx, runID, to, detail, hint)
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: end queued run", slog.String("run_id", runID.String()), slog.Any("err", err))
		return false, fmt.Errorf("end queued run %s: %w", runID, err)
	}
	if applied {
		s.metrics.runTerminal(to)
		s.runEvents.moved(runID, types.RunQueued, to)
	}
	return applied, nil
}

// killQueuedTail is the kill's tail for a run that was still QUEUED: nothing was minted, built or
// started for it, so there is no sandbox to stop and no identity, broker credential or proxy config to
// revoke, and the kill writes no revocation rows. The run.kill row says it was waiting.
func (s *Server) killQueuedTail(ctx context.Context, run types.AgentRun, killerType types.ActorType, killer string, extra map[string]any) map[string]any {
	data := map[string]any{"queued": true}
	maps.Copy(data, extra)
	s.metrics.queueWait(queueOutcomeCancelled, s.cfg.Now().Sub(run.CreatedAt))
	s.recordAudit(ctx, s.auditEvent(&run.ID, killerType, killer, "run.kill", run.ID.String(), "success", mustJSON(data)))
	return map[string]any{}
}

// auditPoolRemovalCancels records the runs a pool's removal ended while they waited, as the system.
func (s *Server) auditPoolRemovalCancels(ctx context.Context, ids []uuid.UUID) {
	for _, id := range ids {
		s.metrics.runTerminal(types.RunKilled)
		s.metrics.queueWait(queueOutcomeCancelled, 0)
		s.runEvents.moved(id, types.RunQueued, types.RunKilled)
		s.recordAudit(ctx, s.auditEvent(&id, types.ActorSystem, "wardynd", "run.kill", id.String(), "success",
			mustJSON(map[string]any{"queued": true, "reason": string(types.RunEndRunnerPoolRemoved)})))
	}
}

// The run.kill reasons of the waiting runs a person's credential revocation and a service identity's
// retirement cancel.
const (
	queueCancelSessionRevoked = "session_revoked"
	queueCancelServiceRetired = "service_identity_retired"
	// queueCancelRunnerRevoked is a run waiting for the one runner its launch named, revoked since.
	queueCancelRunnerRevoked = "runner_revoked"
	// queueCancelCreateAborted is a run a promoter took before its create had recorded it in full.
	queueCancelCreateAborted = "create_aborted"
)

// cancelQueuedRunsOf kills each run owner still has waiting in a pool's queue, as the system, with
// reason on the run.kill row, and returns how many it cancelled. The queue row holds the owner's
// request and identity snapshot, so a person's erasure and a leaver's purge end their waiting runs
// instead of leaving them to start. A run another exit takes first is not counted.
func (s *Server) cancelQueuedRunsOf(ctx context.Context, owner, reason string) (int, error) {
	qs, ok := s.queueStore()
	if !ok {
		return 0, nil
	}
	entries, err := qs.QueueOwnerEntries(ctx, owner)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		run, err := s.cfg.Store.GetRun(ctx, e.RunID)
		if err != nil {
			return n, err
		}
		applied, _, err := s.killRunCascade(ctx, run, types.ActorSystem, "wardynd", map[string]any{"reason": reason})
		if err != nil {
			return n, err
		}
		if applied {
			n++
		}
	}
	return n, nil
}
