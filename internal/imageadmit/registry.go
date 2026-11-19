// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package imageadmit checks container image signatures before a run starts. This file is the registry
// client: it reads manifests, referrers, legacy cosign tags and blobs from an OCI distribution registry.
package imageadmit

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/distribution/reference"
)

// Bounds on what one check will read from a registry.
const (
	MaxManifestBytes = 4 << 20 // a manifest or index
	MaxBlobBytes     = 1 << 20 // one signature or bundle blob
	MaxReferrers     = 64      // referrer descriptors returned
	MaxRedirects     = 3
	maxTokenBytes    = 64 << 10
	defaultTimeout   = 20 * time.Second
)

const (
	ociIndex       = "application/vnd.oci.image.index.v1+json"
	ociManifest    = "application/vnd.oci.image.manifest.v1+json"
	dockerList     = "application/vnd.docker.distribution.manifest.list.v2+json"
	dockerManifest = "application/vnd.docker.distribution.manifest.v2+json"
)

var (
	// ErrNotFound is a 404 for what was asked. Whether that means "unsigned" is the caller's call.
	ErrNotFound = errors.New("not found in the registry")
	// ErrDigestMismatch means bytes did not hash to the digest they were asked for or announced.
	ErrDigestMismatch = errors.New("content does not match its digest")
	// ErrMalformed means the registry answered a referrers index that does not parse: its content, not an outage.
	ErrMalformed = errors.New("not a valid referrers index")
	// ErrTooLarge means a response exceeded its bound.
	ErrTooLarge = errors.New("response larger than its bound")

	sha256Re = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// Cause says why a registry could not be read, for the admission "unavailable" sentence.
type Cause string

const (
	CauseUnauthorized Cause = "unauthorized" // 401
	CauseForbidden    Cause = "forbidden"    // 403
	CauseUnreachable  Cause = "unreachable"  // connect or DNS failure
	CauseTLS          Cause = "tls"          // certificate not trusted
	CauseTimeout      Cause = "timeout"
	CauseServer       Cause = "server"    // 5xx, 429 or an unexpected status
	CauseRefused      Cause = "refused"   // a target the client will not contact
	CauseAuthFile     Cause = "auth_file" // the registry auth file could not be read when a credential was asked for
)

// Unavailable is a registry read that failed for a reason outside the image's content.
// Host is the host that answered or was refused (it can be a token realm or a redirect's destination, which the
// registry chose); Registry is the one the image ref names. Only Registry is ever printed. It never carries a
// credential.
type Unavailable struct {
	Cause         Cause
	Host          string
	Registry      string
	HasCredential bool // a credential for Registry was configured (meaningful for CauseUnauthorized)
	Withheld      bool // the 401 came from another origin a redirect led to, where Registry's credential is never sent
	Err           error
}

func (u *Unavailable) Error() string {
	switch u.Cause {
	case CauseUnauthorized:
		if u.Withheld {
			return "checking images from " + u.Registry + " got 401 from an address it redirected to, where its credential is not sent"
		}
		if !u.HasCredential {
			return "checking images from " + u.Registry + " got 401 and no credential is configured for it"
		}
		return "checking images from " + u.Registry + " got 401 for the configured credential"
	case CauseForbidden:
		return "checking images from " + u.Registry + " got 403"
	case CauseTLS:
		return fmt.Sprintf("a TLS certificate met while checking images from %s is not trusted", u.Registry)
	case CauseTimeout:
		return fmt.Sprintf("checking images from %s got no answer in time", u.Registry)
	case CauseRefused:
		if reason := u.ownReason(); reason != "" {
			return fmt.Sprintf("%s was not contacted: %s", u.Registry, reason)
		}
		return u.Registry + " was not contacted: it or an address it named is not one Wardyn contacts"
	case CauseAuthFile:
		return fmt.Sprintf("the credential for %s could not be looked up: %v", u.Registry, u.Err)
	}
	if reason := u.ownReason(); reason != "" {
		return fmt.Sprintf("%s could not be read: %s", u.Registry, reason)
	}
	return u.Registry + " could not be read"
}

// ownReason is Err's text when this package wrote all of it (a status code, a token realm refused before any
// request, the auth file's error); "" for a transport error, whose text holds the URL and addresses a redirect
// or realm named.
func (u *Unavailable) ownReason() string {
	var al authLookupError
	if errors.As(u.Err, &al) {
		return al.Error()
	}
	var ue *url.Error
	if u.Err == nil || u.Cause == CauseUnreachable || errors.As(u.Err, &ue) {
		return ""
	}
	return u.Err.Error()
}

// authLookupError is the auth file failing for a redirect destination. Its text is the auth file's error, never
// the destination, which the registry chose.
type authLookupError struct{ err error }

func (e authLookupError) Error() string {
	return "reading the credential for a redirect destination: " + e.err.Error()
}
func (e authLookupError) Unwrap() error { return e.err }

func (u *Unavailable) Unwrap() error { return u.Err }

// Ref names a repository on a registry, optionally with a tag or digest.
type Ref struct {
	Registry string // host or host:port, as dialled
	Repo     string
	Tag      string
	Digest   string // sha256:<hex>
}

// ParseRef parses a Docker-style reference. docker.io maps to registry-1.docker.io.
func ParseRef(s string) (Ref, error) {
	n, err := reference.ParseNormalizedNamed(s)
	if err != nil {
		return Ref{}, err
	}
	r := Ref{Registry: strings.ToLower(reference.Domain(n)), Repo: reference.Path(n)}
	if r.Registry == "docker.io" {
		r.Registry = "registry-1.docker.io"
	}
	if t, ok := n.(reference.Tagged); ok {
		r.Tag = t.Tag()
	}
	if d, ok := n.(reference.Digested); ok {
		r.Digest = d.Digest().String()
		if !sha256Re.MatchString(r.Digest) {
			return Ref{}, fmt.Errorf("only sha256 digests are supported, got %q", r.Digest)
		}
	}
	return r, nil
}

// Descriptor is an OCI content descriptor.
type Descriptor struct {
	MediaType    string            `json:"mediaType"`
	Digest       string            `json:"digest"`
	Size         int64             `json:"size"`
	ArtifactType string            `json:"artifactType,omitempty"`
	Annotations  map[string]string `json:"annotations,omitempty"`
}

// Manifest is a parsed image manifest or index. Digest is computed from Raw, never taken from a header.
type Manifest struct {
	Digest       string
	MediaType    string
	ArtifactType string
	Raw          []byte
	Config       Descriptor
	Layers       []Descriptor
	Manifests    []Descriptor
	Annotations  map[string]string
}

// RegistryOptions configures a RegistryClient.
type RegistryOptions struct {
	// Base is the transport to copy; it should be http.DefaultTransport after the daemon's trusted CA pool and
	// forward proxy were installed on it. Nil uses http.DefaultTransport.
	Base *http.Transport
	// Auth supplies credentials; nil means anonymous.
	Auth CredentialSource
	// Timeout bounds one HTTP exchange; zero means 20 s. The caller's context bounds the whole check.
	Timeout time.Duration
}

// RegistryClient reads an OCI distribution registry. https only; it follows at most three redirects and never
// carries Authorization across an origin change.
type RegistryClient struct {
	hc   *http.Client
	auth CredentialSource

	mu     sync.Mutex
	tokens map[string]cachedToken
}

type cachedToken struct {
	header  string
	expires time.Time
}

// NewRegistryClient returns a client whose dials are guarded (see guardedTransport).
func NewRegistryClient(o RegistryOptions) *RegistryClient {
	return newRegistryClient(o, defaultGuard())
}

func newRegistryClient(o RegistryOptions, g *targetGuard) *RegistryClient {
	base := o.Base
	if base == nil {
		base, _ = http.DefaultTransport.(*http.Transport)
	}
	if base == nil {
		base = &http.Transport{}
	}
	if o.Timeout == 0 {
		o.Timeout = defaultTimeout
	}
	c := &RegistryClient{auth: o.Auth, tokens: map[string]cachedToken{}}
	c.hc = &http.Client{Transport: guardedTransport(base, g), Timeout: o.Timeout, CheckRedirect: c.checkRedirect}
	return c
}

// checkRedirect refuses a non-https destination and a fourth hop. On ANY origin change (scheme, host
// including subdomains, port) Authorization is dropped and only a credential configured for the new
// destination is added, so a blob redirect to a CDN never carries the registry's token.
func (c *RegistryClient) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > MaxRedirects {
		return fmt.Errorf("stopped after %d redirects", MaxRedirects)
	}
	if req.URL.Scheme != "https" {
		return fmt.Errorf("%w: a redirect to an address that is not https", ErrRefusedTarget)
	}
	if req.Context().Value(tokenRequestKey{}) != nil && originOf(req.URL) != originOf(via[len(via)-1].URL) {
		// A token request carries the registry's credential (a Basic header or a refresh token in the body,
		// which a 307 or 308 replays). It is never followed to another origin.
		return fmt.Errorf("%w: the token realm redirected to another origin", ErrRefusedTarget)
	}
	// Go copies the ORIGINAL request's headers onto every hop and keeps Authorization for subdomains and across
	// ports, so the copied header is never trusted. Each hop's Authorization is rebuilt from what is approved
	// for that exact origin: the header this client set for it (a Bearer from its own realm, a Basic it chose),
	// else the credential configured for it.
	req.Header.Del("Authorization")
	dest := originOf(req.URL)
	if g, ok := req.Context().Value(authzGrantKey{}).(authzGrant); ok && g.origin == dest {
		req.Header.Set("Authorization", g.header)
		return nil
	}
	if c.auth != nil {
		a, ok, err := c.auth.Lookup(normalizeHost(req.URL.Host))
		if err != nil {
			// Not an anonymous request: the destination may expect the credential that could not be read.
			return authLookupError{err}
		}
		if ok && a.Username != "" {
			req.SetBasicAuth(a.Username, a.Password)
		}
	}
	return nil
}

