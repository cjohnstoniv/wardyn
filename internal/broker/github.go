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
// lazily on the first mint, then caches the app-authenticated go-github
// client, so adding App secrets after wardynd started needs no restart. The
// App private key never leaves this process.
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
// runs inside mint()'s transaction, holding the grant row lock and a pooled
// Postgres connection, so a blackholed api.github.com would otherwise pin
// both. Applied twice — as the http.Client Timeout (per round trip) and as
// the derived ctx (per mint) — so a tighter caller deadline still wins.
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

// client returns the app-authenticated go-github client, re-reading the App
// credentials on EVERY mint and rebuilding only when their content hash
// changed — this is what picks up a rotated github-app-id/key without a
// restart. A read failure fails closed and retries next mint.
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
// must share one owner (installation). ttl is informational only: GitHub
// fixes token TTL at ~1h, so its returned expiry is authoritative.
func (m *githubMinter) MintInstallationToken(ctx context.Context, repos []string, permissions map[string]string, ttl time.Duration) (string, time.Time, error) {
	if len(repos) == 0 {
		return "", time.Time{}, errors.New("broker: github token requires at least one repo")
	}
	owner, names, err := splitRepos(repos)
	if err != nil {
		return "", time.Time{}, err
	}
	// One ceiling for the whole mint (see githubClientTimeout), so
	// VerifyRefRuleset's probe mint is bounded too. Tighter caller deadline still wins.
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

// Revoke hands token back to GitHub, authenticated AS THE TOKEN ITSELF —
// hence its own client rather than the app-authenticated one.
//
// WithoutCancel is load-bearing: callers reach this after something already
// went wrong, often with an already-expired ctx — exactly when a live token
// would otherwise linger for GitHub's full ~1h. Empty token is a no-op.
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

// splitRepos validates "owner/name" form (exactly two segments; a GitHub repo
// name cannot contain "/"), requires a single shared owner, and returns owner
// plus bare names. Gates both policy-write validation and minting, and must
// agree with githubScopeRepos (api package) or a bad shape could dispatch a
// run with neither the broker route nor the injected host deny.
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
// would refuse at mint time — shape only (owner/name repos sharing one owner;
// recognized permission keys), using the same predicates minting uses so the
// two can never drift apart. The permissions ceiling is separately enforced
// by clampGitHubPermissions before minting.
//
// An empty scope is valid: eligible_grants are templates and the run supplies
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
// typed go-github struct via a JSON round-trip, so permission names track
// go-github's json tags rather than a hand-written switch over ~100 fields.
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
