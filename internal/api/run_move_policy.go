// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/agentpolicy"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A move's destination and the policy it is held to there. The destination is resolved for the run's owner, never
// the caller, and judged before the run is stopped and before any byte moves: a stricter destination narrows the
// run or refuses with the layer named, and a move never widens it. The judgement runs again just before commit,
// and an answer that differs fails the move.

// moveRefusal is a move refused or failed with its reason: status is the HTTP answer of a refusal in the request,
// layer the policy layer a destination refusal names.
type moveRefusal struct {
	status             int
	reason, layer, msg string
}

func (r *moveRefusal) Error() string { return r.reason + ": " + r.msg }

func refuseMove(status int, reason, msg string) *moveRefusal {
	return &moveRefusal{status: status, reason: reason, msg: msg}
}

// refuseMoveAt is a destination refusal naming its layer.
func refuseMoveAt(reason, layer, msg string) *moveRefusal {
	return &moveRefusal{status: http.StatusForbidden, reason: reason, layer: layer, msg: msg}
}

// moveDest is a move's destination, resolved for the run's owner.
type moveDest struct {
	To            types.MoveDestination
	Placement     types.Placement
	RunnerID      *uuid.UUID
	Runner        types.Runner
	PoolID        *uuid.UUID
	PoolRevision  int64
	PoolSelection types.RunnerPoolSelection
	Executor      string
	// Filled is placement_filled: the destination named a pool, laptop or org rather than one runner.
	Filled bool
	// poolRefused is set when the named pool exists for the owner but its use policy does not admit them; the
	// destination judgement refuses it with its layer.
	poolRefused bool
}

func (d moveDest) local() bool { return d.Placement == types.PlacementLocal }

// personal reports a person's own runner, which needs to_personal_runners.
func (d moveDest) personal() bool { return d.local() && d.Runner.OwnerKind != types.RunnerOwnerOrg }

// moveOwnerContext is ctx's deadline and cancellation with only the owner's subject as its caller: a destination is
// resolved as the owner would resolve it, and nothing of the caller's session reaches the reads.
func moveOwnerContext(ctx context.Context, owner string) context.Context {
	return withOIDCHuman(moveBareContext{ctx}, owner)
}

// moveBareContext keeps a context's deadline and cancellation and none of its values.
type moveBareContext struct{ context.Context }

func (moveBareContext) Value(any) any { return nil }

// moveRunnerCaps is runner id's last reported caps on this replica's stream; false when it is not connected or has
// reported none.
func (s *Server) moveRunnerCaps(id uuid.UUID) (runnerwire.Caps, bool) {
	s.runnerStreams.subsMu.Lock()
	ep, ok := s.runnerStreams.subs[id]
	s.runnerStreams.subsMu.Unlock()
	if !ok || ep.sub == nil {
		return runnerwire.Caps{}, false
	}
	return ep.sub.Caps()
}

// resolveMoveDestination resolves to for the run's owner: a runner they own (or an organisation runner in a pool
// they may use), a pool in their catalogue, their own runner ("laptop"), or the organisation's executors.
func (s *Server) resolveMoveDestination(ctx context.Context, run types.AgentRun, to types.MoveDestination, sc types.SiteConfig) (moveDest, *moveRefusal, error) {
	octx := moveOwnerContext(ctx, run.CreatedBy)
	unknown := func(msg string) (moveDest, *moveRefusal, error) {
		return moveDest{}, refuseMove(http.StatusUnprocessableEntity, reasonMoveDestinationUnknown, msg), nil
	}
	var d moveDest
	var err error
	switch to.Kind {
	case types.MoveToSite:
		return moveDest{}, refuseMove(http.StatusConflict, reasonMoveSitesOff, "Moving a run to a site is not available on this server."), nil
	case types.MoveToOrg:
		d = moveDest{To: to, Placement: types.PlacementRemote, Filled: true}
	case types.MoveToRunner:
		id, _ := uuid.Parse(to.Ref)
		if err := s.callerOwnsRunner(octx, id); err != nil {
			var ref *placement.Refusal
			if errors.As(err, &ref) {
				return unknown("The run's owner has no runner with that id.")
			}
			return moveDest{}, nil, err
		}
		d, err = s.moveRunnerDest(octx, to, id, false)
	case types.MoveToLaptop:
		d, err = s.moveLaptopDest(octx, run.CreatedBy, to)
	case types.MoveToPool:
		d, err = s.movePoolDest(octx, run.CreatedBy, to)
	default:
		return unknown("Name the destination as runner:<id>, pool:<id>, laptop, org or site:<name>.")
	}
	if errors.Is(err, errMoveNoDestination) {
		return unknown(fmt.Sprintf("%s does not resolve to a destination the run's owner can use.", to))
	}
	if err != nil {
		return moveDest{}, nil, err
	}
	if d.personal() && !moveToPersonalRunners(sc) {
		return moveDest{}, refuseMove(http.StatusForbidden, reasonMoveToPersonalOff,
			"Moving a run onto a person's own runner is off: an admin turns it on with features.run_move.to_personal_runners."), nil
	}
	return d, nil, nil
}

