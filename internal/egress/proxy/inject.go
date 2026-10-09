// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// procRegistry is the proxy-process-global secret mask registry. SECURITY: every proxy-held
// credential is registered here, in every rendering it can appear in, so it's masked from decision-log
// stdout and sandbox-facing error bodies. A nil *Registry is safe (missed init is a no-op, not a panic).
var procRegistry = secretmask.NewRegistry()

// procMask registers v with procRegistry for the process's life (filed under uuid.Nil, never evicted).
func procMask(v []byte) { _ = procRegistry.Add(uuid.Nil, v) } // process-local: no Backend, so it cannot fail

// InjectionConfig pairs an egress.InjectionRule with the credential grant the proxy mints from at
// startup. SECURITY: the minted secret lives only in proxy memory, never exposed to the sandbox.
// CONNECT tunnels (hostname-only TLS visibility) can't be injected into, so this is plain-HTTP only.
type InjectionConfig struct {
	egress.InjectionRule
	// GrantID: the credential_grant minted at startup via POST /api/v1/internal/credentials/mint.
	GrantID uuid.UUID `json:"grant_id"`
}

// injectRefreshMargin re-resolves an expiring injection this long before expiry; must be <= the
// provider's own refresh margin so the provider has refreshed by the time the proxy asks.
const injectRefreshMargin = 5 * time.Minute

// lastGoodGrace is how long past expiry an entry keeps serving its last-good header while re-resolve
// fails transiently; a definitive refusal drops the header at once regardless of grace.
const lastGoodGrace = 15 * time.Minute

// lastGoodRetry paces re-resolves during last-good serving, so an outage is asked once per interval, not once per request.
const lastGoodRetry = 30 * time.Second

// injector holds the per-host injection headers: static (expiresAt==0) is fetched once at startup;
// dynamic (expiresAt!=0) re-resolves near expiry so a store-removed credential stops being injected.
type injector struct {
	mu     sync.Mutex // guards byHost lookups
	byHost map[string]*injEntry
	base   string
	token  *tokenSource
	client *http.Client
	// reauth owns the bounded mid-run credential re-auth workflows (credhold.go);
	// nil means "no hold" so a resolve that would have held fails closed instead.
	reauth    *reauthCoordinator
	approvals approvalReader
}

type injectedHeader struct {
	name  string
	value string
	// jti names the credential (ResolvedInjection.JTI), so an upstream refusal of it can say which one went stale.
	jti string
}

// injEntry is one host's injection: grantID is immutable; header + expiresAt are guarded by reMu,
// which also single-flights re-resolution. expiresAt==0 marks a static credential that never re-resolves.
type injEntry struct {
	grantID uuid.UUID
	reMu    sync.Mutex
	header  injectedHeader
	// reauth is the re-auth workflow open for this entry (single-flight stopping a second control-plane
	// call for one lapse). Guarded by reMu; the wait happens with reMu released (resolveCtx).
	reauth *reauthWorkflow
	// requireTLS is the rule's transport declaration; immutable after buildInjector, so no lock needed.
	requireTLS bool
	// rule is the authored rule, kept for PinPath/PinQuery; immutable after buildInjector, so no lock needed.
	rule      egress.InjectionRule
	expiresAt int64 // unix ms
	// retryAt is set while last-good is served after a transient failure, or a stale re-resolve
	// (install). Guarded by reMu.
	retryAt time.Time
	// staleJTI is the credential Azure DevOps refused after dropStale dropped the header: the next
	// re-resolve sends it as stale_jti, until a credential is installed. staleAt paces dropStale.
	// Guarded by reMu.
	staleJTI string
	staleAt  time.Time
}

// install writes a re-resolved credential onto e, pacing like an outage if the answer is already
// inside injectRefreshMargin (else every request would re-resolve: a mint + secret.read row). Caller holds reMu.
func (e *injEntry) install(resolved types.ResolvedInjection, now time.Time) {
	e.header = injectedHeader{name: resolved.Header, value: resolved.Value, jti: resolved.JTI}
	e.expiresAt = resolved.ExpiresAt
	e.retryAt, e.staleJTI = time.Time{}, ""
	if e.expiresAt != 0 && !now.Before(time.UnixMilli(e.expiresAt).Add(-injectRefreshMargin)) {
		e.retryAt = now.Add(lastGoodRetry)
	}
}

