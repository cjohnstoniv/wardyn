// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Keeping a macOS VM's machine between runs: a create asks for machine {keep, name} or {resume, fresh}.
// stepKeptMachine refuses what nobody may have (site switch, run shape, the caller's own Mac in their own
// pool, governance, desktop sign-ins, lasting credentials, the limits, the machine's own state) at create
// and Review, and leaves a keptMachinePlan. Nothing is written for it until dispatch: there the plan's
// machine is inserted (keep) or claimed (resume) in one conditional store write, before any runner work,
// and recorded on the run (agent_runs.kept_machine_id). A queued resume holds nothing while it waits.
// A run whose sandbox never came up gives its machine back: a new one is dropped, a resumed one is kept
// again, never marked cleaned.
package api

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/macvm/admission"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// The modes a run gets its machine by, as the runner's KeptMachineSpec.Mode names them.
const (
	keptModeNew    = "new"
	keptModeResume = "resume"
	keptModeFresh  = "fresh"
)

// keptMachineNameRule is the name rule: 1 to 64 letters, digits, spaces, hyphens or underscores.
var keptMachineNameRule = regexp.MustCompile(`^[A-Za-z0-9 _-]{1,64}$`)

// keptMachineWorstBytes is one kept machine at its worst case on its Mac, as the runner counts it when its
// manifest names no size: a base disk's logical size (macvm.BaseDiskBytes) plus the run reservation's extra.
var keptMachineWorstBytes = admission.RunDiskReservation(50 << 30)

// keptMachineStore is what this file reads and writes of the kept machines (store.KeptMachineStore's).
type keptMachineStore interface {
	ListRunnerPoolsWithMember(ctx context.Context, runnerID uuid.UUID) ([]types.RunnerPool, error)
	GetKeptMachine(ctx context.Context, id uuid.UUID) (types.KeptMachine, error)
	FindKeptMachineOf(ctx context.Context, owner, ref string) (types.KeptMachine, error)
	ListKeptMachinesOf(ctx context.Context, owner string) ([]types.KeptMachine, error)
	BeginKeptMachineUse(ctx context.Context, id uuid.UUID, owner string, runID uuid.UUID) (types.KeptMachine, error)
	RecordKeptMachineStop(ctx context.Context, st store.KeptMachineStop) (types.KeptMachine, error)
	DropUnstartedKeptMachine(ctx context.Context, id, runID uuid.UUID) error
	KeptMachineResetPending(ctx context.Context, id uuid.UUID) (bool, error)
}

// keptMachinePlan is what stepKeptMachine decided: what dispatch inserts (new) or claims (resume, fresh).
type keptMachinePlan struct {
	mode string
	// id is the machine a resume or fresh start claims; a new one gets its id at the insert.
	id uuid.UUID
	// name is the name asked for a new machine ("" lets the server pick one at the insert), or a resumed
	// machine's name now.
	name     string
	owner    string
	runner   uuid.UUID
	mac      string
	base     string
	prepared time.Time
	// limit is the site's machines per person; idleDays its idle limit, the runner's expiry.
	limit, idleDays int
}

// keptMachineAsk is what the fold knows when stepKeptMachine runs.
type keptMachineAsk struct {
	req      createRunRequest
	ceiling  governanceCeiling
	place    runPlacement
	spec     types.RunPolicySpec
	comps    runComponents
	keepWork bool
	queued   bool
	// create is the create door's: only it starts the delete of an expired machine it meets.
	create bool
}

// keptMac is the caller's own Mac the run is placed on, as the step read it.
type keptMac struct {
	runner  types.Runner
	name    string
	offline bool
	caps    *runnerwire.MacVMCaps
}

// keptResume reports whether req resumes a kept machine as it is (not fresh): such a run is checked against
// the machine's own base, never the Mac's current one (macOSVMOffer, placementCapabilityRefusalFor).
func keptResume(req createRunRequest) bool {
	return req.Machine != nil && req.Machine.Resume != "" && !req.Machine.Fresh
}

// stepKeptMachine refuses a create or Review whose machine keptMachineDecision refuses, with its sentence,
// and leaves the plan dispatch carries out. A create writes the denied machine.create or machine.resume row.
func (s *Server) stepKeptMachine(f *runFold, _ *foldRecorder) bool {
	if f.req.Machine == nil {
		return true
	}
	ask := keptMachineAsk{req: *f.req, ceiling: f.ceiling, place: f.place, spec: f.spec,
		comps: f.comps, keepWork: f.keep != nil, queued: f.queued}
	if f.mode == foldCreate {
		ask.create = true
	}
	plan, rf := s.keptMachineDecision(f.r, ask)
	if rf == nil {
		f.machine = plan
		return true
	}
	s.auditKeptMachineDenied(f.r, *f.req, rf, f.mode == foldCreate)
	return !rf.write(s, f.w, f.r)
}

// keptMachineShapeRefusal is the 400 for a machine the request cannot carry; nil when it can.
func keptMachineShapeRefusal(m client.RunMachine) *runRefusal {
	bad := func(msg string) *runRefusal { return runError(http.StatusBadRequest, reasonKeptMachineInvalid, msg) }
	switch {
	case m.Keep && m.Resume != "":
		return bad(keptMachineKeepOrResume)
	case !m.Keep && strings.TrimSpace(m.Resume) == "":
		return bad(keptMachineNothingAsked)
	case m.Fresh && m.Resume == "":
		return bad(keptMachineFreshNeedsMsg)
	case m.Name != "" && !m.Keep:
		return bad(keptMachineNameNeedsKeep)
	case m.Name != "" && !keptMachineNameRule.MatchString(m.Name):
		return bad(keptMachineNameRuleMsg)
	}
	return nil
}

