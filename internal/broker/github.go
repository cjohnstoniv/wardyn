// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
	gh "github.com/google/go-github/v88/github"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

// GitHubMinterConfig names the secrets holding the GitHub App credentials.
type GitHubMinterConfig struct {
	// AppIDSecret is the secretstore name holding the numeric App ID (ASCII).
	AppIDSecret string
	// PrivateKeySecret is the secretstore name holding the PEM private key.
	PrivateKeySecret string
}

// githubMinter is the production GitHubMinter. It reads the App credentials
// LAZILY — on the FIRST mint, not at construction — then caches the
// app-authenticated go-github client, so adding App secrets after wardynd
// started needs no restart. The App private key never leaves this process.
type githubMinter struct {
	// store is always the operator namespace: the GitHub App credential is
	// operator-provisioned, not per-member.
	store secretstore.Store
	cfg   GitHubMinterConfig

	mu           sync.Mutex
	appClient    *gh.Client       // nil until the first successful lazy init
	credHash     [32]byte         // sha256(appID||0||pem) appClient was built from
	installByOrg map[string]int64 // cache: owner -> installation id
	// baseURL, when set, overrides the go-github API base URL. Test seam only.
	baseURL string
	// httpTimeout overrides githubClientTimeout. Test seam only; zero means
	// githubClientTimeout.
	httpTimeout time.Duration
}

// githubClientTimeout bounds the WHOLE mint, not each hop: MintInstallationToken
// runs inside mint()'s transaction, which holds the grant row lock and a
// pooled Postgres connection, so a blackholed api.github.com would otherwise
// pin both for as long as the caller's own ctx allows. It is applied TWICE —
// as the http.Client Timeout (per round trip; a cold installByOrg makes up to
// two) and as the ctx MintInstallationToken derives from the caller's (per
// mint) — so a tighter caller deadline still wins (WithTimeout takes the
// earlier of the two).
const githubClientTimeout = 15 * time.Second

// timeout returns this minter's HTTP client budget: the test seam when set,
// else the production const.
func (m *githubMinter) timeout() time.Duration {
	if m.httpTimeout > 0 {
		return m.httpTimeout
	}
	return githubClientTimeout
}

// NewGitHubMinter builds a LAZY GitHubMinter: it validates the secret names
// but does NOT read the App credentials until the first mint. When the
// secrets are genuinely absent at mint time the mint fails closed.
func NewGitHubMinter(store secretstore.Store, cfg GitHubMinterConfig) (GitHubMinter, error) {
	if cfg.AppIDSecret == "" || cfg.PrivateKeySecret == "" {
		return nil, errors.New("broker: github minter requires app id and private key secret names")
	}
	return &githubMinter{
		store:        store,
		cfg:          cfg,
		installByOrg: make(map[string]int64),
	}, nil
}

// client returns the app-authenticated go-github client, reading the App
// credentials from the secret store on EVERY mint and rebuilding only when
// their content hash has changed since the cached client was built — this is
// what picks up an operator rotating github-app-id/key without a restart. A
// read failure (secrets absent/invalid) fails closed and retries next mint.
func (m *githubMinter) client(ctx context.Context) (*gh.Client, error) {
	rctx := secretstore.WithPurpose(ctx, secretstore.PurposeBrokerMint)
	idRaw, err := m.store.Get(rctx, m.cfg.AppIDSecret)
	if err != nil {
		return nil, fmt.Errorf("broker: read github app id secret: %w", err)
	}
	pem, err := m.store.Get(rctx, m.cfg.PrivateKeySecret)
	if err != nil {
		return nil, fmt.Errorf("broker: read github app private key: %w", err)
	}
	h := sha256.New()
	h.Write(idRaw)
	h.Write([]byte{0})
	h.Write(pem)
	hash := [32]byte(h.Sum(nil))

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.appClient != nil && hash == m.credHash {
		return m.appClient, nil
	}
	appID, err := strconv.ParseInt(strings.TrimSpace(string(idRaw)), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("broker: parse github app id: %w", err)
	}
	atr, err := ghinstallation.NewAppsTransport(http.DefaultTransport, appID, pem)
	if err != nil {
		return nil, fmt.Errorf("broker: build github apps transport: %w", err)
	}
	opts := []gh.ClientOptionsFunc{gh.WithHTTPClient(&http.Client{Transport: atr, Timeout: m.timeout()})}
	if m.baseURL != "" {
		opts = append(opts, gh.WithURLs(&m.baseURL, nil))
	}
	c, err := gh.NewClient(opts...)
	if err != nil {
		return nil, fmt.Errorf("broker: build github client: %w", err)
	}
	if m.appClient != nil {
		// A credential rotation invalidates the per-owner installation-id
		// cache too, rather than self-healing reactively after a 401/404.
		clear(m.installByOrg)
	}
	m.appClient = c
	m.credHash = hash
	return c, nil
}

