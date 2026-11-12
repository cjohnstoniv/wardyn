// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/sigstore/sigstore-go/pkg/root"

	"github.com/cjohnstoniv/wardyn/internal/imageadmit"
	"github.com/cjohnstoniv/wardyn/internal/imageadmit/rootfile"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// imageDigestRequiredMsg and imageDigestSeededMsg are the digest rule's two refusal sentences: the
// first for an image the member named, the second for an attached workspace's base image, whoever owns it.
const (
	imageDigestRequiredMsg = "Image %s is not pinned by digest — this organisation requires images pinned by digest " +
		"(image@sha256:<digest>), because an image grant names what an admin approved and only a digest makes that what runs " +
		"(site setting images.require_digest). Use the digest form of the image, or launch with the agent's convention image"
	imageDigestSeededMsg = "Image %s comes from the workspace's base image and is not pinned by digest — this organisation requires " +
		"images pinned by digest (image@sha256:<digest>) (site setting images.require_digest). " +
		"Set the workspace's base image to its digest form (or ask its owner to), or launch with the agent's convention image"
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
	return s.digestVerdictBy(ctx, &image, func(ctx context.Context) bool { return s.runUngoverned(ctx) })
}

// digestVerdictBy is imageDigestVerdict on a recorded input (AgentRun.ImageInput): nil is a run whose create
// recorded none, which the rule, while on, holds as an unpinned image. ungoverned names whose authority the
// rule asks about: the caller's at a launch, the run owner's at a revive (reviveImageDigestRefusal).
func (s *Server) digestVerdictBy(ctx context.Context, image *string, ungoverned func(context.Context) bool) (digestVerdict, error) {
	if image != nil && (*image == "" || imagePinnedByDigest(*image)) {
		return digestAllowed, nil
	}
	sc, ok := s.siteConfigSnapshot(ctx)
	if s.cfg.Store != nil && !ok {
		return digestAllowed, errImageDigestRuleUnreadable
	}
	if !imageRequireDigest(sc) {
		return digestAllowed, nil
	}
	if ungoverned(ctx) {
		return digestExempt, nil
	}
	return digestRefused, nil
}

// imageDigestReviveSuffix ends a revive's digest refusal: a kept sandbox cannot change its image.
const imageDigestReviveSuffix = ". Start a new run to use an allowed image"

// reviveImageDigestRefusal re-runs create's digest decision on the input the run's create recorded, under the
// rule as it is now, so turning the rule on binds the sandboxes already kept, and a run from before the input
// was recorded is refused while it is on. The authority is the run owner's, as ownerCapabilityRefusal reads
// it: an ungoverned owner reviving their own run, or anyone reviving an operator-owned run, is exempt (mark is
// then image_digest_exempt, for the run.revive row); an admin reviving a member's run is bound as the member.
// An unreadable site config refuses.
func (s *Server) reviveImageDigestRefusal(ctx context.Context, run types.AgentRun, actorType types.ActorType, actor string) (mark map[string]any, rerr *reviveError) {
	v, err := s.digestVerdictBy(ctx, run.ImageInput, func(ctx context.Context) bool {
		if actor == run.CreatedBy {
			return s.runUngoverned(ctx)
		}
		return run.OperatorOwned
	})
	if err != nil {
		return nil, reviveRefused(http.StatusServiceUnavailable, reasonReviveOwnerAuthorityUnreadable,
			loggedMsg(ctx, "re-check the image digest rule", err))
	}
	if v == digestExempt {
		return map[string]any{"image_digest_exempt": true}, nil
	}
	if v != digestRefused {
		return nil, nil
	}
	ref := run.Image
	if run.ImageInput != nil {
		ref = *run.ImageInput
	}
	s.recordAudit(ctx, s.auditEvent(&run.ID, actorType, actor, "run.revive", run.ID.String(), "denied",
		mustJSON(map[string]any{"subject": run.CreatedBy, "reason": reasonImageDigestRequired})))
	return nil, reviveRefused(http.StatusForbidden, reasonImageDigestRequired, fmt.Sprintf(imageDigestRequiredMsg, ref)+imageDigestReviveSuffix)
}

var errImageDigestRuleUnreadable = errors.New("read the site config for the image digest rule")

func imageRequireDigest(sc types.SiteConfig) bool {
	return sc.Images != nil && sc.Images.RequireDigest
}

// imageDigestRefusal is the digest rule as a run refusal: nil when the image may start. seeded says the
// image came from an attached workspace's base image, which has its own sentence.
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

