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
	FailStalePromotedRun(ctx context.Context, id uuid.UUID, staleAfter time.Duration) (bool, error)
	StartQueuedRun(ctx context.Context, id uuid.UUID, st store.QueueStart) (types.AgentRun, bool, error)
	SetQueueWait(ctx context.Context, id uuid.UUID, detail string) error
	SetHeldQueueWaits(ctx context.Context, poolID uuid.UUID, details map[uuid.UUID]string) error
	SetPromotedStatusDetail(ctx context.Context, id uuid.UUID, detail string) error
	RequeuePromotedRun(ctx context.Context, e store.RunQueueEntry, detail string) (bool, error)
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
	// UnitID is the unit the owner's run context resolved to at launch; nil for the Default unit.
	UnitID *uuid.UUID `json:"unit_id,omitempty"`
	// RunIn and OwningUnits are the launch's context choice (contextChoice), re-stamped on the start
	// so its resolve selects the context the launch did, and sameUnit compares like with like.
	RunIn       uuid.UUID   `json:"run_in,omitzero"`
	OwningUnits []uuid.UUID `json:"owning_units,omitempty"`
	// StrictPins are the choice's strict pins (contextChoice.StrictPins); a launch queued before them
	// has none.
	StrictPins []contextPin `json:"strict_pins,omitempty"`
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
	// DriveRunner is the one runner every drive is bound to (drivesBoundRunner), nil for none: the
	// verdict then keeps that runner of the person's own, as the start's fold does.
	DriveRunner *uuid.UUID `json:"drive_runner,omitempty"`
	// MountRunner is the one runner the run's workspace folders are bound to (boundMountsRunner), nil for
	// none: the verdict then keeps that runner alone, as the start's fold does.
	MountRunner *uuid.UUID `json:"mount_runner,omitempty"`
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
// name, version and the SHA-256 of its request; a wall preset is its id and version. An inline or
// default policy is the request's own, so nothing can change under it. A policy or preset that has gone
// is recorded as gone. frozen is the wall preset a fold froze for req, nil for none: its id and version
// are recorded, never a second read that an edit in between could answer differently. With no fold, the
// preset is read as it is now, and one that r's caller can no longer read is gone (r nil: not asked).
func (s *Server) queuePolicyRef(ctx context.Context, r *http.Request, req createRunRequest, frozen *types.WallPresetOnRun) (json.RawMessage, error) {
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
	if req.WallPreset != "" {
		id, version := "", 0
		if frozen != nil {
			id, version = frozen.ID, frozen.Version
		} else {
			var err error
			if id, version, err = s.wallPresetNow(ctx, r, req.WallPreset); err != nil {
				return nil, fmt.Errorf("read the wall preset: %w", err)
			}
		}
		if id == "" {
			ref["wall_preset"], ref["wall_preset_gone"] = req.WallPreset, true
		} else {
			ref["wall_preset"], ref["wall_preset_version"] = id, version
		}
	}
	return json.Marshal(ref)
}

// wallPresetNow is the id and version of the wall preset name names as it is now: a built-in's, or a
// stored preset's. id is empty when it no longer exists or, with r, when r's caller may no longer read
// it, judged as the fold judges it (launchWallPreset).
func (s *Server) wallPresetNow(ctx context.Context, r *http.Request, name string) (id string, version int, err error) {
	if p, ok := types.BuiltinWallPreset(name); ok {
		return p.ID, p.Version, nil
	}
	ps, ok := s.cfg.Store.(store.WallPresetStore)
	uid, perr := uuid.Parse(name)
	if !ok || perr != nil {
		return "", 0, nil
	}
	p, err := ps.GetWallPreset(ctx, uid)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return "", 0, nil
	case err != nil || r == nil:
		return p.ID, p.Version, err
	}
	c, err := s.wallPresetCaller(ctx, r, ps)
	if err != nil {
		return "", 0, err
	}
	if types.TemplateAuthorize(c.actor, p.Owner(), types.TemplateRead) != types.TemplateAllowed {
		return "", 0, nil
	}
	return p.ID, p.Version, nil
}

// The points a test edits a wall preset at (Server.queueRace): between the create's fold and the
// queue row's reference, and between the start's check of the reference and its fold.
const (
	queueRaceEnqueueAfterFold   = "enqueue_after_fold"
	queueRaceStartAfterPrecheck = "start_after_precheck"
)