// MintInstallationToken mints a short-lived installation token scoped to the
// given repositories with the given (already-clamped) permissions. All repos
// must belong to the same installation (owner). ttl is informational: GitHub
// fixes installation token TTL at ~1h and ignores client-supplied lifetimes,
// so we record GitHub's returned expiry as authoritative.
func (m *githubMinter) MintInstallationToken(ctx context.Context, repos []string, permissions map[string]string, ttl time.Duration) (string, time.Time, error) {
	if len(repos) == 0 {
		return "", time.Time{}, errors.New("broker: github token requires at least one repo")
	}
	owner, names, err := splitRepos(repos)
	if err != nil {
		return "", time.Time{}, err
	}
	// ONE ceiling for the WHOLE mint, not one per hop (see githubClientTimeout),
	// so VerifyRefRuleset's probe mint, run inside the same transaction, is
	// bounded by it too. A tighter caller deadline still wins.
	ctx, cancel := context.WithTimeout(ctx, m.timeout())
	defer cancel()
	client, err := m.client(ctx)
	if err != nil {
		return "", time.Time{}, err
	}
	instID, err := m.installationID(ctx, client, owner, names[0])
	if err != nil {
		return "", time.Time{}, err
	}

	perms, err := toInstallationPermissions(permissions)
	if err != nil {
		return "", time.Time{}, err
	}
	opts := &gh.InstallationTokenOptions{
		Repositories: names,
		Permissions:  perms,
	}
	tok, _, err := client.Apps.CreateInstallationToken(ctx, instID, opts)
	if err != nil {
		if isStaleInstallation(err) {
			// Dead cached installation id: drop it so the NEXT mint re-resolves.
			m.mu.Lock()
			delete(m.installByOrg, owner)
			m.mu.Unlock()
		}
		return "", time.Time{}, fmt.Errorf("broker: create installation token: %w", err)
	}
	exp := time.Now().Add(defaultMaxTTL)
	if tok.ExpiresAt != nil {
		if t := tok.ExpiresAt.GetTime(); t != nil {
			exp = *t
		}
	}
	return tok.GetToken(), exp, nil
}

// Revoke hands token back to GitHub — DELETE /installation/token, authenticated
// AS THE TOKEN ITSELF, which is why this builds its own client rather than
// reusing the app-authenticated one.
//
// WithoutCancel is load-bearing: every caller reaches this after something
// already went wrong, and on the timeout arm the caller's ctx is ALREADY
// expired — exactly when a live token would otherwise linger for GitHub's
// full ~1h. An empty token is a no-op.
func (m *githubMinter) Revoke(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	opts := []gh.ClientOptionsFunc{gh.WithAuthToken(token), gh.WithTimeout(m.timeout())}
	if m.baseURL != "" {
		opts = append(opts, gh.WithURLs(&m.baseURL, nil))
	}
	c, err := gh.NewClient(opts...)
	if err != nil {
		return fmt.Errorf("broker: build github client for revoke: %w", err)
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refRevokeTimeout)
	defer cancel()
	if _, err := c.Apps.RevokeInstallationToken(rctx); err != nil {
		return fmt.Errorf("broker: revoke installation token: %w", err)
	}
	return nil
}

