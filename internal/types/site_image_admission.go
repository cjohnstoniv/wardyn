// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode"
)

// Image admission modes and scopes (SiteImageAdmission.Mode and .Scope).
const (
	ImageAdmissionOff     = "off"
	ImageAdmissionAudit   = "audit"
	ImageAdmissionEnforce = "enforce"

	ImageAdmissionScopeMember = "member"
	ImageAdmissionScopeAll    = "all"
)

// BuiltinImageSigner and PublicGoodTrustedRoot are names the site config cannot define: the first is the
// release signer every install knows, the second the trust root built into the binary.
const (
	BuiltinImageSigner    = "wardyn-release"
	PublicGoodTrustedRoot = "public-good"
)

// Bounds on what an admin may store, so one write cannot grow the site config without limit.
const (
	MaxImageAdmissionSigners = 64
	MaxImageAdmissionRoots   = 16
	MaxImageSignerEntries    = 256 // images, require_attestations and signature_predicates, each
	MaxImageSignerFieldLen   = 4096
	MaxRequiredImageSigners  = MaxImageAdmissionSigners + 1 // every site signer and the built-in one
)

// SiteImageAdmission is SiteImages.Admission: whether a member's image must be vouched for by a signer the
// admin named before a run may start it. OFF by default, because it adds a registry read to every create and
// would refuse images an install runs today; an install that never writes the block changes nothing.
// Operators are not bound by it (their runs are marked in the audit row), as with RequireDigest.
type SiteImageAdmission struct {
	// Mode is off (the default), audit (a would-refuse is a warning and a recorded verdict, nothing is
	// refused) or enforce. Signers may be defined while the mode is off: a profile can still require them.
	Mode string `json:"mode,omitempty"`
	// Scope is member (the default: the image a member names) or all (also the convention, harness and
	// architecture-variant images the operator chose).
	Scope string `json:"scope,omitempty"`
	// TrustedRoots are extra Sigstore trust roots a keyless signer may name. "public-good" always exists.
	TrustedRoots []ImageTrustedRoot `json:"trusted_roots,omitempty"`
	// Signers are the identities and keys whose signature admits an image.
	Signers []ImageSigner `json:"signers,omitempty"`
}

// ImageTrustedRoot names a Sigstore trusted-root file (the JSON a private Sigstore deployment publishes).
type ImageTrustedRoot struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// ImageSigner is one signer: a keyless identity or a public key, and the images it covers. Exactly one of
// Keyless and PublicKey is set.
type ImageSigner struct {
	Name string `json:"name"`
	// Images are the registry prefixes ("registry.example/team/*") and exact references this signer covers.
	Images  []string      `json:"images"`
	Keyless *ImageKeyless `json:"keyless,omitempty"`
	// PublicKey is a PEM public key: ECDSA P-256 or P-384, Ed25519, or RSA of at least 2048 bits.
	PublicKey string `json:"public_key,omitempty"`
	// TransparencyLog is whether a key signature must be in the transparency log. Absent reads true.
	TransparencyLog *bool `json:"transparency_log,omitempty"`
	// RequireAttestations are in-toto predicate types that must also be attested by this signer.
	RequireAttestations []string `json:"require_attestations,omitempty"`
	// SignaturePredicates adds predicate types whose statement counts as this signer's signature, beyond
	// the built-in set. A statement of any other predicate type never counts as a signature.
	SignaturePredicates []string `json:"signature_predicates,omitempty"`
}

// ImageKeyless is a keyless signer's certificate identity. Exactly one of Identity and IdentityRegexp is set.
type ImageKeyless struct {
	Issuer string `json:"issuer"`
	// Identity is the exact certificate subject alternative name.
	Identity string `json:"identity,omitempty"`
	// IdentityRegexp is an anchored (^...$) pattern for the subject alternative name.
	IdentityRegexp string `json:"identity_regexp,omitempty"`
	// SourceRepository optionally matches the certificate's source repository extension.
	SourceRepository string `json:"source_repository,omitempty"`
	// TrustedRoot names the trust root; absent reads public-good.
	TrustedRoot string `json:"trusted_root,omitempty"`
}

// EffectiveMode is the mode with the default applied. A nil block is off.
func (a *SiteImageAdmission) EffectiveMode() string {
	if a == nil || a.Mode == "" {
		return ImageAdmissionOff
	}
	return a.Mode
}

// EffectiveScope is the scope with the default applied.
func (a *SiteImageAdmission) EffectiveScope() string {
	if a == nil || a.Scope == "" {
		return ImageAdmissionScopeMember
	}
	return a.Scope
}

