// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A Windows VM run is a desktop-only interactive run of an image an admin registered and bound to a licence
// pool. The database holds the licence fence (migration 0401's triggers): the insert takes the seat and only
// the run's end frees it. The steps here give the friendly refusal before the insert; the triggers stay the
// fence for every path.

const (
	windowsVMsOffMsg          = "Windows VM sandboxes are turned off. An admin turns them on with features.windows_vm.enabled in the site config."
	windowsImageRequiredMsg   = "A Windows VM run starts from a registered Windows image: name it in windows_image."
	windowsNotAPersonMsg      = "A Windows VM run is started by a person signed in to Wardyn, not by a service identity, a schedule or a token."
	windowsTokenMsg           = "A Windows VM run is started by a person signed in interactively, not with an API token, the CLI's sign-in token or a token delegated to a portal."
	windowsRecordingMsg       = "Your organisation or this pool requires recording, and a Windows VM run cannot be recorded."
	windowsRecordedLevelMsg   = "This run would be recorded at %s, and a Windows VM run cannot be recorded. Choose recording off in the run's policy."
	windowsNotOrgRunnerMsg    = "A Windows VM runs only on an organisation runner that offers Windows VM sandboxes, never on the deployment's own executor or a runner a person registered."
	windowsRunnerNoCapsMsg    = "This runner does not offer Windows VM sandboxes: it is older than 0.11, or its operator has not turned them on."
	windowsVMFencedMsg        = "The Windows licence fence refused this change to a Windows VM run."
	windowsLicencesMissingMsg = "This server keeps no Windows licence pools: they need the Postgres store."
)

// windowsFold is what stepWindowsImage resolved for a windows_vm run; zero for every other kind.
type windowsFold struct {
	windowsRef    string               // the registered image's ref: the run row's image at insert
	windowsLaunch *store.WindowsLaunch // the image, pool and holder the seat is taken through
}

// windowsImageInput is the run's image_input: a Windows run's is its registered image's name, any other
// run's the digest rule's input.
func (w windowsFold) windowsImageInput(digestInput string) string {
	if w.windowsLaunch != nil {
		return w.windowsLaunch.Image.Name
	}
	return digestInput
}

func windowsDeskOnlyMsg(what string) string {
	return "A Windows VM run is desktop-only: no " + what + "."
}

func windowsFeatureRefused(status int, field, what string) *runRefusal {
	rf := runError(status, reasonWindowsVMFeatureRefused, windowsDeskOnlyMsg(what))
	rf.body.Field = field
	return rf
}

// windowsVMDoorRefusal is the 409 a door answers on a live Windows VM run, nil for any other run. The web
// application gateway passes, because the desktop rides it; every other door, a later one included, refuses.
func windowsVMDoorRefusal(run types.AgentRun, door string) *runRefusal {
	if run.MachineKind != types.MachineWindowsVM || door == "web application gateway" {
		return nil
	}
	return runError(http.StatusConflict, reasonWindowsVMFeatureRefused, windowsDeskOnlyMsg(door))
}

// stepWindowsImage holds a windows_vm request to the switch, a person, a desktop-only shape and a registered
// image whose pool the caller holds a free seat in. It runs after stepPolicy, before any folder, drive,
// credential or image step. A refusal at create writes a run.create denied row.
func (s *Server) stepWindowsImage(f *runFold, _ *foldRecorder) bool {
	if types.MachineKind(f.req.MachineKind) != types.MachineWindowsVM {
		return true
	}
	launch, rf := s.windowsImageDecision(f.r, *f.req, f.spec)
	if rf == nil {
		f.windowsRef, f.windowsLaunch = launch.Image.Ref, &launch
		return true
	}
	if f.mode == foldCreate {
		s.auditWindowsDenied(f.r, *f.req, rf)
	}
	return !rf.write(s, f.w, f.r)
}

