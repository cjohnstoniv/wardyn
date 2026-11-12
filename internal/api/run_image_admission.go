// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/distribution/reference"

	"github.com/cjohnstoniv/wardyn/internal/imageadmit"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// ImageAdmissionConfig is Config's image admission check, embedded so its wiring lives beside the feature
// rather than in server.go.
type ImageAdmissionConfig struct {
	// ImageAdmitter checks a member's image against images.admission and a governance profile's required
	// signers. Nil while a check applies: enforce refuses with image_admission_unavailable and audit warns,
	// so no deployment reads a run as checked when nothing checked it.
	ImageAdmitter ImageAdmitter
}

// ImageAdmitter is image admission's check (internal/imageadmit).
//
// Require reads no registry. It picks the signers ref must satisfy under the site block a (nil: none
// defined): the site's own signers that cover ref when site is true, and every signer named in profile. A
// non-nil verdict is a refusal decided by coverage alone (no_policy), so a member cannot make the control
// plane contact a host no admin named. The error is a block that cannot be compiled at run time (a trust
// root file gone or unreadable): it is logged, never shown.
//
// Admit resolves req.Ref to a digest from its registry and verifies the digest's signatures against req, as
// (*imageadmit.Admitter).Admit does. The error is non-nil exactly when the verdict is
// image_admission_unavailable: the error is for the log, and Verdict.Detail is the public cause.
type ImageAdmitter interface {
	Require(a *types.SiteImageAdmission, ref string, site bool, profile []string) (imageadmit.Requirements, *imageadmit.Verdict, error)
	Admit(ctx context.Context, req imageadmit.Requirements) (imageadmit.Verdict, error)
}

// imageAdmissionAuditPrefix starts audit mode's warning: the refusal enforce would have made.
const imageAdmissionAuditPrefix = "Image check (audit only): "

// The refusal sentences. The first %s is always the image as the member typed it, with its short digest
// once one is known.
const (
	imageNoPolicyMsg = "Image %s is not covered by any image signer this organisation trusts (site setting images.admission). " +
		"Ask an admin to add a signer that covers it, or launch with an image one does"
	imageProfileUncoveredMsg = "Image %s cannot be launched: your governance profile requires signer %s, which does not cover it. " +
		"Launch with an image it covers, or ask an admin"
	imageProfileUndefinedMsg = "Image %s cannot be launched: your governance profile requires signer %s, which this deployment no longer defines. " +
		"Ask an admin to update the profile"
	imageSignatureMissingMsg = "Image %s has no signature from a signer this organisation trusts for it (%s). " +
		"Sign it and push the signature to its registry, or launch with a signed image"
	imageSignatureInvalidMsg    = "Image %s is signed, but not by a signer this organisation trusts for it: found %s from %s; %s requires %s from %s"
	imageSignatureInvalidKeyMsg = "Image %s has no signature that verifies with a key this organisation trusts for it; %s requires key %s"
	imageAttestationMissingMsg  = "Image %s has no %s attestation signed by %s, which this organisation requires"
	// The second %s is the check's own cause (imageadmit.Verdict.Detail): the image's registry and what it answered.
	imageAdmissionUnavailableMsg = "Wardyn could not check image %s: %s. Try again, or ask an admin"
	imageAdmitterMissingMsg      = "Wardyn could not check image %s: image admission is not running in this control plane. Ask an admin"
	imagePolicyUnusableMsg       = "Wardyn could not check image %s: this organisation's image trust settings could not be loaded " +
		"(a trust root file may be missing or unreadable). Ask an admin"
	imageNoAnswerMsg = "Wardyn could not check image %s: the check gave no usable answer. Try again, or ask an admin"
)

var errImageAdmissionSiteUnreadable = errors.New("read the site config for image admission")

// stepImageAdmission holds a governed member's image to image admission (images.admission and the
// profile's required_image_signers), at create and Review. The preview makes no registry read.
func (s *Server) stepImageAdmission(f *runFold, rec *foldRecorder) bool {
	v, warning, refusal := s.imageAdmission(f.r, *f.req, f.carriedImage, f.ceiling.Limits.RequiredImageSigners, f.mode == foldCreate)
	if refusal.write(s, f.w, f.r) {
		return false
	}
	f.admission = v
	if warning != "" {
		f.policyWarns = append(f.policyWarns, warning)
	}
	return true
}