func (s *Server) queueRaceAt(point string) {
	if s.queueRace != nil {
		s.queueRace(point)
	}
}

func queueSHA256JSON(v any) string {
	raw, _ := json.Marshal(v) //nolint:errcheck // a policy spec always marshals
	return queueSHA256(raw)
}

func queueSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// queuedRunCommitted runs once a queued run and its queue row have committed, before the rest of its
// answer. Tests set it to promote in that window.
var queuedRunCommitted = func(context.Context, uuid.UUID) {}

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
	// The kept work folder's drive commits with the queue row too: a promotion never finds a waiting
	// keep run without it, and a drive refused at the store's count leaves no run behind.
	var kept types.UserDrive
	if k := q.f.keep; k != nil {
		kept = keptDrive(r, k, q.run)
		entry.KeptDrive, entry.KeptDriveLimit = &kept, k.limit
	}
	created, err := s.createQueuedRun(ctx, q.run, entry)
	if q.f.keep != nil {
		if refusal := keptDriveRefusal(err, q.f.keep, kept); refusal != nil {
			refusal.write(s, w, r)
			return
		}
	}
	switch {
	case errors.Is(err, store.ErrRunQueueFull):
		writePoolRefusal(w, runnerpool.Refusal{Reason: runnerpool.ReasonQueueFull, Name: pool.Name, Count: queueMaxPerOwner})
		return
	case errors.Is(err, store.ErrRunnerPoolStale):
		writePoolRefusal(w, runnerpool.Refusal{Reason: runnerpool.ReasonStale, Name: pool.Name})
		return
	case errors.Is(err, store.ErrRunCreateKeySuperseded):
		writeRunCreateKeyInFlight(w)
		return
	case err != nil:
		writeServerError(w, r, "create run", err)
		return
	}
	queuedRunCommitted(ctx, created.ID)
	s.recordPolicyApplyAudits(ctx, q.f.source)
	if q.f.keep != nil {
		s.auditKeptDrive(r, q.f.keep, created, kept)
	}
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
		runs := []types.AgentRun{created}
		s.projectStatusDetailFor(r, runs) // the 201 carries the same wait reason and place a GET does
		s.projectQueueLimit(r, &runs[0])
		created = runs[0]
		sentence := runnerpool.WaitAtCapacityMsg(pool.Name, capMax)
		switch {
		case q.f.schedule != nil:
			sentence = scheduledWaitMsg(q.f.schedule)
		case created.Queue != nil && created.Queue.LimitSentence != "":
			// The sentence the run's own read gives, so the answer and the run page never differ.
			sentence = created.Queue.LimitSentence
		case q.f.place.waitsForRoom || created.StatusDetail == statusDetailQueuePrefix+string(runnerpool.WaitRunnersAtCapacity):
			// The second test is a capped pool with room whose runners are full: the promotion's token says so.
			sentence = q.runnerLimitSentence(s, ctx)
		}
		warnings = append(warnings, sentence)
	}
	w.Header().Set("Location", s.cfg.BasePath+"/api/v1/runs/"+created.ID.String())
	writeJSON(w, http.StatusCreated, createRunResponse{AgentRun: created, Warnings: warnings,
		WarningContext: s.warningContexts(ctx, q.f.ceiling, warnings), ResourcesApplied: &q.f.place.applied})
}

// waitReason is why the launch waits: it is scheduled for later, the pool is at its own cap, or (a pool
// with none) every runner that could take the run is at one of its limits.
func (q queuedCreate) waitReason() runnerpool.WaitReason {
	if q.f.schedule != nil {
		return runnerpool.WaitScheduled
	}
	if q.f.place.waitsForRoom {
		return runnerpool.WaitRunnersAtCapacity
	}
	return runnerpool.WaitAtCapacity
}

// runnerLimitSentence is the wait sentence of a launch its runners hold back, from the placement's own
// eligible runners. A read that fails, or runners that have found room since, leave the sentence that names
// no limit: this is the answer's wording when the run's own read gave none, and the promotion decides.
func (q queuedCreate) runnerLimitSentence(s *Server, ctx context.Context) string {
	pool := q.f.place.pool.Name
	fits, err := s.runnerFits(ctx)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: read runner limits for the wait sentence", slog.Any("err", err))
		return runnerpool.WaitRunnersAtCapacityMsg(pool)
	}
	if sentence := s.waitLimitSentence(ctx, pool, q.f.place.eligible, fits, q.f.place.unbound); sentence != "" {
		return sentence
	}
	return runnerpool.WaitRunnersAtCapacityMsg(pool)
}

