// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
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
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// procRegistry is the proxy-process-global secret mask registry. Every proxy-held
// credential is registered here — in EVERY rendering it can appear in, not just
// the one the holding call site carries (registerHeaderCredential /
// registerBasicAuthCredential) — so it is masked from decision-log stdout lines
// and from every sandbox-facing error body before it leaves the process.
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
	token  *tokenSource
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
func buildInjector(ctx context.Context, base string, token *tokenSource, pol *Policy, rules []InjectionConfig, client *http.Client) (*injector, error) {
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
		resolved, err := resolveInjection(ctx, base, token.Get(), r.GrantID, client)
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

		// Register the injected credential in the process-global Registry so it
		// is masked from decision-log output and from every sandbox-facing
		// error body before it can leave the proxy process. EVERY rendering,
		// not just the formatted header value — see registerHeaderCredential.
		registerHeaderCredential(resolved.Value)
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

	resolved, err := resolveInjection(context.Background(), i.base, i.token.Get(), e.grantID, i.client)
	if err != nil {
		return injectedHeader{}, true, fmt.Errorf("re-resolve injection for %q: %w", key, err)
	}
	e.header = injectedHeader{name: resolved.Header, value: resolved.Value}
	e.expiresAt = resolved.ExpiresAt
	registerHeaderCredential(resolved.Value)
	return e.header, true, nil
}

// ─── credential mask renderings ──────────────────────────────────────────────

// registerHeaderCredential registers, with the process-global mask registry,
// every rendering of ONE header credential the proxy holds — not merely the
// rendering the call site happens to be carrying.
//
// TRUST BOUNDARY (F155 — the mask is per-RENDERING, not per-credential; read
// before trimming an arm): procRegistry is what stands between a proxy-held
// credential and every sandbox-facing error body (Proxy.httpError ->
// maskDecisionBytes) and every decision-log line (decisions.go). And
// secretmask.Masker.Mask is EXACT BYTES: a credential is protected in exactly
// the renderings that were registered, so registering only "Bearer sk-…" leaves
// a bare "sk-…" in the same buffer untouched — and the bare form is the one a
// vendor echoes back in an error body and the one an operator sees in a config.
//
// The renderings, in the order they are registered:
//
//	"Bearer sk-…"     the formatted header value, as the header carries it
//	"sk-…"            the credential alone — the header scheme is a PREFIX, so
//	                  the space-separated tail is the credential itself
//	"user:pass"       when that tail decodes as base64 "user:pass" (the Basic
//	                  scheme), the decoded pair …
//	"pass"            … and its password half, the most sensitive part
//
// This is the shape upstreamProxy.maskValues (upstream.go) already applies to
// the corp-proxy credential, and the shape the control plane states outright at
// api/injection.go ("the formatted value … is what the agent might observe in
// proxy error messages; the raw value covers direct leakage"). One definition,
// every proxy-side site.
//
// HONEST RESIDUAL, narrowed but not closed: masking still catches only the
// renderings listed above, verbatim. A credential the proxy never sees in a
// given rendering (an arbitrary Format string that glues the secret to a
// suffix, e.g. "%s;v=1") cannot be derived here, and a hex-encoded or
// model-narrated form is not caught at all.
func registerHeaderCredential(formatted string) {
	if formatted == "" {
		return
	}
	procRegistry.AddGlobal([]byte(formatted))
	i := strings.LastIndexByte(formatted, ' ')
	if i < 0 || i+1 >= len(formatted) {
		return // no scheme prefix: the formatted value IS the credential
	}
	tail := formatted[i+1:]
	procRegistry.AddGlobal([]byte(tail))
	dec, err := base64.StdEncoding.DecodeString(tail)
	if err != nil {
		return
	}
	if c := bytes.IndexByte(dec, ':'); c >= 0 && c+1 < len(dec) {
		procRegistry.AddGlobal(dec)
		procRegistry.AddGlobal(dec[c+1:])
	}
}

