// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// imageDigestRequiredMsg and imageDigestSeededMsg are the digest rule's two refusal sentences: the
// first for an image the member named, the second for the base image of their own workspace.
const (
	imageDigestRequiredMsg = "image %s is not pinned by digest — this organisation requires images pinned by digest " +
		"(image@sha256:<digest>), because an image grant names what an admin approved and only a digest makes that what runs " +
		"(site setting images.require_digest). Use the digest form of the image, or launch with the agent's convention image"
	imageDigestSeededMsg = "image %s comes from your own workspace's base image and is not pinned by digest — this organisation requires " +
		"images pinned by digest (image@sha256:<digest>) (site setting images.require_digest). " +
		"Set the workspace's base image to its digest form, or launch with the agent's convention image"
)

// imagePinnedByDigest reports whether ref ends in @sha256:<64 hex digits>, the only form that names one
// image. A tag beside the digest (name:tag@sha256:...) is pinned: the registry resolves the digest.
func imagePinnedByDigest(ref string) bool {
	_, d, ok := strings.Cut(ref, "@")
	hex, isSHA := strings.CutPrefix(d, "sha256:")
	if !ok || !isSHA || len(hex) != 64 {
		return false
	}
	return strings.IndexFunc(hex, func(r rune) bool { return !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') }) < 0
}

type digestVerdict int

const (
	digestAllowed digestVerdict = iota // the rule is off, the image is pinned or empty, or the run names no image
	digestRefused                      // a governed caller's image is not pinned and the rule is on
	digestExempt                       // an operator's image is not pinned and the rule is on
)

// imageDigestVerdict is the one answer to the digest rule (images.require_digest): every door that can start a
// member-chosen image asks it, so the refusal and the operator's audit mark cannot disagree. A pinned or empty
// image never reads the site config. Operators and the break-glass admin are runUngoverned: never refused,
// reported as exempt so the audit row can say so.
func (s *Server) imageDigestVerdict(ctx context.Context, image string) (digestVerdict, error) {
	if image == "" || imagePinnedByDigest(image) {
		return digestAllowed, nil
	}
	sc, ok := s.siteConfigSnapshot(ctx)
	if s.cfg.Store != nil && !ok {
		return digestAllowed, errImageDigestRuleUnreadable
	}
	if !imageRequireDigest(sc) {
		return digestAllowed, nil
	}
	if s.runUngoverned(ctx) {
		return digestExempt, nil
	}
	return digestRefused, nil
}

var errImageDigestRuleUnreadable = errors.New("read the site config for the image digest rule")

func imageRequireDigest(sc types.SiteConfig) bool {
	return sc.Images != nil && sc.Images.RequireDigest
}

// imageDigestRefusal is the digest rule as a run refusal: nil when the image may start. seeded says the
// image came from the member's own workspace, which has its own sentence.
func (s *Server) imageDigestRefusal(ctx context.Context, image string, seeded bool) *runRefusal {
	v, err := s.imageDigestVerdict(ctx, image)
	if err != nil {
		return runServerError("resolve image digest rule", err)
	}
	if v != digestRefused {
		return nil
	}
	msg := imageDigestRequiredMsg
	if seeded {
		msg = imageDigestSeededMsg
	}
	return runError(http.StatusForbidden, reasonImageDigestRequired, fmt.Sprintf(msg, image))
}

// markImageDigestExempt stamps image_digest_exempt on a run.create datum when the rule is on and an operator's
// run names an image that is not pinned, so the org can see the runs the rule did not bind. The key is absent
// otherwise, so a deployment with the rule off writes the rows it always did.
func (s *Server) markImageDigestExempt(ctx context.Context, image string, data map[string]any) {
	if v, err := s.imageDigestVerdict(ctx, image); err == nil && v == digestExempt {
		data["image_digest_exempt"] = true
	}
}

// carryForwardSiteImages keeps the stored images block when the body does not name it (an older client or a
// re-applied managed config cannot name it), and stores an all-default block as none so a GET stays
// byte-identical to an install that never set one.
func carryForwardSiteImages(cfg *types.SiteConfig, existing types.SiteConfig, present map[string]bool) {
	if !present["images"] {
		cfg.Images = existing.Images
	} else if cfg.Images != nil && *cfg.Images == (types.SiteImages{}) {
		cfg.Images = nil
	}
}

// auditSiteImages puts the digest rule on the site_config.write row, only once a block is stored or named.
func auditSiteImages(datum map[string]any, im *types.SiteImages, named bool) {
	if im == nil && !named {
		return
	}
	datum["images_require_digest"] = im != nil && im.RequireDigest
}
