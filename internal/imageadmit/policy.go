// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package imageadmit decides whether a member's container image may run: it compiles the deployment's image
// signers, picks the ones that cover an image reference, and verifies Sigstore bundles for the image digest
// against them, offline. Reading the registry, caching and the run-create gate live with its callers.
package imageadmit

import (
	"cmp"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/imageadmit/rootfile"
	"github.com/cjohnstoniv/wardyn/internal/releasesig"
	"github.com/cjohnstoniv/wardyn/internal/version"
	"github.com/sigstore/sigstore-go/pkg/fulcio/certificate"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"
	"github.com/sigstore/sigstore/pkg/signature"
)

// PublicGood names the Sigstore public-good trust root embedded in the binary. It always exists.
const PublicGood = "public-good"

// ErrPolicy wraps every reason Compile refuses a policy.
var ErrPolicy = errors.New("image admission policy is invalid")

// Config is the trust part of the site setting images.admission: the trust roots and signers an admin wrote.
type Config struct {
	TrustedRoots []RootSpec   `json:"trusted_roots,omitempty"`
	Signers      []SignerSpec `json:"signers,omitempty"`
}

// RootSpec is a Sigstore trusted_root.json an admin supplies by absolute path, for a private Sigstore.
type RootSpec struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// SignerSpec is one trusted signer: the images it covers and who it is, keyless or by key.
type SignerSpec struct {
	Name    string       `json:"name"`
	Images  []string     `json:"images"`
	Keyless *KeylessSpec `json:"keyless,omitempty"`
	// PublicKey is a PEM public key: ECDSA P-256 or P-384, Ed25519, or RSA of at least 2048 bits.
	PublicKey string `json:"public_key,omitempty"`
	// TransparencyLog (key signers): require a transparency-log entry. Default true, because a key signature
	// without one carries no time and no public record; false is for air-gapped key signing.
	TransparencyLog *bool `json:"transparency_log,omitempty"`
	// RequireAttestations are predicate types the signer must also have attested about the image.
	RequireAttestations []string `json:"require_attestations,omitempty"`
	// SignaturePredicates add predicate types that count as this signer's signature, beyond the closed default
	// set (cosign sign, SLSA provenance v0.2 and v1).
	SignaturePredicates []string `json:"signature_predicates,omitempty"`
}

// KeylessSpec is a Fulcio certificate identity: an exact identity or an anchored pattern, the OIDC issuer,
// and optionally the source repository the certificate must name. IdentityRegexp must match the whole
// identity: it is matched as a group between \A and \z, so an alternation like ^a|b$ cannot match a prefix
// or a suffix. The issuer is always an exact string.
type KeylessSpec struct {
	Issuer           string `json:"issuer"`
	Identity         string `json:"identity,omitempty"`
	IdentityRegexp   string `json:"identity_regexp,omitempty"`
	SourceRepository string `json:"source_repository,omitempty"`
	TrustedRoot      string `json:"trusted_root,omitempty"` // default PublicGood, the root embedded in the binary
}

// Options are the build facts Compile needs.
type Options struct {
	Version        string                       // the running build; default version.String()
	PublicGoodRoot *root.TrustedRoot            // default: the embedded public-good root
	ReadFile       func(string) ([]byte, error) // reads a RootSpec.Path; default rootfile.Read in RootDir
	RootDir        string                       // where the default ReadFile reads; default rootfile.Dir
}

// Policy is a compiled Config plus the built-in signer.
type Policy struct {
	signers []*Signer // the admin's, in order, then the built-in
	byName  map[string]*Signer
}

// Signer is one compiled signer.
type Signer struct {
	name          string
	images        []string
	verifier      *verify.Verifier
	identity      *verify.CertificateIdentity // keyless
	want          string                      // the identity, pattern or key id the refusal names
	wantIssuer    string
	keyHints      map[string]bool // key signers: the hints that name this key
	sigPredicates map[string]bool
	attestations  []string
	fingerprint   [32]byte // sha256 of the canonical spec and the trust root used
}

// Name is the signer's name.
func (s *Signer) Name() string { return s.name }

// covers reports whether one of the signer's images entries covers ref.
func (s *Signer) covers(ref string) bool {
	for _, e := range s.images {
		if imagesEntryCovers(e, ref) {
			return true
		}
	}
	return false
}

type trustRoot struct {
	material *root.TrustedRoot
	digest   [32]byte
}