// keptMachineDecision runs the checks in order, what nobody can wait out first.
func (s *Server) keptMachineDecision(r *http.Request, in keptMachineAsk) (*keptMachinePlan, *runRefusal) {
	m := *in.req.Machine
	if rf := keptMachineShapeRefusal(m); rf != nil {
		return nil, rf
	}
	ctx := r.Context()
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		return nil, runServerError("read site config", err)
	}
	if !sc.MacOSVM.KeptMachinesAllowed() {
		return nil, runError(http.StatusForbidden, reasonKeptMachineSiteOff, keptMachineSiteOffMsg)
	}
	if !types.MachineKind(in.req.MachineKind).IsMacOSVM() || in.req.EffectiveExperience() != types.ExperienceInteractive {
		return nil, runError(http.StatusUnprocessableEntity, reasonKeptMachineInteractiveOnly, keptMachineInteractiveMsg)
	}
	at, principal := actorFromRequest(r)
	owner := types.CanonicalUserSubject(oidcHumanFromContext(ctx))
	if s.cfg.LocalMode || at != types.ActorHuman || principal == "" || owner == "" || in.ceiling.Operator ||
		macWindowAuthorityFrom(ctx, at, principal, s.cfg.Now().UTC()).Via {
		return nil, keptNotYourMac("")
	}
	if rf := s.keptMachineGovernanceRefusal(ctx, in.ceiling); rf != nil {
		return nil, rf
	}
	if s.macOSDesktopSignInsOff(ctx, types.AgentRun{MachineKind: types.MachineKind(in.req.MachineKind), Agent: in.req.Agent}) {
		return nil, runError(http.StatusForbidden, reasonDesktopSignInsOff, macOSDesktopSignInsOffMsg(in.req.Agent))
	}
	if kinds := keptResidentKinds(in.spec, in.comps, s.cfg.DisableGitPATBroker); len(kinds) > 0 {
		return nil, runError(http.StatusUnprocessableEntity, reasonKeptMachineResidentSecrets, keptMachineResidentSecretsMsg(kinds))
	}
	if in.keepWork {
		return nil, runError(http.StatusUnprocessableEntity, reasonKeptMachineWithWorkFolder, keptMachineWorkFolderMsg)
	}
	st, ok := s.cfg.Store.(keptMachineStore)
	if !ok {
		return nil, runServerError("read kept machines", errors.New("the store keeps no kept machines"))
	}
	mac, rf := s.keptOwnMac(ctx, st, in.place, principal)
	if rf != nil {
		return nil, rf
	}
	plan := &keptMachinePlan{owner: owner, runner: mac.runner.ID, mac: mac.name,
		limit: sc.MacOSVM.MaxKeptMachinesPerPersonOrDefault(), idleDays: sc.MacOSVM.KeptMachineIdleDaysOrDefault()}
	if m.Resume != "" {
		rf = s.keptResumeRefusal(ctx, st, in, mac, plan)
	} else {
		rf = s.keptNewRefusal(ctx, st, in, mac, plan)
	}
	if rf != nil {
		return nil, rf
	}
	return plan, nil
}

// keptNotYourMac is the not-your-Mac refusal (also for another person's Mac), naming the Mac when it is known.
func keptNotYourMac(mac string) *runRefusal {
	return runError(http.StatusForbidden, reasonKeptMachineNotYourMac, keptRunNotYourMacMsg(mac))
}

// keptMachineGovernanceRefusal is the governance refusal: the caller's profile denies kept machines.
func (s *Server) keptMachineGovernanceRefusal(ctx context.Context, c governanceCeiling) *runRefusal {
	if !c.Limits.DenyKeptMachines || s.runUngoverned(ctx) {
		return nil
	}
	d := authz.Deny(authz.ReasonGovernanceUnit, "runs.kept_machine", keptMachineGovernanceMsg).WithPolicy(s.ceilingPolicy(ctx, c))
	if c.Unit != nil {
		d = d.OnKnob(s.runLockOf(ctx, c.Unit, "deny_kept_machines"))
	}
	return runDenied(d)
}

// keptResidentKinds is the lasting-credential check at create: the kinds of lasting credential the run would put inside the
// machine, in keptSecretKinds' order. Env and file secrets, SSH keys, a git token the helper would hold
// (only when the never-resident git broker is off), and a component secret delivered as env or a file.
// Dispatch checks the built spec again (keptSpecSecretKinds), which also catches a Bedrock sign-in's
// role credentials.
func keptResidentKinds(spec types.RunPolicySpec, comps runComponents, gitBrokerOff bool) []string {
	has := map[string]bool{}
	for _, g := range spec.EligibleGrants {
		switch g.Kind {
		case types.GrantEnvSecret:
			has[keptKindEnv] = true
		case types.GrantFileSecret:
			has[keptKindFile] = true
		case types.GrantSSHKey:
			has[keptKindSSH] = true
		case types.GrantGitPAT:
			has[keptKindGit] = has[keptKindGit] || gitBrokerOff
		}
	}
	for _, a := range comps.attached {
		for _, sec := range a.snapshot.Definition.Secrets {
			switch sec.Delivery.Mode {
			case types.ComponentDeliveryEnv:
				has[keptKindEnv] = true
			case types.ComponentDeliveryFile:
				has[keptKindFile] = true
			}
		}
	}
	var out []string
	for _, k := range keptSecretKinds {
		if has[k] {
			out = append(out, k)
		}
	}
	return out
}

