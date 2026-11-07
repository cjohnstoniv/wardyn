// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"
	"strings"

	"github.com/google/uuid"

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

// conventionImageAdmission holds the image a run gets by convention (its agent's, its harness set's or an
// architecture variant of either) to image admission when the site's scope is all and its mode enforce, and
// returns the ref to dispatch: the pin when it was checked, else image. It runs at launch, where the placed
// runner's architecture has chosen the image. Operators and the break-glass admin are not checked, as at
// create; audit mode and scope member leave it unchecked. A refusal fails the run (the image is the operator's
// choice, so the member gets no 403); an unreadable site config refuses, since the mode lives in it.
func (s *Server) conventionImageAdmission(ctx context.Context, run types.AgentRun, image string) (string, *runRefusal) {
	if image == "" || run.MachineKind.IsMacOSVM() {
		return image, nil
	}
	sc, ok := s.siteConfigSnapshot(ctx)
	if s.cfg.Store != nil && !ok {
		return "", runServerError("resolve image admission", errImageAdmissionSiteUnreadable)
	}
	var adm *types.SiteImageAdmission
	if sc.Images != nil {
		adm = sc.Images.Admission
	}
	if adm.EffectiveMode() != types.ImageAdmissionEnforce || adm.EffectiveScope() != types.ImageAdmissionScopeAll || s.runUngoverned(ctx) {
		return image, nil
	}
	v, refusal := s.checkImage(ctx, adm, image, true, nil)
	if refusal != nil {
		return "", refusal
	}
	pinned, err := imageAdmissionPinnedRef(image, v.Digest)
	if err != nil {
		return "", runServerError("pin the admitted image", err)
	}
	return pinned, nil
}

// failConventionImage fails a run whose convention image admission refused: the refusal sentence is the run's
// hint, and a run.dispatch failure row carries the reason and the image.
func (s *Server) failConventionImage(ctx context.Context, runID uuid.UUID, image string, refusal *runRefusal) {
	ctx, cancel := context.WithTimeout(ctx, killCascadeTimeout)
	defer cancel()
	if refusal.err != nil {
		slog.ErrorContext(ctx, "wardynd: convention image admission failed", slog.String("run_id", runID.String()), slog.Any("err", refusal.err))
	}
	s.failAndRevoke(ctx, runID, types.RunPending, refusal.body.Error)
	s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.dispatch", runID.String(), "failure",
		mustJSON(map[string]any{"reason": refusal.body.Reason, "image": image, "image_admission": &types.ImageAdmissionVerdict{
			Mode: types.ImageAdmissionEnforce, Verdict: imageAdmissionRefused, Reason: refusal.body.Reason}})))
}
