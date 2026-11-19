// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/imageadmit"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// imagePin is the ref image admission pinned the run's member image to (canonical name, any tag,
// @sha256:<verified digest>), "" when nothing pinned it: admission off or in audit mode, an operator's run, or
// no member image. Every consumer of the member's image uses the pin when there is one, so an image re-tagged
// after the check is not what runs.
func imagePin(run types.AgentRun) string {
	if v := run.ImageAdmission; v != nil {
		return v.PinnedRef
	}
	return ""
}

// wrapBuiltFromPin reports whether ws's stored wrap may serve a run pinned to pin: always without a pin; with
// one, only when the wrap's current build record says it was built FROM the pin's digest. Any other base
// digest, or a record that names none, is rebuilt FROM the pin. The match is exact, so a wrap built from one
// platform's digest of an index the check admitted is rebuilt rather than reasoned about.
func (s *Server) wrapBuiltFromPin(ctx context.Context, ws types.Workspace, pin string) bool {
	if pin == "" {
		return true
	}
	_, digest, _ := strings.Cut(pin, "@")
	rec, missing, _ := s.imageBuildView(ctx, ws)
	return rec != nil && !missing && digest != "" && rec.Inputs.Base.Digest == digest
}

// imageOriginMember is the origin of an image the member's request or a workspace's base image named. It is
// never recorded: the member's image is the run's ImageInput.
const imageOriginMember = "member"

// launchImageAdmission is the site's image admission block as a launch reads it; an unreadable site config
// refuses, since the mode lives in it.
func (s *Server) launchImageAdmission(ctx context.Context) (*types.SiteImageAdmission, *runRefusal) {
	sc, ok := s.siteConfigSnapshot(ctx)
	if s.cfg.Store != nil && !ok {
		return nil, runServerError("resolve image admission", errImageAdmissionSiteUnreadable)
	}
	if sc.Images == nil {
		return nil, nil
	}
	return sc.Images.Admission, nil
}

// conventionImageAdmission holds the image a run gets by convention (its agent's, its harness set's or an
// architecture variant of either) to image admission when the site's scope is all, and returns the ref to
// dispatch: the pin when enforce admitted it, else image. It runs at launch, where the placed runner's
// architecture has chosen the image. Every launch records origin convention on the run, whatever the mode:
// alone when the image is not checked (admission off, scope member, an operator or the break-glass admin, a
// macOS VM), else in the check's verdict. Under enforce, an admitted image's verdict (the image as configured,
// its digest and pin) is written before dispatch, so a revive re-checks that digest; a refusal fails the run
// (the image is the operator's choice, so the member gets no 403). Under audit the check is made the same way
// and its outcome (admitted, or would_refuse with the reason) is written; nothing is pinned or refused. A
// record that cannot be written fails the run under enforce and is logged otherwise. An unreadable site config
// refuses, since the mode lives in it.
func (s *Server) conventionImageAdmission(ctx context.Context, run types.AgentRun, image string) (string, *runRefusal) {
	adm, refusal := s.launchImageAdmission(ctx)
	if refusal != nil {
		return "", refusal
	}
	mode := adm.EffectiveMode()
	if image == "" || run.MachineKind.IsMacOSVM() || mode == types.ImageAdmissionOff ||
		adm.EffectiveScope() != types.ImageAdmissionScopeAll || s.runUngoverned(ctx) {
		return image, s.recordImageOrigin(ctx, run, mode, types.ImageOriginConvention)
	}
	v, refusal := s.checkImage(ctx, adm, image, true, nil)
	if mode == types.ImageAdmissionAudit {
		s.recordConventionAudit(ctx, run, image, v, refusal)
		return image, nil
	}
	if refusal != nil {
		return "", refusal
	}
	pinned, err := imageAdmissionPinnedRef(image, v.Digest)
	if err != nil {
		return "", runServerError("pin the admitted image", err)
	}
	verdict := admittedImage(types.ImageAdmissionEnforce, v)
	verdict.PinnedRef, verdict.Ref, verdict.Origin = pinned, image, types.ImageOriginConvention
	if err := s.cfg.Store.SetRunImageAdmission(ctx, run.ID, verdict); err != nil {
		return "", runServerError("record the convention image's admission", err)
	}
	return pinned, nil
}