// keptOwnMac is the own-Mac check: the run is placed on exactly one Mac, a claimed runner the caller registered
// as their own, and no pool that holds it is another person's or the organisation's. It reads the Mac's
// last caps; an offline Mac is said by the caller's row.
func (s *Server) keptOwnMac(ctx context.Context, st keptMachineStore, place runPlacement, principal string) (keptMac, *runRefusal) {
	if !place.pooled || place.placement != types.PlacementLocal {
		return keptMac{}, keptNotYourMac("")
	}
	id := place.placedRunner()
	if id == nil {
		return keptMac{}, runError(http.StatusForbidden, reasonKeptMachineNotYourMac, keptMachinePoolMsg)
	}
	rs, ok := s.cfg.Store.(macWindowRunnerStore)
	if !ok {
		return keptMac{}, keptNotYourMac("")
	}
	rn, err := rs.GetRunner(ctx, *id)
	if errors.Is(err, store.ErrNotFound) {
		return keptMac{}, keptNotYourMac("")
	}
	if err != nil {
		return keptMac{}, runServerError("read the run's runner", err)
	}
	mac := keptMac{runner: rn, name: macRunnerName(rn)}
	if rn.State != types.RunnerClaimed || rn.OwnerKind != types.RunnerOwnerPerson || rn.Owner != principal {
		return keptMac{}, keptNotYourMac(mac.name)
	}
	pools, err := st.ListRunnerPoolsWithMember(ctx, rn.ID)
	if err != nil {
		return keptMac{}, runServerError("read the runner's pools", err)
	}
	for _, p := range pools {
		if p.State != types.RunnerPoolDeleted && (p.Scope != types.RunnerPoolUser || p.Owner != principal) {
			return keptMac{}, runError(http.StatusForbidden, reasonKeptMachineNotYourMac, keptMachinePoolMsg)
		}
	}
	pc, err := s.runnerStreamCapabilities(ctx, rn.ID)
	var ref *placement.Refusal
	switch {
	case errors.As(err, &ref):
		mac.offline = true
	case err != nil:
		return keptMac{}, runServerError("read the runner's capabilities", err)
	}
	if rc, ok := pc.impl.(runnerCaps); ok && !mac.offline {
		if caps, have := rc.Caps(); have {
			mac.caps = caps.MacOSVM
		}
	}
	return mac, nil
}

// keptRunnerRefusal is the Mac check: the Mac is online and keeps machines. A run that waits in its pool's
// queue may wait for its Mac; the queue's start asks again.
func keptRunnerRefusal(mac keptMac, queued bool) *runRefusal {
	switch {
	case mac.offline && queued:
		return nil
	case mac.offline:
		return runError(http.StatusConflict, reasonKeptMachineOffline, keptMachineOfflineMsg(mac.name))
	case mac.caps == nil || !mac.caps.KeptMachines:
		return runError(http.StatusConflict, reasonKeptMachineRunnerTooOld, keptMachineRunnerTooOldMsg(mac.name))
	}
	return nil
}

// keptNewRefusal is a keep's own checks: the Mac keeps machines, the caller is under the
// site's limit, the Mac under its disk limit, and the name is not already one of the caller's.
func (s *Server) keptNewRefusal(ctx context.Context, st keptMachineStore, in keptMachineAsk, mac keptMac, plan *keptMachinePlan) *runRefusal {
	if rf := keptRunnerRefusal(mac, in.queued); rf != nil {
		return rf
	}
	mine, err := st.ListKeptMachinesOf(ctx, plan.owner)
	if err != nil {
		return runServerError("list kept machines", err)
	}
	if len(mine) >= plan.limit {
		return runError(http.StatusUnprocessableEntity, reasonKeptMachinePersonLimit, keptMachinePersonLimitMsg(len(mine), plan.limit))
	}
	if mac.caps != nil {
		if rf := keptMacLimitRefusal(mac.name, mac.caps.Kept); rf != nil {
			return rf
		}
	}
	plan.mode, plan.name = keptModeNew, in.req.Machine.Name
	if plan.name != "" {
		if _, err := st.FindKeptMachineOf(ctx, plan.owner, plan.name); err == nil {
			return runError(http.StatusConflict, reasonKeptMachineNameTaken, keptMachineNameTakenMsg(plan.name))
		} else if !errors.Is(err, store.ErrNotFound) {
			return runServerError("read kept machines", err)
		}
	}
	plan.base, plan.prepared = keptCurrentBase(mac.caps, in.req.MacOSBase)
	return nil
}

// keptMacLimitRefusal is the Mac's kept-disk limit: 0 keeps none, and another machine at its worst case
// must fit beside the worst case of those kept. nil when the Mac has not counted yet.
func keptMacLimitRefusal(mac string, k *runnerwire.MacVMKept) *runRefusal {
	if k == nil {
		return nil
	}
	if k.LimitBytes <= 0 {
		return runError(http.StatusUnprocessableEntity, reasonKeptMachineMacLimit, keptMachineMacOffMsg(mac))
	}
	if over := k.WorstBytes + keptMachineWorstBytes - k.LimitBytes; over > 0 {
		return runError(http.StatusUnprocessableEntity, reasonKeptMachineMacLimit, keptMachineMacLimitMsg(mac, k.LimitBytes>>30, wholeGiB(over)))
	}
	return nil
}

// wholeGiB is b in whole GiB, rounded up.
func wholeGiB(b int64) int64 { return (b + 1<<30 - 1) >> 30 }

// keptCurrentBase is the base a new or fresh machine clones on the Mac now: name ("" is Standard), and when
// it was prepared (zero when the Mac has not said).
func keptCurrentBase(mc *runnerwire.MacVMCaps, name string) (string, time.Time) {
	name = cmp.Or(name, types.MacOSStandardBase)
	if mc == nil {
		return name, time.Time{}
	}
	base, _, _ := macOSChosenBase(mc, []macOSBaseLimit{{Base: name}})
	at, _ := time.Parse(time.RFC3339, base.CreatedAt)
	return name, at
}

