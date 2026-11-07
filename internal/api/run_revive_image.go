// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// reviveImageNotAdmittedMsg refuses a revive while the image is held when the run's create pinned no digest.
// %s is the image as the run's create named it.
const reviveImageNotAdmittedMsg = "Image %s was not checked and pinned by image admission when this run was created " +
	"(it was created before images.admission was enforced, or in audit mode), so its tag may now name another image"

// reviveImageRefusals is a revive's image decisions in order, the digest rule then image admission, and the
// marks the run.revive row carries for an owner they exempt.
func (s *Server) reviveImageRefusals(ctx context.Context, run types.AgentRun, profile []string, actorType types.ActorType, actor string) (map[string]any, *reviveError) {
	digestMark, rerr := s.reviveImageDigestRefusal(ctx, run, actorType, actor)
	if rerr != nil {
		return nil, rerr
	}
	admissionMark, rerr := s.reviveImageAdmission(ctx, run, profile, actorType, actor)
	if rerr != nil {
		return nil, rerr
	}
	mark := map[string]any{}
	maps.Copy(mark, digestMark)
	maps.Copy(mark, admissionMark)
	return mark, nil
}

// reviveImageAdmission re-runs image admission at a revive under the policy as it is now: the site mode and
// signers, and the signers the owner's profile requires (profile). So turning enforce on, or removing a
// signer, binds the sandboxes already kept. The check is of the digest the run's create admitted (its pin),
// never of its tag, which may name another image by now: while the image is held, a run whose create pinned
// nothing (created before admission was enforced, in audit mode, which never pins, or before the run recorded
// its image) is refused, and its owner starts a new run. Coverage is asked about the image as the create asked
// it, so a signer entry written for a short name still covers the run whose pin is the canonical name. The
// authority is the run owner's, as reviveImageDigestRefusal reads it: an exempt owner is not checked, and the
// mark says so. Audit mode refuses nothing here, as at create. An unreadable site config refuses.
func (s *Server) reviveImageAdmission(ctx context.Context, run types.AgentRun, profile []string, actorType types.ActorType, actor string) (map[string]any, *reviveError) {
	sc, ok := s.siteConfigSnapshot(ctx)
	if s.cfg.Store != nil && !ok {
		return nil, reviveRefused(http.StatusServiceUnavailable, reasonReviveOwnerAuthorityUnreadable,
			loggedMsg(ctx, "re-check image admission", errImageAdmissionSiteUnreadable))
	}
	var adm *types.SiteImageAdmission
	if sc.Images != nil {
		adm = sc.Images.Admission
	}
	mode := adm.EffectiveMode()
	if (mode == types.ImageAdmissionOff && len(profile) == 0) ||
		(run.ImageInput != nil && *run.ImageInput == "") || // the convention image: no member chose it
		run.MachineKind.IsMacOSVM() { // a macOS VM never runs the member image
		return nil, nil
	}
	exempt := run.OperatorOwned
	if actor == run.CreatedBy {
		exempt = s.runUngoverned(ctx)
	}
	if exempt {
		return map[string]any{"image_admission_exempt": true}, nil
	}
	if mode != types.ImageAdmissionEnforce && len(profile) == 0 {
		return nil, nil
	}
	pin, ref := imagePin(run), run.Image
	if run.ImageInput != nil {
		ref = *run.ImageInput
	}
	if pin == "" || run.ImageInput == nil {
		return nil, s.reviveImageAdmissionDenied(ctx, run, actorType, actor, ref, "",
			reasonReviveImageNotAdmitted, fmt.Sprintf(reviveImageNotAdmittedMsg, ref))
	}
	_, refusal := s.checkImageAt(ctx, adm, ref, pin, mode == types.ImageAdmissionEnforce, profile)
	switch {
	case refusal == nil:
		return nil, nil
	case refusal.status != http.StatusForbidden: // the check could not be made: nothing is refused for good
		return nil, reviveRefused(refusal.status, refusal.body.Reason, refusal.body.Error)
	}
	_, digest, _ := strings.Cut(pin, "@")
	return nil, s.reviveImageAdmissionDenied(ctx, run, actorType, actor, ref, digest, refusal.body.Reason, refusal.body.Error)
}

// reviveImageAdmissionDenied writes the run.revive denied row for an image admission refusal, with the reason
// and the check's facts, and returns the refusal with the revive suffix.
func (s *Server) reviveImageAdmissionDenied(ctx context.Context, run types.AgentRun, actorType types.ActorType, actor, ref, digest, reason, msg string) *reviveError {
	s.recordAudit(ctx, s.auditEvent(&run.ID, actorType, actor, "run.revive", run.ID.String(), "denied",
		mustJSON(map[string]any{"subject": run.CreatedBy, "reason": reason, "image": ref, "image_admission": &types.ImageAdmissionVerdict{
			Mode: types.ImageAdmissionEnforce, Verdict: imageAdmissionRefused, Reason: reason, Digest: digest}})))
	return reviveRefused(http.StatusForbidden, reason, msg+imageDigestReviveSuffix)
}