// recordConventionAudit writes audit mode's outcome for the convention image to the run: admitted, or
// would_refuse with the refusal's reason (an unreachable registry included). A write that fails is logged
// only: audit never changes what runs.
func (s *Server) recordConventionAudit(ctx context.Context, run types.AgentRun, image string, v imageadmit.Verdict, refusal *runRefusal) {
	verdict := admittedImage(types.ImageAdmissionAudit, v)
	if refusal != nil {
		verdict = &types.ImageAdmissionVerdict{Mode: types.ImageAdmissionAudit, Verdict: types.ImageAdmissionWouldRefuse,
			Reason: refusal.body.Reason, Digest: v.Digest}
	}
	verdict.Ref, verdict.Origin = image, types.ImageOriginConvention
	if err := s.cfg.Store.SetRunImageAdmission(ctx, run.ID, verdict); err != nil {
		slog.WarnContext(ctx, "wardynd: record the convention image's audit verdict", slog.String("run_id", run.ID.String()), slog.Any("err", err))
	}
}

// recordBuiltImageOrigin records, at every launch whatever the admission mode, that the run's image is one
// Wardyn built for it (origin devcontainer or workspace). Admission does not cover such an image, so no
// registry is read; a revive reads the origin and does not refuse the sandbox for having no admitted digest.
// An unreadable site config refuses, since the mode decides what a failed write does.
func (s *Server) recordBuiltImageOrigin(ctx context.Context, run types.AgentRun, origin string) *runRefusal {
	adm, refusal := s.launchImageAdmission(ctx)
	if refusal != nil {
		return refusal
	}
	return s.recordImageOrigin(ctx, run, adm.EffectiveMode(), origin)
}

// recordImageOrigin writes the origin alone to the run, no verdict: nothing was decided about the image. A
// write that fails fails the run under enforce, as the convention image's verdict does, so no sandbox is kept
// without the record its revive reads; otherwise it is logged, and the run revives as one launched before
// origins were recorded.
func (s *Server) recordImageOrigin(ctx context.Context, run types.AgentRun, mode, origin string) *runRefusal {
	err := s.cfg.Store.SetRunImageAdmission(ctx, run.ID, &types.ImageAdmissionVerdict{Origin: origin})
	switch {
	case err == nil:
		return nil
	case mode == types.ImageAdmissionEnforce:
		return runServerError("record the run image's origin", err)
	}
	slog.WarnContext(ctx, "wardynd: record the run image's origin", slog.String("run_id", run.ID.String()), slog.Any("err", err))
	return nil
}

// failConventionImage fails a run whose launch-time image admission refused it, or could not read or write
// what it needs. A refusal's sentence is the run's hint and its run.dispatch failure row carries the reason;
// a server error's hint and row say what could not be done, never that the image was refused.
func (s *Server) failConventionImage(ctx context.Context, runID uuid.UUID, image string, refusal *runRefusal) {
	ctx, cancel := context.WithTimeout(ctx, killCascadeTimeout)
	defer cancel()
	hint, data := refusal.body.Error, map[string]any{"image": image}
	if refusal.err != nil {
		hint = loggedMsg(ctx, "the launch could not "+refusal.operation, refusal.err)
		data["error"] = hint
	} else {
		data["reason"], data["image_admission"] = refusal.body.Reason, &types.ImageAdmissionVerdict{
			Mode: types.ImageAdmissionEnforce, Verdict: imageAdmissionRefused, Reason: refusal.body.Reason}
	}
	s.failAndRevoke(ctx, runID, types.RunPending, hint)
	s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.dispatch", runID.String(), "failure", mustJSON(data)))
}