// keptResumeRefusal is a resume's own checks: the machine is the caller's, kept, not expired, its base
// young enough and allowed, and the run is on the machine's own Mac, online, keeping machines.
func (s *Server) keptResumeRefusal(ctx context.Context, st keptMachineStore, in keptMachineAsk, mac keptMac, plan *keptMachinePlan) *runRefusal {
	ref := strings.TrimSpace(in.req.Machine.Resume)
	km, err := st.FindKeptMachineOf(ctx, plan.owner, ref)
	if errors.Is(err, store.ErrNotFound) {
		return runError(http.StatusNotFound, reasonKeptMachineNotFound, keptMachineNotFoundMsg(ref))
	}
	if err != nil {
		return runServerError("read kept machines", err)
	}
	if rf := s.keptStateRefusal(ctx, km); rf != nil {
		return rf
	}
	now := s.cfg.Now().UTC()
	if !now.Before(km.ExpiresAt) {
		return s.keptExpiredRefusal(ctx, km, in.create)
	}
	limit, err := s.macOSBaseLimitOf(ctx)
	if err != nil {
		return runServerError("read site config", err)
	}
	if age := int(now.Sub(km.BasePreparedAt).Hours() / 24); !in.req.Machine.Fresh && limit.MaxDays > 0 && age > limit.MaxDays {
		return runError(http.StatusConflict, reasonKeptMachineBaseTooOld, keptMachineBaseTooOldMsg(km.Name, age, limit.MaxDays))
	}
	if !in.req.Machine.Fresh {
		pending, err := st.KeptMachineResetPending(ctx, km.ID)
		if err != nil {
			return runServerError("read kept machines", err)
		}
		if pending {
			return runError(http.StatusConflict, reasonKeptMachineResetUnconfirmed, keptMachineResetUnconfirmedMsg(km.Name))
		}
	}
	if km.Base != types.MacOSStandardBase {
		if rf := s.macOSBaseDeniedRefusal(ctx, km.Base, in.ceiling); rf != nil {
			return rf
		}
	}
	if mac.runner.ID != km.RunnerID {
		return runError(http.StatusConflict, reasonKeptMachineOffline, keptMachineElsewhereMsg(km.Name, s.keptRunnerName(ctx, km.RunnerID)))
	}
	if rf := keptRunnerRefusal(mac, in.queued); rf != nil {
		return rf
	}
	plan.mode, plan.id, plan.name, plan.base, plan.prepared = keptModeResume, km.ID, km.Name, km.Base, km.BasePreparedAt
	if in.req.Machine.Fresh {
		plan.mode = keptModeFresh
		plan.base, plan.prepared = keptCurrentBase(mac.caps, km.Base)
	}
	return nil
}

// keptExpiredRefusal is the refusal of a resume of an expired machine. A create starts its delete now;
// the sentence says it was deleted only once the Mac confirmed it, and that it will be until then. The
// dry doors delete nothing.
func (s *Server) keptExpiredRefusal(ctx context.Context, km types.KeptMachine, create bool) *runRefusal {
	if ks, ok := s.cfg.Store.(store.KeptMachineStore); ok && create {
		if d := s.expireKeptMachine(ctx, ks, km); keptMachineDeleteConfirmed(d.outcome) {
			days := types.KeptMachineIdleDaysDefault
			if sc, err := s.cfg.Store.GetSiteConfig(ctx); err == nil {
				days = sc.MacOSVM.KeptMachineIdleDaysOrDefault()
			}
			return runError(http.StatusGone, reasonKeptMachineExpired, keptMachineExpiredDeletedMsg(km.Name, days))
		}
	}
	return runError(http.StatusGone, reasonKeptMachineExpired, keptMachineExpiredMsg)
}

// keptStateRefusal refuses a machine being deleted, or one a run holds.
func (s *Server) keptStateRefusal(ctx context.Context, km types.KeptMachine) *runRefusal {
	switch {
	case km.State == types.KeptMachineDeleting || km.DeleteStartedAt != nil:
		return runError(http.StatusConflict, reasonKeptMachineDeleting, keptMachineDeletingMsg(km.Name))
	case km.State != types.KeptMachineKept:
		return runError(http.StatusConflict, reasonKeptMachineInUse, keptMachineInUseMsg(km.Name, s.keptHolderLabel(ctx, km.RunID)))
	}
	return nil
}

// keptHolderLabel names the run that holds a machine: the run's title, else its short id.
func (s *Server) keptHolderLabel(ctx context.Context, runID *uuid.UUID) string {
	if runID == nil {
		return "another run"
	}
	if run, err := s.cfg.Store.GetRun(ctx, *runID); err == nil && strings.TrimSpace(run.Title) != "" {
		return strings.TrimSpace(run.Title)
	}
	return runID.String()[:8]
}

// keptRunnerName is the name of the Mac a machine is kept on, "its Mac" when it cannot be read.
func (s *Server) keptRunnerName(ctx context.Context, id uuid.UUID) string {
	if rs, ok := s.cfg.Store.(macWindowRunnerStore); ok {
		if rn, err := rs.GetRunner(ctx, id); err == nil {
			return macRunnerName(rn)
		}
	}
	return "its Mac"
}

// keptResumeOf is the caller's machine a resume names, which placement holds the run to (keptPlacementPin)
// and a fresh start takes its base from (keptFreshBase); nil for any other request, or a machine that
// cannot be found, which stepKeptMachine then refuses with its own sentence.
func (s *Server) keptResumeOf(r *http.Request, req createRunRequest) *types.KeptMachine {
	if req.Machine == nil || strings.TrimSpace(req.Machine.Resume) == "" || !types.MachineKind(req.MachineKind).IsMacOSVM() {
		return nil
	}
	st, ok := s.cfg.Store.(keptMachineStore)
	owner := types.CanonicalUserSubject(oidcHumanFromContext(r.Context()))
	if !ok || owner == "" {
		return nil
	}
	km, err := st.FindKeptMachineOf(r.Context(), owner, strings.TrimSpace(req.Machine.Resume))
	if err != nil {
		return nil
	}
	return &km
}

// keptPinOf is the Mac machine km is kept on; nil without one.
func keptPinOf(km *types.KeptMachine) *uuid.UUID {
	if km == nil {
		return nil
	}
	return &km.RunnerID
}

// keptFreshBase makes a fresh start's base its machine's own, before placement and its checks read it, so
// the run records it and its sandbox spec asks the Mac for it. A request that names another base is
// refused: a fresh start keeps the machine's base.
func (s *Server) keptFreshBase(req *createRunRequest, km *types.KeptMachine) *runRefusal {
	if km == nil || req.Machine == nil || !req.Machine.Fresh {
		return nil
	}
	if req.MacOSBase != "" && req.MacOSBase != km.Base {
		return runError(http.StatusUnprocessableEntity, reasonKeptMachineFreshBase, keptMachineFreshBaseMsg(km.Base))
	}
	if km.Base != types.MacOSStandardBase {
		req.MacOSBase = km.Base
	}
	return nil
}