// authzGrant is an Authorization value this client set for one origin; redirects inherit it in the request context
// and reuse it only for that same origin.
type authzGrant struct{ origin, header string }

type authzGrantKey struct{}

// tokenRequestKey marks a request to a token realm in its context, which redirects inherit.
type tokenRequestKey struct{}

func originOf(u *url.URL) string {
	p := u.Port()
	if p == "" {
		p = map[string]string{"http": "80", "https": "443"}[u.Scheme]
	}
	return u.Scheme + "://" + strings.ToLower(u.Hostname()) + ":" + p
}

// Manifest fetches the manifest or index a ref names, by digest when it has one, else by tag (default latest),
// and computes its digest from the bytes. A Docker-Content-Digest header that disagrees, or a digest ref the
// bytes do not hash to, is refused.
func (c *RegistryClient) Manifest(ctx context.Context, ref Ref) (*Manifest, error) {
	id := ref.Digest
	if id == "" {
		if id = ref.Tag; id == "" {
			id = "latest"
		}
	}
	accept := strings.Join([]string{ociIndex, ociManifest, dockerList, dockerManifest}, ", ")
	body, hdr, err := c.fetch(ctx, ref, "manifests/"+id, nil, accept, MaxManifestBytes)
	if err != nil {
		return nil, err
	}
	return parseManifest(body, hdr, ref.Digest)
}