// waitLimitSentence is the wait sentence of a run its runners hold back: it names the limit, by the same
// size the promotion holds each runner to (queueRunnerFree). With several runners it gives, as an example,
// the one holding the fewest runs, the first by id on a tie. "" when no runner is short of room by a limit.
func (s *Server) waitLimitSentence(ctx context.Context, pool string, eligible []poolTarget, fits map[string]runnerFit, unbound *types.RunnerAmounts) string {
	var shown poolTarget
	limit := ""
	for _, t := range eligible {
		f := fits[t.id]
		var need types.RunnerAmounts
		if unbound != nil && f.limits != nil {
			need = startSizeOn(*unbound, f.limits)
		}
		reason := runnerpool.NoRoomReason(f.limits, f.used, need)
		if reason != "" && (limit == "" || f.used.Runs < fits[shown.id].used.Runs || (f.used.Runs == fits[shown.id].used.Runs && t.id < shown.id)) {
			shown, limit = t, reason
		}
	}
	if limit == "" {
		return ""
	}
	name := fits[shown.id].runner.Name
	if rs, ok := s.cfg.Store.(store.RemoteRunnerStore); ok && shown.runnerID == nil {
		if got, err := rs.GetRemoteRunner(ctx, shown.id); err == nil {
			name = got.Runner.Name
		}
	}
	if name == "" {
		name = shown.id
	}
	if len(eligible) == 1 {
		return runnerpool.WaitRunnerAtLimitMsg(name, limit)
	}
	return runnerpool.WaitRunnersAtLimitMsg(pool, name, limit)
}

// projectQueueLimit fills Queue.LimitSentence of the one run a single-run route serves, when the run waits
// because its runners are at a limit and the reader is its owner or an admin (ownsRunOrAdmin: the creator,
// an admin, a security admin or the shared credential, the same readers the runner lists serve, so no
// reader is told a runner name the lists would not give them): the sentence is worked out
// from the runners' limits as they stand now, for the size the promotion holds the run to, so it stays
// true as the wait changes. Only handleGetRun and the create answer call it: a list, even of one row, would
// cost a read of every runner per row, so it never does.
// A read that fails leaves the field off, and the console says the sentence its reason alone gives.
func (s *Server) projectQueueLimit(r *http.Request, run *types.AgentRun) {
	if run.Queue == nil || run.Queue.Reason != string(runnerpool.WaitRunnersAtCapacity) || !s.ownsRunOrAdmin(r, *run) {
		return
	}
	qs, ok := s.queueStore()
	if !ok {
		return
	}
	// Judged as the owner, on the daemon's own context as the promotion is: the reader's identity never reaches it.
	// Both reads, the entry and the runners, are under one bound.
	base := s.cfg.BaseCtx
	if base == nil {
		base = context.Background()
	}
	pctx, cancel := context.WithTimeout(inheritQueuePass(context.WithoutCancel(base), r.Context()), queueEnqueueBound)
	defer cancel()
	e, err := qs.GetRunQueueEntry(pctx, run.ID)
	if errors.Is(err, store.ErrNotFound) {
		return // the run left the queue between the two reads
	}
	var launch queuedLaunch
	if err == nil {
		err = json.Unmarshal(e.Launch, &launch)
	}
	if err != nil {
		slog.WarnContext(r.Context(), "wardynd: read the queue entry for the wait sentence", slog.String("run_id", run.ID.String()), slog.Any("err", err))
		return
	}
	var auth queueAuthority
	_ = json.Unmarshal(e.Authority, &auth) //nolint:errcheck // an unreadable authority fails the start's own checks
	room, err := s.queueRunnerFree(withQueueOwner(auth.context(pctx), e.Owner, e.Authority), types.RunnerPool{ID: e.PoolID}, launch)
	if err != nil {
		slog.WarnContext(r.Context(), "wardynd: read runner limits for the wait sentence", slog.String("run_id", run.ID.String()), slog.Any("err", err))
		return
	}
	if room.wait == runnerpool.WaitRunnersAtCapacity {
		run.Queue.LimitSentence = s.waitLimitSentence(pctx, room.pool, room.candidates, room.fits, launch.Unbound)
	}
}