// keptPlacementPin is placement's drive pin with a resumed machine's Mac joined: the resume goes to the
// Mac its machine is kept on, as a disk drive's run goes to its Mac. A run whose drives are bound
// elsewhere keeps no pin; stepKeptMachine refuses it there.
func keptPlacementPin(drives bool, driveRunner, machineRunner *uuid.UUID) (bool, *uuid.UUID) {
	switch {
	case machineRunner == nil:
		return drives, driveRunner
	case drives && (driveRunner == nil || *driveRunner != *machineRunner):
		return true, nil
	}
	return true, machineRunner
}

// auditKeptMachineDenied writes a create's denied machine.create or machine.resume row for rf, with no
// run to name yet. The dry doors pass create false and write none.
func (s *Server) auditKeptMachineDenied(r *http.Request, req createRunRequest, rf *runRefusal, create bool) {
	if !create || req.Machine == nil {
		return
	}
	at, principal := actorFromRequest(r)
	action, data := keptAuditAction(req.Machine.Resume != ""), map[string]any{"reason": macWindowRefusalReason(rf)}
	if req.Machine.Resume != "" {
		data["fresh"] = req.Machine.Fresh
	} else if req.Machine.Name != "" {
		data["name"] = req.Machine.Name
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, at, principal, action, "create", "denied", mustJSON(data)))
}

// keptAuditAction is the row a keep or a resume writes.
func keptAuditAction(resume bool) string {
	if resume {
		return "machine.resume"
	}
	return "machine.create"
}

// keptDispatch is the machine dispatch inserted or claimed for a run, and how it came by it.
type keptDispatch struct {
	plan    *keptMachinePlan
	machine types.KeptMachine
}

// spec is the runner's KeptMachineSpec for the machine; nil for a run that keeps none.
func (k keptDispatch) spec() *runner.KeptMachineSpec {
	if k.plan == nil {
		return nil
	}
	return &runner.KeptMachineSpec{ID: k.machine.ID, Mode: k.plan.mode, OwnerHash: types.DriveSubjectHash(k.plan.owner),
		IdleDays: k.plan.idleDays, Name: k.machine.Name}
}

// runKeptMachineStore is the run's record of its machine (store.RunKeptMachineStore).
type runKeptMachineStore interface {
	CreateKeptMachineForRun(ctx context.Context, m types.KeptMachine, limit int) (types.KeptMachine, error)
	ClaimKeptMachineForRun(ctx context.Context, id uuid.UUID, owner string, runID uuid.UUID, mode string) (types.KeptMachine, error)
	SetRunKeptMachine(ctx context.Context, runID uuid.UUID, id *uuid.UUID, mode string) error
	RunKeptMachine(ctx context.Context, runID uuid.UUID) (*uuid.UUID, string, error)
	MarkKeptMachineResetting(ctx context.Context, id, runID uuid.UUID) error
	ClearKeptMachineResetting(ctx context.Context, id, runID uuid.UUID) error
	ResetKeptMachineBase(ctx context.Context, id, runID uuid.UUID, base string, preparedAt time.Time) error
}

// keptRunGone is the reason of an insert or claim for a run a kill or stop already ended: nothing was
// made or changed, and the run is not failed again.
const keptRunGone = "run_not_starting"

// keptMachineAtDispatch inserts (keep) or claims (resume, fresh) the plan's machine for run, before any
// runner work, and records it on the run. The insert counts the caller's machines and checks the name
// under one lock; the claim is one owner-bound compare-and-swap, so two resumes never both get a machine.
// The insert or claim holds the run's row and needs it still starting: a kill that lands first leaves no
// machine, one that lands after finds it on the run. A race the fold could not see fails the run with
// the sentence the create would have answered. A resume of a machine whose last run still holds it in
// its grace ends that run now. ok false means the run has ended or been failed. A fresh start's base is
// not written here: keptMachineUp records it once the Mac has made it.
func (s *Server) keptMachineAtDispatch(ctx context.Context, run types.AgentRun, plan *keptMachinePlan) (keptDispatch, bool) {
	if plan == nil {
		return keptDispatch{}, true
	}
	st, ok := s.cfg.Store.(keptMachineStore)
	rs, rok := s.cfg.Store.(runKeptMachineStore)
	if !ok || !rok {
		s.failKeptDispatch(ctx, run, plan, reasonKeptMachineUnavailable, "This run was not launched: this server keeps no kept machines.")
		return keptDispatch{}, false
	}
	var km types.KeptMachine
	var reason, msg string
	if plan.mode == keptModeNew {
		km, reason, msg = s.insertKeptMachine(ctx, st, rs, run, plan)
	} else {
		km, reason, msg = s.claimKeptMachine(ctx, st, rs, run, plan)
	}
	if reason == keptRunGone {
		return keptDispatch{}, false
	}
	if msg != "" {
		s.failKeptDispatch(ctx, run, plan, reason, msg)
		return keptDispatch{}, false
	}
	return keptDispatch{plan: plan, machine: km}, true
}