// installationID resolves (and caches) the installation id for owner via the
// repository installation lookup.
func (m *githubMinter) installationID(ctx context.Context, client *gh.Client, owner, repo string) (int64, error) {
	m.mu.Lock()
	if id, ok := m.installByOrg[owner]; ok {
		m.mu.Unlock()
		return id, nil
	}
	m.mu.Unlock()

	inst, _, err := client.Apps.GetRepositoryInstallation(ctx, owner, repo)
	if err != nil {
		return 0, fmt.Errorf("broker: find installation for %s/%s: %w", owner, repo, err)
	}
	id := inst.GetID()
	if id == 0 {
		return 0, fmt.Errorf("broker: no installation for %s", owner)
	}
	m.mu.Lock()
	m.installByOrg[owner] = id
	m.mu.Unlock()
	return id, nil
}

// isStaleInstallation reports whether err is a GitHub 401/404 response — what
// GitHub returns for an installation id that no longer resolves.
func isStaleInstallation(err error) bool {
	var ghErr *gh.ErrorResponse
	if !errors.As(err, &ghErr) || ghErr.Response == nil {
		return false
	}
	switch ghErr.Response.StatusCode {
	case http.StatusUnauthorized, http.StatusNotFound:
		return true
	default:
		return false
	}
}

// splitRepos validates "owner/name" form, requires a single owner across all
// repos, and returns the owner plus bare repo names for the token request.
// EXACTLY two segments (a GitHub repo name cannot contain "/"): this predicate
// gates both policy-write validation (ValidateGitHubScopeShape) and minting,
// and must agree with githubScopeRepos (api package, builds the per-run
// git-broker allowlist) or a bad shape could pass write and dispatch a run
// that gets neither the broker route nor the injected host deny.
func splitRepos(repos []string) (owner string, names []string, err error) {
	for _, r := range repos {
		parts := strings.Split(r, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return "", nil, fmt.Errorf("broker: repo %q is not in owner/name form", r)
		}
		if owner == "" {
			owner = parts[0]
		} else if owner != parts[0] {
			return "", nil, fmt.Errorf("broker: all repos in one grant must share an owner (got %q and %q)", owner, parts[0])
		}
		names = append(names, parts[1])
	}
	return owner, names, nil
}

// ValidateGitHubScopeShape rejects a github_token grant scope that mintGitHub
// would refuse at MINT time — shape only (repos in owner/name form, sharing
// one owner; permission keys go-github recognizes) via the same two
// predicates minting uses, so policy-write validation and the mint gate can
// never drift apart. The permissions CEILING is a separate concern enforced
// by clampGitHubPermissions before minting.
//
// An EMPTY scope is valid: eligible_grants are TEMPLATES and the run supplies
// the concrete repos.
func ValidateGitHubScopeShape(scope json.RawMessage) error {
	if len(scope) == 0 {
		return nil
	}
	var sc githubScope
	if err := json.Unmarshal(scope, &sc); err != nil {
		return fmt.Errorf("broker: decode github scope: %w", err)
	}
	if _, _, err := splitRepos(sc.Repos); err != nil {
		return err
	}
	_, err := toInstallationPermissions(sc.Permissions)
	return err
}

// toInstallationPermissions maps a string->string permission map onto the
// typed go-github InstallationPermissions struct via a JSON round-trip, so
// permission names track go-github's json tags rather than a hand-written
// switch over ~100 fields.
func toInstallationPermissions(perms map[string]string) (*gh.InstallationPermissions, error) {
	if len(perms) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(perms)
	if err != nil {
		return nil, fmt.Errorf("broker: encode permissions: %w", err)
	}
	var ip gh.InstallationPermissions
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields() // reject perms GitHub would not recognize (fail closed)
	if err := dec.Decode(&ip); err != nil {
		return nil, fmt.Errorf("broker: unknown github permission in scope: %w", err)
	}
	return &ip, nil
}