func (s *Server) windowsImageDecision(r *http.Request, req createRunRequest, spec types.RunPolicySpec) (store.WindowsLaunch, *runRefusal) {
	ctx := r.Context()
	if rf := s.windowsVMsOffRefusal(ctx); rf != nil {
		return store.WindowsLaunch{}, rf
	}
	at, principal := actorFromRequest(r)
	_, _, service := serviceIdentityFromContext(ctx)
	if s.cfg.LocalMode || at != types.ActorHuman || principal == "" || oidcHumanFromContext(ctx) == "" || service ||
		req.Service != nil || req.StartAt != "" {
		return store.WindowsLaunch{}, runError(http.StatusForbidden, reasonWindowsVMFeatureRefused, windowsNotAPersonMsg)
	}
	if _, delegated := audit.DelegationFrom(ctx); delegated || apiTokenIDFromContext(ctx) != uuid.Nil {
		return store.WindowsLaunch{}, runError(http.StatusForbidden, reasonWindowsVMFeatureRefused, windowsTokenMsg)
	}
	if rf := windowsShapeRefusal(req, spec); rf != nil {
		return store.WindowsLaunch{}, rf
	}
	ws, ok := s.cfg.Store.(store.WindowsLicenceStore)
	if !ok {
		return store.WindowsLaunch{}, runError(http.StatusNotImplemented, reasonWindowsLicencesUnavailable, windowsLicencesMissingMsg)
	}
	img, rf := windowsImageNamed(ctx, ws, req.WindowsImage)
	if rf != nil {
		return store.WindowsLaunch{}, rf
	}
	launch, err := ws.WindowsLaunchCheck(ctx, img.Ref, principal)
	var inUse *store.WindowsLicenceInUseError
	if errors.As(err, &inUse) && inUse.RunID == promotedRun(ctx) {
		err = nil // a queued run took its seat when it was inserted: its own start finds it held by itself
	}
	if err != nil {
		if rf := windowsLicenceRefusal(err, launch.Pool.Name); rf != nil {
			return store.WindowsLaunch{}, rf
		}
		return store.WindowsLaunch{}, runServerError("check the Windows licence seat", err)
	}
	return launch, nil
}

// windowsVMsOffRefusal is the 403 while features.windows_vm is off; a site config that cannot be read refuses.
func (s *Server) windowsVMsOffRefusal(ctx context.Context) *runRefusal {
	sc, ok := s.siteConfigSnapshot(ctx)
	if !ok {
		return runServerError("read the site config for Windows VMs", errors.New("the site config could not be read"))
	}
	if !siteFeatureOn(sc, featureWindowsVM) {
		return runError(http.StatusForbidden, reasonWindowsVMsOff, windowsVMsOffMsg)
	}
	return nil
}

// windowsShapeRefusal refuses what a Windows VM run cannot have: it has no guest agent, so no agent or
// harness, terminal start, folder, drive, kept machine, credential, sign-in or TLS inspection, and it starts
// only from a registered image.
func windowsShapeRefusal(req createRunRequest, spec types.RunPolicySpec) *runRefusal {
	for _, c := range []struct {
		refused     bool
		field, what string
	}{
		{req.EffectiveExperience() != types.ExperienceInteractive, "experience", "task or background run"},
		{req.Agent != "" || len(req.Tools) > 0 || req.Startup != nil, "agent", "agent or harness"},
		{req.InteractiveStart != "" || strings.TrimSpace(req.Task) != "", "interactive_start", "agent or terminal start"},
		{req.Repo != "" || req.WorkspaceID != nil || len(req.Workspaces) > 0 || req.StartFolder != nil, "workspaces", "folders or repositories"},
		{req.Drive != nil || len(req.Drives) > 0, "drives", "drives"},
		{req.LocalFolder != nil, "local_folder", "computer folders"},
		{req.KeepWorkFolder, "keep_work_folder", "kept work folder"},
		{req.Machine != nil, "machine", "kept machine"},
		{len(req.Components) > 0 || req.ModelProvider != "" || req.IntegrationID != "", "components", "credentials or sign-ins"},
		{len(spec.EligibleGrants) > 0, "eligible_grants", "credentials, sign-ins or secret env"},
		{spec.LLMInspection != nil && spec.LLMInspection.InterceptTLS, "intercept_tls", "TLS inspection"},
	} {
		if c.refused {
			return windowsFeatureRefused(http.StatusForbidden, c.field, c.what)
		}
	}
	for field, v := range map[string]string{"image": req.Image, "allowed_image": req.AllowedImage, "devcontainer_repo": req.DevcontainerRepo} {
		if v != "" {
			rf := runError(http.StatusBadRequest, reasonWindowsImageRequired, windowsImageRequiredMsg+" A raw image in "+field+" is refused.")
			rf.body.Field = field
			return rf
		}
	}
	if strings.TrimSpace(req.WindowsImage) == "" {
		rf := runError(http.StatusBadRequest, reasonWindowsImageRequired, windowsImageRequiredMsg)
		rf.body.Field = "windows_image"
		return rf
	}
	return nil
}