// insertKeptMachine makes a new machine for run in state creating. A name the server picked that a race
// took is picked again; a name the person asked for is refused.
func (s *Server) insertKeptMachine(ctx context.Context, st keptMachineStore, rs runKeptMachineStore, run types.AgentRun, plan *keptMachinePlan) (types.KeptMachine, string, string) {
	runID := run.ID
	for range 3 {
		name := plan.name
		if name == "" {
			var err error
			if name, err = s.pickKeptMachineName(ctx, st, plan.owner); err != nil {
				return types.KeptMachine{}, reasonKeptMachineUnavailable, "This run was not launched: its kept machine could not be made."
			}
		}
		km, err := rs.CreateKeptMachineForRun(ctx, types.KeptMachine{ID: uuid.New(), Name: name, OwnerSubject: plan.owner, RunnerID: plan.runner,
			Base: plan.base, BasePreparedAt: plan.prepared, RunID: &runID,
			ExpiresAt: s.cfg.Now().UTC().AddDate(0, 0, plan.idleDays)}, plan.limit)
		switch {
		case err == nil:
			s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "machine.create", km.ID.String(), "success",
				mustJSON(keptAuditData(km, run.ID))))
			return km, "", ""
		case errors.Is(err, store.ErrRunNotDispatching):
			return types.KeptMachine{}, keptRunGone, ""
		case errors.Is(err, store.ErrKeptMachineLimit):
			return types.KeptMachine{}, reasonKeptMachinePersonLimit, keptMachinePersonLimitMsg(plan.limit, plan.limit)
		case errors.Is(err, store.ErrKeptMachineNameTaken) && plan.name == "":
			continue
		case errors.Is(err, store.ErrKeptMachineNameTaken):
			return types.KeptMachine{}, reasonKeptMachineNameTaken, keptMachineNameTakenMsg(name)
		default:
			slog.WarnContext(ctx, "wardynd: make a kept machine", slog.String("run_id", run.ID.String()), slog.Any("err", err))
			return types.KeptMachine{}, reasonKeptMachineUnavailable, "This run was not launched: its kept machine could not be made."
		}
	}
	return types.KeptMachine{}, reasonKeptMachineNameTaken, "This run was not launched: no free name for its kept machine was found. Name it, and launch it again."
}

// pickKeptMachineName is the name of a machine the person did not name: "machine-N" for the smallest N
// none of theirs has (names compare without case).
func (s *Server) pickKeptMachineName(ctx context.Context, st keptMachineStore, owner string) (string, error) {
	mine, err := st.ListKeptMachinesOf(ctx, owner)
	if err != nil {
		return "", err
	}
	return nextKeptMachineName(mine), nil
}

// nextKeptMachineName is "machine-N" for the smallest N no machine of mine is named.
func nextKeptMachineName(mine []types.KeptMachine) string {
	taken := map[string]bool{}
	for _, m := range mine {
		taken[strings.ToLower(m.Name)] = true
	}
	for n := 1; ; n++ {
		if name := "machine-" + strconv.Itoa(n); !taken[name] {
			return name
		}
	}
}

// claimKeptMachine gives the plan's machine to run in one owner-bound compare-and-swap, then ends the run
// that kept it in its grace, if one still does.
func (s *Server) claimKeptMachine(ctx context.Context, st keptMachineStore, rs runKeptMachineStore, run types.AgentRun, plan *keptMachinePlan) (types.KeptMachine, string, string) {
	before, err := st.GetKeptMachine(ctx, plan.id)
	if err != nil || before.OwnerSubject != plan.owner {
		return types.KeptMachine{}, reasonKeptMachineNotFound, keptMachineNotFoundMsg(plan.name)
	}
	if !s.cfg.Now().UTC().Before(before.ExpiresAt) {
		return types.KeptMachine{}, reasonKeptMachineExpired, keptMachineExpiredMsg
	}
	km, err := rs.ClaimKeptMachineForRun(ctx, plan.id, plan.owner, run.ID, plan.mode)
	switch {
	case errors.Is(err, store.ErrRunNotDispatching):
		return types.KeptMachine{}, keptRunGone, ""
	case errors.Is(err, store.ErrKeptMachineExpired):
		return types.KeptMachine{}, reasonKeptMachineExpired, keptMachineExpiredMsg
	case errors.Is(err, store.ErrKeptMachineResetUnconfirmed):
		return types.KeptMachine{}, reasonKeptMachineResetUnconfirmed, keptMachineResetUnconfirmedMsg(before.Name)
	case errors.Is(err, store.ErrKeptMachineInUse):
		return types.KeptMachine{}, reasonKeptMachineInUse, keptMachineInUseMsg(before.Name, s.keptHolderLabel(ctx, before.RunID))
	case errors.Is(err, store.ErrKeptMachineDeleting):
		return types.KeptMachine{}, reasonKeptMachineDeleting, keptMachineDeletingMsg(before.Name)
	case errors.Is(err, store.ErrNotFound):
		return types.KeptMachine{}, reasonKeptMachineNotFound, keptMachineNotFoundMsg(before.Name)
	case err != nil:
		slog.WarnContext(ctx, "wardynd: claim a kept machine", slog.String("run_id", run.ID.String()), slog.Any("err", err))
		return types.KeptMachine{}, reasonKeptMachineUnavailable, "This run was not launched: its kept machine could not be claimed."
	}
	data := keptAuditData(km, run.ID)
	data["fresh"], data["last_scrubbed"] = plan.mode == keptModeFresh, before.Scrubbed
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "machine.resume", km.ID.String(), "success", mustJSON(data)))
	if before.RunID != nil && *before.RunID != run.ID {
		s.endKeptGraceRun(ctx, *before.RunID)
	}
	return km, "", ""
}

// keptAuditData is the fields machine.create and machine.resume share.
func keptAuditData(km types.KeptMachine, runID uuid.UUID) map[string]any {
	return map[string]any{"machine_id": km.ID, "name": km.Name, "runner_id": km.RunnerID, "base": km.Base, "run_id": runID}
}

// endKeptGraceRun ends the run that last held a resumed machine when it is still kept after its end, or
// lost (the ended-run grace): a macOS run cannot be revived, so the grace holds nothing its person loses,
// and the end drops its proxy config and CA key. Its row says the resume ended it.
func (s *Server) endKeptGraceRun(ctx context.Context, runID uuid.UUID) {
	leaser, ok := s.cfg.Store.(store.RunLeaser)
	if !ok {
		return
	}
	old, err := s.cfg.Store.GetRun(ctx, runID)
	if err != nil || old.State != types.RunRunning || !runIsKept(old) {
		return
	}
	action, data := "run.ended.expired", map[string]any{"ended_at": old.LostAt, "cause": "machine_resumed"}
	if old.LostReason != types.LostEnded {
		action, data = "run.lost.expired", map[string]any{"lost_at": old.LostAt, "reason": string(old.LostReason), "cause": "machine_resumed"}
	}
	s.stopKeptRun(ctx, leaser, old, types.RunStopped, action, data)
}