// errMoveNoDestination: a laptop or pool destination has no target the owner can use now.
var errMoveNoDestination = errors.New("no destination")

// moveRunnerDest reads runner id's row as a destination.
func (s *Server) moveRunnerDest(ctx context.Context, to types.MoveDestination, id uuid.UUID, filled bool) (moveDest, error) {
	rs, ok := s.cfg.Store.(interface {
		GetRunner(context.Context, uuid.UUID) (types.Runner, error)
	})
	if !ok {
		return moveDest{}, errMoveNoDestination
	}
	rn, err := rs.GetRunner(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return moveDest{}, errMoveNoDestination
	}
	if err != nil {
		return moveDest{}, err
	}
	return moveDest{To: to, Placement: types.PlacementLocal, RunnerID: &rn.ID, Runner: rn, Filled: filled}, nil
}

// moveLaptopDest is the owner's own claimed runner that is connected and serves moves, the first one listed.
func (s *Server) moveLaptopDest(ctx context.Context, owner string, to types.MoveDestination) (moveDest, error) {
	rs, ok := s.cfg.Store.(interface {
		ListRunnersByOwner(context.Context, string) ([]types.Runner, error)
	})
	if !ok {
		return moveDest{}, errMoveNoDestination
	}
	rows, err := rs.ListRunnersByOwner(ctx, owner)
	if err != nil {
		return moveDest{}, err
	}
	for _, rn := range rows {
		if rn.OwnerKind == types.RunnerOwnerOrg || rn.State != types.RunnerClaimed {
			continue
		}
		if c, ok := s.moveRunnerCaps(rn.ID); ok && c.MoveState {
			return moveDest{To: to, Placement: types.PlacementLocal, RunnerID: &rn.ID, Runner: rn, Filled: true}, nil
		}
	}
	return moveDest{}, errMoveNoDestination
}

// movePoolDest resolves a pool in the owner's catalogue to its first target: one of its executors, else one of its
// runners that is connected and serves moves. A pool the owner sees but whose use policy does not admit them is
// returned refused, for the destination judgement to name.
func (s *Server) movePoolDest(ctx context.Context, owner string, to types.MoveDestination) (moveDest, error) {
	ps, ok := s.cfg.Store.(store.RunnerPoolStore)
	if !ok {
		return moveDest{}, errMoveNoDestination
	}
	id, _ := uuid.Parse(to.Ref)
	all, err := ps.ListRunnerPoolCatalogue(ctx, owner, false)
	if err != nil {
		return moveDest{}, err
	}
	row, ok := catalogueRow(all, id)
	if !ok {
		return moveDest{}, errMoveNoDestination
	}
	permitted, refusal := s.permittedPools(ctx, ps, true)
	if refusal != nil {
		return moveDest{}, fmt.Errorf("list the owner's pools: %s", refusal.body.Error)
	}
	d := moveDest{To: to, PoolID: &row.Pool.ID, PoolRevision: row.Pool.Revision, PoolSelection: types.RunnerPoolSelectedExplicit, Filled: true}
	if _, ok := catalogueRow(permitted, id); !ok {
		d.Placement, d.poolRefused = types.PlacementRemote, true
		return d, nil
	}
	if len(row.ExecutorIDs) > 0 {
		d.Placement, d.Executor = types.PlacementRemote, row.ExecutorIDs[0]
		return d, nil
	}
	ids := slices.Clone(row.OwnRunnerIDs)
	for _, raw := range row.OrgRunnerIDs {
		if rid, err := uuid.Parse(raw); err == nil {
			ids = append(ids, rid)
		}
	}
	for _, rid := range ids {
		if c, ok := s.moveRunnerCaps(rid); !ok || !c.MoveState {
			continue
		}
		rd, err := s.moveRunnerDest(ctx, to, rid, true)
		if errors.Is(err, errMoveNoDestination) {
			continue
		}
		if err != nil {
			return moveDest{}, err
		}
		rd.PoolID, rd.PoolRevision, rd.PoolSelection = d.PoolID, d.PoolRevision, d.PoolSelection
		return rd, nil
	}
	return moveDest{}, errMoveNoDestination
}