// IsZero is whether the block says nothing a missing block would not: off, member scope, nothing defined.
// PUT /site-config stores such a block as none, so a GET stays what it was before the setting existed.
func (a *SiteImageAdmission) IsZero() bool {
	return a == nil || (a.EffectiveMode() == ImageAdmissionOff && a.EffectiveScope() == ImageAdmissionScopeMember &&
		len(a.TrustedRoots) == 0 && len(a.Signers) == 0)
}

// SignerNames are the names of the signers the site defines, in order. The built-in signer is not listed.
func (a *SiteImageAdmission) SignerNames() []string {
	if a == nil {
		return nil
	}
	names := make([]string, 0, len(a.Signers))
	for _, s := range a.Signers {
		names = append(names, s.Name)
	}
	return names
}

// IsZero is whether the block sets no image rule.
func (i SiteImages) IsZero() bool { return !i.RequireDigest && i.Admission.IsZero() }

var (
	imageSignerNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
	errNotAnchored    = errors.New("must be anchored: start with ^ and end with $")
)

// ValidImageSignerName is whether s can name a signer or a trust root: lowercase letters, digits, '.', '_'
// and '-', at most 63 characters, starting with a letter or digit.
func ValidImageSignerName(s string) bool { return imageSignerNameRE.MatchString(s) }

// ValidRequiredImageSigners refuses a required-signers list a profile could not hold: a malformed or repeated
// name, or more than MaxRequiredImageSigners. Whether each name is a signer the site defines is the writer's
// check, since only it can read the site config. The message names no field; the caller adds it. An empty
// list is valid and requires nothing.
func ValidRequiredImageSigners(list []string) error {
	if len(list) > MaxRequiredImageSigners {
		return fmt.Errorf("at most %d signers", MaxRequiredImageSigners)
	}
	seen := make(map[string]bool, len(list))
	for _, n := range list {
		switch {
		case !ValidImageSignerName(n):
			return fmt.Errorf("%q is not a signer name — use lowercase letters, digits, '.', '_' and '-'", n)
		case seen[n]:
			return fmt.Errorf("%q is listed twice", n)
		}
		seen[n] = true
	}
	return nil
}

// Validate is the write-time shape check of the block; the first problem is returned, its message naming the
// field. prefixOK says whether an images entry is a well-formed registry prefix or image reference (the
// image-grant grammar, which lives with the API). Reading a trust root's file is the caller's too. A nil
// block is valid.
func (a *SiteImageAdmission) Validate(prefixOK func(string) bool) error {
	if a == nil {
		return nil
	}
	switch a.Mode {
	case "", ImageAdmissionOff, ImageAdmissionAudit, ImageAdmissionEnforce:
	default:
		return fmt.Errorf("mode: %q is not a mode — use off, audit or enforce", a.Mode)
	}
	switch a.Scope {
	case "", ImageAdmissionScopeMember, ImageAdmissionScopeAll:
	default:
		return fmt.Errorf("scope: %q is not a scope — use member or all", a.Scope)
	}
	if len(a.TrustedRoots) > MaxImageAdmissionRoots {
		return fmt.Errorf("trusted_roots: at most %d roots", MaxImageAdmissionRoots)
	}
	roots := map[string]bool{PublicGoodTrustedRoot: true}
	for i, r := range a.TrustedRoots {
		switch {
		case !ValidImageSignerName(r.Name):
			return fmt.Errorf("trusted_roots[%d].name: %q is not a name — use lowercase letters, digits, '.', '_' and '-'", i, r.Name)
		case r.Name == PublicGoodTrustedRoot:
			return fmt.Errorf("trusted_roots[%d].name: %q is built in and cannot be redefined", i, r.Name)
		case roots[r.Name]:
			return fmt.Errorf("trusted_roots[%d].name: %q is listed twice", i, r.Name)
		case !strings.HasPrefix(r.Path, "/") || strings.ContainsFunc(r.Path, unicode.IsControl) || len(r.Path) > MaxImageSignerFieldLen:
			return fmt.Errorf("trusted_roots[%d].path: must be an absolute path", i)
		}
		roots[r.Name] = true
	}
	if len(a.Signers) > MaxImageAdmissionSigners {
		return fmt.Errorf("signers: at most %d signers", MaxImageAdmissionSigners)
	}
	names := map[string]bool{}
	for i, s := range a.Signers {
		if err := s.validate(roots, prefixOK); err != nil {
			return fmt.Errorf("signers[%d]: %w", i, err)
		}
		if names[s.Name] {
			return fmt.Errorf("signers[%d].name: %q is listed twice", i, s.Name)
		}
		names[s.Name] = true
	}
	if a.Mode == ImageAdmissionEnforce && len(a.Signers) == 0 && a.EffectiveScope() == ImageAdmissionScopeMember {
		return errors.New("mode: enforce with no signers would refuse every member image — define a signer, or use audit first")
	}
	return nil
}

