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

// reviveImageNotAdmittedMsg refuses a revive while the image is held when the run has no digest admission
// pinned at its start. It says only what is true of every such run. %s is the image: the member's as named,
// the convention image as configured, else the image the run recorded.
const reviveImageNotAdmittedMsg = "Image %s has no digest that image admission checked and pinned when this run " +
	"started (admission was off, in audit mode, or did not check this image), so it cannot be re-checked under the " +
	"policy as it is now"

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
// it, so a signer entry written for a short name still covers the run whose pin is the canonical name. A
// run whose create named an image is the member's, whatever its verdict records: a launch's record for another
// image (an origin, or a convention ref) holds no admitted pin of the member's image, so it is refused. A run
// whose create named none is classified by the origin its launch recorded: an image Wardyn built for it
// (devcontainer, workspace) or a run Wardyn launched itself (wardyn, recorded only by newStepRun at create) is
// not covered by admission and never refused here; the convention image is held
// only under scope all, by the site's signers alone, at the digest its launch recorded (Ref). A run launched
// before origins were recorded is taken as on the convention image, as before; under scope member it stays
// unchecked. The authority is the run
// owner's, as reviveImageDigestRefusal reads it: an exempt owner is not checked, and the mark says so. Audit
// mode refuses nothing here, as at create. An unreadable site config refuses.
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
	mode, recorded := adm.EffectiveMode(), run.ImageAdmission
	var origin, convRef string
	if recorded != nil {
		origin, convRef = recorded.Origin, recorded.Ref
	}
	// convention: the create named no image (nil: a row from before ImageInput, handled as the member's,
	// unpinned). A profile's signers bind only a member's image.
	convention := run.ImageInput != nil && *run.ImageInput == ""
	if convention && (origin == types.ImageOriginDevcontainer || origin == types.ImageOriginWorkspace || origin == types.ImageOriginWardyn) {
		return nil, nil // an image Wardyn built for the run, or a run Wardyn launched itself: image admission does not cover it
	}
	if convention {
		if adm.EffectiveScope() != types.ImageAdmissionScopeAll {
			return nil, nil
		}
		profile = nil
	}
	if (mode == types.ImageAdmissionOff && len(profile) == 0) || run.MachineKind.IsMacOSVM() { // a macOS VM never runs the image
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
	ref, pin := run.Image, imagePin(run)
	switch {
	case convention && origin == types.ImageOriginConvention && convRef != "":
		ref = convRef
	case convention, run.ImageInput == nil:
		pin = "" // nothing admitted was recorded for the image this sandbox runs
	default:
		ref = *run.ImageInput
		if origin != "" || convRef != "" {
			pin = "" // a launch's record for an image the member did not name: never a pin of the member's
		}
	}
	if pin == "" {
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