// moveCapsRefusal is step 1's runner terms, judged before anything is stopped: both runners serve moves, a runner
// destination's substrate reads an imported, unstarted sandbox present and carries every proxy setting the run's
// stored config sets, and no destination resolves the run's class to an exec-less runtime. It answers the
// destination's capabilities, which the destination judgement reads.
func (s *Server) moveCapsRefusal(ctx context.Context, run types.AgentRun, rec runSandboxSpecRecord, d moveDest) (runner.Capabilities, *moveRefusal, error) {
	tooOld := func(msg string) (runner.Capabilities, *moveRefusal, error) {
		return runner.Capabilities{}, refuseMove(http.StatusConflict, reasonMoveRunnerTooOld, msg), nil
	}
	if run.RunnerID != nil {
		if c, ok := s.moveRunnerCaps(*run.RunnerID); !ok || !c.MoveState {
			return tooOld("The run's runner cannot move a run: it is offline, or its wardyn-runnerd is older than 0.11.0.")
		}
	} else if _, ok := s.cfg.Runner.(runner.StateExporter); !ok {
		return tooOld("This server's executor cannot export a run's files.")
	}
	var caps runner.Capabilities
	if d.local() {
		c, ok := s.moveRunnerCaps(*d.RunnerID)
		if !ok {
			return runner.Capabilities{}, refuseMove(http.StatusUnprocessableEntity, reasonMoveDestinationUnknown, "The destination runner is offline."), nil
		}
		if !c.MoveState {
			return tooOld("The destination runner's wardyn-runnerd is older than 0.11.0; upgrade it to move runs onto it.")
		}
		if !c.MoveImportPresent {
			return runner.Capabilities{}, refuseMove(http.StatusConflict, reasonMoveDestinationSubstrate,
				"The destination runner's substrate cannot hold a moved run before it starts."), nil
		}
		if setting, err := s.moveProxySettingMissing(ctx, run.ID, c); err != nil || setting != "" {
			if err != nil {
				return runner.Capabilities{}, nil, err
			}
			return tooOld("The destination runner's wardyn-runnerd is too old for " + setting + "; upgrade it.")
		}
		pc, err := s.runnerStreamCapabilities(ctx, *d.RunnerID)
		if err != nil {
			return runner.Capabilities{}, nil, err
		}
		caps = pc.Capabilities
	} else {
		if _, ok := s.cfg.Runner.(runner.StateImporter); !ok {
			return tooOld("This server's executor cannot take a moved run.")
		}
		c, err := s.cfg.Runner.Capabilities(ctx)
		if err != nil {
			return runner.Capabilities{}, nil, err
		}
		caps = c
	}
	if isExecLess(caps, moveClass(rec)) {
		return runner.Capabilities{}, refuseMove(http.StatusConflict, reasonMoveDestinationSubstrate,
			"The destination runs this run's class on a runtime that has no container before the agent starts, so a moved run cannot be written into it."), nil
	}
	return caps, nil, nil
}

// moveClass is the recorded class, CC1 when none was recorded.
func moveClass(rec runSandboxSpecRecord) types.ConfinementClass {
	if rec.ConfinementClass == "" {
		return types.CC1
	}
	return rec.ConfinementClass
}