func parseManifest(body []byte, hdr http.Header, wantDigest string) (*Manifest, error) {
	got := sum256(body)
	if wantDigest != "" && got != wantDigest {
		// The digest asked for is not repeated: for a referrer it is a descriptor the registry served.
		return nil, fmt.Errorf("%w: the bytes are %s, not the digest asked for", ErrDigestMismatch, got)
	}
	if h := hdr.Get("Docker-Content-Digest"); strings.HasPrefix(h, "sha256:") && h != got {
		// The announced value is not repeated: a registry that was sent a Basic credential could echo it here.
		return nil, fmt.Errorf("%w: the registry announced another digest, the bytes are %s", ErrDigestMismatch, got)
	}
	var j struct {
		MediaType    string            `json:"mediaType"`
		ArtifactType string            `json:"artifactType"`
		Config       Descriptor        `json:"config"`
		Layers       []Descriptor      `json:"layers"`
		Manifests    []Descriptor      `json:"manifests"`
		Annotations  map[string]string `json:"annotations"`
	}
	if err := json.Unmarshal(body, &j); err != nil {
		return nil, fmt.Errorf("manifest %s is not JSON", got)
	}
	mt := j.MediaType
	if ct, _, _ := strings.Cut(hdr.Get("Content-Type"), ";"); mt == "" {
		mt = strings.TrimSpace(ct)
	}
	return &Manifest{Digest: got, MediaType: mt, ArtifactType: j.ArtifactType, Raw: body, Config: j.Config,
		Layers: j.Layers, Manifests: j.Manifests, Annotations: j.Annotations}, nil
}