// registerBasicAuthCredential registers a credential the proxy puts on the wire
// with http.Request.SetBasicAuth(user, tok) — the git-broker installation token
// and the PAT lane's minted token.
//
// TRUST BOUNDARY (F155): SetBasicAuth does not send tok; it sends
// base64(user + ":" + tok). Registering only the raw token therefore leaves the
// form that is actually on the wire — and the form that lands in a transport
// error quoting the request — unmasked. Both go in, via the same one definition
// registerHeaderCredential holds.
func registerBasicAuthCredential(user, tok string) {
	if tok == "" {
		return
	}
	procRegistry.AddGlobal([]byte(tok))
	registerHeaderCredential("Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+tok)))
}

// stripSandboxCredentials removes EVERY credential header the sandbox may have
// put on a request that is about to be injected with an operator-brokered one.
//
// TRUST BOUNDARY (F104 — this is the single definition, do not re-spell it at a
// call site): setting the brokered header is not enough, because the sandbox
// chooses the OTHER headers. A rule that injects under `X-Api-Key` leaves an
// `Authorization: Bearer <sandbox key>` untouched, and which of the two the
// upstream honours is then the UPSTREAM's choice, not Wardyn's — so an agent
// can substitute its own credential for the operator's on an allowlisted host
// and Wardyn's decision row still reads as brokered egress. forwardInspectedLLM
// (the brokered LLM route and the MITM path) has always stripped these four;
// injector.apply — the plain forward lane's injection — only did Header.Set, so
// the same request could reach the upstream carrying BOTH. One list, both
// injecting paths.
func stripSandboxCredentials(h http.Header) {
	h.Del("Authorization")
	h.Del("X-Api-Key")
	h.Del("Api-Key")
	h.Del("X-Auth-Token")
}

// injectableTransport reports whether a brokered credential may be attached to
// a forward request bound for scheme://host:port.
//
// TRUST BOUNDARY (F110, partial — the residual is filed): injection keys on the
// lowercased hostname alone — no scheme, no port — and addAPIKeyGrant couples
// each grant to a BARE exact allowlist entry, which policy.go matches on ANY
// port. The sandbox therefore picks the transport: `POST http://<host>:443/…`
// on the plain lane made the proxy attach the operator's credential to a
// request it then sent in CLEARTEXT to the TLS port — visible to every on-path
// device and to the corporate proxy hop the deployment guide tells operators to
// put in front of egress. The no-resident-secrets invariant still holds (the
// sandbox never sees the value), but the value leaves the proxy unencrypted.
//
// Cleartext to :443 is the one half of that the code can decide alone: nothing
// serves plaintext on the https port, so refusing it cannot break a connector
// an operator authored, and it is only reachable at all because the bare
// allowlist entry is port-blind. It is REFUSED (no injection — the upstream
// answers 401) rather than denied, so no audit string changes.
//
// The other half is NOT closed here: an operator who authored an api_key for an
// https vendor still cannot say so, because InjectionRule carries no scheme, so
// `POST http://<host>/…` on port 80 is indistinguishable from a legitimately
// plaintext internal connector — the ONLY shape plain-lane injection has ever
// worked in (a CONNECT tunnel cannot be injected into). Fixing that requires
// declaring transport intent in policy; see the filed follow-up.
func injectableTransport(scheme string, port int) bool {
	if strings.EqualFold(scheme, "https") {
		return true // the proxy itself runs the TLS leg
	}
	return port != 443
}

// apply strips the sandbox's own credential headers and sets the injected one
// if an exactly-allowed rule matches req's host AND the transport may carry it
// (injectableTransport). (Forward-proxy plain-HTTP path; a dynamic entry that
// fails to re-resolve simply isn't injected here — dynamic credentials target
// the TLS-MITM path, which fails closed via resolve.) A host with NO rule is
// left byte-for-byte alone: the strip is part of injection, never a blanket
// header filter on ordinary forward egress.
func (i *injector) apply(req *http.Request, host string, port int) {
	h, ok, err := i.resolve(host)
	if err != nil || !ok {
		return
	}
	if req.URL == nil || !injectableTransport(req.URL.Scheme, port) {
		return
	}
	stripSandboxCredentials(req.Header)
	req.Header.Set(h.name, h.value)
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

// resolveInjection calls GET /api/v1/internal/injection/{grantID} with the run
// token. This endpoint is the ONLY place the proxy obtains secret values; it
// is structurally unreachable from the sandbox (no brokered local route
// forwards it). Any non-200 (approval pending, missing secret, wrong kind) is
// a hard startup failure: we fail closed rather than start a proxy that
// silently forwards uncredentialed requests.
func resolveInjection(ctx context.Context, base, token string, grantID uuid.UUID, client *http.Client) (types.ResolvedInjection, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		base+"/api/v1/internal/injection/"+grantID.String(), nil)
	if err != nil {
		return types.ResolvedInjection{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return types.ResolvedInjection{}, fmt.Errorf("injection request: %w", err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		// Echo only a short slice of the upstream body: this error reaches the
		// SANDBOX on the MITM refresh-failure path (serveMITMRequest), so it is an
		// amplifier for control-plane text. Enough to diagnose a fail-closed
		// startup, not a 4 KiB relay. (The mask still covers it — see httpError.)
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return types.ResolvedInjection{}, fmt.Errorf("injection status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var ri types.ResolvedInjection
	if err := json.NewDecoder(resp.Body).Decode(&ri); err != nil {
		return types.ResolvedInjection{}, fmt.Errorf("decode injection: %w", err)
	}
	if ri.Header == "" || ri.Value == "" {
		return types.ResolvedInjection{}, fmt.Errorf("injection resolve returned empty header/value")
	}
	return ri, nil
}