// moveProxySettingMissing names the first proxy setting the run's stored config sets that the runner's caps do not
// carry, as a create refuses one; "" when there is none.
func (s *Server) moveProxySettingMissing(ctx context.Context, runID uuid.UUID, c runnerwire.Caps) (string, error) {
	spec, err := s.moveStoredSpec(ctx, runID)
	if err != nil {
		return "", err
	}
	return missingProxySetting(spec.ProxyConfig, c), nil
}

// missingProxySetting names the first setting pc sets that caps c does not carry; "" when there is none.
func missingProxySetting(pc runner.ProxyConfig, c runnerwire.Caps) string {
	for _, setting := range runnerwire.ProxySettingsSet(pc) {
		if !slices.Contains(c.ProxySettings, setting) {
			return setting
		}
	}
	return ""
}

// moveStoredSpec is the run's stored proxy config as a spec, the input a local revive classifies.
func (s *Server) moveStoredSpec(ctx context.Context, runID uuid.UUID) (runner.SandboxSpec, error) {
	raw, err := s.loadRunProxyConfig(ctx, runID)
	if err != nil {
		return runner.SandboxSpec{}, err
	}
	cfg, err := s.loadRenderedProxyConfig(raw)
	if err != nil {
		return runner.SandboxSpec{}, err
	}
	return localStoredSpec(*cfg), nil
}

// moveDestPlan is what the destination judgement accepted, recorded at admission and compared at commit.
type moveDestPlan struct {
	// Trust is the destination runner's trust class ("" for a remote destination).
	Trust types.RunnerTrustClass
	Mint  moveMintPlan
	// Deliveries is each grant's delivery mode at the destination ("" for a remote destination).
	Deliveries map[uuid.UUID]string
	Narrowed   moveNarrowed
	// DropSettings leaves Claude Code's managed settings out of the destination's spec, and SettingsWithheld is
	// why the destination's agent policy row reads delivered:false (dispatch's fallback for a run without tool
	// rules).
	DropSettings     bool
	SettingsWithheld string
	// Drives are the recorded drives the destination re-attaches (moveDestDrives), and WorkDrive says the destination
	// keeps the work folder on a new drive the move makes for it (a Kubernetes destination).
	Drives    []types.DriveMount
	WorkDrive bool
}

// moveNarrowed is the egress narrowing a destination applies, recorded as run_moves.narrowed and handed to the
// destination's revive as its one live egress edit.
type moveNarrowed struct {
	Remove []string `json:"remove,omitempty"`
	Deny   []string `json:"deny,omitempty"`
}

func (n moveNarrowed) edits() []liveEgressEdit {
	if len(n.Remove) == 0 && len(n.Deny) == 0 {
		return nil
	}
	return []liveEgressEdit{{remove: slices.Clone(n.Remove), deny: slices.Clone(n.Deny)}}
}

// moveFieldClass is how a move treats one field of the run's policy at the destination.
type moveFieldClass int

const (
	// moveFieldKeep: the run keeps its own value; the destination never decides it again, or its revive re-applies
	// the destination's classification of it.
	moveFieldKeep moveFieldClass = iota
	// moveFieldNarrow: the destination may narrow it, and the narrowing is a live egress edit.
	moveFieldNarrow
	// moveFieldRefuse: a destination whose value differs refuses the move with the field as its layer.
	moveFieldRefuse
)