// Compile checks cfg and builds the verifiers. Every refusal wraps ErrPolicy.
func Compile(cfg Config, o Options) (*Policy, error) {
	ownReader := o.ReadFile == nil
	if ownReader {
		// Confined like the site setting's check at write, so a root path stored before that check, or
		// outside the directory, refuses the compile and is never read.
		dir := cmp.Or(o.RootDir, rootfile.Dir)
		o.ReadFile = func(path string) ([]byte, error) { return rootfile.Read(dir, path) }
	}
	if o.Version == "" {
		o.Version = version.String()
	}
	pg := o.PublicGoodRoot
	if pg == nil {
		tm, err := releasesig.TrustedRoot()
		if err != nil {
			return nil, fmt.Errorf("%w: embedded trust root: %v", ErrPolicy, err)
		}
		var ok bool
		if pg, ok = tm.(*root.TrustedRoot); !ok {
			return nil, fmt.Errorf("%w: embedded trust root is %T", ErrPolicy, tm)
		}
	}
	pgJSON, err := pg.MarshalJSON()
	if err != nil {
		return nil, fmt.Errorf("%w: trust root %s: %v", ErrPolicy, PublicGood, err)
	}
	roots := map[string]trustRoot{PublicGood: {material: pg, digest: sha256.Sum256(pgJSON)}}
	for _, r := range cfg.TrustedRoots {
		if r.Name == "" || r.Name == PublicGood || r.Name == BuiltinSigner {
			return nil, fmt.Errorf("%w: trust root name %q is empty or reserved", ErrPolicy, r.Name)
		}
		if _, dup := roots[r.Name]; dup {
			return nil, fmt.Errorf("%w: trust root %q is defined twice", ErrPolicy, r.Name)
		}
		if !filepath.IsAbs(r.Path) {
			return nil, fmt.Errorf("%w: trust root %q: path %q is not absolute", ErrPolicy, r.Name, r.Path)
		}
		b, err := o.ReadFile(r.Path)
		if err != nil {
			if ownReader { // rootfile's texts name the directory and nothing of the file
				return nil, fmt.Errorf("%w: trust root %q could not be read: %v", ErrPolicy, r.Name, err)
			}
			return nil, fmt.Errorf("%w: trust root %q could not be read", ErrPolicy, r.Name) // a caller's reader may say more than it should
		}
		tr, err := root.NewTrustedRootFromJSON(b)
		if err != nil {
			return nil, fmt.Errorf("%w: trust root %q is not a Sigstore trusted root", ErrPolicy, r.Name)
		}
		roots[r.Name] = trustRoot{material: tr, digest: sha256.Sum256(b)}
	}
	p := &Policy{byName: map[string]*Signer{}}
	specs := append(append([]SignerSpec{}, cfg.Signers...), builtin(o.Version))
	for i, spec := range specs {
		if i < len(cfg.Signers) && (spec.Name == BuiltinSigner || spec.Name == PublicGood) {
			return nil, fmt.Errorf("%w: signer name %q is reserved", ErrPolicy, spec.Name)
		}
		if _, dup := p.byName[spec.Name]; dup {
			return nil, fmt.Errorf("%w: signer %q is defined twice", ErrPolicy, spec.Name)
		}
		s, err := compileSigner(spec, roots, i == len(cfg.Signers))
		if err != nil {
			return nil, fmt.Errorf("%w: signer %q: %v", ErrPolicy, spec.Name, err)
		}
		p.signers = append(p.signers, s)
		p.byName[s.name] = s
	}
	return p, nil
}