// windowsImageNamed is the active registered image of that name.
func windowsImageNamed(ctx context.Context, ws store.WindowsLicenceStore, name string) (types.WindowsImage, *runRefusal) {
	imgs, err := ws.ListWindowsImages(ctx)
	if err != nil {
		return types.WindowsImage{}, runServerError("list Windows images", err)
	}
	for _, img := range imgs {
		if img.State == types.WindowsImageActive && strings.EqualFold(img.Name, name) { // as the lower(name) unique index
			return img, nil
		}
	}
	rf := runError(http.StatusUnprocessableEntity, reasonWindowsImageUnregistered,
		fmt.Sprintf("No active Windows image is registered as %q. Ask an admin to register it.", name))
	rf.body.Field = "windows_image"
	return types.WindowsImage{}, rf
}

// windowsLicenceRefusal is the refusal for an error of the licence fence or its writers, nil for any other
// error. name is the pool, image or holder the caller asked about, "" when unknown.
func windowsLicenceRefusal(err error, name string) *runRefusal {
	pool := "the Windows licence pool"
	if name != "" {
		pool += " " + name
	}
	var inUse *store.WindowsLicenceInUseError
	var live *store.WindowsSeatLiveError
	switch {
	case errors.As(err, &inUse):
		return runError(http.StatusConflict, reasonWindowsLicenceInUse, fmt.Sprintf("Your seat in %s is in use by run %s. End that run first.", pool, inUse.RunID))
	case errors.Is(err, store.ErrWindowsLicenceInUse):
		return runError(http.StatusConflict, reasonWindowsLicenceInUse, fmt.Sprintf("Your seat in %s is in use by another run. End that run first.", pool))
	case errors.As(err, &live):
		what := name
		if what == "" {
			what = "This Windows licence pool or image"
		}
		return runError(http.StatusConflict, reasonWindowsLicenceSeatLive, fmt.Sprintf("%s is in use by run %s. End that run first.", what, live.RunID))
	case errors.Is(err, store.ErrWindowsVMsOff):
		return runError(http.StatusForbidden, reasonWindowsVMsOff, windowsVMsOffMsg)
	case errors.Is(err, store.ErrWindowsImageUnregistered), errors.Is(err, store.ErrWindowsLicencePoolInactive):
		return runError(http.StatusUnprocessableEntity, reasonWindowsImageUnregistered, "The Windows image is not registered, or its licence pool was deleted. Ask an admin.")
	case errors.Is(err, store.ErrWindowsLicenceNotHolder):
		return runError(http.StatusForbidden, reasonWindowsLicenceNotHolder, fmt.Sprintf("You do not hold a seat in %s. Ask an admin to add you.", pool))
	case errors.Is(err, store.ErrWindowsLicencePoolFull):
		return runError(http.StatusConflict, reasonWindowsLicencePoolFull, fmt.Sprintf("Every seat in %s has a holder. Raise its seats, or remove a holder first.", pool))
	case errors.Is(err, store.ErrWindowsLicenceSeatsBelowHolders):
		return runError(http.StatusConflict, reasonWindowsLicenceConflict, "A Windows licence pool can't have fewer seats than holders. Remove holders first.")
	case errors.Is(err, store.ErrWindowsLicenceStale):
		return runError(http.StatusConflict, reasonWindowsLicenceConflict, "The pool or image changed since you read it. Read it again, then retry with its revision.")
	case errors.Is(err, store.ErrWindowsVMFenced):
		return runError(http.StatusConflict, reasonWindowsVMFenced, windowsVMFencedMsg)
	}
	return nil
}