// lastGood reports whether e may keep serving its header after a transient re-resolve failure,
// while the header isn't already dropped and e is within lastGoodGrace of expiry. Caller holds reMu.
func (e *injEntry) lastGood(err error, now time.Time) bool {
	if !transientResolveFailure(err) || e.header.value == "" || e.expiresAt == 0 ||
		!now.Before(time.UnixMilli(e.expiresAt).Add(lastGoodGrace)) {
		return false
	}
	e.retryAt = now.Add(lastGoodRetry)
	return true
}

// transientResolveFailure reports whether a re-resolve failed because nothing answered (503 or no answer); every other failure is a refusal.
func transientResolveFailure(err error) bool {
	var se injectionStatusError
	if errors.As(err, &se) {
		return se.status == http.StatusServiceUnavailable
	}
	var ue *url.Error
	return errors.As(err, &ue)
}

// buildInjector mints each injection rule's secret once and formats its header. SECURITY: a rule
// whose host fails the exact allowlist is rejected — injection must never widen egress or leak a
// secret to a wildcard/approved host — and so is a second rule for a host that already has one.
// Any mint failure fails the whole startup closed.
func buildInjector(ctx context.Context, base string, token *tokenSource, pol *Policy, rules []InjectionConfig, client *http.Client) (*injector, error) {
	inj := &injector{
		byHost: make(map[string]*injEntry), base: base, token: token, client: client,
		reauth:    newReauthCoordinator(),
		approvals: httpApprovalReader{base: base, token: token, client: client},
	}
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
		// One rule per host, refused before the second one is resolved: the map is keyed by host, so
		// a second rule would replace the first and its credential would ride the first's traffic.
		if _, dup := inj.byHost[host]; dup {
			return nil, fmt.Errorf("injection rule host %q has more than one rule; a host carries one credential", host)
		}
		// No hold at boot: this runs under the proxy's 30s startupCtx, seconds after dispatch refreshed the
		// credential synchronously, so a 423 here just fails closed like any other error.
		resolved, err := resolveInjectionQuery(ctx, base, token.Get(), r.GrantID, bootResolveQuery, client)
		if err != nil {
			return nil, fmt.Errorf("resolve injection for %q: %w", host, err)
		}
		// The control plane resolves header + formatted value server-side; the local rule only governs the host binding, gated above.
		inj.byHost[host] = &injEntry{
			grantID:    r.GrantID,
			header:     injectedHeader{name: resolved.Header, value: resolved.Value, jti: resolved.JTI},
			requireTLS: r.RequireTLS,
			rule:       r.InjectionRule,
			expiresAt:  resolved.ExpiresAt,
		}

		// SECURITY: register in the process-global Registry so every rendering is masked from
		// decision-log output and sandbox-facing errors.
		registerHeaderCredential(resolved.Value)
	}
	return inj, nil
}

// resolve returns host's current injection header: startup-minted for a static entry, re-resolved
// near expiry for a dynamic one. The bool reports whether a rule exists; a non-nil error means the
// dynamic credential couldn't refresh — SECURITY: the caller must fail closed, never forward a stale credential.
func (i *injector) resolve(host string) (injectedHeader, bool, error) {
	return i.resolveCtx(context.Background(), host)
}