// imageAdmissionRefused is the verdict a refusal's audit row carries; a refused run has no run row.
const imageAdmissionRefused = "refused"

// auditImageAdmissionDenied writes the run.create denied row for a refusal at create, with the reason and
// the check's facts, so an admin can find what admission refused. Preflight is a dry run and writes none.
func (s *Server) auditImageAdmissionDenied(r *http.Request, ref string, v *types.ImageAdmissionVerdict, reason string) {
	if s.cfg.Audit == nil { // auditEvent stamps from cfg.Now, which a Server assembled without New() lacks
		return
	}
	at, principal := actorFromRequest(r)
	s.recordAudit(r.Context(), s.auditEvent(nil, at, principal, "run.create", "", "denied",
		mustJSON(map[string]any{"subject": principal, "reason": reason, "image": ref, "image_admission": v})))
}

// imageAdmission decides the member's image: the verdict the run row and the run.create row keep (nil when
// no check applies), audit mode's warning ("" for none), or the refusal. With a refusal from the check, the
// verdict is the refused one, for the denied audit row only; at create (not preflight, a dry run) that row
// is written here.
//
// The site config is read before the mode is known, so an unreadable config refuses (500) even on an
// install whose admission is off: the mode lives in that config, and reading it as off would admit
// unchecked images on an enforcing install.
//
// Default off: with the site mode off and no profile signer it returns before any check, so nothing an
// install refused, wrote or answered changes until an admin turns it on. A profile's signers are enforced
// whatever the site mode: a profile only ever adds requirements. The input is the digest rule's
// (createDigestInput): the member's image, an attached workspace's base image, or a policy-carried one; the
// convention image is not a member's choice. Operators and the break-glass admin are not checked, as with
// the digest rule.
func (s *Server) imageAdmission(r *http.Request, req createRunRequest, carried string, profile []string, create bool) (*types.ImageAdmissionVerdict, string, *runRefusal) {
	ctx := r.Context()
	ref := createDigestInput(req, carried)
	if ref == "" || s.runUngoverned(ctx) {
		return nil, "", nil
	}
	sc, ok := s.siteConfigSnapshot(ctx)
	if s.cfg.Store != nil && !ok {
		return nil, "", runServerError("resolve image admission", errImageAdmissionSiteUnreadable)
	}
	adm := siteImageAdmission(sc)
	mode := adm.EffectiveMode()
	if mode == types.ImageAdmissionOff && len(profile) == 0 {
		return nil, "", nil
	}
	siteEnforced := mode == types.ImageAdmissionEnforce
	held := types.ImageAdmissionAudit
	if siteEnforced || len(profile) > 0 {
		held = types.ImageAdmissionEnforce
	}
	// A macOS VM never runs the member image (its only container is the egress proxy), so there is nothing to check.
	if types.MachineKind(req.MachineKind).IsMacOSVM() {
		return &types.ImageAdmissionVerdict{Mode: held, Verdict: types.ImageAdmissionNotApplicable}, "", nil
	}
	var verdict *types.ImageAdmissionVerdict
	if held == types.ImageAdmissionEnforce {
		v, refusal := s.checkImage(ctx, adm, ref, siteEnforced, profile)
		if refusal != nil {
			refused := &types.ImageAdmissionVerdict{Mode: types.ImageAdmissionEnforce, Verdict: imageAdmissionRefused,
				Reason: refusal.body.Reason, Digest: v.Digest, Signer: v.Signer}
			if create {
				s.auditImageAdmissionDenied(r, ref, refused, refusal.body.Reason)
			}
			return refused, "", refusal
		}
		pinned, err := imageAdmissionPinnedRef(ref, v.Digest)
		if err != nil {
			return nil, "", runServerError("pin the admitted image", err)
		}
		verdict = admittedImage(types.ImageAdmissionEnforce, v)
		verdict.PinnedRef = pinned
	}
	if mode != types.ImageAdmissionAudit {
		return verdict, "", nil
	}
	// Audit: the site's signers are asked on their own and only reported. Nothing is refused or pinned here.
	v, refusal := s.checkImage(ctx, adm, ref, true, nil)
	switch {
	case refusal == nil && verdict == nil:
		return admittedImage(types.ImageAdmissionAudit, v), "", nil
	case refusal == nil:
		return verdict, "", nil
	case verdict != nil:
		verdict.SiteAudit = refusal.body.Reason
	default:
		verdict = &types.ImageAdmissionVerdict{Mode: types.ImageAdmissionAudit, Verdict: types.ImageAdmissionWouldRefuse,
			Reason: refusal.body.Reason, Digest: v.Digest}
	}
	return verdict, imageAdmissionAuditPrefix + refusal.body.Error, nil
}