// Referrers lists the manifests that name subject, through the referrers API and, when the registry answers
// 404, the tag schema (sha256-<hex>). A non-empty artifactType filters, client-side as well, because the
// fallback and some registries ignore the query. At most MaxReferrers descriptors are returned.
func (c *RegistryClient) Referrers(ctx context.Context, ref Ref, subject, artifactType string) ([]Descriptor, error) {
	if !sha256Re.MatchString(subject) {
		return nil, fmt.Errorf("subject %q is not a sha256 digest", subject)
	}
	var q url.Values
	if artifactType != "" {
		q = url.Values{"artifactType": {artifactType}}
	}
	body, hdr, err := c.fetch(ctx, ref, "referrers/"+subject, q, ociIndex, MaxManifestBytes)
	if errors.Is(err, ErrNotFound) {
		body, hdr, err = c.fetch(ctx, ref, "manifests/"+tagOf(subject), nil, ociIndex, MaxManifestBytes)
		if errors.Is(err, ErrNotFound) {
			return nil, nil
		}
	}
	if err != nil {
		return nil, err
	}
	m, err := parseManifest(body, hdr, "")
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMalformed, err)
	}
	out := make([]Descriptor, 0, min(len(m.Manifests), MaxReferrers))
	for _, d := range m.Manifests {
		if (artifactType == "" || d.ArtifactType == artifactType) && len(out) < MaxReferrers {
			out = append(out, d)
		}
	}
	return out, nil
}

// LegacyTag fetches the cosign tag manifest sha256-<hex><suffix> for subject; suffix is ".sig" or ".att".
// The layers carry the payloads and the signature, certificate and bundle annotations.
func (c *RegistryClient) LegacyTag(ctx context.Context, ref Ref, subject, suffix string) (*Manifest, error) {
	if !sha256Re.MatchString(subject) {
		return nil, fmt.Errorf("subject %q is not a sha256 digest", subject)
	}
	if suffix != ".sig" && suffix != ".att" {
		return nil, fmt.Errorf("legacy tag suffix %q is not .sig or .att", suffix)
	}
	body, hdr, err := c.fetch(ctx, ref, "manifests/"+tagOf(subject)+suffix, nil, ociManifest+", "+dockerManifest, MaxManifestBytes)
	if err != nil {
		return nil, err
	}
	return parseManifest(body, hdr, "")
}

// Blob fetches the blob d describes (at most MaxBlobBytes) and verifies size and digest before returning it.
func (c *RegistryClient) Blob(ctx context.Context, ref Ref, d Descriptor) ([]byte, error) {
	if !sha256Re.MatchString(d.Digest) {
		return nil, errors.New("a blob descriptor's digest is not sha256")
	}
	if d.Size < 0 || d.Size > MaxBlobBytes {
		return nil, fmt.Errorf("%w: blob %s is %d bytes, the bound is %d", ErrTooLarge, d.Digest, d.Size, MaxBlobBytes)
	}
	body, _, err := c.fetch(ctx, ref, "blobs/"+d.Digest, nil, "", d.Size)
	if err != nil {
		return nil, err
	}
	if int64(len(body)) != d.Size || sum256(body) != d.Digest {
		return nil, fmt.Errorf("%w: blob %s", ErrDigestMismatch, d.Digest)
	}
	return body, nil
}

func tagOf(digest string) string { return "sha256-" + strings.TrimPrefix(digest, "sha256:") }

func sum256(b []byte) string { h := sha256.Sum256(b); return "sha256:" + hex.EncodeToString(h[:]) }

// fetch GETs https://<registry>/v2/<repo>/<path>, answering a 401 challenge once.
func (c *RegistryClient) fetch(ctx context.Context, ref Ref, path string, q url.Values, accept string, limit int64) ([]byte, http.Header, error) {
	u := &url.URL{Scheme: "https", Host: ref.Registry, Path: "/v2/" + ref.Repo + "/" + path}
	if q != nil {
		u.RawQuery = q.Encode()
	}
	scope := "repository:" + ref.Repo + ":pull"
	authz := c.cachedToken(ref.Registry, scope)
	for attempt := 0; ; attempt++ {
		rctx := ctx
		if authz != "" {
			rctx = context.WithValue(ctx, authzGrantKey{}, authzGrant{origin: originOf(u), header: authz})
		}
		req, err := http.NewRequestWithContext(rctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, nil, err
		}
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		if authz != "" {
			req.Header.Set("Authorization", authz)
		}
		resp, err := c.hc.Do(req)
		if err != nil {
			return nil, nil, classify(err, ref.Registry, ref.Registry)
		}
		if resp.StatusCode == http.StatusUnauthorized && originOf(resp.Request.URL) != originOf(u) {
			// A redirect led to another origin and it sent the challenge, on the first request or after the
			// registry accepted its credential. The registry's credential is not that origin's and was not sent
			// there: answering would hand it to a realm the registry never chose.
			resp.Body.Close()
			un := c.unavailable(CauseUnauthorized, resp.Request.URL.Host, ref.Registry, nil)
			un.Withheld = true
			return nil, nil, un
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			challenge := resp.Header.Values("Www-Authenticate")
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
			resp.Body.Close()
			if authz, err = c.authorize(ctx, ref, challenge, scope); err != nil {
				return nil, nil, err
			}
			if authz != "" {
				continue
			}
			return nil, nil, c.unavailable(CauseUnauthorized, ref.Registry, ref.Registry, nil)
		}
		return c.finish(resp, ref, limit)
	}
}