// moveSpecFields classes every types.RunPolicySpec field (TestMovePolicyCoversEverySpecField).
var moveSpecFields = map[string]moveFieldClass{
	"AllowedDomains":          moveFieldNarrow,
	"DeniedDomains":           moveFieldNarrow,
	"AllowAllEgress":          moveFieldRefuse,
	"RefuseIPLiterals":        moveFieldRefuse,
	"FirstUseApproval":        moveFieldRefuse,
	"FirstUseHoldSeconds":     moveFieldRefuse,
	"MaxHolds":                moveFieldRefuse,
	"AllowedMethods":          moveFieldRefuse,
	"HostRules":               moveFieldRefuse,
	"MinConfinementClass":     moveFieldRefuse,
	"EligibleGrants":          moveFieldKeep,
	"AutoStopAfterSec":        moveFieldKeep,
	"WorkspaceMounts":         moveFieldKeep,
	"WorkspaceRepos":          moveFieldKeep,
	"LLMInspection":           moveFieldKeep,
	"UIApps":                  moveFieldKeep,
	"Resources":               moveFieldKeep,
	"ToolRules":               moveFieldRefuse,
	"GitPushAnyBranch":        moveFieldRefuse,
	"PushRules":               moveFieldRefuse,
	"AzureDevOpsCapabilities": moveFieldRefuse,
	"GitHubCapabilities":      moveFieldRefuse,
	"HarnessInnerWalls":       moveFieldRefuse,
	"Desktop":                 moveFieldKeep,
	"Recording":               moveFieldKeep,
	"DataOut":                 moveFieldRefuse,
	// The run keeps the outage terms recorded at its create; the destination's proxy reads them from its outage key.
	"Outage": moveFieldKeep,
	// Walls on folders from a person's computer are kept by the control plane's doors; like DataOut, a destination
	// that would change them refuses.
	"LocalFolders": moveFieldRefuse,
}

// moveNarrowing is what the destination's policy dst, with the owner's current denies, does to the run's policy
// src: allowed domains it drops are removed and denies it adds are added, the only narrowing a revive takes. A
// field classed refuse whose value differs refuses, named by the field; a field the destination widens is never
// widened. An unclassified field refuses.
func moveNarrowing(src, dst types.RunPolicySpec, ownerDeny []string) (moveNarrowed, string) {
	var n moveNarrowed
	sv, dv := reflect.ValueOf(src), reflect.ValueOf(dst)
	for i := range sv.NumField() {
		name := sv.Type().Field(i).Name
		class, ok := moveSpecFields[name]
		if !ok {
			return moveNarrowed{}, name
		}
		if class == moveFieldRefuse && !reflect.DeepEqual(sv.Field(i).Interface(), dv.Field(i).Interface()) {
			return moveNarrowed{}, name
		}
	}
	for _, d := range src.AllowedDomains {
		if !slices.Contains(dst.AllowedDomains, d) && !slices.Contains(n.Remove, d) {
			n.Remove = append(n.Remove, d)
		}
	}
	for _, d := range slices.Concat(dst.DeniedDomains, ownerDeny) {
		if !slices.Contains(src.DeniedDomains, d) && !slices.Contains(n.Deny, d) {
			n.Deny = append(n.Deny, d)
		}
	}
	return n, ""
}