// failKeptDispatch fails run at dispatch for its machine, and writes the denied machine row.
func (s *Server) failKeptDispatch(ctx context.Context, run types.AgentRun, plan *keptMachinePlan, reason, msg string) {
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", keptAuditAction(plan.mode != keptModeNew), run.ID.String(), "denied",
		mustJSON(map[string]any{"reason": reason, "run_id": run.ID, "fresh": plan.mode == keptModeFresh})))
	s.failAndRevoke(ctx, run.ID, types.RunStarting, msg)
}

// keptMachineResetting marks a fresh start's machine as being reset just before its Mac is asked to, so a
// reset that may or may not have happened blocks a normal resume (keptResumeRefusal) until a fresh start
// confirms one. A mark that cannot be written fails the run before the Mac is asked; false then.
func (s *Server) keptMachineResetting(ctx context.Context, run types.AgentRun, k keptDispatch) bool {
	rs, ok := s.cfg.Store.(runKeptMachineStore)
	if k.plan == nil || k.plan.mode != keptModeFresh || !ok {
		return true
	}
	if err := rs.MarkKeptMachineResetting(ctx, k.machine.ID, run.ID); err != nil {
		slog.WarnContext(ctx, "wardynd: mark a kept machine's fresh start", slog.String("run_id", run.ID.String()), slog.Any("err", err))
		s.failKeptDispatch(ctx, run, k.plan, reasonKeptMachineUnavailable, "This run was not launched: its kept machine's fresh start could not be recorded.")
		return false
	}
	return true
}

// keptResetNotAsked takes back the mark of a fresh start whose Mac was never asked to create the sandbox.
func (s *Server) keptResetNotAsked(ctx context.Context, run types.AgentRun, k keptDispatch) {
	rs, ok := s.cfg.Store.(runKeptMachineStore)
	if k.plan == nil || k.plan.mode != keptModeFresh || !ok {
		return
	}
	if err := rs.ClearKeptMachineResetting(context.WithoutCancel(ctx), k.machine.ID, run.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		slog.WarnContext(ctx, "wardynd: clear a kept machine's fresh start", slog.String("run_id", run.ID.String()), slog.Any("err", err))
	}
}

// keptMachineUp runs once the sandbox is up: a new machine moves from creating to in use, and a fresh
// start, which the Mac has now made, records the base its disk was cloned from (sb.KeptBase, the Mac's
// record after the boot) and clears its mark. A Mac that does not report it keeps the mark: a normal
// resume stays refused rather than read an age nobody knows.
func (s *Server) keptMachineUp(ctx context.Context, run types.AgentRun, k keptDispatch, sb runner.Sandbox) {
	if k.plan == nil {
		return
	}
	s.auditKeptBootScrub(ctx, run.ID, k.machine.ID, sb.KeptScrub)
	switch k.plan.mode {
	case keptModeNew:
		if st, ok := s.cfg.Store.(keptMachineStore); ok {
			if _, err := st.BeginKeptMachineUse(ctx, k.machine.ID, k.plan.owner, run.ID); err != nil {
				slog.WarnContext(ctx, "wardynd: mark a kept machine in use", slog.String("run_id", run.ID.String()), slog.Any("err", err))
			}
		}
	case keptModeFresh:
		rs, ok := s.cfg.Store.(runKeptMachineStore)
		if !ok || sb.KeptBase == nil || sb.KeptBase.PreparedAt.IsZero() {
			return
		}
		base := cmp.Or(sb.KeptBase.Name, k.plan.base)
		if err := rs.ResetKeptMachineBase(ctx, k.machine.ID, run.ID, base, sb.KeptBase.PreparedAt.UTC()); err != nil {
			slog.WarnContext(ctx, "wardynd: record a fresh kept machine's base", slog.String("run_id", run.ID.String()), slog.Any("err", err))
		}
	}
}

// auditKeptBootScrub writes machine.scrub for the boot scrub a resumed machine's Mac reported at create;
// nothing when it reported none (a new machine, or a Mac that predates the report).
func (s *Server) auditKeptBootScrub(ctx context.Context, runID, machineID uuid.UUID, scrub *runner.KeptScrub) {
	if scrub == nil {
		return
	}
	result := "success"
	if scrub.Outcome == keptScrubFailed {
		result = "failure"
	}
	s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "machine.scrub", machineID.String(), result, mustJSON(map[string]any{
		"machine_id": machineID, "run_id": runID, "when": cmp.Or(scrub.When, "boot"), "outcome": scrub.Outcome, "removed": scrub.Removed,
		"ca_sha256": scrub.CAs,
	})))
}

// releaseUnstartedKeptMachine, deferred by dispatch, gives back the machine of a run whose sandbox was never
// created: a new machine is dropped (and the run no longer names it), a resumed one is kept again. Kept
// again with scrubbed false: no stop report came, so nothing says Wardyn cleaned it.
func (s *Server) releaseUnstartedKeptMachine(ctx context.Context, run types.AgentRun, k keptDispatch, created *bool) {
	st, ok := s.cfg.Store.(keptMachineStore)
	if *created || k.plan == nil || !ok {
		return
	}
	ctx = context.WithoutCancel(ctx)
	if k.plan.mode == keptModeNew {
		err := st.DropUnstartedKeptMachine(ctx, k.machine.ID, run.ID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			slog.WarnContext(ctx, "wardynd: drop the kept machine of a run that never started", slog.String("run_id", run.ID.String()), slog.Any("err", err))
		}
		if err == nil { // insertKeptMachine audited machine.create as a success: say the machine never existed
			s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "machine.create", k.machine.ID.String(), "failure",
				mustJSON(map[string]any{"reason": "sandbox_not_created", "machine_id": k.machine.ID, "run_id": run.ID})))
		}
		if rs, ok := s.cfg.Store.(runKeptMachineStore); ok {
			_ = rs.SetRunKeptMachine(ctx, run.ID, nil, "")
		}
		return
	}
	stop := keptUnstartedStop(k.machine, run.ID)
	if _, err := st.RecordKeptMachineStop(ctx, stop); err != nil && !errors.Is(err, store.ErrNotFound) {
		slog.WarnContext(ctx, "wardynd: give back the kept machine of a run that never started", slog.String("run_id", run.ID.String()), slog.Any("err", err))
	}
}