func (s ImageSigner) validate(roots map[string]bool, prefixOK func(string) bool) error {
	switch {
	case !ValidImageSignerName(s.Name):
		return fmt.Errorf("name: %q is not a name — use lowercase letters, digits, '.', '_' and '-'", s.Name)
	case s.Name == BuiltinImageSigner:
		return fmt.Errorf("name: %q is built in and cannot be redefined", s.Name)
	case len(s.Images) > MaxImageSignerEntries:
		return fmt.Errorf("images: at most %d entries", MaxImageSignerEntries)
	}
	for i, im := range s.Images {
		if len(im) > MaxImageSignerFieldLen || !prefixOK(im) {
			return fmt.Errorf("images[%d]: %q is not an image reference or a registry prefix (registry.example.com/team/*)", i, im)
		}
	}
	switch {
	case s.Keyless == nil && s.PublicKey == "":
		return errors.New("keyless: a signer needs either keyless or public_key")
	case s.Keyless != nil && s.PublicKey != "":
		return errors.New("a signer takes keyless or public_key, not both")
	case s.Keyless != nil:
		if err := s.Keyless.validate(roots); err != nil {
			return fmt.Errorf("keyless.%w", err)
		}
	default:
		if err := validateImageSignerKey(s.PublicKey); err != nil {
			return fmt.Errorf("public_key: %w", err)
		}
	}
	if err := validateTypeURIs("require_attestations", s.RequireAttestations); err != nil {
		return err
	}
	return validateTypeURIs("signature_predicates", s.SignaturePredicates)
}

func (k ImageKeyless) validate(roots map[string]bool) error {
	if u, err := url.Parse(k.Issuer); err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("issuer: %q is not an https URL", k.Issuer)
	}
	switch {
	case k.Identity != "" && k.IdentityRegexp != "":
		return errors.New("identity: set identity or identity_regexp, not both")
	case k.Identity == "" && k.IdentityRegexp == "":
		return errors.New("identity: set identity or identity_regexp")
	case len(k.Identity) > MaxImageSignerFieldLen || len(k.IdentityRegexp) > MaxImageSignerFieldLen:
		return errors.New("identity: too long")
	}
	if k.IdentityRegexp != "" {
		if !strings.HasPrefix(k.IdentityRegexp, "^") || !strings.HasSuffix(k.IdentityRegexp, "$") {
			return fmt.Errorf("identity_regexp: %w", errNotAnchored)
		}
		if _, err := regexp.Compile(k.IdentityRegexp); err != nil {
			return fmt.Errorf("identity_regexp: does not compile: %v", err)
		}
	}
	if k.SourceRepository != "" {
		if u, err := url.Parse(k.SourceRepository); err != nil || u.Scheme != "https" || u.Host == "" {
			return fmt.Errorf("source_repository: %q is not an https URL", k.SourceRepository)
		}
	}
	if k.TrustedRoot != "" && !roots[k.TrustedRoot] {
		return fmt.Errorf("trusted_root: %q is not defined — name public-good or a root in trusted_roots", k.TrustedRoot)
	}
	return nil
}

// validateImageSignerKey accepts one PEM public key of a kind the verifier can hold.
func validateImageSignerKey(p string) error {
	block, rest := pem.Decode([]byte(p))
	if block == nil || strings.TrimSpace(string(rest)) != "" {
		return errors.New("not a single PEM public key")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return errors.New("not a PEM public key (PKIX)")
	}
	switch k := pub.(type) {
	case ed25519.PublicKey:
	case *ecdsa.PublicKey:
		if k.Curve != elliptic.P256() && k.Curve != elliptic.P384() {
			return errors.New("an ECDSA key must be P-256 or P-384")
		}
	case *rsa.PublicKey:
		if k.N.BitLen() < 2048 {
			return errors.New("an RSA key must be at least 2048 bits")
		}
	default:
		return errors.New("use an ECDSA P-256 or P-384, Ed25519 or RSA key of at least 2048 bits")
	}
	return nil
}

// validateTypeURIs refuses an in-toto predicate-type list holding an empty entry, whitespace, a control
// character or an over-long value, or past the entry bound.
func validateTypeURIs(field string, list []string) error {
	if len(list) > MaxImageSignerEntries {
		return fmt.Errorf("%s: at most %d entries", field, MaxImageSignerEntries)
	}
	for i, v := range list {
		if v == "" || len(v) > MaxImageSignerFieldLen || strings.ContainsFunc(v, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
			return fmt.Errorf("%s[%d]: %q is not a predicate type URI", field, i, v)
		}
	}
	return nil
}