// resolveCtx is resolve with the caller's context, needed by the re-resolve hold: the MITM request's
// ctx lets a disconnected SDK release reMu instead of pinning it for the whole re-auth budget.
func (i *injector) resolveCtx(ctx context.Context, host string) (injectedHeader, bool, error) {
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

	// reMu single-flights the ordinary re-resolve but never spans a HOLD — that would queue every later
	// caller on an uncancellable mutex for the whole re-auth budget, so a hung-up SDK would never release.
	for {
		e.reMu.Lock()
		now := time.Now()
		// A dropped header (empty) is never served: it re-resolves whatever its expiry says.
		if e.header.value != "" &&
			(e.expiresAt == 0 || now.Before(time.UnixMilli(e.expiresAt).Add(-injectRefreshMargin)) || now.Before(e.retryAt)) {
			h := e.header
			e.reMu.Unlock()
			return h, true, nil // static, dynamic and still fresh, or riding out an outage
		}
		if wf := e.reauth; wf != nil {
			if wf.finished() {
				// Hold over: drop it and re-resolve (the owner may have signed in, or the control plane
				// may name a new request). The coordinator still holds the old workflow by approval id
				// so a repeat 423 gets its terminal result at once.
				e.reauth = nil
				e.reMu.Unlock()
				continue
			}
			// A hold is open: join it rather than make a second control-plane call for one lapse; reMu released first.
			e.reMu.Unlock()
			resolved, err := wf.await(ctx)
			if err != nil {
				e.dropIfFinished(wf)
				return injectedHeader{}, true, fmt.Errorf("re-resolve injection for %q: %w", key, err)
			}
			return i.installHeader(e, wf, resolved), true, nil
		}

		var query url.Values
		if e.staleJTI != "" {
			query = url.Values{"stale_jti": {e.staleJTI}}
		}
		resolved, err := resolveInjectionQuery(ctx, i.base, i.token.Get(), e.grantID, query, i.client)
		if err == nil {
			e.install(resolved, time.Now())
			h := e.header
			e.reMu.Unlock()
			registerHeaderCredential(resolved.Value)
			return h, true, nil
		}
		if e.lastGood(err, time.Now()) {
			h := e.header
			e.reMu.Unlock()
			slog.WarnContext(ctx, "wardyn-proxy: re-resolving an injected credential failed transiently; serving the last-good value",
				slog.String("host", key), slog.Time("until", time.UnixMilli(e.expiresAt).Add(lastGoodGrace)), slog.Any("err", err))
			return h, true, nil
		}
		// Definitive, or grace spent: drop the header now so nothing later serves it as last-good (a 423 counts).
		e.header, e.retryAt = injectedHeader{}, time.Time{}
		var pending errReauthPending
		if !errors.As(err, &pending) {
			e.reMu.Unlock()
			// errReauthTimedOut travels out wrapped but intact: errors.Is still answers true on a wrapped sentinel.
			return injectedHeader{}, true, fmt.Errorf("re-resolve injection for %q: %w", key, err)
		}
		// 423: the control plane needs a human — open or join the workflow by approval id, publish it, release reMu before waiting.
		if i.reauth == nil || i.approvals == nil {
			e.reMu.Unlock()
			// No hold lane on this injector: the 423 is a refusal to report, but nothing expired.
			return injectedHeader{}, true, fmt.Errorf("re-resolve injection for %q: %w", key,
				errReauthEnded{reason: "this proxy has no re-auth hold lane"})
		}
		wf, fresh, admitted := i.reauth.admit(pending.approvalID, credentialReauthBudget())
		if !admitted {
			e.reMu.Unlock()
			return injectedHeader{}, true, fmt.Errorf("re-resolve injection for %q: %w", key, errReauthCapped)
		}
		e.reauth = wf
		e.reMu.Unlock()
		if fresh {
			go wf.run(i.base, i.token, e.grantID, i.client, i.approvals)
		}
		// Wait here, not around the loop: looping back would re-read the entry, see the terminal sticky
		// workflow, drop it and re-resolve — round and round.
		held, herr := wf.await(ctx)
		if herr != nil {
			e.dropIfFinished(wf)
			return injectedHeader{}, true, fmt.Errorf("re-resolve injection for %q: %w", key, herr)
		}
		return i.installHeader(e, wf, held), true, nil
	}
}

