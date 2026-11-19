// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package imageadmit

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"
)

// Registry is what a check reads from the image's registry. *RegistryClient is the implementation; LegacyTag
// is for cosign's legacy signature tags.
type Registry interface {
	Manifest(ctx context.Context, ref Ref) (*Manifest, error)
	Referrers(ctx context.Context, ref Ref, subject, artifactType string) ([]Descriptor, error)
	LegacyTag(ctx context.Context, ref Ref, subject, suffix string) (*Manifest, error)
	Blob(ctx context.Context, ref Ref, d Descriptor) ([]byte, error)
}

// CheckTimeout bounds one check: a caller waits at most this long, and a shared check reads for at most this long.
const CheckTimeout = 20 * time.Second

// bundleArtifactTypes are the referrer artifact types that carry a Sigstore bundle, newest first. Each is asked
// for on its own, so referrers of other types (SBOMs, scans) never count against MaxReferrers.
var bundleArtifactTypes = []string{"application/vnd.dev.sigstore.bundle.v0.3+json", "application/vnd.dev.sigstore.bundle+json;version=0.2"}

// bundleMediaType prefixes the media type of a referrer layer that holds a bundle.
const bundleMediaType = "application/vnd.dev.sigstore.bundle"

var timeoutDetail = fmt.Sprintf("the check took longer than %d seconds", int(CheckTimeout/time.Second))

// AdmitterOptions configures an Admitter.
type AdmitterOptions struct {
	Now func() time.Time // the clock verdict expiry reads; default time.Now
}

// Admitter checks images against compiled requirements, reading signatures through a Registry and keeping
// verdicts per replica (see PositiveTTL). Safe for concurrent use.
type Admitter struct {
	reg    Registry
	now    func() time.Time
	cache  *verdictCache
	flight singleflight.Group
}

// NewAdmitter returns an Admitter that reads through reg.
func NewAdmitter(reg Registry, o AdmitterOptions) *Admitter {
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Admitter{reg: reg, now: o.Now, cache: newVerdictCache()}
}

// Admit checks the image req names (req.Ref, from Policy.Require) against req.
//   - Requirements with no signer refuse no_policy with no registry read, so only a registry an admin's signer
//     covers is ever contacted.
//   - A tag is resolved to a digest on every call and never kept, so a moved tag is checked again; the digest
//     comes from the manifest bytes. A digest reference is fetched by digest on a miss.
//   - The verdict is kept for req.Fingerprint() and the digest. Concurrent checks of one key share one set of
//     reads, and a caller that gives up does not fail the others.
//
// The error is non-nil exactly when the verdict is image_admission_unavailable: it is the registry's error, for
// the log, and Verdict.Detail is its public sentence. Such a verdict is never kept.
func (a *Admitter) Admit(ctx context.Context, req Requirements) (Verdict, error) {
	if len(req.AnyOf) == 0 && len(req.AllOf) == 0 {
		return Verdict{Reason: NoPolicy}, nil
	}
	ref, err := ParseRef(req.Ref)
	if err != nil {
		return Verdict{Reason: AdmissionUnavailable, Detail: fmt.Sprintf("%q is not an image reference Wardyn can read", req.Ref)}, err
	}
	ctx, cancel := context.WithTimeout(ctx, CheckTimeout)
	defer cancel()
	digest := ref.Digest
	if digest == "" {
		m, err := a.reg.Manifest(ctx, ref)
		if err != nil {
			return unavailable(ref, "", err), err
		}
		digest = m.Digest
	}
	key := cacheKey{fingerprint: req.Fingerprint(), digest: digest}
	if v, ok := a.cache.get(key, a.now()); ok {
		v.Cached = true
		return v, nil
	}
	ch := a.flight.DoChan(string(key.fingerprint[:])+digest, func() (any, error) {
		return a.check(context.WithoutCancel(ctx), req, ref, key)
	})
	select {
	case r := <-ch:
		return cloneVerdict(r.Val.(Verdict)), r.Err
	case <-ctx.Done():
		return unavailable(ref, digest, ctx.Err()), ctx.Err()
	}
}

// check reads the signatures for key's digest, from its referrers and its legacy cosign tags, verifies them and
// keeps the verdict. It runs once per key at a time, under its own deadline. A legacy tag the registry has not
// got adds nothing; any other legacy read error fails the check, as a referrer read does.
func (a *Admitter) check(ctx context.Context, req Requirements, ref Ref, key cacheKey) (Verdict, error) {
	if v, ok := a.cache.get(key, a.now()); ok { // a check that just ended kept it
		v.Cached = true
		return v, nil
	}
	ctx, cancel := context.WithTimeout(ctx, CheckTimeout)
	defer cancel()
	if ref.Digest != "" {
		if _, err := a.reg.Manifest(ctx, ref); err != nil {
			return unavailable(ref, key.digest, err), err
		}
	}
	repo := Ref{Registry: ref.Registry, Repo: ref.Repo}
	bundles, err := a.bundles(ctx, repo, key.digest)
	if err != nil {
		return unavailable(ref, key.digest, err), err
	}
	legacy, err := ReadLegacy(ctx, a.reg, repo, key.digest)
	if err != nil {
		return unavailable(ref, key.digest, err), err
	}
	v := CheckCandidates(req, key.digest, Candidates{Bundles: bundles, Legacy: legacy})
	a.cache.put(key, v, a.now())
	return v, nil
}