// writeWindowsLicenceRefusal answers a licence fence error that reached writeServerError, such as the take
// trigger refusing an insert the pre-check passed; false for any other error.
func writeWindowsLicenceRefusal(w http.ResponseWriter, err error) bool {
	rf := windowsLicenceRefusal(err, "")
	if rf == nil {
		return false
	}
	writeErrorReason(w, rf.status, rf.body.Reason, rf.body.Error)
	return true
}

// auditWindowsDenied writes the run.create denied row for a refused Windows VM launch, naming the image asked
// for and the reason. A refused run has no run row.
func (s *Server) auditWindowsDenied(r *http.Request, req createRunRequest, rf *runRefusal) {
	if s.cfg.Audit == nil || rf.body.Reason == "" { // a server error is logged by its writer, not denied
		return
	}
	at, principal := actorFromRequest(r)
	data := map[string]any{"subject": principal, "reason": rf.body.Reason, "machine_kind": string(types.MachineWindowsVM),
		"windows_image": req.WindowsImage}
	if rf.body.Field != "" {
		data["field"] = rf.body.Field
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, at, principal, "run.create", "", "denied", mustJSON(data)))
}

// stepWindowsPlacement holds a placed Windows VM run to an organisation runner that offers Windows VMs, and
// refuses one that would be recorded or declares web apps: the desktop is a Windows run's only app, and it is
// never recorded, so a recording requirement refuses it rather than start it unrecorded.
func (s *Server) stepWindowsPlacement(f *runFold, _ *foldRecorder) bool {
	if types.MachineKind(f.req.MachineKind) != types.MachineWindowsVM {
		return true
	}
	rf := s.windowsPlacementRefusal(f.r, *f.req, f.spec, f.place, f.ceiling, f.policyID == nil)
	if rf == nil {
		return true
	}
	if f.mode == foldCreate {
		s.auditWindowsDenied(f.r, *f.req, rf)
	}
	return !rf.write(s, f.w, f.r)
}

func (s *Server) windowsPlacementRefusal(r *http.Request, req createRunRequest, spec types.RunPolicySpec, place runPlacement,
	ceiling governanceCeiling, policyless bool) *runRefusal {
	if len(spec.UIApps) > 0 {
		return windowsFeatureRefused(http.StatusForbidden, "ui_apps", "web apps beside its desktop")
	}
	if rf := s.windowsRecordingRefusal(r, spec.Recording, place, ceiling, policyless); rf != nil {
		return rf
	}
	ctx := r.Context()
	runners := []*uuid.UUID{}
	if place.pooled {
		if place.placement != types.PlacementLocal {
			return runError(http.StatusUnprocessableEntity, reasonWindowsVMRunnerUnsupported, windowsNotOrgRunnerMsg)
		}
		for _, t := range place.eligible {
			runners = append(runners, t.runnerID)
		}
	} else if id := requestRunnerID(req); requestPlacement(req) == types.PlacementLocal && id != uuid.Nil {
		runners = append(runners, &id)
	}
	if len(runners) == 0 {
		return runError(http.StatusUnprocessableEntity, reasonWindowsVMRunnerUnsupported, windowsNotOrgRunnerMsg)
	}
	for _, id := range runners {
		if rf := s.windowsRunnerRefusal(ctx, id); rf != nil {
			return rf
		}
	}
	return nil
}

