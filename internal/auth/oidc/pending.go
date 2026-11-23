// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc

// A pending authenticator: one that was built from its configuration but whose identity provider has not
// answered since the process started. It is non-nil, so every caller that reads "an authenticator exists" as
// "SSO is configured" keeps its protections. Only the operations that need the provider act differently
// while it is pending: sign-in is refused with a 503, back-channel logouts are held in a bounded queue
// (logout_queue.go), and the subject and actor token checks refuse.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// ErrProviderUnreachable is what Discover wraps when the provider gave no answer at all (dial, DNS, TLS, a
// deadline) or answered 5xx or 429. Any other failure is a configuration problem and does not wrap it.
var ErrProviderUnreachable = errors.New("oidc: identity provider unreachable")

// providerState is everything that comes from the provider's discovery document. It is published once.
type providerState struct {
	endpoint oauth2.Endpoint
	// verifier checks ID tokens for this client; subjectVerifier accepts any audience.
	verifier, subjectVerifier *gooidc.IDTokenVerifier
	issuer, jwksURL           string
	signingAlgs               []string
	// httpClient is the token-exchange client: any injected transport, bounded by idpHTTPTimeout.
	httpClient *http.Client
	// groupsScopeUnrequested records whether discovery advertised a `groups` scope the authorization
	// request does not ask for. False for every Entra tenant.
	groupsScopeUnrequested bool
}

// emptyProviderState is what a zero-value Authenticator reads: nil verifiers and an empty endpoint, which is
// exactly what its fields were before the provider state moved out of the struct.
var emptyProviderState = &providerState{}

// state returns the published provider state. closed is true for an authenticator built by NewPending whose
// provider has not been reached: provider-dependent operations refuse then. The zero value is never closed.
func (a *Authenticator) state() (st *providerState, closed bool) {
	if p := a.prov.Load(); p != nil {
		return p, false
	}
	return emptyProviderState, a.built
}

// Pending reports whether Discover has failed with ErrProviderUnreachable and has not published a state
// since. A zero-value Authenticator, one whose Discover has not run, and one whose Discover failed with a
// configuration error are not pending.
func (a *Authenticator) Pending() bool { return a.pending.Load() }

// oauthConfig is the OAuth2 client config with the provider's endpoints.
func (a *Authenticator) oauthConfig(p *providerState) oauth2.Config {
	oa := a.oauth2
	oa.Endpoint = p.endpoint
	return oa
}

// NewPending validates cfg and builds an Authenticator that has not reached its provider. Discover reaches it.
// hmacKey signs session cookies and is never logged.
func NewPending(cfg Config, hmacKey []byte) (*Authenticator, error) {
	if cfg.IssuerURL == "" || cfg.ClientID == "" || cfg.RedirectURL == "" {
		return nil, errors.New("oidc: IssuerURL, ClientID, and RedirectURL are required (ClientSecret is optional for a public client)")
	}
	if len(hmacKey) < 32 {
		return nil, errors.New("oidc: hmacKey must be at least 32 bytes")
	}
	return &Authenticator{
		cfg: cfg,
		oauth2: oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			RedirectURL:  cfg.RedirectURL,
			Scopes:       append([]string{gooidc.ScopeOpenID, "profile", "email"}, cfg.ExtraScopes...),
		},
		hmacKey: hmacKey,
		entra:   isEntraIssuer(cfg.IssuerURL),
		built:   true,
	}, nil
}

// New constructs an Authenticator by performing OIDC discovery against cfg.IssuerURL. hmacKey signs session
// cookies and is never logged. A provider that does not answer is an error wrapping ErrProviderUnreachable.
func New(ctx context.Context, cfg Config, hmacKey []byte) (*Authenticator, error) {
	a, err := NewPending(cfg, hmacKey)
	if err != nil {
		return nil, err
	}
	if err := a.Discover(ctx); err != nil {
		return nil, err
	}
	return a, nil
}

// Discover performs OIDC discovery, then applies the logouts held while the provider was away and publishes
// the provider state. It returns nil at once when the state is already published. On a failure that wraps
// ErrProviderUnreachable the authenticator is marked pending. The state is published only after every held
// logout is applied or recorded as unverifiable, so a failed cut or an unreachable key set keeps it pending.
// The injected HTTP client is read from ctx, as New always did.
func (a *Authenticator) Discover(ctx context.Context) error {
	a.discoverMu.Lock()
	defer a.discoverMu.Unlock()
	if a.prov.Load() != nil {
		return nil
	}
	if !a.built {
		return errors.New("oidc: Discover needs an Authenticator built by NewPending")
	}
	st, err := a.discoverProvider(ctx)
	if err != nil {
		if errors.Is(err, ErrProviderUnreachable) {
			a.pending.Store(true)
		}
		return err
	}
	return a.drainAndPublish(ctx, st)
}