// keptUnstartedStop is the release of a resumed machine whose run never started: kept as it was, but
// never marked cleaned, since no stop report says so.
func keptUnstartedStop(km types.KeptMachine, runID uuid.UUID) store.KeptMachineStop {
	return store.KeptMachineStop{ID: km.ID, RunnerID: km.RunnerID, RunID: runID, Scrubbed: false, DiskBytes: km.DiskBytes, ExpiresAt: km.ExpiresAt}
}

// keptSpecSecretKinds is the lasting-credential check at dispatch, over the classified spec of a run that keeps a machine:
// every secret env key and every file under the component secret folder must be one the platform minted
// for this run alone. Anything else (a stored secret, an SSH key, a resident git token, a Bedrock sign-in's
// role credentials) is named by its kind; an origin that names no kind counts by where it lands.
func keptSpecSecretKinds(spec runner.SandboxSpec, origins map[string]placement.CredentialOrigin) []string {
	has := map[string]bool{}
	note := func(key, fallback string) {
		o, ok := origins[key]
		if ok && o.Class == placement.ClassPlatform {
			return
		}
		switch o.GrantKind {
		case types.GrantFileSecret:
			has[keptKindFile] = true
		case types.GrantSSHKey:
			has[keptKindSSH] = true
		case types.GrantGitPAT, types.GrantGitHubToken:
			has[keptKindGit] = true
		case types.GrantEnvSecret:
			has[keptKindEnv] = true
		default:
			has[fallback] = true
		}
	}
	for key := range spec.SecretEnv {
		note("SandboxSpec.SecretEnv["+key+"]", keptKindEnv)
	}
	for i, f := range spec.ManagedFiles {
		if strings.HasPrefix(f.Path, runner.ComponentSecretDir+"/") {
			note(placementCredentialIndex("SandboxSpec.ManagedFiles", i), keptKindFile)
		}
	}
	var out []string
	for _, k := range keptSecretKinds {
		if has[k] {
			out = append(out, k)
		}
	}
	return out
}

// keptSecretRefusal fails a run that keeps a machine and whose classified spec carries a lasting
// credential (keptSpecSecretKinds); true when it did. The caller asks only for a run with a machine.
func (s *Server) keptSecretRefusal(ctx context.Context, run types.AgentRun, spec runner.SandboxSpec, origins map[string]placement.CredentialOrigin) bool {
	kinds := keptSpecSecretKinds(spec, origins)
	if len(kinds) == 0 {
		return false
	}
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.dispatch", run.ID.String(), "failure",
		mustJSON(map[string]any{"reason": reasonKeptMachineResidentSecrets, "kinds": kinds})))
	s.failAndRevoke(ctx, run.ID, types.RunStarting, keptMachineResidentSecretsMsg(kinds))
	return true
}

// runMachineView is GET /runs/{id}'s machine: the kept machine the run kept or resumed, read now. A
// machine whose row is gone is {id, deleted}; who deleted it stays in the audit log. Every field but
// the run's own mode is the machine's row as it is now, and the row is shared by every run that used the
// machine: HolderRunID names the run that holds it now or held it last (a stop does not clear it), so
// the console says what a stop did to the machine only on that run's page. Base, DiskBytes (what the
// last stop wrote) and ExpiresAt are the row's. last_used_at is left out: a claim overwrites it with
// the claim's time, so a resumed run's page could not say when the machine was last used before it.
type runMachineView struct {
	ID          uuid.UUID              `json:"id"`
	Name        string                 `json:"name,omitempty"`
	Mode        string                 `json:"mode,omitempty"`
	KeepAtEnd   bool                   `json:"keep_at_end,omitempty"`
	State       types.KeptMachineState `json:"state,omitempty"`
	Scrubbed    bool                   `json:"scrubbed"`
	RunnerName  string                 `json:"runner_name,omitempty"`
	HolderRunID *uuid.UUID             `json:"holder_run_id,omitempty"`
	Base        string                 `json:"base,omitempty"`
	DiskBytes   int64                  `json:"disk_bytes,omitempty"`
	ExpiresAt   *time.Time             `json:"expires_at,omitempty"`
	Deleted     bool                   `json:"deleted"`
}

// runMachineOf is run's machine view, nil for a run that keeps none or a store that cannot say.
func (s *Server) runMachineOf(ctx context.Context, run types.AgentRun) *runMachineView {
	rs, ok := s.cfg.Store.(runKeptMachineStore)
	st, sok := s.cfg.Store.(keptMachineStore)
	if !ok || !sok {
		return nil
	}
	id, mode, err := rs.RunKeptMachine(ctx, run.ID)
	if err != nil || id == nil {
		return nil
	}
	v := &runMachineView{ID: *id, Mode: runMachineMode(mode), KeepAtEnd: true}
	km, err := st.GetKeptMachine(ctx, *id)
	if errors.Is(err, store.ErrNotFound) {
		v.Deleted, v.KeepAtEnd = true, false
		return v
	}
	if err != nil {
		slog.WarnContext(ctx, "wardynd: read a run's kept machine", slog.String("run_id", run.ID.String()), slog.Any("err", err))
		return nil
	}
	v.Name, v.State, v.Scrubbed, v.RunnerName = km.Name, km.State, km.Scrubbed, s.keptRunnerName(ctx, km.RunnerID)
	v.HolderRunID, v.Base, v.DiskBytes = km.RunID, km.Base, km.DiskBytes
	if !km.ExpiresAt.IsZero() {
		v.ExpiresAt = &km.ExpiresAt
	}
	return v
}

// runMachineMode is the run read's mode for a stored one: new, resumed or fresh.
func runMachineMode(mode string) string {
	if mode == keptModeResume {
		return "resumed"
	}
	return mode
}