func (c *RegistryClient) finish(resp *http.Response, ref Ref, limit int64) ([]byte, http.Header, error) {
	defer resp.Body.Close()
	answered := resp.Request.URL.Host
	switch {
	case resp.StatusCode == http.StatusOK:
		body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
		if err != nil {
			return nil, nil, classify(err, answered, ref.Registry)
		}
		if int64(len(body)) > limit {
			return nil, nil, fmt.Errorf("%w: more than %d bytes from %s", ErrTooLarge, limit, ref.Registry)
		}
		return body, resp.Header, nil
	case resp.StatusCode == http.StatusNotFound:
		return nil, nil, ErrNotFound
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, nil, c.unavailable(CauseUnauthorized, answered, ref.Registry, nil)
	case resp.StatusCode == http.StatusForbidden:
		return nil, nil, c.unavailable(CauseForbidden, answered, ref.Registry, nil)
	}
	return nil, nil, c.unavailable(CauseServer, answered, ref.Registry, fmt.Errorf("status %d", resp.StatusCode))
}

func (c *RegistryClient) unavailable(cause Cause, host, registry string, err error) *Unavailable {
	has := false
	if c.auth != nil {
		a, ok, _ := c.auth.Lookup(normalizeHost(registry))
		has = ok && (a.Username != "" || a.IdentityToken != "")
	}
	return &Unavailable{Cause: cause, Host: host, Registry: registry, HasCredential: has, Err: err}
}

// classify maps a transport error to an Unavailable for registry, the one the image ref names. A caller's
// cancellation is returned as is.
func classify(err error, fallbackHost, registry string) error {
	host := fallbackHost
	var ue *url.Error
	if errors.As(err, &ue) {
		if u, perr := url.Parse(ue.URL); perr == nil && u.Host != "" {
			host = u.Host
		}
	}
	u := &Unavailable{Host: host, Registry: registry, Err: err, Cause: CauseUnreachable}
	var (
		unknownCA *x509.UnknownAuthorityError
		hostErr   x509.HostnameError
		invalid   x509.CertificateInvalidError
		verify    *tls.CertificateVerificationError
		nerr      net.Error
	)
	switch {
	case errors.Is(err, ErrRefusedTarget):
		u.Cause = CauseRefused
	case errors.As(err, &unknownCA), errors.As(err, &hostErr), errors.As(err, &invalid), errors.As(err, &verify):
		u.Cause = CauseTLS
	case errors.Is(err, context.Canceled):
		return err
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &nerr) && nerr.Timeout():
		u.Cause = CauseTimeout
	}
	return u
}

func (c *RegistryClient) cachedToken(registry, scope string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.tokens[registry+"|"+scope]
	if !ok || time.Now().After(t.expires) {
		return ""
	}
	return t.header
}