func compileSigner(spec SignerSpec, roots map[string]trustRoot, isBuiltin bool) (*Signer, error) {
	if spec.Name == "" {
		return nil, errors.New("a signer needs a name")
	}
	if len(spec.Images) == 0 && !isBuiltin {
		return nil, errors.New("images is empty")
	}
	for _, e := range spec.Images {
		if !imagesEntryValid(e) {
			return nil, fmt.Errorf("images entry %q is neither an exact reference nor a \"<registry host>/<path>/*\" prefix", e)
		}
	}
	s := &Signer{name: spec.Name, images: spec.Images, sigPredicates: map[string]bool{}}
	if !isBuiltin {
		for _, pt := range defaultSignaturePredicates {
			s.sigPredicates[pt] = true
		}
	}
	for _, pt := range spec.SignaturePredicates {
		if !predicateTypeValid(pt) {
			return nil, fmt.Errorf("signature predicate %q is not an absolute URI", pt)
		}
		s.sigPredicates[pt] = true
	}
	for _, pt := range spec.RequireAttestations {
		if !predicateTypeValid(pt) {
			return nil, fmt.Errorf("required attestation %q is not an absolute URI", pt)
		}
	}
	s.attestations = spec.RequireAttestations
	var used trustRoot
	var err error
	switch {
	case spec.Keyless != nil && spec.PublicKey != "":
		return nil, errors.New("a signer is keyless or has a public key, not both")
	case spec.Keyless != nil:
		used, err = s.compileKeyless(*spec.Keyless, roots)
	case spec.PublicKey != "":
		used, err = s.compileKey(spec, roots[PublicGood])
	default:
		return nil, errors.New("a signer needs keyless or public_key")
	}
	if err != nil {
		return nil, err
	}
	canon, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	s.fingerprint = sha256.Sum256(append(canon, used.digest[:]...))
	return s, nil
}

// compileKeyless derives the verifier from the named root, not from knobs: an SCT when the root has CT logs
// and a transparency-log entry when it has logs (the public-good root then gets cosign verify's policy), and
// always an observer timestamp (a log's integrated time or a signed timestamp) for the short-lived certificate.
func (s *Signer) compileKeyless(k KeylessSpec, roots map[string]trustRoot) (trustRoot, error) {
	u, err := url.Parse(k.Issuer)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return trustRoot{}, fmt.Errorf("issuer %q is not an https URL", k.Issuer)
	}
	if (k.Identity == "") == (k.IdentityRegexp == "") {
		return trustRoot{}, errors.New("set exactly one of identity and identity_regexp")
	}
	if k.IdentityRegexp != "" {
		if !strings.HasPrefix(k.IdentityRegexp, "^") || !strings.HasSuffix(k.IdentityRegexp, "$") {
			return trustRoot{}, fmt.Errorf("identity_regexp %q must be anchored with ^ and $", k.IdentityRegexp)
		}
		if _, err := regexp.Compile(k.IdentityRegexp); err != nil {
			return trustRoot{}, fmt.Errorf("identity_regexp %q: %v", k.IdentityRegexp, err)
		}
	}
	name := k.TrustedRoot
	if name == "" {
		name = PublicGood
	}
	tr, ok := roots[name]
	if !ok {
		return trustRoot{}, fmt.Errorf("trusted_root %q is not defined", name)
	}
	opts := []verify.VerifierOption{verify.WithObserverTimestamps(1), verify.WithoutStatementPredicate()}
	if len(tr.material.CTLogs()) > 0 {
		opts = append(opts, verify.WithSignedCertificateTimestamps(1))
	}
	if len(tr.material.RekorLogs()) > 0 {
		opts = append(opts, verify.WithTransparencyLog(1))
	}
	if s.verifier, err = verify.NewVerifier(tr.material, opts...); err != nil {
		return trustRoot{}, err
	}
	// The matcher finds the pattern anywhere in the identity, and alternation binds looser than ^ and $, so
	// group the authored text and pin it to the whole identity.
	re := k.IdentityRegexp
	if re != "" {
		re = `\A(?:` + re + `)\z`
	}
	san, err := verify.NewSANMatcher(k.Identity, re)
	if err != nil {
		return trustRoot{}, err
	}
	issuer, err := verify.NewIssuerMatcher(k.Issuer, "")
	if err != nil {
		return trustRoot{}, err
	}
	id, err := verify.NewCertificateIdentity(san, issuer, certificate.Extensions{SourceRepositoryURI: k.SourceRepository})
	if err != nil {
		return trustRoot{}, err
	}
	s.identity, s.want, s.wantIssuer = &id, k.Identity+k.IdentityRegexp, k.Issuer
	return tr, nil
}

