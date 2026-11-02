// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A prewrapped base image (design final §3) is built FROM agent-base, pinned by digest, and runs as is
// with no builder. Registering one starts its check on this control plane's own Kubernetes cluster
// (runner.PrewrappedImageChecker); only a ready row is offered to a workspace or a run.

// prewrappedDigestRef is <repository>[:tag]@sha256:<64 hex>.
var prewrappedDigestRef = regexp.MustCompile(`^[^@\s]+@sha256:[0-9a-f]{64}$`)

func prewrappedPinned(ref string) bool { return prewrappedDigestRef.MatchString(ref) }

// prewrappedBase reports whether a workspace's base image is a prewrapped one.
func prewrappedBase(b *types.WorkspaceBaseImage) bool {
	return b != nil && b.Kind == types.BaseImageKindPrewrapped && strings.TrimSpace(b.Image) != ""
}

// prewrappedRunImage is the ready prewrapped row ref names, nil when ref names no prewrapped row, and a
// refusal when it names one that is not ready: only a ready row runs (design final §3).
func (s *Server) prewrappedRunImage(ctx context.Context, ref string) (*types.BaseImageEntry, *runRefusal) {
	if s.cfg.Store == nil || !prewrappedPinned(ref) { // every prewrapped row is pinned by digest
		return nil, nil
	}
	b, err := s.cfg.Store.GetBaseImageByRef(ctx, types.BaseImageKindPrewrapped, ref)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, nil
	case err != nil:
		return nil, runServerError("read the prewrapped image", err)
	case b.Check == nil || b.Check.Status != types.BaseImageCheckReady:
		return nil, runError(http.StatusUnprocessableEntity, reasonPrewrappedImageNotRegistered,
			"image "+ref+" is a prewrapped image that has not passed its check: check it again under Images")
	}
	return &b, nil
}

// imageCheckInterrupted is the reason a checking row past its deadline reads with: its attempt can no
// longer publish a verdict (store.FinishImageCheck), and "Check again" may claim it.
const imageCheckInterrupted = "check_interrupted"

// prewrappedView is b as it is served: a checking row past its deadline reads as refused with
// imageCheckInterrupted. The row itself is unchanged; the store decides expiry on its own clock.
func prewrappedView(b types.BaseImageEntry, now time.Time) types.BaseImageEntry {
	if c := b.Check; c != nil && c.Status == types.BaseImageCheckChecking && !now.Before(c.Deadline) {
		v := *c
		v.Status, v.Reason = types.BaseImageCheckRefused, imageCheckInterrupted
		b.Check = &v
	}
	return b
}

// prewrappedChecker is the substrate that checks a prewrapped image, or nil and why there is none.
func (s *Server) prewrappedChecker() (runner.PrewrappedImageChecker, string) {
	c, ok := s.cfg.Runner.(runner.PrewrappedImageChecker)
	if !ok {
		return nil, "this control plane has no Kubernetes substrate"
	}
	if why := c.PrewrappedCheckUnavailable(); why != "" {
		return nil, why
	}
	return c, ""
}

// startImageCheck runs b's claimed attempt in the background and publishes its one verdict. A verdict
// the store refuses (a newer attempt, or the deadline passed) changes nothing. Shutdown stops the
// check; its row then reads as interrupted once the deadline passes, and "Check again" retries it.
func (s *Server) startImageCheck(r *http.Request, c runner.PrewrappedImageChecker, b types.BaseImageEntry) {
	actorType, principal := actorTypeFromRequest(r), principalFromRequest(r)
	attempt := b.Check.Attempt
	ctx := s.cfg.BaseCtx
	if ctx == nil {
		ctx = context.WithoutCancel(r.Context())
	}
	s.goBackground(func() {
		status, reason, accepted := runner.AggregateImageCheck(c.CheckPrewrappedImage(ctx, attempt, b.Image))
		if _, err := s.cfg.Store.FinishImageCheck(ctx, b.ID, b.Image, attempt, status, reason, accepted); err != nil {
			level := slog.LevelWarn
			if errors.Is(err, store.ErrConflict) {
				level = slog.LevelInfo // superseded or expired: the row has moved on
			}
			slog.Log(ctx, level, "wardynd: prewrapped image check verdict not published",
				slog.String("base_image", b.ID.String()), slog.String("attempt", attempt.String()), slog.Any("err", err))
			return
		}
		outcome := "success"
		if status != types.BaseImageCheckReady {
			outcome = "failure"
		}
		s.recordAudit(ctx, s.auditEvent(nil, actorType, principal, "base_image.admit", b.ID.String(), outcome,
			mustJSON(map[string]any{"image": b.Image, "status": status, "reason": reason, "accepted_arches": accepted})))
	})
}

// handleCheckBaseImage starts a new check of a refused or interrupted prewrapped row ("Check again").
// The claim is one fenced store write, so concurrent calls start one attempt; the others get 409.
//
//	POST /api/v1/base-images/{id}/check
func (s *Server) handleCheckBaseImage(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "base image")
	if !ok {
		return
	}
	b, err := s.cfg.Store.GetBaseImage(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErrorReason(w, http.StatusNotFound, reasonBaseImageNotFound, "no such base image")
		return
	}
	if err != nil {
		writeServerError(w, r, "read base image", err)
		return
	}
	if !s.mayWriteBaseImageKind(r.Context(), b.Kind) {
		s.refuse(w, r, authz.Deny(authz.ReasonAdminSurface, r.URL.Path, ""))
		return
	}
	if b.Kind != types.BaseImageKindPrewrapped {
		writeErrorReason(w, http.StatusBadRequest, reasonBaseImageWriteInvalid, "only a prewrapped image is checked")
		return
	}
	c, why := s.prewrappedChecker()
	if c == nil {
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonPrewrappedCheckUnavailable,
			"a prewrapped image is checked on this control plane's own Kubernetes cluster, and it cannot check one: "+why)
		return
	}
	claimed, err := s.cfg.Store.ClaimImageCheck(r.Context(), id, uuid.New())
	if errors.Is(err, store.ErrConflict) {
		if b.Check != nil && b.Check.Status == types.BaseImageCheckReady {
			writeErrorReason(w, http.StatusConflict, reasonPrewrappedAlreadyReady, "this image is already checked and ready")
			return
		}
		writeErrorReason(w, http.StatusConflict, reasonPrewrappedCheckInProgress, "a check of this image is already running")
		return
	}
	if err != nil {
		writeServerError(w, r, "claim image check", err)
		return
	}
	s.startImageCheck(r, c, claimed)
	writeJSON(w, http.StatusAccepted, prewrappedView(claimed, s.cfg.Now()))
}