// discoverProvider is the provider half of construction: the discovery request, the scope check and the
// verifiers.
func (a *Authenticator) discoverProvider(ctx context.Context) (*providerState, error) {
	cfg := a.cfg
	// Tests inject a custom transport via ctx; production ctx carries nil => http.DefaultClient.
	var httpClient *http.Client
	if c, ok := ctx.Value(oauth2.HTTPClient).(*http.Client); ok {
		httpClient = c
	}

	// Split-horizon issuer: discover against the internal URL but expect the public issuer.
	discoverURL := cfg.IssuerURL
	if cfg.InternalIssuerURL != "" && cfg.InternalIssuerURL != cfg.IssuerURL {
		rt, err := newRewriteTransport(cfg.IssuerURL, cfg.InternalIssuerURL, httpClient)
		if err != nil {
			return nil, err
		}
		httpClient = &http.Client{Transport: rt}
		discoverURL = cfg.InternalIssuerURL
		// Tell go-oidc the public URL is the expected issuer even though we fetched from internal.
		ctx = gooidc.InsecureIssuerURLContext(ctx, cfg.IssuerURL)
	}

	// The discovery request goes through a per-attempt copy of the client wrapped in a recorder: go-oidc
	// reports a non-200 untyped, so the recorder is what tells "did not answer" from "answered no".
	rec := &statusRecorder{}
	discoveryClient := &http.Client{}
	if httpClient != nil {
		*discoveryClient = *httpClient
	}
	rec.inner = discoveryClient.Transport
	discoveryClient.Transport = rec
	provider, err := gooidc.NewProvider(gooidc.ClientContext(ctx, discoveryClient), discoverURL)
	if err != nil {
		if rec.unreachable(ctx) {
			return nil, fmt.Errorf("%w: oidc: provider discovery for %q: %v", ErrProviderUnreachable, discoverURL, err)
		}
		return nil, fmt.Errorf("oidc: provider discovery for %q: %w", discoverURL, err)
	}
	// Refuse on an unadvertised extra scope BEFORE it reaches the scopes.
	if err := validateExtraScopes(provider, cfg.ExtraScopes); err != nil {
		return nil, err
	}

	endpoint := provider.Endpoint()
	if cfg.ClientSecret == "" {
		// A public client's token request must never include a client_secret param: AutoDetect
		// tries HTTP Basic first ("clientID:" with an empty password, still client_secret-shaped),
		// so force AuthStyleInParams, the one style x/oauth2 skips the field for entirely when empty.
		endpoint.AuthStyle = oauth2.AuthStyleInParams
	}
	groupsScopeUnrequested := providerGatesGroupsScope(provider, a.oauth2.Scopes)
	warnUnrequestedGroupsScope(groupsScopeUnrequested, a.oauth2.Scopes, cfg)
	var doc struct {
		JWKSURI     string   `json:"jwks_uri"`
		SigningAlgs []string `json:"id_token_signing_alg_values_supported"`
	}
	if err := provider.Claims(&doc); err != nil {
		return nil, fmt.Errorf("oidc: provider discovery for %q: %w", discoverURL, err)
	}

	// The ID-token key set uses a JWKS-TOLERANT client so one unparseable entry skips instead of
	// failing the whole document and locking every human out. VerifierContext (not Verifier) is only
	// so the key set gets its own HTTP client, using background context since it outlives this call.
	// The token exchange and the key set share one client bounded by idpHTTPTimeout (discovery above
	// is bounded by the caller's ctx). A copy, so the caller's transport and settings survive. It is built
	// from the unwrapped client, so no recorder outlives the attempt.
	idpClient := &http.Client{}
	if httpClient != nil {
		*idpClient = *httpClient
	}
	if idpClient.Timeout == 0 {
		idpClient.Timeout = idpHTTPTimeout
	}
	keySetCtx := gooidc.ClientContext(context.Background(), newTolerantJWKSClient(idpClient))
	return &providerState{
		endpoint:        endpoint,
		verifier:        provider.VerifierContext(keySetCtx, &gooidc.Config{ClientID: cfg.ClientID}),
		subjectVerifier: provider.VerifierContext(keySetCtx, &gooidc.Config{SkipClientIDCheck: true}),
		// The configured issuer, which go-oidc's own verifiers expect too: under a split-horizon issuer the
		// internal document may name another value, and the held-logout drain must accept the public one.
		issuer:                 cfg.IssuerURL,
		jwksURL:                doc.JWKSURI,
		signingAlgs:            doc.SigningAlgs,
		httpClient:             idpClient,
		groupsScopeUnrequested: groupsScopeUnrequested,
	}, nil
}

// statusRecorder wraps a discovery client's transport and remembers how the last request ended.
type statusRecorder struct {
	inner http.RoundTripper
	mu    sync.Mutex
	calls int
	last  int // the last response's status; 0 when it got none
}

func (s *statusRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	inner := s.inner
	if inner == nil {
		inner = http.DefaultTransport
	}
	resp, err := inner.RoundTrip(req)
	s.mu.Lock()
	s.calls++
	s.last = 0
	if err == nil && resp != nil {
		s.last = resp.StatusCode
	}
	s.mu.Unlock()
	return resp, err
}

// unreachable is true when the provider gave no answer to the last request (or the context ended before one
// went out), or answered 5xx or 429. A 4xx and a malformed 200 are configuration, not an outage.
func (s *statusRecorder) unreachable(ctx context.Context) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.calls == 0 {
		return ctx.Err() != nil
	}
	return s.last == 0 || s.last >= http.StatusInternalServerError || s.last == http.StatusTooManyRequests
}

// pendingSignIn503 refuses a sign-in step while the provider has not been reached. It sets no cookie.
func pendingSignIn503(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Retry-After", "30")
	http.Error(w, "sign-in is unavailable: this server has not reached the identity provider since it started; try again shortly", http.StatusServiceUnavailable)
}