// judgeMoveDestination is the destination judgement (step 2), with no runner call that changes anything: the
// owner's current ceiling and its placement denies, the pool's use policy, the destination's keep of a stopped
// sandbox, a folder bound to another runner, the image's architecture, the run's class, a team or read-only drive,
// the managed files, a local destination's placement and trust classification, the policy narrowing and the
// grants. A refusal names its layer; an error is a read the judgement needs that failed.
func (s *Server) judgeMoveDestination(ctx context.Context, run types.AgentRun, rec runSandboxSpecRecord, d moveDest,
	caps runner.Capabilities, sc types.SiteConfig,
) (moveDestPlan, *moveRefusal, error) {
	destRun := moveDestRun(run, d, "")
	refused := func(layer, msg string) (moveDestPlan, *moveRefusal, error) {
		return moveDestPlan{}, refuseMoveAt(reasonMoveDestinationRefused, layer, msg), nil
	}
	c, rerr := s.reviveCeilingAdmitting(ctx, destRun, types.ActorSystem, "wardynd")
	if rerr != nil {
		if rerr.status >= http.StatusInternalServerError {
			return moveDestPlan{}, nil, errors.New(rerr.msg)
		}
		return refused(moveCeilingLayer(rerr.reason), "The run owner's current ceiling does not admit the run at the destination: "+rerr.msg)
	}
	if d.poolRefused {
		return refused("pool_use_policy", "The destination pool's use policy does not admit the run's owner.")
	}
	// The keep is judged on the drives the destination's spec mounts: the re-attached ones and the new work drive. A
	// drive that does not re-attach refuses below.
	var plan moveDestPlan
	plan.Drives, _ = moveDestDrives(rec, run, d, s.cfg.RunnerTarget)
	plan.WorkDrive = s.moveDestKubernetes(d, caps)
	keep, err := movePortStopRefusal.call(ctx, moveKeepArgs{Run: destRun, Drives: moveKeepDrives(plan)})
	if err != nil {
		return moveDestPlan{}, nil, err
	}
	if keep != "" {
		return refused("keep", "The destination cannot keep a stopped run: "+keep)
	}
	if ref := moveRecordRefusal(rec, run, d, caps, s.cfg.RunnerTarget); ref != nil {
		return moveDestPlan{}, ref, nil
	}
	var settingsRefusal string
	if plan.DropSettings, plan.SettingsWithheld, settingsRefusal = moveSettingsFallback(rec, caps); settingsRefusal != "" {
		return refused("managed_files", settingsRefusal)
	}
	src, err := s.moveStoredSpec(ctx, run.ID)
	if err != nil {
		return moveDestPlan{}, nil, err
	}
	dst := src.ProxyConfig.Policy
	if d.local() {
		p, stripped, ref, err := s.moveDestinationPlan(ctx, destRun, rec, src, sc)
		if err != nil || ref != nil {
			return moveDestPlan{}, ref, err
		}
		dst, plan.Trust = stripped, p.Trust
		if plan.Deliveries, err = s.moveDeliveries(ctx, run, p); err != nil {
			return moveDestPlan{}, nil, err
		}
	} else if plan.Deliveries, err = s.moveDeliveries(ctx, run, placement.LocalPlan{}); err != nil {
		return moveDestPlan{}, nil, err
	}
	var field string
	if plan.Narrowed, field = moveNarrowing(src.ProxyConfig.Policy, dst, c.deny); field != "" {
		return refused(field, "The destination would change the run's "+field+", which a move cannot narrow.")
	}
	mint, ref, err := s.moveMintPlan(ctx, run, destRun)
	if err != nil || ref != nil {
		return moveDestPlan{}, ref, err
	}
	plan.Mint = mint
	return plan, nil, nil
}

// moveCeilingLayer names the layer of a refusal from the owner's current ceiling: the profile's placement denies, its
// component and model-provider denies, the boundary floor, or else the governance unit itself.
func moveCeilingLayer(reason string) string {
	switch reason {
	case string(placement.ReasonPlacementDenied):
		return "placement"
	case reasonComponentDenied:
		return "component"
	case reasonModelProviderDenied:
		return "model_provider"
	case reasonConfinementClassConflict:
		return "boundary_floor"
	}
	return "governance_unit"
}

// moveRecordRefusal is the destination judgement's terms on the recorded spec alone: a folder bound to another
// runner, a prewrapped image's architectures, the run's class, and a drive the destination does not re-attach
// (moveDestDrives). orgTarget is the substrate the organisation's runners dispatch to.
func moveRecordRefusal(rec runSandboxSpecRecord, run types.AgentRun, d moveDest, caps runner.Capabilities, orgTarget string) *moveRefusal {
	for _, m := range rec.Mounts {
		if m.BoundRunner != "" && (!d.local() || m.BoundRunner != d.RunnerID.String()) {
			return refuseMoveAt(reasonMoveDestinationRefused, "bound_folder", "A folder of this run is bound to its runner, so it moves nowhere else.")
		}
	}
	if len(rec.ImageArches) > 0 && d.local() {
		if err := prewrappedRunnerArch(rec.ImageArches, d.Runner.Arch); err != nil {
			return refuseMoveAt(reasonMoveDestinationRefused, "image_arch", "The run's image does not run on the destination's architecture.")
		}
	}
	if !slices.Contains(caps.ConfinementClasses, moveClass(rec)) {
		return refuseMoveAt(reasonMoveDestinationRefused, "confinement", fmt.Sprintf("The destination does not enforce the run's class %s.", moveClass(rec)))
	}
	_, ref := moveDestDrives(rec, run, d, orgTarget)
	return ref
}