// checkImage is one check: coverage first, with no registry read, then the registry and the signatures. The
// refusal is nil when the image is admitted. Why a check could not be made goes to the log as a fixed cause word and the image's registry.
func (s *Server) checkImage(ctx context.Context, adm *types.SiteImageAdmission, ref string, site bool, profile []string) (imageadmit.Verdict, *runRefusal) {
	return s.checkImageAt(ctx, adm, ref, ref, site, profile)
}

// checkImageAt is checkImage with coverage asked about ref and the registry about at, a pin of ref: a revive
// re-checks the digest its create admitted, never the tag.
func (s *Server) checkImageAt(ctx context.Context, adm *types.SiteImageAdmission, ref, at string, site bool, profile []string) (imageadmit.Verdict, *runRefusal) {
	a := s.cfg.ImageAdmitter
	if a == nil {
		return imageadmit.Verdict{}, runError(http.StatusServiceUnavailable, reasonImageAdmissionUnavailable, fmt.Sprintf(imageAdmitterMissingMsg, ref))
	}
	req, v, err := a.Require(adm, ref, site, profile)
	switch {
	case err != nil:
		slog.ErrorContext(ctx, "api: image admission settings could not be compiled", slog.String("image", ref), slog.Any("err", err))
		return imageadmit.Verdict{}, runError(http.StatusServiceUnavailable, reasonImageAdmissionUnavailable, fmt.Sprintf(imagePolicyUnusableMsg, ref))
	case v != nil:
		return *v, imageAdmissionRefusal(ref, *v)
	}
	req.Ref = at
	got, err := a.Admit(ctx, req)
	if err != nil {
		slog.WarnContext(ctx, "api: image admission check could not be made", slog.String("image", ref), slog.String("cause", imageadmit.LogCause(err)), slog.String("registry", imageadmit.LogRegistry(err)))
		got.Reason = imageadmit.AdmissionUnavailable
	}
	return got, imageAdmissionRefusal(ref, got)
}

func admittedImage(mode string, v imageadmit.Verdict) *types.ImageAdmissionVerdict {
	return &types.ImageAdmissionVerdict{Mode: mode, Verdict: types.ImageAdmissionAdmitted, Digest: v.Digest, Signer: v.Signer,
		Identity: v.Identity, Issuer: v.Issuer, KeyID: v.KeyID, Attestations: v.Attestations, Cached: v.Cached}
}

// imageAdmissionRefusal is a check's answer as a run refusal: nil when the image is admitted.
func imageAdmissionRefusal(ref string, v imageadmit.Verdict) *runRefusal {
	named := imageWithShortDigest(ref, v.Digest)
	var reason, msg string
	switch v.Reason {
	case "":
		return nil
	case imageadmit.NoPolicy:
		reason = reasonImageAdmissionNoPolicy
		switch {
		case v.Signer == "":
			msg = fmt.Sprintf(imageNoPolicyMsg, ref)
		case v.Undefined:
			msg = fmt.Sprintf(imageProfileUndefinedMsg, ref, v.Signer)
		default:
			msg = fmt.Sprintf(imageProfileUncoveredMsg, ref, v.Signer)
		}
	case imageadmit.SignatureMissing:
		reason = reasonImageSignatureMissing
		signers := v.Signers
		if v.Signer != "" {
			signers = []string{v.Signer}
		}
		msg = fmt.Sprintf(imageSignatureMissingMsg, named, strings.Join(signers, ", "))
	case imageadmit.SignatureInvalid:
		reason = reasonImageSignatureInvalid
		if v.FoundIssuer == "" && v.WantIssuer == "" {
			// A key signer never learns which key signed: the other key cannot be verified, only named by the bundle.
			msg = fmt.Sprintf(imageSignatureInvalidKeyMsg, named, v.Signer, v.Want)
		} else {
			msg = fmt.Sprintf(imageSignatureInvalidMsg, named, v.Found, v.FoundIssuer, v.Signer, v.Want, v.WantIssuer)
		}
	case imageadmit.AttestationMissing:
		reason = reasonImageAttestationMissing
		msg = fmt.Sprintf(imageAttestationMissingMsg, named, v.Want, v.Signer)
	case imageadmit.AdmissionUnavailable:
		if v.Detail != "" {
			return runError(http.StatusServiceUnavailable, reasonImageAdmissionUnavailable, fmt.Sprintf(imageAdmissionUnavailableMsg, ref, v.Detail))
		}
		fallthrough
	default:
		// A reason this build does not know is never an admission.
		return runError(http.StatusServiceUnavailable, reasonImageAdmissionUnavailable, fmt.Sprintf(imageNoAnswerMsg, ref))
	}
	return runError(http.StatusForbidden, reason, msg)
}

