// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The Claude sign-in image is a core prerequisite for the anthropic_subscription
// model-provider kind (multi-provider design 2.2): the container-login sandbox
// needs the vendor CLI image (agent-claude-code), which Wardyn does not publish
// (deploy/images/THIRD-PARTY-TERMS.md) and cannot: an operator who wants the
// kind must build it locally and pin it via WARDYN_AGENT_IMAGES.
//
// "Resolves" is two-part: the pin must be SET (WARDYN_AGENT_IMAGES["claude-code"]),
// and, when the wired Runner can confirm its own local image store
// (runner.ImageChecker — the docker substrate), the pinned ref must actually be
// PRESENT there. A pin alone is not enough on this repo's own compose/run-host
// defaults, which bake in a claude-code pin unconditionally (deploy/compose/
// docker-compose.yaml, scripts/run-host.sh) whether or not the image has ever
// been built — the live check is what tells "pinned" from "built". A Runner
// that cannot answer (unwired, or a substrate with no local cache notion — k8s
// pulls fresh per launch) is trusted: a missing image there surfaces at the
// login run itself, naming the image, which is cheaper than a false warning on
// every k8s install.

// claudeSignInImageState is resolveClaudeSignInImage's result.
type claudeSignInImageState struct {
	ref      string // the pinned ref, "" when unpinned
	resolved bool   // ref is set AND (unverifiable-so-trusted OR confirmed present)
	verified bool   // true when `resolved` came from an actual ImagePresent check
	checkErr error  // the ImagePresent error when the check was inconclusive
}

// resolveClaudeSignInImage is the one place that decides whether the Claude
// sign-in image resolves — shared by the setup check (informational) and the
// anthropic_subscription write-time refusal (E4, model_providers.go).
func resolveClaudeSignInImage(ctx context.Context, images map[string]string, rnr runner.Runner) claudeSignInImageState {
	ref := strings.TrimSpace(images["claude-code"])
	if ref == "" {
		return claudeSignInImageState{}
	}
	ic, ok := rnr.(runner.ImageChecker)
	if !ok {
		return claudeSignInImageState{ref: ref, resolved: true}
	}
	present, err := ic.ImagePresent(ctx, ref)
	if err != nil {
		// Fail-open on an inconclusive check — the same posture
		// cachedImageStillPresent already takes for the same reason: an
		// unreachable daemon must not manufacture a refusal for every
		// anthropic_subscription write.
		return claudeSignInImageState{ref: ref, resolved: true, checkErr: err}
	}
	return claudeSignInImageState{ref: ref, resolved: present, verified: true}
}

// claudeSignInImageResolves is resolveClaudeSignInImage's boolean half, for the
// write-time refusal.
func claudeSignInImageResolves(ctx context.Context, images map[string]string, rnr runner.Runner) bool {
	return resolveClaudeSignInImage(ctx, images, rnr).resolved
}

// claudeSignInImageOK is a write door's image answer for E4
// (validateModelProviderImagePrereqs). It is a runner call, so each door asks
// BEFORE taking siteConfigMu, and only when block holds a subscription that is
// on (introduced against nothing) — the only block E4 can refuse.
func (s *Server) claudeSignInImageOK(ctx context.Context, block *types.ModelProviders) bool {
	return introducedSubscription(block, nil) == "" || claudeSignInImageResolves(ctx, s.cfg.AgentImages, s.cfg.Runner)
}

// claudeSignInImageCheck is the /setup/status row. Never Blocking: the kind is
// optional (an install that never offers Claude subscriptions needs nothing
// here), so a stale pin gates only that one kind (the E4 refusal), not the
// console.
func claudeSignInImageCheck(ctx context.Context, images map[string]string, rnr runner.Runner) SetupCheck {
	const id, label = "claude_signin_image", "Claude sign-in image"
	fix := "Build it: `make agent-images-core` (or `make setup`, which builds and pins it). " +
		"See Operations → Claude sign-in image."
	st := resolveClaudeSignInImage(ctx, images, rnr)
	if st.ref == "" {
		return SetupCheck{
			ID: id, Label: label, Status: "warn",
			Detail: "WARDYN_AGENT_IMAGES[\"claude-code\"] is not set, so the sign-in sandbox has no image " +
				"carrying the vendor CLI — anthropic_subscription model providers are refused until it resolves.",
			Fix: fix,
		}
	}
	if st.resolved {
		detail := "Claude sign-in image resolves: " + st.ref + "."
		switch {
		case st.checkErr != nil:
			detail += " (Pin only — the image presence check could not answer: " + st.checkErr.Error() +
				"; a missing image surfaces at the login run instead.)"
		case !st.verified:
			detail += " (Pin only — this runner cannot confirm the image is present locally; a missing " +
				"image surfaces at the login run instead.)"
		}
		return SetupCheck{ID: id, Label: label, Status: "info", Detail: detail}
	}
	return SetupCheck{
		ID: id, Label: label, Status: "warn",
		Detail: "WARDYN_AGENT_IMAGES[\"claude-code\"] is pinned to " + st.ref + ", but it is not present on this " +
			"Docker daemon — anthropic_subscription model providers are refused until it resolves.",
		Fix: fix,
	}
}
