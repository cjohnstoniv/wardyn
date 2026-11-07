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
	imageSignatureInvalidMsg     = "Image %s is signed, but not by a signer this organisation trusts for it: found %s from %s; %s requires %s from %s"
	imageSignatureInvalidKeyMsg  = "Image %s is signed, but not by a signer this organisation trusts for it: signed with key %s; %s requires key %s"
	imageAttestationMissingMsg   = "Image %s has no %s attestation signed by %s, which this organisation requires"
	imageAdmissionUnavailableMsg = "Wardyn could not check image %s: %s. Try again, or ask an admin"
)

// The causes the step itself names in imageAdmissionUnavailableMsg.
const (
	imageAdmitterMissingDetail = "image admission is not running in this control plane"
	imagePolicyUnusableDetail  = "this organisation's image trust settings could not be loaded (a trust root file may be missing or unreadable)"
	imageNoAnswerDetail        = "the check gave no usable answer"
)

var errImageAdmissionSiteUnreadable = errors.New("read the site config for image admission")

// stepImageAdmission holds a governed member's image to image admission (images.admission and the
// profile's required_image_signers), at create and Review. The preview makes no registry read.
func (s *Server) stepImageAdmission(f *runFold, rec *foldRecorder) bool {
	return !s.imageAdmission(f).write(s, f.w, f.r)
}

// imageAdmission decides the member's image and leaves f.admission (the run row's verdict) and, in audit
// mode, the warning on f.policyWarns.
//
// Default off: with the site mode off and no profile signer it returns before any check, so nothing an
// install refused, wrote or answered changes until an admin turns it on. A profile's signers are enforced
// whatever the site mode: a profile only ever adds requirements. The input is the digest rule's
// (createDigestInput): the member's image, an attached workspace's base image, or a policy-carried one; the
// convention image is not a member's choice. Operators and the break-glass admin are not checked, as with
// the digest rule.
func (s *Server) imageAdmission(f *runFold) *runRefusal {
	ctx := f.r.Context()
	ref := createDigestInput(*f.req, f.carriedImage)
	if ref == "" || s.runUngoverned(ctx) {
		return nil
	}
	sc, ok := s.siteConfigSnapshot(ctx)
	if s.cfg.Store != nil && !ok {
		return runServerError("resolve image admission", errImageAdmissionSiteUnreadable)
	}
	var adm *types.SiteImageAdmission
	if sc.Images != nil {
		adm = sc.Images.Admission
	}
	mode, profile := adm.EffectiveMode(), f.ceiling.Limits.RequiredImageSigners
	if mode == types.ImageAdmissionOff && len(profile) == 0 {
		return nil
	}
	siteEnforced := mode == types.ImageAdmissionEnforce
	held := types.ImageAdmissionAudit
	if siteEnforced || len(profile) > 0 {
		held = types.ImageAdmissionEnforce
	}
	// A macOS VM never runs the member image (its only container is the egress proxy), so there is nothing to check.
	if types.MachineKind(f.req.MachineKind).IsMacOSVM() {
		f.admission = &types.ImageAdmissionVerdict{Mode: held, Verdict: types.ImageAdmissionNotApplicable}
		return nil
	}
	if held == types.ImageAdmissionEnforce {
		v := s.checkImage(ctx, adm, ref, siteEnforced, profile)
		if refusal := imageAdmissionRefusal(ref, v); refusal != nil {
			return refusal
		}
		pinned, err := imageAdmissionPinnedRef(ref, v.Digest)
		if err != nil {
			return runServerError("pin the admitted image", err)
		}
		f.admission = admittedImage(types.ImageAdmissionEnforce, v)
		f.admission.PinnedRef = pinned
	}
	if mode != types.ImageAdmissionAudit {
		return nil
	}
	// Audit: the site's signers are asked on their own and only reported. Nothing is refused or pinned here.
	v := s.checkImage(ctx, adm, ref, true, nil)
	refusal := imageAdmissionRefusal(ref, v)
	switch {
	case refusal != nil && f.admission != nil:
		f.admission.SiteAudit = refusal.body.Reason
	case refusal != nil:
		f.admission = &types.ImageAdmissionVerdict{Mode: types.ImageAdmissionAudit, Verdict: types.ImageAdmissionWouldRefuse,
			Reason: refusal.body.Reason, Digest: v.Digest}
	case f.admission == nil:
		f.admission = admittedImage(types.ImageAdmissionAudit, v)
	}
	if refusal != nil {
		f.policyWarns = append(f.policyWarns, imageAdmissionAuditPrefix+refusal.body.Error)
	}
	return nil
}

// checkImage is one check: coverage first, with no registry read, then the registry and the signatures. A
// check that could not be made is an image_admission_unavailable verdict whose Detail is the public cause;
// its error goes to the log only.
func (s *Server) checkImage(ctx context.Context, adm *types.SiteImageAdmission, ref string, site bool, profile []string) imageadmit.Verdict {
	a := s.cfg.ImageAdmitter
	if a == nil {
		return imageadmit.Verdict{Reason: imageadmit.AdmissionUnavailable, Detail: imageAdmitterMissingDetail}
	}
	req, v, err := a.Require(adm, ref, site, profile)
	switch {
	case err != nil:
		slog.ErrorContext(ctx, "api: image admission settings could not be compiled", slog.String("image", ref), slog.Any("err", err))
		return imageadmit.Verdict{Reason: imageadmit.AdmissionUnavailable, Detail: imagePolicyUnusableDetail}
	case v != nil:
		return *v
	}
	got, err := a.Admit(ctx, req)
	if err != nil {
		slog.WarnContext(ctx, "api: image admission check could not be made", slog.String("image", ref), slog.Any("err", err))
		got.Reason = imageadmit.AdmissionUnavailable
	}
	return got
}

func admittedImage(mode string, v imageadmit.Verdict) *types.ImageAdmissionVerdict {
	return &types.ImageAdmissionVerdict{Mode: mode, Verdict: types.ImageAdmissionAdmitted, Digest: v.Digest, Signer: v.Signer,
		Identity: v.Identity, Issuer: v.Issuer, KeyID: v.KeyID, Attestations: v.Attestations}
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
			msg = fmt.Sprintf(imageSignatureInvalidKeyMsg, named, v.Found, v.Signer, v.Want)
		} else {
			msg = fmt.Sprintf(imageSignatureInvalidMsg, named, v.Found, v.FoundIssuer, v.Signer, v.Want, v.WantIssuer)
		}
	case imageadmit.AttestationMissing:
		reason = reasonImageAttestationMissing
		msg = fmt.Sprintf(imageAttestationMissingMsg, named, v.Want, v.Signer)
	default:
		// AdmissionUnavailable, or a reason this build does not know: never an admission.
		detail := v.Detail
		if v.Reason != imageadmit.AdmissionUnavailable || detail == "" {
			detail = imageNoAnswerDetail
		}
		return runError(http.StatusServiceUnavailable, reasonImageAdmissionUnavailable, fmt.Sprintf(imageAdmissionUnavailableMsg, ref, detail))
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
