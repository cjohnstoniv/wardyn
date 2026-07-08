// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
)

// procRegistry is the proxy-process-global secret mask registry. Formatted
// injection values (the actual header credential bytes) are registered here at
// startup so they can be masked from decision-log stdout lines and any other
// output stream before it leaves the process.
//
// A nil *Registry is safe throughout the secretmask package, so failing to
// initialise it (or not needing it) is a safe no-op rather than a panic.
var procRegistry = secretmask.NewRegistry()

// InjectionConfig pairs an egress.InjectionRule with the credential grant the
// proxy mints from at startup. The minted secret lives ONLY in proxy memory:
// it is never exposed to the sandbox (no env, no disk, no args). CONNECT
// tunnels cannot be injected into — the proxy has hostname-only visibility on
// TLS and never sees the encrypted request headers — so injection applies to
// plain-HTTP requests only.
type InjectionConfig struct {
	egress.InjectionRule
	// GrantID identifies the credential_grant to mint the secret value from
	// at startup via POST /api/v1/internal/credentials/mint.
	GrantID uuid.UUID `json:"grant_id"`
}

// injectRefreshMargin: re-resolve a rotating (expiring) injection this long
// before its expiry, so a fresh value is always injected. Must be <= the
// provider's own refresh margin so the provider refreshes when the proxy asks.
const injectRefreshMargin = 5 * time.Minute

// injector holds the per-host injection headers. A STATIC entry (api-key grant,
// expiresAt == 0) is fetched once at startup and cached for the run. A DYNAMIC
// entry (the subscription OAuth token, expiresAt != 0) is re-resolved via the
// control plane when it nears expiry — so the injected credential never goes
// stale. base/token/client are retained for those re-resolves.
type injector struct {
	mu     sync.Mutex // guards byHost lookups
	byHost map[string]*injEntry
	base   string
	token  string
	client *http.Client
}

type injectedHeader struct {
	name  string
	value string
}

// injEntry is one host's injection. grantID is immutable; header + expiresAt are
// guarded by reMu, which also single-flights re-resolution (only one goroutine
// refreshes a given host at a time; others block on reMu and then read the fresh
// value). expiresAt == 0 marks a static credential that never re-resolves.
type injEntry struct {
	grantID   uuid.UUID
	reMu      sync.Mutex
	header    injectedHeader
	expiresAt int64 // unix ms
}

// buildInjector mints each injection rule's secret once and formats its
// header. A rule whose host does not pass the EXACT allowlist is rejected:
// injection must never widen egress nor leak a secret to a wildcard/approved
// host. Returns an error if any mint fails (fail closed at startup).
func buildInjector(ctx context.Context, base, token string, pol *Policy, rules []InjectionConfig, client *http.Client) (*injector, error) {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	inj := &injector{byHost: make(map[string]*injEntry), base: base, token: token, client: client}
	for _, r := range rules {
		host := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(r.Host), "."))
		if host == "" {
			return nil, fmt.Errorf("injection rule with empty host")
		}
		if !pol.AllowedExactHost(host) {
			return nil, fmt.Errorf("injection rule host %q is not in the exact allowlist", host)
		}
		if r.GrantID == uuid.Nil {
			return nil, fmt.Errorf("injection rule for %q missing grant_id", host)
		}
		resolved, err := resolveInjection(ctx, base, token, r.GrantID, client)
		if err != nil {
			return nil, fmt.Errorf("resolve injection for %q: %w", host, err)
		}
		// The control plane resolves header + FORMATTED value server-side
		// (it holds the secret store); the local rule is authoritative only
		// for the host binding, which the exact-allowlist check above gates.
		inj.byHost[host] = &injEntry{
			grantID:   r.GrantID,
			header:    injectedHeader{name: resolved.Header, value: resolved.Value},
			expiresAt: resolved.ExpiresAt,
		}

		// Register the formatted secret value (e.g. "Bearer sk-...") in the
		// process-global Registry so it is masked from decision-log output
		// before it can leave the proxy process.
		//
		// HONEST RESIDUAL: masking catches verbatim byte-identical leakage only;
		// base64/hex/model-narrated forms of the credential are NOT caught.
		if resolved.Value != "" {
			procRegistry.AddGlobal([]byte(resolved.Value))
		}
	}
	return inj, nil
}