// imageWithShortDigest is "ref (sha256:<first 12 hex>)", or ref alone before a digest is known.
func imageWithShortDigest(ref, digest string) string {
	if digest == "" {
		return ref
	}
	if algo, hex, ok := strings.Cut(digest, ":"); ok && len(hex) > 12 {
		digest = algo + ":" + hex[:12]
	}
	return ref + " (" + digest + ")"
}

// imageAdmissionPinnedRef pins ref to the verified digest: its canonical name, its tag if it named one, and
// @digest (registry/team/app:1@sha256:...). A digest already in ref is replaced by the verified one.
// Docker, containerd and the kubelet pull the digest and ignore the tag.
func imageAdmissionPinnedRef(ref, digest string) (string, error) {
	if !imagePinnedByDigest("x@" + digest) {
		return "", fmt.Errorf("image admission returned a malformed digest %q", digest)
	}
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return "", fmt.Errorf("parse image %q: %w", ref, err)
	}
	pinned := reference.TrimNamed(named).String()
	if tagged, ok := named.(reference.Tagged); ok {
		pinned += ":" + tagged.Tag()
	}
	return pinned + "@" + digest, nil
}

// stampImageAdmission puts the run's verdict on its run.create row; the key is absent when there is none, so
// a deployment with admission off writes the rows it always did.
func stampImageAdmission(data map[string]any, v *types.ImageAdmissionVerdict) {
	if v != nil {
		data["image_admission"] = v
	}
}

// siteImageAdmission is the site's images.admission block, nil when none is set (which reads as off).
func siteImageAdmission(sc types.SiteConfig) *types.SiteImageAdmission {
	if sc.Images == nil {
		return nil
	}
	return sc.Images.Admission
}

// imageAdmissionPreview is the policy preview's image_admission fact: "checked at launch" when launch would
// check the member's image (imageAdmission's decisions up to the check itself), "" when it would not: no
// member image, an operator or the break-glass admin, a macOS VM, or the site mode off with no profile
// signer. It reads no registry. An unreadable site config reports the check, since launch then refuses.
func (s *Server) imageAdmissionPreview(ctx context.Context, req createRunRequest, carried string, profile []string) client.ImageAdmissionFact {
	if createDigestInput(req, carried) == "" || s.runUngoverned(ctx) || types.MachineKind(req.MachineKind).IsMacOSVM() {
		return ""
	}
	if sc, ok := s.siteConfigSnapshot(ctx); ok || s.cfg.Store == nil {
		if siteImageAdmission(sc).EffectiveMode() == types.ImageAdmissionOff && len(profile) == 0 {
			return ""
		}
	}
	return client.ImageAdmissionCheckedAtLaunch
}

// markImageAdmissionExempt stamps image_admission_exempt on a run.create datum when the site mode is on
// (enforce or audit) and an operator's or the break-glass admin's run names a member image, which admission
// does not check, so the org can see the runs it did not bind. A profile's signers bind governed callers only,
// so they never make an operator's run exempt. The key is absent otherwise, and when the site config cannot
// be read (as with image_digest_exempt), so a deployment with admission off writes the rows it always did.
func (s *Server) markImageAdmissionExempt(ctx context.Context, image string, data map[string]any) {
	if image == "" || !s.runUngoverned(ctx) {
		return
	}
	if sc, ok := s.siteConfigSnapshot(ctx); ok && siteImageAdmission(sc).EffectiveMode() != types.ImageAdmissionOff {
		data["image_admission_exempt"] = true
	}
}