// authorize answers a 401 challenge and returns the Authorization header value, "" when there is nothing
// to send (no credential for a Basic challenge). A Bearer realm must be https; the credential configured for
// the registry goes to it only over https, and every realm is dialled through the same guard.
func (c *RegistryClient) authorize(ctx context.Context, ref Ref, challenges []string, scope string) (string, error) {
	scheme, params := pickChallenge(challenges)
	var cred RegistryAuth
	var have bool
	if c.auth != nil {
		var err error
		if cred, have, err = c.auth.Lookup(normalizeHost(ref.Registry)); err != nil {
			// A local fault, not the image's content: the check fails as unavailable and is never kept.
			return "", &Unavailable{Cause: CauseAuthFile, Host: ref.Registry, Registry: ref.Registry, Err: err}
		}
	}
	switch scheme {
	case "basic":
		if !have || cred.Username == "" {
			return "", nil
		}
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(cred.Username+":"+cred.Password)), nil
	case "bearer":
	default:
		return "", nil
	}
	realm, err := url.Parse(params["realm"])
	if err != nil || realm.Host == "" || realm.User != nil {
		return "", &Unavailable{Cause: CauseRefused, Host: ref.Registry, Registry: ref.Registry, Err: fmt.Errorf("%w: the token realm is not a usable URL", ErrRefusedTarget)}
	}
	if realm.Scheme != "https" {
		return "", &Unavailable{Cause: CauseRefused, Host: realm.Host, Registry: ref.Registry, Err: fmt.Errorf("%w: the token realm is not https", ErrRefusedTarget)}
	}
	// The token is cached under the scope fetch computed, whatever spelling the challenge asks for.
	cacheScope := scope
	if s := params["scope"]; s != "" {
		scope = s
	}
	q := realm.Query()
	if s := params["service"]; s != "" {
		q.Set("service", s)
	}
	q.Set("scope", scope)
	ctx = context.WithValue(ctx, tokenRequestKey{}, true)
	var req *http.Request
	if have && cred.IdentityToken != "" {
		form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {cred.IdentityToken}, "service": {params["service"]}, "scope": {scope}, "client_id": {"wardyn"}}
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, realm.String(), strings.NewReader(form.Encode()))
		if req != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
	} else {
		realm.RawQuery = q.Encode()
		rctx := ctx
		basic := ""
		if have && cred.Username != "" {
			basic = "Basic " + base64.StdEncoding.EncodeToString([]byte(cred.Username+":"+cred.Password))
			rctx = context.WithValue(ctx, authzGrantKey{}, authzGrant{origin: originOf(realm), header: basic})
		}
		req, err = http.NewRequestWithContext(rctx, http.MethodGet, realm.String(), nil)
		if req != nil && basic != "" {
			req.Header.Set("Authorization", basic)
		}
	}
	if err != nil {
		return "", err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", classify(err, realm.Host, ref.Registry)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return "", c.unavailable(CauseUnauthorized, realm.Host, ref.Registry, nil)
	case http.StatusForbidden:
		return "", c.unavailable(CauseForbidden, realm.Host, ref.Registry, nil)
	default:
		return "", c.unavailable(CauseServer, realm.Host, ref.Registry, fmt.Errorf("token request: status %d", resp.StatusCode))
	}
	var tok struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxTokenBytes)).Decode(&tok); err != nil {
		return "", c.unavailable(CauseServer, realm.Host, ref.Registry, errors.New("token response is not JSON"))
	}
	if tok.Token == "" {
		tok.Token = tok.AccessToken
	}
	if tok.Token == "" {
		return "", c.unavailable(CauseServer, realm.Host, ref.Registry, errors.New("token response has no token"))
	}
	header := "Bearer " + tok.Token
	ttl := time.Duration(min(max(tok.ExpiresIn, 30), 300)) * time.Second
	c.keepToken(ref.Registry+"|"+cacheScope, header, time.Now().Add(ttl))
	return header, nil
}

// keepToken caches a Bearer header until expires. One client serves the daemon's life and every repository
// name under a covered prefix is its own scope, so a write first drops the expired entries and, at
// CacheEntries, one live entry: the map never holds more than one token lifetime's distinct scopes, capped.
// ponytail: a full sweep per write; a write follows a token round trip, which costs far more than 1024 checks.
func (c *RegistryClient) keepToken(key, header string, expires time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	maps.DeleteFunc(c.tokens, func(_ string, t cachedToken) bool { return now.After(t.expires) })
	if _, ok := c.tokens[key]; !ok && len(c.tokens) >= CacheEntries {
		for k := range c.tokens {
			delete(c.tokens, k)
			break
		}
	}
	c.tokens[key] = cachedToken{header: header, expires: expires}
}

var challengeParam = regexp.MustCompile(`(\w+)="([^"]*)"`)

// pickChallenge returns the first Bearer challenge, else the first Basic, with its key="value" parameters.
func pickChallenge(headers []string) (string, map[string]string) {
	pick := ""
	var params map[string]string
	for _, h := range headers {
		scheme, rest, _ := strings.Cut(strings.TrimSpace(h), " ")
		scheme = strings.ToLower(scheme)
		if scheme != "bearer" && scheme != "basic" || (pick == "bearer") || (pick == "basic" && scheme == "basic") {
			continue
		}
		pick, params = scheme, map[string]string{}
		for _, m := range challengeParam.FindAllStringSubmatch(rest, -1) {
			params[strings.ToLower(m[1])] = m[2]
		}
	}
	return pick, params
}
