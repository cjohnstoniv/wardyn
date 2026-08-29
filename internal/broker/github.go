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
// app-authenticated go-github client. Reading late removes the boot-time
// footgun: adding the App secrets after wardynd started no longer requires a
// restart before github_token grants can mint (the wizard's "add a key" path).
// The App private key never leaves this process and is never placed in a sandbox.
type githubMinter struct {
	// store is always the operator namespace (buildGitHubMinter passes the raw
	// store, never a .For(owner) view) — the GitHub App credential is
	// operator-provisioned, not per-member.
	store secretstore.Store
	cfg   GitHubMinterConfig

	mu           sync.Mutex
	appClient    *gh.Client       // nil until the first successful lazy init
	credHash     [32]byte         // sha256(appID||0||pem) appClient was built from
	installByOrg map[string]int64 // cache: owner -> installation id
	// baseURL, when set, overrides the go-github API base URL. Test seam only
	// (an httptest server); empty in production (the default api.github.com).
	baseURL string
}

// NewGitHubMinter builds a LAZY GitHubMinter: it validates the secret names but
// does NOT read the App credentials — those are read (and the ghinstallation
// transport built) on the first mint, then cached. When the secrets are
// genuinely absent at mint time the mint fails closed with a clear error.
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
// credentials from the secret store on EVERY mint (two cheap local Gets) and
// rebuilding only when their content hash has changed since the cached
// client was built. This is what picks up an operator rotating (or first
// setting) github-app-id / github-app-key without a wardynd restart: a
// one-time construction cached forever would keep serving the pre-rotation
// client (or never leave "secrets absent" once they were briefly missing at
// boot) for the life of the process. A read failure (secrets absent/invalid)
// is returned to the caller (fail closed) and retried on the next mint.
func (m *githubMinter) client(ctx context.Context) (*gh.Client, error) {
	idRaw, err := m.store.Get(ctx, m.cfg.AppIDSecret)
	if err != nil {
		return nil, fmt.Errorf("broker: read github app id secret: %w", err)
	}
	pem, err := m.store.Get(ctx, m.cfg.PrivateKeySecret)
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
	// go-github v88's NewClient takes functional options and is fallible; the
	// base URL is now supplied at construction (WithURLs), not by mutating an
	// exported field after the fact. WithURLs normalizes a missing trailing
	// slash itself, matching the test seam's srv.URL+"/".
	opts := []gh.ClientOptionsFunc{gh.WithHTTPClient(&http.Client{Transport: atr})}
	if m.baseURL != "" {
		opts = append(opts, gh.WithURLs(&m.baseURL, nil))
	}
	c, err := gh.NewClient(opts...)
	if err != nil {
		return nil, fmt.Errorf("broker: build github client: %w", err)
	}
	if m.appClient != nil {
		// A credential rotation invalidates the per-owner installation-id
		// cache in the same step as the client rebuild — otherwise a stale
		// id (from the App now-superseded) only self-heals reactively, after
		// CreateInstallationToken already 401s/404s against it.
		clear(m.installByOrg)
	}
	m.appClient = c
	m.credHash = hash
	return c, nil
}

// MintInstallationToken mints a short-lived installation token scoped to the
// given repositories with the given (already-clamped) permissions. All repos
// must belong to the same installation (owner); GitHub installation tokens are
// per-installation. ttl is informational: GitHub fixes installation token TTL
// at ~1h and ignores client-supplied lifetimes, so we record GitHub's returned
// expiry as authoritative.
func (m *githubMinter) MintInstallationToken(ctx context.Context, repos []string, permissions map[string]string, ttl time.Duration) (string, time.Time, error) {
	if len(repos) == 0 {
		return "", time.Time{}, errors.New("broker: github token requires at least one repo")
	}
	owner, names, err := splitRepos(repos)
	if err != nil {
		return "", time.Time{}, err
	}
	// Lazily read the App credentials + build the client on first mint. This is
	// where an absent secret fails closed (clear error, no panic).
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
			// The cached installation id is dead (the App was uninstalled and
			// reinstalled on the org, or credentials rotated to a different
			// App): drop it so the NEXT mint re-resolves instead of repeating
			// this same failure forever without a restart.
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
// GitHub returns for an installation id that no longer resolves (uninstalled
// and reinstalled on the org, or the id belonged to a different App). Both
// codes count: 401 is what CreateInstallationToken returns for a dead id,
// 404 is what a lookup on a never-existed one would return.
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
//
// EXACTLY two segments. A GitHub repo name cannot contain "/", so a deeper
// "owner/name/extra" is malformed — and it used to be accepted here (SplitN
// kept "name/extra" as the name) while githubScopeRepos in the api package
// dropped it. That disagreement decided two different things about one policy:
// this predicate gates policy-write validation (ValidateGitHubScopeShape, called
// from validatePolicySpec) and minting, while githubScopeRepos builds the
// per-run git-broker allowlist that decides whether a run is BROKERED at all. A
// three-segment scope therefore passed write, produced no broker entry, and
// dispatched a run that got neither the /wardyn/gh/ route nor the injected deny
// of the four GitHub hosts. Rejecting the shape at write time is what keeps the
// two in agreement — it never reaches dispatch.
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
// would refuse at MINT time — repos in owner/name form sharing one owner
// (splitRepos) and permission keys go-github recognizes
// (toInstallationPermissions). It lives HERE, next to the two predicates it
// runs, so a policy-write check and the mint gate can never drift apart; the api
// package (which already imports this one) calls it from validatePolicySpec so a
// bad key/repo is a 400 at author time instead of a run-time mint failure hours
// later. Results are discarded — this is the shape only. The permissions CEILING
// is a separate concern enforced by clampGitHubPermissions before minting.
//
// An EMPTY scope is valid: eligible_grants are TEMPLATES and the run supplies the
// concrete repos, so every shipped example policy carries "repos": []. (A literal
// `null` is 4 bytes, not len 0, and unmarshals into the struct as a no-op — which
// is what keeps a scope-less github grant passing.)
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

// toInstallationPermissions maps a string->string permission map onto the typed
// go-github InstallationPermissions struct. We round-trip through JSON so the
// permission names track go-github's json tags exactly (e.g. pull_requests),
// rather than maintaining a brittle hand-written switch over ~100 fields.
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