// entry is the run_queue row a launch is stored with.
func (q queuedCreate) entry(s *Server, ctx context.Context, waitSec int) (store.RunQueueEntry, error) {
	s.queueRaceAt(queueRaceEnqueueAfterFold)
	ref, err := s.queuePolicyRef(ctx, nil, q.req, q.f.source.wallPreset)
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
		Barrier: q.f.enforced, NoLocal: q.f.place.noLocal, MountRunner: q.f.place.mountRunner,
	}
	// The promotion holds a resume to its machine's Mac as the fold's placement did (keptPlacementPin).
	launch.Drives, launch.DriveRunner = keptPlacementPin(mountsDrives(q.f.drives, q.f.keep), drivesBoundRunner(q.f.drives, q.f.keep), q.f.keptPin)
	launch.Repos = append(launch.Repos, repoLocatorsOf(q.f.spec.WorkspaceRepos)...)
	if q.f.ceiling.Unit != nil {
		id := q.f.ceiling.Unit.ID
		launch.UnitID = &id
	}
	choice := contextChoiceFrom(ctx)
	launch.RunIn, launch.OwningUnits, launch.StrictPins = choice.RunIn, choice.OwningUnits, choice.StrictPins
	raw, err := json.Marshal(launch)
	if err != nil {
		return store.RunQueueEntry{}, err
	}
	now := q.run.CreatedAt
	e := store.RunQueueEntry{
		PoolID: *q.run.RunnerPoolID, Owner: q.run.CreatedBy, QueuedAt: now, DeadlineAt: now.Add(time.Duration(waitSec) * time.Second),
		PolicyRef: ref, Authority: auth, Launch: raw,
	}
	if sch := q.f.schedule; sch != nil {
		at := sch.at
		e.StartAt, e.DeadlineAt = &at, scheduledDeadline(now, at, waitSec)
	}
	return e, nil
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
	stampDriveAudit(createData, q.f.drives, q.f.keep != nil, created.ID)
	stampHarnessSet(createData, q.f.harnesses, q.f.image)
	queue := map[string]any{
		"pool_id": e.PoolID, "position": position, "max_wait_sec": waitSec, "deadline_at": e.DeadlineAt, "capped": !q.f.place.waitsForRoom,
		"reason": string(q.waitReason()),
	}
	if sch := q.f.schedule; sch != nil {
		queue["start_at"], queue["time_zone"] = sch.at, sch.zone
	}
	createData["queue"] = queue
	q.f.comps.stampRunCreate(createData)
	maps.Copy(createData, q.f.place.auditFields(created, s.runnerNameOf(ctx, created)))
	s.markRunCreateExempt(ctx, createDigestInput(q.req, q.f.carriedImage), createData)
	stampImageAdmission(createData, q.f.admission)
	s.stampBreakGlass(ctx, created.ID, q.createdByType, created.CreatedBy, createData)
	stampIdempotencyKey(ctx, createData)
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
		locked: ceilingForDispatch(q.f.ceiling, q.f.adoGrade, q.f.bedrockGrade).agentGuardrailLocks, class: q.f.enforced, place: q.f.place, comps: q.f.comps}, nil)...)
	warnings = append(warnings, s.repoSourceWarnings(ctx, created.ID, q.f.spec, q.req)...)
	warnings = append(warnings, q.f.fitWarnings...)
	warnings = append(warnings, q.f.end.warnings(created)...)
	warnings = append(warnings, queuedServiceWarnings(created, q.f.ceiling.Limits.RunLimits, q.f.end)...)
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
		s.dropUnstartedKeptDrive(ctx, runID)
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
	s.dropUnstartedKeptDrive(ctx, run.ID)
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
		s.dropUnstartedKeptDrive(ctx, id)
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
	ids := make([]uuid.UUID, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.RunID)
	}
	// A suspension tail leaves the runs a reactivated person made after its deactivation (withSuspensionBound).
	spared, err := runsMadeAfterBound(ctx, s.cfg.Store, ids)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if spared[e.RunID] {
			continue
		}
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