// bundles reads the Sigstore bundles that refer to digest: from at most MaxReferrers referrers in all, and only
// the first MaxCandidatesPerSigner bundles, since Check verifies no more. A referrer whose manifest or blob is
// missing, too large or not its digest is skipped, as an unsigned one is; a registry that cannot be read fails
// the whole check. A referrers index that does not parse lists none.
func (a *Admitter) bundles(ctx context.Context, repo Ref, digest string) ([][]byte, error) {
	var refs []Descriptor
	for _, at := range bundleArtifactTypes {
		ds, err := a.reg.Referrers(ctx, repo, digest, at)
		if errors.Is(err, ErrMalformed) { // an index that does not parse names no bundle: unsigned, as an empty one is
			continue
		}
		if err != nil {
			return nil, err
		}
		refs = append(refs, ds...)
	}
	if len(refs) > MaxReferrers {
		refs = refs[:MaxReferrers]
	}
	var out [][]byte
	for _, d := range refs {
		if len(out) == MaxCandidatesPerSigner {
			break
		}
		b, err := a.bundle(ctx, repo, d)
		switch {
		case err == nil && b != nil:
			out = append(out, b)
		case err != nil && registryDown(err):
			return nil, err
		}
	}
	return out, nil
}

// bundle fetches the bundle a referrer manifest holds as its first bundle layer; nil when it holds none.
func (a *Admitter) bundle(ctx context.Context, repo Ref, d Descriptor) ([]byte, error) {
	m, err := a.reg.Manifest(ctx, Ref{Registry: repo.Registry, Repo: repo.Repo, Digest: d.Digest})
	if err != nil {
		return nil, err
	}
	for _, l := range m.Layers {
		if strings.HasPrefix(l.MediaType, bundleMediaType) {
			return a.reg.Blob(ctx, repo, l)
		}
	}
	return nil, nil
}

// registryDown reports whether err is the registry failing, rather than one referrer's content.
func registryDown(err error) bool {
	var u *Unavailable
	return errors.As(err, &u) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

func unavailable(ref Ref, digest string, err error) Verdict {
	return Verdict{Reason: AdmissionUnavailable, Digest: digest, Detail: unavailableDetail(ref, err)}
}

// unavailableDetail is why a check could not be made, for the sentence "Wardyn could not check image <ref>:
// <detail>. Try again, or ask an admin." It names the registry the image ref names and settings, never a
// credential, and never a host a redirect or token realm named (Unavailable.Host), which the registry chose. The
// failing request may have gone to such a host, so a sentence says "checking images from <registry>", never that
// the registry itself answered.
func unavailableDetail(ref Ref, err error) string {
	var u *Unavailable
	if errors.As(err, &u) {
		switch u.Cause {
		case CauseUnauthorized:
			if u.Withheld {
				return "checking images from " + ref.Registry + " got 401 from an address it redirected to, where its credential is not sent"
			}
			if u.HasCredential {
				return "checking images from " + ref.Registry + " got 401 for its credential in WARDYN_REGISTRY_AUTH_FILE"
			}
			return "checking images from " + ref.Registry + " got 401 — no credential for it in WARDYN_REGISTRY_AUTH_FILE"
		case CauseForbidden:
			return "checking images from " + ref.Registry + " got 403"
		case CauseTLS:
			return "a TLS certificate met while checking images from " + ref.Registry + " is not trusted (WARDYN_TRUSTED_CA_FILE)"
		case CauseTimeout:
			return timeoutDetail
		case CauseServer:
			return "checking images from " + ref.Registry + " got an error answer"
		case CauseRefused:
			return ref.Registry + " or an address it named is not one Wardyn contacts"
		case CauseAuthFile:
			return "WARDYN_REGISTRY_AUTH_FILE could not be read"
		}
		return "a host met while checking images from " + ref.Registry + " could not be reached"
	}
	image := ref.Repo + ":" + ref.Tag
	if ref.Digest != "" {
		image = ref.Repo + "@" + ref.Digest
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return timeoutDetail
	case errors.Is(err, context.Canceled):
		return "the check was cancelled"
	case errors.Is(err, ErrNotFound):
		return ref.Registry + " has no image " + image + " (check the name and tag)"
	case errors.Is(err, ErrDigestMismatch):
		return ref.Registry + " answered content that does not match its digest"
	case errors.Is(err, ErrTooLarge):
		return ref.Registry + " answered more than Wardyn reads for one image"
	}
	return ref.Registry + " could not be read"
}

// LogCause names why a check could not be made with a fixed word, for wardynd's log. It never returns text
// taken from the error, so nothing a registry or the auth file path put into the error reaches the log.
func LogCause(err error) string {
	var u *Unavailable
	if errors.As(err, &u) {
		switch u.Cause {
		case CauseUnauthorized:
			return "unauthorized"
		case CauseForbidden:
			return "forbidden"
		case CauseUnreachable:
			return "unreachable"
		case CauseTLS:
			return "tls"
		case CauseTimeout:
			return "timeout"
		case CauseServer:
			return "server"
		case CauseRefused:
			return "refused"
		case CauseAuthFile:
			return "auth_file"
		}
		return "unavailable"
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, ErrNotFound):
		return "not_found"
	case errors.Is(err, ErrDigestMismatch):
		return "digest_mismatch"
	case errors.Is(err, ErrTooLarge):
		return "too_large"
	}
	return "other"
}

// LogRegistry is the registry the image ref names, for wardynd's log beside LogCause; "" when the error is
// not an *Unavailable. It is never the host a redirect or token realm named, which the registry chose.
func LogRegistry(err error) string {
	var u *Unavailable
	if errors.As(err, &u) {
		return u.Registry
	}
	return ""
}