// markRunCreateExempt is the run.create writers' one call for every exemption mark: governance_exempt
// (the admin switch), image_digest_exempt and image_admission_exempt.
func (s *Server) markRunCreateExempt(ctx context.Context, image string, data map[string]any) {
	s.markGovernanceExempt(ctx, data)
	s.markImageDigestExempt(ctx, image, data)
	s.markImageAdmissionExempt(ctx, image, data)
}

// carryForwardSiteImages keeps the stored images block when the body does not name it (an older client or a
// re-applied managed config cannot name it), and stores an all-default block as none so a GET stays
// byte-identical to an install that never set one. An admission block that says nothing a missing one would
// not (off, member scope, nothing defined) is dropped from the block the same way.
func carryForwardSiteImages(cfg *types.SiteConfig, existing types.SiteConfig, present map[string]bool) {
	if !present["images"] {
		cfg.Images = existing.Images
		return
	}
	if cfg.Images == nil {
		return
	}
	if cfg.Images.Admission.IsZero() && cfg.Images.Admission != nil {
		im := *cfg.Images
		im.Admission = nil
		cfg.Images = &im
	}
	if cfg.Images.IsZero() {
		cfg.Images = nil
	}
}

// auditSiteImages puts the image rules on the site_config.write row, only once a block is stored or named.
// The admission keys name signers and never their identities, issuers or keys, and appear only when an
// admission block is or was stored; the before and after signer sets and the earlier mode appear only when
// they changed.
func auditSiteImages(datum map[string]any, before, im *types.SiteImages, named bool) {
	if im == nil && !named {
		return
	}
	datum["images_require_digest"] = im != nil && im.RequireDigest
	var was, now *types.SiteImageAdmission
	if before != nil {
		was = before.Admission
	}
	if im != nil {
		now = im.Admission
	}
	if was == nil && now == nil {
		return
	}
	datum["images_admission_mode"] = now.EffectiveMode()
	datum["images_admission_scope"] = now.EffectiveScope()
	datum["images_admission_signers"] = append([]string{}, now.SignerNames()...)
	if was.EffectiveMode() != now.EffectiveMode() {
		datum["images_admission_mode_before"] = was.EffectiveMode()
	}
	if b, a := was.SignerNames(), now.SignerNames(); !slices.Equal(b, a) {
		datum["images_admission_signers_before"] = append([]string{}, b...)
		datum["images_admission_signers_after"] = append([]string{}, a...)
	}
}

// imageTrustedRootDir is rootfile.Dir, a variable only so tests can point it at a temporary directory.
var imageTrustedRootDir = rootfile.Dir

// validateSiteImageAdmission is PUT /site-config's check of images.admission: the block's shape (types) with
// the image-grant grammar for each images entry, each trust root's file, read here as branding's logo is,
// and then run create's own compile of the block, so a setting the run check would refuse refuses the write
// rather than every later run. Every message starts "images.admission". A body with no admission block is
// valid.
func validateSiteImageAdmission(im *types.SiteImages) error {
	if im == nil || im.Admission == nil {
		return nil
	}
	if err := im.Admission.Validate(imageAdmissionEntryOK); err != nil {
		return fmt.Errorf("images.admission: %w", err)
	}
	for _, r := range im.Admission.TrustedRoots {
		if err := checkImageTrustedRootFile(r.Path); err != nil {
			return fmt.Errorf("images.admission: trusted root %q: %w", r.Name, err)
		}
	}
	if err := imageadmit.CheckSite(im.Admission, imageadmit.Options{RootDir: imageTrustedRootDir}); err != nil {
		return fmt.Errorf("images.admission: %w", err)
	}
	return nil
}

// checkImageTrustedRootFile reads path through rootfile (a file in imageTrustedRootDir, never any other) and
// parses it as a Sigstore trusted root. No message quotes the file.
func checkImageTrustedRootFile(path string) error {
	raw, err := rootfile.Read(imageTrustedRootDir, path)
	if err != nil {
		return err
	}
	if _, err := root.NewTrustedRootFromJSON(raw); err != nil {
		return errors.New("the file is not a Sigstore trusted root")
	}
	return nil
}

// imageAdmissionEntryOK is the grammar of a signer's images entry: a registry prefix ("host/path/*") or an
// exact reference, as an image grant is written.
func imageAdmissionEntryOK(v string) bool {
	if strings.TrimSpace(v) != v {
		return false
	}
	if strings.HasSuffix(v, imagePrefixSuffix) || strings.Contains(v, "*") {
		return isImagePrefixGrant(v)
	}
	_, ok := imageRepoAsTyped(v)
	return ok
}