// compileKey trusts exactly one public key, whatever hint a bundle carries. With the transparency log on
// (the default) the entry is checked against the public-good Rekor and its time bounds the signature.
func (s *Signer) compileKey(spec SignerSpec, pg trustRoot) (trustRoot, error) {
	block, _ := pem.Decode([]byte(spec.PublicKey))
	if block == nil {
		return trustRoot{}, errors.New("public_key is not PEM")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return trustRoot{}, fmt.Errorf("public_key: %v", err)
	}
	hash, err := keyHash(pub)
	if err != nil {
		return trustRoot{}, err
	}
	sv, err := signature.LoadVerifier(pub, hash)
	if err != nil {
		return trustRoot{}, err
	}
	key := root.NewExpiringKey(sv, time.Time{}, time.Time{})
	keys := root.NewTrustedPublicKeyMaterial(func(string) (root.TimeConstrainedVerifier, error) { return key, nil })
	sum := sha256.Sum256(block.Bytes)
	s.want = base64.StdEncoding.EncodeToString(sum[:])
	s.keyHints = map[string]bool{s.want: true, fmt.Sprintf("%x", sum[:]): true}
	var tm root.TrustedMaterial = keys
	opts := []verify.VerifierOption{verify.WithCurrentTime(), verify.WithoutStatementPredicate()}
	used := trustRoot{}
	if spec.TransparencyLog == nil || *spec.TransparencyLog {
		tm = root.TrustedMaterialCollection{keys, pg.material}
		opts = []verify.VerifierOption{verify.WithTransparencyLog(1), verify.WithIntegratedTimestamps(1), verify.WithoutStatementPredicate()}
		used = pg
	}
	if s.verifier, err = verify.NewVerifier(tm, opts...); err != nil {
		return trustRoot{}, err
	}
	return used, nil
}

// keyHash is the digest a key's signatures use; it also refuses weak or unsupported keys.
func keyHash(pub crypto.PublicKey) (crypto.Hash, error) {
	switch k := pub.(type) {
	case *ecdsa.PublicKey:
		switch k.Curve {
		case elliptic.P256():
			return crypto.SHA256, nil
		case elliptic.P384():
			return crypto.SHA384, nil
		}
		return 0, fmt.Errorf("public_key curve %s is not P-256 or P-384", k.Curve.Params().Name)
	case ed25519.PublicKey:
		return crypto.Hash(0), nil
	case *rsa.PublicKey:
		if k.N.BitLen() < 2048 {
			return 0, fmt.Errorf("public_key is RSA-%d; at least 2048 bits are required", k.N.BitLen())
		}
		return crypto.SHA256, nil
	}
	return 0, fmt.Errorf("public_key type %T is not supported", pub)
}

func predicateTypeValid(pt string) bool {
	u, err := url.Parse(pt)
	return err == nil && u.Scheme != "" && (u.Host != "" || u.Opaque != "") && !strings.ContainsAny(pt, " \t\r\n")
}

// Requirements are the signers one image must satisfy: one of AnyOf (the site's signers that cover it), and
// every one of AllOf (the member's governance profile's signers).
type Requirements struct {
	Ref   string
	AnyOf []*Signer
	AllOf []*Signer
}

// Require picks the signers for ref. site says whether the site's own signers apply (the site mode is audit or
// enforce); profile is the signer names the member's governance profile requires, which apply whatever the
// site mode, so a profile only ever adds requirements. The refusal is no_policy, decided before any registry
// read, when a profile names an undefined signer or one that does not cover ref, or when nothing applies.
// A caller in audit mode with profile signers asks twice: the profile's requirement is enforced and the
// site's is only reported.
func (p *Policy) Require(ref string, site bool, profile []string) (Requirements, *Verdict) {
	r := Requirements{Ref: ref}
	if site {
		for _, s := range p.signers {
			if s.covers(ref) {
				r.AnyOf = append(r.AnyOf, s)
			}
		}
	}
	for _, name := range profile {
		s, ok := p.byName[name]
		if !ok {
			return Requirements{}, &Verdict{Reason: NoPolicy, Signer: name, Undefined: true}
		}
		if !s.covers(ref) {
			return Requirements{}, &Verdict{Reason: NoPolicy, Signer: name}
		}
		r.AllOf = append(r.AllOf, s)
	}
	if len(r.AnyOf) == 0 && len(r.AllOf) == 0 {
		return Requirements{}, &Verdict{Reason: NoPolicy}
	}
	return r, nil
}

// Fingerprint identifies the requirements, for a verdict cache: any change to a signer, a trust root it uses
// or the profile's set gives a new value. The image digest is the caller's to add.
func (r Requirements) Fingerprint() [32]byte {
	h := sha256.New()
	for _, part := range []struct {
		tag     string
		signers []*Signer
	}{{"any", r.AnyOf}, {"all", r.AllOf}} {
		h.Write([]byte(part.tag))
		for _, s := range part.signers {
			h.Write(s.fingerprint[:])
		}
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}