// windowsRecordingRefusal refuses a Windows run the organisation would record: under a requirement (the
// organisation's or the pool's), or at any level but off. A deployment with no recording store records
// nothing, and its requirement is refused at placement for every kind.
func (s *Server) windowsRecordingRefusal(r *http.Request, policy types.RecordingLevel, place runPlacement, ceiling governanceCeiling,
	policyless bool) *runRefusal {
	if s.cfg.RecordingStore == nil || s.cfg.Store == nil {
		return nil
	}
	choice, rf := s.createRecordingChoice(r.Context(), r, policy, place, ceiling, policyless)
	if rf != nil {
		return rf
	}
	return windowsRecordingChoiceRefusal(choice)
}

// windowsRecordingChoiceRefusal refuses a recording choice that records anything: a requirement, or a level
// other than off (no level is terminal, as dispatch reads it).
func windowsRecordingChoiceRefusal(choice types.RecordingChoice) *runRefusal {
	var rf *runRefusal
	if choice.Required {
		rf = runError(http.StatusForbidden, reasonWindowsVMFeatureRefused, windowsRecordingMsg)
	} else if level := choice.Resolve(); level != types.RecordingOff {
		rf = runError(http.StatusForbidden, reasonWindowsVMFeatureRefused, fmt.Sprintf(windowsRecordedLevelMsg, cmp.Or(level, types.RecordingTerminal)))
	}
	if rf != nil {
		rf.body.Field = "recording"
	}
	return rf
}

// windowsRunnerRefusal refuses a target that is not an organisation's registered runner reporting Windows VM
// caps: the deployment's executor, a person's own runner, and a Kubernetes runner (which reports none).
func (s *Server) windowsRunnerRefusal(ctx context.Context, id *uuid.UUID) *runRefusal {
	unsupported := runError(http.StatusUnprocessableEntity, reasonWindowsVMRunnerUnsupported, windowsNotOrgRunnerMsg)
	st, ok := s.cfg.Store.(interface {
		GetRunner(context.Context, uuid.UUID) (types.Runner, error)
	})
	if id == nil || !ok {
		return unsupported
	}
	rn, err := st.GetRunner(ctx, *id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return unsupported
	case err != nil:
		return runServerError("read the runner", err)
	case rn.OwnerKind != types.RunnerOwnerOrg:
		return unsupported
	}
	pc, err := s.runnerStreamCapabilities(ctx, *id)
	var ref *placement.Refusal
	switch {
	case errors.As(err, &ref):
		return placementRunRefusal(ref, ref.Detail)
	case err != nil:
		return runServerError("read runner capabilities", err)
	}
	rc, ok := pc.impl.(runnerCaps)
	if ok {
		caps, reported := rc.Caps()
		ok = reported && caps.WindowsVM != nil
	}
	if !ok {
		return runError(http.StatusUnprocessableEntity, reasonWindowsVMRunnerUnsupported, windowsRunnerNoCapsMsg)
	}
	return nil
}

// launchRunImage is the image a created run starts from: a Windows VM run's registered ref, as inserted (the
// fence keeps it), with no build, wrap, admission or SetRunImage; every other run's resolveCreateRunImage.
func (s *Server) launchRunImage(ctx context.Context, req createRunRequest, run types.AgentRun, wsRefs []types.Workspace) (string, []string, bool) {
	if run.MachineKind == types.MachineWindowsVM {
		return run.Image, nil, false
	}
	return s.resolveCreateRunImage(ctx, req, run, wsRefs)
}

// stampWindowsLicence adds a Windows run's image, digest and licence pool to its run.create row.
func stampWindowsLicence(data map[string]any, l *store.WindowsLaunch) {
	if l == nil {
		return
	}
	_, digest, _ := strings.Cut(l.Image.Ref, "@")
	data["windows_image"], data["windows_image_digest"] = l.Image.Name, digest
	data["windows_licence_pool_id"], data["windows_licence_pool"] = l.Pool.ID.String(), l.Pool.Name
}