// resolve returns the current injection header for host. For a static entry it
// returns the startup-minted value; for a dynamic (expiring) entry it re-resolves
// via the control plane when within injectRefreshMargin of expiry. The bool
// reports whether a rule EXISTS for the host; a non-nil error means a rule exists
// but its (dynamic) credential could not be refreshed — the caller MUST fail
// closed rather than forward a stale credential.
func (i *injector) resolve(host string) (injectedHeader, bool, error) {
	if i == nil {
		return injectedHeader{}, false, nil
	}
	key := strings.ToLower(strings.TrimSuffix(host, "."))
	i.mu.Lock()
	e, ok := i.byHost[key]
	i.mu.Unlock()
	if !ok {
		return injectedHeader{}, false, nil
	}

	// Single-flight per host: hold reMu across the (rare) re-resolve so concurrent
	// requests for this host block once, then read the refreshed value. Other
	// hosts are unaffected (separate entries/locks).
	e.reMu.Lock()
	defer e.reMu.Unlock()
	if e.expiresAt == 0 || time.Now().Before(time.UnixMilli(e.expiresAt).Add(-injectRefreshMargin)) {
		return e.header, true, nil // static, or dynamic and still fresh
	}

	resolved, err := resolveInjection(context.Background(), i.base, i.token, e.grantID, i.client)
	if err != nil {
		return injectedHeader{}, true, fmt.Errorf("re-resolve injection for %q: %w", key, err)
	}
	e.header = injectedHeader{name: resolved.Header, value: resolved.Value}
	e.expiresAt = resolved.ExpiresAt
	if resolved.Value != "" {
		procRegistry.AddGlobal([]byte(resolved.Value))
	}
	return e.header, true, nil
}

// apply sets the injected header on req if an exactly-allowed rule matches its
// host. Returns true when a header was injected. (Forward-proxy plain-HTTP path;
// a dynamic entry that fails to re-resolve simply isn't injected here — dynamic
// credentials target the TLS-MITM path, which fails closed via resolve.)
func (i *injector) apply(req *http.Request, host string) bool {
	h, ok, err := i.resolve(host)
	if err != nil || !ok {
		return false
	}
	req.Header.Set(h.name, h.value)
	return true
}

// headerFor returns the current injection header for host, if a rule exists.
// The LLM local route uses this to apply the SAME credential the forward-proxy
// path would inject. A dynamic credential that cannot be re-resolved yields
// (_, false) here so the brokered LLM route reports no_llm_credential.
func (i *injector) headerFor(host string) (injectedHeader, bool) {
	h, ok, err := i.resolve(host)
	if err != nil {
		return injectedHeader{}, false
	}
	return h, ok
}

// resolvedInjection is the control plane's injection-resolve result: the
// header name and the FORMATTED secret value (format applied server-side).
// ExpiresAt (unix ms, 0 = never) marks a rotating credential the proxy must
// re-resolve before it lapses (the subscription OAuth token); a static api-key
// grant leaves it 0.
type resolvedInjection struct {
	Host      string `json:"host"`
	Header    string `json:"header"`
	Value     string `json:"value"`
	JTI       string `json:"jti"`
	ExpiresAt int64  `json:"expires_at,omitempty"`
}

// resolveInjection calls GET /api/v1/internal/injection/{grantID} with the run
// token. This endpoint is the ONLY place the proxy obtains secret values; it
// is structurally unreachable from the sandbox (no brokered local route
// forwards it). Any non-200 (approval pending, missing secret, wrong kind) is
// a hard startup failure: we fail closed rather than start a proxy that
// silently forwards uncredentialed requests.
func resolveInjection(ctx context.Context, base, token string, grantID uuid.UUID, client *http.Client) (resolvedInjection, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		base+"/api/v1/internal/injection/"+grantID.String(), nil)
	if err != nil {
		return resolvedInjection{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return resolvedInjection{}, fmt.Errorf("injection request: %w", err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
		return resolvedInjection{}, fmt.Errorf("injection status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var ri resolvedInjection
	if err := json.NewDecoder(resp.Body).Decode(&ri); err != nil {
		return resolvedInjection{}, fmt.Errorf("decode injection: %w", err)
	}
	if ri.Header == "" || ri.Value == "" {
		return resolvedInjection{}, fmt.Errorf("injection resolve returned empty header/value")
	}
	return ri, nil
}