// dropStale drops host's header after Azure DevOps refused the credential named jti, so the host's
// next request re-resolves with stale_jti. The control plane decides what the hint means (a newer
// credential it already holds, or a fresh one); the proxy only asks.
//
// Paced: at most one drop per entry per lastGoodRetry, and none once the entry holds another
// credential than jti, so concurrent refusals of one header make one re-resolve, and a credential
// refused again straight after its re-resolve is not asked about in a loop.
func (i *injector) dropStale(host, jti string) {
	if i == nil {
		return
	}
	key := strings.ToLower(strings.TrimSuffix(host, "."))
	i.mu.Lock()
	e, ok := i.byHost[key]
	i.mu.Unlock()
	if !ok {
		return
	}
	e.reMu.Lock()
	defer e.reMu.Unlock()
	now := time.Now()
	if e.header.value == "" || e.header.jti != jti || now.Before(e.staleAt.Add(lastGoodRetry)) {
		return
	}
	e.header, e.retryAt = injectedHeader{}, time.Time{}
	e.staleJTI, e.staleAt = jti, now
}

// reresolveStale is dropStale followed at once by the re-resolve, for a request that will be
// retried with the answer. The re-resolve is resolveCtx's own: single-flighted on reMu, and a 423
// joins the re-auth hold exactly as an expiry re-resolve does. A paced drop re-resolves nothing and
// answers the header the entry holds.
func (i *injector) reresolveStale(ctx context.Context, host, jti string) (injectedHeader, error) {
	i.dropStale(host, jti)
	h, _, err := i.resolveCtx(ctx, host)
	return h, err
}

// dropIfFinished takes a workflow off the entry only once terminal: a caller that hangs up must
// leave a live hold in place, or the next retry would spare-mint before joining the same workflow.
func (e *injEntry) dropIfFinished(wf *reauthWorkflow) {
	if !wf.finished() {
		return
	}
	e.reMu.Lock()
	if e.reauth == wf {
		e.reauth = nil // the next caller re-resolves
	}
	e.reMu.Unlock()
}

// installHeader writes a hold's resolved credential onto the entry, taking reMu only for the write so the wait above holds no lock.
func (i *injector) installHeader(e *injEntry, wf *reauthWorkflow, resolved types.ResolvedInjection) injectedHeader {
	e.reMu.Lock()
	e.install(resolved, time.Now())
	if e.reauth == wf {
		e.reauth = nil // the hold is over and its credential is installed
	}
	h := e.header
	e.reMu.Unlock()
	registerHeaderCredential(resolved.Value)
	return h
}

// requiresTLS reports whether host's injection rule declares require_tls, without minting or
// re-resolving: the plain lane uses it to decide whether to refuse, and a refusal must not touch the
// credential. False for an unknown host.
func (i *injector) requiresTLS(host string) bool {
	if i == nil {
		return false
	}
	key := strings.ToLower(strings.TrimSuffix(host, "."))
	i.mu.Lock()
	e, ok := i.byHost[key]
	i.mu.Unlock()
	return ok && e.requireTLS
}

// Credential mask renderings

// registerHeaderCredential registers every rendering of ONE header credential with the
// process-global mask registry (mirrors upstreamProxy.maskValues).
//
// TRUST BOUNDARY (per-rendering, not per-credential): mask is exact bytes, so registering only
// "Bearer sk-…" would leave a bare "sk-…" unmasked in sandbox-facing errors/logs. Registers, in
// order: the formatted value, the credential alone, the base64-decoded "user:pass" if Basic, and the
// password alone. Residual: only these exact-byte forms are caught, not one glued to a suffix,
// hex-encoded, or model-narrated.
func registerHeaderCredential(formatted string) {
	if formatted == "" {
		return
	}
	procMask([]byte(formatted))
	i := strings.LastIndexByte(formatted, ' ')
	if i < 0 || i+1 >= len(formatted) {
		return // no scheme prefix: the formatted value IS the credential
	}
	tail := formatted[i+1:]
	procMask([]byte(tail))
	dec, err := base64.StdEncoding.DecodeString(tail)
	if err != nil {
		return
	}
	if c := bytes.IndexByte(dec, ':'); c >= 0 && c+1 < len(dec) {
		procMask(dec)
		procMask(dec[c+1:])
	}
}