// moveDestDrives is the recorded drives the destination re-attaches. A move writes only into storage it creates: the
// work folder's drive is never re-attached (it may hold an earlier placement's state, and stays the person's drive),
// and every other drive is re-attached only where the destination mounts the same storage, never copied. That is a
// drive bound to the destination runner, or, between the organisation's own executors, a drive of the substrate
// orgTarget the organisation dispatches to. Any other drive refuses with layer drive.
func moveDestDrives(rec runSandboxSpecRecord, run types.AgentRun, d moveDest, orgTarget string) ([]types.DriveMount, *moveRefusal) {
	var out []types.DriveMount
	for _, dr := range rec.Drives {
		if dr.Target == runner.ScratchWorkPath {
			continue
		}
		var same bool
		if d.local() {
			same = dr.RunnerID != nil && *dr.RunnerID == *d.RunnerID
		} else {
			same = run.RunnerID == nil && dr.RunnerID == nil && dr.Backend.RunnerTarget() == orgTarget
		}
		switch {
		case same:
			out = append(out, dr)
		case dr.Team || dr.ReadOnly:
			return nil, refuseMoveAt(reasonMoveDestinationRefused, "drive",
				fmt.Sprintf("The run mounts the shared or read-only drive %q, which is not carried and does not mount at the destination.", dr.DriveName))
		default:
			return nil, refuseMoveAt(reasonMoveDestinationRefused, "drive", moveDriveElsewhereMsg(dr.DriveName))
		}
	}
	return out, nil
}

// moveDriveElsewhereMsg refuses a move whose destination does not mount one of the run's own drives.
func moveDriveElsewhereMsg(drive string) string {
	return fmt.Sprintf("The run mounts the drive %q, which is not the same storage at the destination. A move re-attaches a drive and never copies one.", drive)
}

// moveKeepDrives is the drives the destination's spec mounts as the keep judges them: the re-attached drives, and the
// new work drive at the work folder. The work drive's row is made only once the move runs (makeWorkDrive), so its
// mount here is the place it takes.
func moveKeepDrives(plan moveDestPlan) []types.DriveMount {
	out := slices.Clone(plan.Drives)
	if plan.WorkDrive {
		out = append(out, types.DriveMount{Backend: types.DriveBackendK8sPVC, Target: runner.ScratchWorkPath})
	}
	return out
}

// moveSettingsFallback judges each recorded managed file at the destination's capabilities, under dispatch's two
// rules: Codex's managed requirements refuse at any gap; Claude Code's managed settings refuse at a gap only when the
// run has tool rules, and otherwise follow dispatch's fallback (the file is left out where the destination delivers
// none, and the agent policy row reads why). A refusal is the sentence that names the file, "" for none.
func moveSettingsFallback(rec runSandboxSpecRecord, caps runner.Capabilities) (drop bool, withheld, refusal string) {
	reason, deliverable := managedFilesGap(caps, moveClass(rec))
	gap := !deliverable || reason != ""
	for _, f := range rec.ManagedFiles {
		switch {
		case !gap:
		case f.Path == agentpolicy.CodexRequirementsPath:
			return false, "", fmt.Sprintf("The destination cannot vouch for the managed file %s: %s", f.Path, reason)
		case f.Path == rec.AgentPolicy.Path && rec.AgentPolicy.ToolRules > 0:
			return false, "", fmt.Sprintf("The destination cannot vouch for the managed file %s, which holds the run's tool rules: %s", f.Path, reason)
		case f.Path == rec.AgentPolicy.Path:
			drop, withheld = !deliverable, reason
		}
	}
	return drop, withheld, ""
}

