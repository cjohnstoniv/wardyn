// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"context"
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
	store secretstore.Store
	cfg   GitHubMinterConfig

	mu           sync.Mutex
	appClient    *gh.Client       // nil until the first successful lazy init
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

// client lazily builds (once) and returns the app-authenticated go-github
// client, reading the App credentials from the secret store on first use. It
// caches on success; a failure (secrets absent/invalid) is returned to the
// caller (fail closed) and retried on the next mint — so no restart is needed
// once the secrets appear.
func (m *githubMinter) client(ctx context.Context) (*gh.Client, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.appClient != nil {
		return m.appClient, nil
	}
	idRaw, err := m.store.Get(ctx, m.cfg.AppIDSecret)
	if err != nil {
		return nil, fmt.Errorf("broker: read github app id secret: %w", err)
	}
	appID, err := strconv.ParseInt(strings.TrimSpace(string(idRaw)), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("broker: parse github app id: %w", err)
	}
	pem, err := m.store.Get(ctx, m.cfg.PrivateKeySecret)
	if err != nil {
		return nil, fmt.Errorf("broker: read github app private key: %w", err)
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
	m.appClient = c
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

// splitRepos validates "owner/name" form, requires a single owner across all
// repos, and returns the owner plus bare repo names for the token request.
func splitRepos(repos []string) (owner string, names []string, err error) {
	for _, r := range repos {
		parts := strings.SplitN(r, "/", 2)
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