// registerBasicAuthCredential registers a credential sent via http.Request.SetBasicAuth(user, tok) —
// the git-broker installation token and the PAT lane's minted token.
//
// TRUST BOUNDARY: SetBasicAuth sends base64(user+":"+tok), not tok — registering only the raw token
// would leave the actual wire form unmasked in a transport error. Both go in via registerHeaderCredential.
func registerBasicAuthCredential(user, tok string) {
	if tok == "" {
		return
	}
	procMask([]byte(tok))
	registerHeaderCredential("Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+tok)))
}

// stripSandboxCredentials removes every credential header the sandbox may have put on a request
// about to carry an operator-brokered one.
//
// TRUST BOUNDARY (single definition, never re-spelled at a call site): the brokered header alone
// isn't enough, since the sandbox picks other headers too — a rule injecting under X-Api-Key would
// leave a sandbox `Authorization: Bearer <key>` untouched, letting the upstream choose which wins.
//
// The fixed list (vendor-doc-verified) covers every header a brokered vendor reads as a credential
// (Authorization, X-Api-Key, Api-Key, X-Auth-Token, Private-Token, X-Goog-Api-Key,
// X-Amz-Security-Token, X-Functions-Key, X-Access-Token, Anthropic-Api-Key, Cookie).
// Proxy-Authorization is hop-by-hop, already stripped elsewhere. owned is stripped alongside the
// list since it can't know a rule-specific header; an injected request overwrites it, a pin-withheld one doesn't.
func stripSandboxCredentials(h http.Header, owned string) {
	if owned != "" {
		h.Del(owned)
	}
	for _, name := range []string{
		"Authorization",
		"X-Api-Key",
		"Api-Key",
		"X-Auth-Token",
		"Anthropic-Api-Key",
		"Cookie",
		"Private-Token",
		"X-Access-Token",
		"X-Amz-Security-Token",
		"X-Functions-Key",
		"X-Goog-Api-Key",
	} {
		h.Del(name)
	}
}

// injectableTransport reports whether a brokered credential may attach to a forward request bound
// for scheme://host:port.
//
// TRUST BOUNDARY: injection keys on the lowercased hostname alone, and the sandbox picks the
// transport, so `POST http://<host>:443/…` on the plain lane could attach the credential to a
// cleartext request — no-resident-secrets still holds, but the value leaves the proxy unencrypted.
//
//   - https: the proxy runs the TLS leg. Always injectable.
//   - cleartext to 443/8443/9443 (tlsConventionalPorts): never, whatever the allowlist says.
//   - cleartext to an https-only host (isLLMHost): never.
//   - cleartext to port 80 with a bare allowlist entry: injectable.
//   - cleartext to any other port: only when the operator authored that port (Policy.AuthoredPortFor).
//
// Refusal here means no injection (upstream answers 401), never a deny.
//
// Residual: an https-only vendor with no table entry is closed via `require_tls` on the rule instead.
func (p *Proxy) injectableTransport(scheme, host string, port int) bool {
	if strings.EqualFold(scheme, "https") {
		return true // the proxy itself runs the TLS leg
	}
	if tlsConventionalPorts[port] {
		return false // unconditional clamp: an authored :8443 does not re-admit it
	}
	if p.isLLMHost(host) {
		return false
	}
	// Port 80 asks the BARE question (a bare entry is silent about port);
	// every other port asks AuthoredPortFor, so vendor.example:8443 is
	// credentialed only on that port.
	return (port == defaultPortForScheme("http") && p.policy.AllowedBareExactHost(host)) ||
		p.policy.AuthoredPortFor(host, port)
}

// tlsConventionalPorts: 443 plus the two alternates every appliance ships as its HTTPS port.
// Cleartext injection is refused to all of them regardless of authoring, since the sandbox still picks
// the scheme; an https-only vendor outside this set is served by `require_tls` on the rule instead.
var tlsConventionalPorts = map[int]bool{443: true, 8443: true, 9443: true}

// applyInjection is the plain forward lane's credential injection, split deliberately: the proxy
// decides whether the transport may carry a brokered credential (needs policy/vendor table), the
// injector decides whether a rule matches the host. A host with no rule is left untouched either way.
func (p *Proxy) applyInjection(req *http.Request, host string, port int) {
	if req.URL == nil || !p.injectableTransport(req.URL.Scheme, host, port) {
		return
	}
	p.inject.apply(req, host, port)
}

// apply strips the sandbox's own credential headers and sets the injected one if an exactly-allowed
// rule matches req's host (plain-HTTP path; a dynamic entry failing re-resolve just isn't injected —
// dynamic credentials target the TLS-MITM path, which fails closed via resolve).
func (i *injector) apply(req *http.Request, host string, port int) {
	h, ok, err := i.resolve(host)
	if err != nil || !ok {
		return
	}
	// Strip always, inject only where the rule's PIN allows — same two decisions the MITM lane makes,
	// or the pin is bypassable by skipping TLS.
	stripSandboxCredentials(req.Header, h.name)
	if !i.allowsInjection(host, req.Method, req.URL.Path, req.URL.RawQuery) {
		return
	}
	req.Header.Set(h.name, h.value)
}

// headerFor returns host's current injection header, if a rule exists — used by the LLM local route
// to apply the same credential the forward-proxy path would; unresolvable yields (_, false).
func (i *injector) headerFor(host string) (injectedHeader, bool) {
	h, ok, err := i.resolve(host)
	if err != nil {
		return injectedHeader{}, false
	}
	return h, ok
}

// bootResolveQuery marks the sidecar's boot-time resolves (mirrors adoResolvePhase/adoResolvePhaseBoot).
var bootResolveQuery = url.Values{"phase": {"boot"}}

// resolveInjectionQuery calls GET /api/v1/internal/injection/{grantID} with the run token. SECURITY: the
// only place the proxy obtains secret values, structurally unreachable from the sandbox. query carries
// the boot phase, the Azure DevOps capability hold's per-(host, capability) ask (ado_hold.go) or a
// stale_jti; nil is the plain resolve.
func resolveInjectionQuery(ctx context.Context, base, token string, grantID uuid.UUID, query url.Values, client *http.Client) (types.ResolvedInjection, error) {
	target := base + "/api/v1/internal/injection/" + grantID.String()
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
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
		// Echo only a short slice: this error reaches the sandbox on the MITM refresh-failure path, so
		// 1 KiB is enough to diagnose without a full relay (masked, see httpError).
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		// 423 is not a refusal: it becomes a typed error the re-resolve path can HOLD on (credhold.go),
		// checked before the generic status-error branch.
		if resp.StatusCode == http.StatusLocked {
			if pending, ok := reauthPendingFrom(b); ok {
				return types.ResolvedInjection{}, pending
			}
		}
		return types.ResolvedInjection{}, injectionStatusError{status: resp.StatusCode, body: strings.TrimSpace(string(b))}
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

// allowsInjection reports whether host's rule lets THIS request carry the credential; true for an
// unknown host and every unpinned rule, so every lane but captured-AWS-SSO is unchanged.
func (i *injector) allowsInjection(host, method, path, rawQuery string) bool {
	if i == nil {
		return true
	}
	key := strings.ToLower(strings.TrimSuffix(host, "."))
	i.mu.Lock()
	e, ok := i.byHost[key]
	i.mu.Unlock()
	if !ok {
		return true
	}
	return e.rule.AllowsInjection(method, path, rawQuery)
}

// applyCredential puts a rule's credential on an upstream request.
//
// Strip and inject are two decisions, not one: a host with an injection rule always has the
// sandbox's headers removed, whether or not the pin then supplies one — a request the pin doesn't
// cover forwards with neither header, keeping the sandbox's header from reaching the portal on
// requests the pin narrows.
//
// ownedHeader == "" means no rule governs this host, so nothing is stripped.
func applyCredential(h http.Header, ownedHeader string, hdr *injectedHeader) {
	if ownedHeader == "" {
		return
	}
	stripSandboxCredentials(h, ownedHeader)
	if hdr != nil {
		h.Set(hdr.name, hdr.value)
	}
}