// moveDestinationPlan is a local destination's classification, as a local revive classifies the stored config: the
// destination runner's facts and trust class, the owner's self-defined component allowance and the site's managed
// device rule, judged by the local placement checks. It answers the plan and the policy those checks leave.
func (s *Server) moveDestinationPlan(ctx context.Context, destRun types.AgentRun, rec runSandboxSpecRecord, spec runner.SandboxSpec,
	sc types.SiteConfig,
) (placement.LocalPlan, types.RunPolicySpec, *moveRefusal, error) {
	refused := func(layer, msg string) (placement.LocalPlan, types.RunPolicySpec, *moveRefusal, error) {
		return placement.LocalPlan{}, types.RunPolicySpec{}, refuseMoveAt(reasonMoveDestinationRefused, layer, msg), nil
	}
	profile, denied := s.ownerUnit(ctx, destRun)
	if denied != nil {
		if denied.status >= http.StatusInternalServerError {
			return placement.LocalPlan{}, types.RunPolicySpec{}, nil, errors.New(denied.msg)
		}
		return refused("governance_unit", denied.msg)
	}
	spec.Env = rec.Env
	p, err := s.localResolvedPlan(ctx, destRun, profile != nil && profile.Limits.LocalSelfDefinedComponents, sc, spec, rec.OrgConfigKeys, llmTransport{})
	if err != nil {
		var ref *placement.Refusal
		if errors.As(err, &ref) {
			return refused("local_placement", ref.Detail)
		}
		return placement.LocalPlan{}, types.RunPolicySpec{}, nil, err
	}
	p.RequireManagedDevice = requiresManagedDevice(sc)
	// The run lands on a runner it has not run on, so the organisation's machine-kind denials judge it there as they
	// judge a new run.
	p.MachineKindDenials = machineKindDenialsOf(sc)
	if err := s.denialSiteOf(ctx, sc, destRun, &p); err != nil {
		return placement.LocalPlan{}, types.RunPolicySpec{}, nil, err
	}
	stripped, lref := placement.LocalEligibility(p)
	if lref != nil {
		layer := "local_placement"
		switch {
		case lref.Field == "machine_kind":
			layer = "machine_kind"
		case lref.Reason == placement.ReasonRunnerTrustRequired:
			layer = "managed_device"
		}
		return refused(layer, lref.Detail)
	}
	return p, stripped.ProxyConfig.Policy, nil, nil
}

// moveDeliveries is each of the run's grants' delivery mode at the destination plan p, the value commit records on
// its credential_grants row: the mode p's delivery policy gives the grant's class at p's trust class, and "" at a
// remote destination (a zero p).
func (s *Server) moveDeliveries(ctx context.Context, run types.AgentRun, p placement.LocalPlan) (map[uuid.UUID]string, error) {
	rows, err := s.cfg.Store.ListGrantsByRun(ctx, run.ID)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]string, len(rows))
	owner := runIdentitySubject(ctx, run.CreatedBy)
	for _, g := range rows {
		if p.Trust == "" {
			out[g.ID] = ""
			continue
		}
		origin, ref := s.localGrantOrigin(ctx, owner, g.Spec)
		if fault := ownershipFault(ref); fault != nil {
			return nil, fault
		}
		out[g.ID] = string(p.Delivery.ForTrust(origin.Delivery, p.Trust).Mode)
	}
	return out, nil
}

// moveMintPlan is the grant judgement at the destination: the destination's withholding, and the revoke at pause's
// mint plan over it. A needed grant the destination withholds refuses with its layer; any other answer of the port
// refuses the move.
func (s *Server) moveMintPlan(ctx context.Context, run, destRun types.AgentRun) (moveMintPlan, *moveRefusal, error) {
	w, err := s.liveWithholding(ctx, destRun)
	if err != nil {
		return moveMintPlan{}, nil, err
	}
	plan, err := movePortMintAtDestination.call(ctx, moveMintArgs{Run: run, Dest: w})
	var gr *moveGrantRefusal
	switch {
	case errors.As(err, &gr):
		return moveMintPlan{}, refuseMoveAt(gr.Reason, gr.Layer, "The destination withholds a credential this run needs ("+gr.Layer+")."), nil
	case err != nil:
		return moveMintPlan{}, refuseMoveAt(reasonMoveDestinationRefused, "grants", "The destination's credential judgement could not be made: "+err.Error()), nil
	}
	return plan, nil, nil
}

// samePlan reports whether commit's judgement answers what admission recorded: the narrowing, the grant plan, the
// trust class and the deliveries. layer names the first difference.
func (p moveDestPlan) samePlan(q moveDestPlan) (layer string, same bool) {
	switch {
	case p.Trust != q.Trust:
		return "trust_class", false
	case !reflect.DeepEqual(p.Mint, q.Mint):
		return "grants", false
	case !reflect.DeepEqual(p.Deliveries, q.Deliveries):
		return "deliveries", false
	case !reflect.DeepEqual(p.Narrowed, q.Narrowed):
		return "narrowed", false
	case p.DropSettings != q.DropSettings:
		return "managed_files", false
	}
	return "", true
}
