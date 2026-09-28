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

// procRegistry is the proxy-process-global secret mask registry. Every
// proxy-held credential is registered here, in EVERY rendering it can appear
// in, so it is masked from decision-log stdout and every sandbox-facing error
// body before it leaves the process.
//
// A nil *Registry is safe throughout the secretmask package, so failing to
// initialise it is a safe no-op rather than a panic.
var procRegistry = secretmask.NewRegistry()

// procMask registers v with procRegistry for the life of this process. The
// sidecar serves one run, so what it holds is that run's corpus, filed under
// uuid.Nil — never evicted, since the process ends with the run.
func procMask(v []byte) { procRegistry.Add(uuid.Nil, v) }

// InjectionConfig pairs an egress.InjectionRule with the credential grant the
// proxy mints from at startup. The minted secret lives ONLY in proxy memory
// (never exposed to the sandbox). CONNECT tunnels cannot be injected into —
// the proxy has hostname-only visibility on TLS — so injection applies to
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

// lastGoodGrace is how long past its expiry an entry keeps serving its
// last-good header while re-resolving it fails TRANSIENTLY. A definitive
// refusal drops the header at once, whatever is left of the grace.
const lastGoodGrace = 15 * time.Minute

// lastGoodRetry paces re-resolves while a last-good header is being served, so
// an outage is asked once per interval per entry, not once per request.
const lastGoodRetry = 30 * time.Second

// injector holds the per-host injection headers. A STATIC entry (expiresAt ==
// 0: an approval-gated api-key grant, single-use mint) is fetched once at
// startup and cached. A DYNAMIC entry (expiresAt != 0) is re-resolved via the
// control plane when it nears expiry, so a credential removed at the store
// stops being injected. base/token/client are retained for those re-resolves.
type injector struct {
	mu     sync.Mutex // guards byHost lookups
	byHost map[string]*injEntry
	base   string
	token  *tokenSource
	client *http.Client
	// reauth owns the bounded mid-run credential re-auth workflows (credhold.go).
	// Nil is safe and means "no hold" — a resolve that would have held fails
	// closed instead.
	reauth    *reauthCoordinator
	approvals approvalReader
}

type injectedHeader struct {
	name  string
	value string
}

// injEntry is one host's injection. grantID is immutable; header + expiresAt are
// guarded by reMu, which also single-flights re-resolution. expiresAt == 0
// marks a static credential that never re-resolves.
type injEntry struct {
	grantID uuid.UUID
	reMu    sync.Mutex
	header  injectedHeader
	// reauth is the re-auth workflow currently open for THIS entry, if any —
	// the single-flight that stops a second control-plane call for one lapse.
	// Guarded by reMu; the WAIT on it happens with reMu released (resolveCtx).
	reauth *reauthWorkflow
	// requireTLS is the rule's own transport declaration. Immutable after
	// buildInjector, so it needs no lock.
	requireTLS bool
	// rule is the authored rule itself, kept for the fields that describe
	// WHICH requests may carry the credential (PinPath/PinQuery). Immutable
	// after buildInjector, so it needs no lock.
	rule      egress.InjectionRule
	expiresAt int64 // unix ms
	// retryAt is set while the last-good header is served after a transient
	// failure, or after a re-resolve whose answer was already stale
	// (install); until then the entry counts as fresh. Guarded by reMu.
	retryAt time.Time
}

// install writes a re-resolved credential onto e. An answer already inside
// injectRefreshMargin by this proxy's clock would make every request a
// re-resolve, each a mint and a secret.read row, so it is paced like an
// outage. The caller holds reMu.
func (e *injEntry) install(resolved types.ResolvedInjection, now time.Time) {
	e.header = injectedHeader{name: resolved.Header, value: resolved.Value}
	e.expiresAt = resolved.ExpiresAt
	e.retryAt = time.Time{}
	if e.expiresAt != 0 && !now.Before(time.UnixMilli(e.expiresAt).Add(-injectRefreshMargin)) {
		e.retryAt = now.Add(lastGoodRetry)
	}
}

// lastGood reports whether e may keep serving its header after a re-resolve
// failed with err: only when err is transient, the header was not already
// dropped, and e is within lastGoodGrace of its expiry. On true it paces the
// next attempt. The caller holds reMu.
func (e *injEntry) lastGood(err error, now time.Time) bool {
	if !transientResolveFailure(err) || e.header.value == "" || e.expiresAt == 0 ||
		!now.Before(time.UnixMilli(e.expiresAt).Add(lastGoodGrace)) {
		return false
	}
	e.retryAt = now.Add(lastGoodRetry)
	return true
}

// transientResolveFailure reports whether a re-resolve failed because nothing
// answered (the sink's 503, or no answer at all). Every other failure is a
// refusal.
func transientResolveFailure(err error) bool {
	var se injectionStatusError
	if errors.As(err, &se) {
		return se.status == http.StatusServiceUnavailable
	}
	var ue *url.Error
	return errors.As(err, &ue)
}

// buildInjector mints each injection rule's secret once and formats its
// header. A rule whose host does not pass the EXACT allowlist is rejected:
// injection must never widen egress nor leak a secret to a wildcard/approved
// host. Returns an error if any mint fails (fail closed at startup).
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
		// No hold at boot, deliberately: this runs under the proxy's 30s
		// startupCtx, seconds after dispatch refreshed the credential
		// synchronously, so a dead credential here is a race measured in
		// seconds, not a person who needs to sign in. A 423 at boot fails
		// closed like any other error, with a hint instead of opening a
		// request nothing will wait on.
		resolved, err := resolveInjectionQuery(ctx, base, token.Get(), r.GrantID, bootResolveQuery, client)
		if err != nil {
			return nil, fmt.Errorf("resolve injection for %q: %w", host, err)
		}
		// The control plane resolves header + FORMATTED value server-side; the
		// local rule is authoritative only for the host binding, gated above.
		inj.byHost[host] = &injEntry{
			grantID:    r.GrantID,
			header:     injectedHeader{name: resolved.Header, value: resolved.Value},
			requireTLS: r.RequireTLS,
			rule:       r.InjectionRule,
			expiresAt:  resolved.ExpiresAt,
		}

		// Register in the process-global Registry so it is masked from
		// decision-log output and every sandbox-facing error body — EVERY
		// rendering, not just the formatted header value.
		registerHeaderCredential(resolved.Value)
	}
	return inj, nil
}

// resolve returns the current injection header for host. For a static entry it
// returns the startup-minted value; for a dynamic (expiring) entry it re-resolves
// via the control plane when within injectRefreshMargin of expiry. The bool
// reports whether a rule EXISTS for the host; a non-nil error means a rule exists
// but its (dynamic) credential could not be refreshed and may not be served as
// last-good — the caller MUST fail closed rather than forward a stale credential.
func (i *injector) resolve(host string) (injectedHeader, bool, error) {
	return i.resolveCtx(context.Background(), host)
}

// resolveCtx is resolve with the CALLER's context, which the re-resolve's hold
// needs: the MITM request's ctx is what makes a disconnected SDK release reMu
// instead of pinning it for the whole re-auth budget. resolve() keeps the
// background ctx for callers with none to give.
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

	// reMu single-flights the ORDINARY re-resolve exactly as it always has.
	// What it no longer does is span a HOLD: that would make every later
	// caller queue on an uncancellable mutex for the whole re-auth budget, so
	// a hung-up SDK would never be released. The wait instead belongs to the
	// workflow, which owns its own goroutine and deadline; reMu is taken only
	// to read freshness, publish/drop the in-flight workflow, and install a
	// refreshed header — never across a network call that can block for minutes.
	for {
		e.reMu.Lock()
		now := time.Now()
		if e.expiresAt == 0 || now.Before(time.UnixMilli(e.expiresAt).Add(-injectRefreshMargin)) || now.Before(e.retryAt) {
			h := e.header
			e.reMu.Unlock()
			return h, true, nil // static, dynamic and still fresh, or riding out an outage
		}
		if wf := e.reauth; wf != nil {
			if wf.finished() {
				// The hold is over. Drop it and re-resolve: the owner may have
				// signed in, or the control plane may name a NEW request. The
				// coordinator still holds the old workflow by approval id, so
				// a 423 repeating that id gets its terminal result at once.
				e.reauth = nil
				e.reMu.Unlock()
				continue
			}
			// A hold is open: JOIN it rather than make a second control-plane
			// call for one lapse. reMu released first — the wait belongs to
			// this caller's own ctx.
			e.reMu.Unlock()
			resolved, err := wf.await(ctx)
			if err != nil {
				e.dropIfFinished(wf)
				return injectedHeader{}, true, fmt.Errorf("re-resolve injection for %q: %w", key, err)
			}
			return i.installHeader(e, wf, resolved), true, nil
		}

		resolved, err := resolveInjection(ctx, i.base, i.token.Get(), e.grantID, i.client)
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
		// Definitive, or the grace is spent: the header goes now, so nothing
		// later can serve it as last-good. A 423 is included — the control
		// plane just said this credential needs a person.
		e.header, e.retryAt = injectedHeader{}, time.Time{}
		var pending errReauthPending
		if !errors.As(err, &pending) {
			e.reMu.Unlock()
			// errReauthTimedOut travels out WRAPPED but intact: serveMITMRequest
			// tests errors.Is for it, and a wrapped sentinel still answers true.
			return injectedHeader{}, true, fmt.Errorf("re-resolve injection for %q: %w", key, err)
		}
		// 423: the control plane is asking for a human. Open the workflow (or
		// join the one this approval id already has), publish it on the entry,
		// and release reMu before waiting.
		if i.reauth == nil || i.approvals == nil {
			e.reMu.Unlock()
			// No hold lane on this injector: the 423 is a refusal the sandbox
			// still has to be told about, but nothing expired.
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
		// Wait here, not around the loop: looping back would re-read the
		// entry, see the ALREADY-terminal sticky workflow this approval id
		// just returned, drop it and re-resolve — round and round.
		held, herr := wf.await(ctx)
		if herr != nil {
			e.dropIfFinished(wf)
			return injectedHeader{}, true, fmt.Errorf("re-resolve injection for %q: %w", key, herr)
		}
		return i.installHeader(e, wf, held), true, nil
	}
}

// dropIfFinished takes a workflow off the entry, but ONLY once it is terminal.
//
// A caller that hangs up must leave a LIVE hold in place: dropping it would
// make the next retry call resolveInjection first (a spare mint and audit
// row) before joining the same workflow through the coordinator — breaking
// the lane's "two hits per lapse" property.
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

// installHeader writes a hold's resolved credential onto the entry and returns
// it. Separate so the wait above holds no lock while it waits and takes reMu
// only for the write.
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

// requiresTLS reports whether host has an injection rule that declares
// require_tls. Separate from resolve because it must answer WITHOUT minting or
// re-resolving anything: the plain lane asks it to decide whether to refuse
// the request, and a refusal must not touch the credential.
//
// False for an unknown host — a run with no injection for it is byte-for-byte
// unaffected.
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

// registerHeaderCredential registers, with the process-global mask registry,
// every rendering of ONE header credential the proxy holds.
//
// TRUST BOUNDARY (per-RENDERING, not per-credential): procRegistry is what
// stands between a proxy-held credential and every sandbox-facing error body
// and decision-log line. secretmask.Masker.Mask is EXACT BYTES, so
// registering only "Bearer sk-…" leaves a bare "sk-…" untouched — the form a
// vendor echoes back and an operator sees in a config.
//
// The renderings, in the order registered:
//
//	"Bearer sk-…"     the formatted header value, as the header carries it
//	"sk-…"            the credential alone — the scheme is a PREFIX
//	"user:pass"       when the tail decodes as base64 "user:pass" (Basic)
//	"pass"            … and its password half, the most sensitive part
//
// One definition, every proxy-side site (mirrors upstreamProxy.maskValues).
//
// Honest residual: masking catches only the renderings above, verbatim. A
// credential glued to a suffix by an arbitrary Format string, or hex-encoded
// or model-narrated, is not caught.
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

// registerBasicAuthCredential registers a credential the proxy puts on the wire
// with http.Request.SetBasicAuth(user, tok) — the git-broker installation token
// and the PAT lane's minted token.
//
// TRUST BOUNDARY: SetBasicAuth sends base64(user + ":" + tok), not tok.
// Registering only the raw token leaves the form actually on the wire (and in
// a transport error quoting the request) unmasked. Both go in, via
// registerHeaderCredential.
func registerBasicAuthCredential(user, tok string) {
	if tok == "" {
		return
	}
	procMask([]byte(tok))
	registerHeaderCredential("Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+tok)))
}

// stripSandboxCredentials removes EVERY credential header the sandbox may have
// put on a request that is about to be injected with an operator-brokered one.
//
// TRUST BOUNDARY (the single definition; do not re-spell it at a call site):
// setting the brokered header is not enough, because the sandbox chooses the
// OTHER headers — a rule injecting under `X-Api-Key` leaves an `Authorization:
// Bearer <sandbox key>` untouched, and which one the upstream honours becomes
// the UPSTREAM's choice, not Wardyn's. One list, all four injecting paths
// (forwardInspectedLLM, injector.apply, handleGitBroker, handleGitPATBroker),
// which previously each re-spelled a narrower Header.Del at the call site.
//
// The list is every header a vendor Wardyn brokers for reads as a credential,
// verified against each vendor's own documentation:
//   - Authorization / X-Api-Key / Api-Key / X-Auth-Token — the generic set.
//   - Private-Token — GitLab's REST API access-token header.
//   - X-Goog-Api-Key — Google's documented API-key header.
//   - X-Amz-Security-Token — the AWS SigV4 temporary-session-token header.
//   - X-Functions-Key — the Azure Functions access-key header.
//   - X-Access-Token, Anthropic-Api-Key — spellings observed on the brokered
//     lanes' own upstreams.
//   - Cookie — a session credential the upstream may prefer over the header we
//     inject; on an injecting path it is the sandbox's, never the operator's.
//
// Proxy-Authorization is deliberately absent: hop-by-hop, already removed by
// removeHopByHop on every one of these paths. owned is the header THIS rule
// supplies, stripped alongside the fixed list because the fixed list can't
// know it (e.g. the captured-AWS-SSO lane's x-amz-sso_bearer_token) — an
// INJECTED request overwrites it anyway, but a request the rule's pin
// withholds does not.
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

// injectableTransport reports whether a brokered credential may be attached to
// a forward request bound for scheme://host:port.
//
// TRUST BOUNDARY: injection keys on the lowercased hostname alone, and any
// allowlisted port matches, so the SANDBOX picks the transport —
// `POST http://<host>:443/…` on the plain lane would attach the operator's
// credential to a request sent in CLEARTEXT. The no-resident-secrets
// invariant still holds (the sandbox never sees the value), but the value
// would leave the proxy unencrypted. The clamp below is stated as a rule, not
// a single magic port:
//
//   - https: the proxy runs the TLS leg. Always injectable.
//   - cleartext to port 443 (or 8443/9443, tlsConventionalPorts): NEVER,
//     whatever the allowlist says — an AUTHORED port cannot re-admit it (the
//     port-scoping remedy docs/POLICIES.md recommends coexists with a bare
//     entry addAPIKeyGrant also appends, so AuthoredPortFor would otherwise
//     answer true for :443).
//   - cleartext to a host the proxy itself only ever speaks TLS to
//     (isLLMHost): NEVER — no plaintext connector exists to break.
//   - cleartext to port 80, for a host with a BARE allowlist entry:
//     injectable — the only shape plain-lane injection has ever meaningfully
//     worked in, and the default port a bare entry (silent about port)
//     implies (Policy.AllowedBareExactHost).
//   - cleartext to any OTHER port: only when the operator authored that port
//     (Policy.AuthoredPortFor) — a bare entry says nothing about a
//     sandbox-chosen non-default port, so the credential is withheld.
//
// Refusal HERE means NO INJECTION (the upstream answers 401), never a deny.
//
// The RESIDUAL — an https-only vendor this proxy has no table for, where
// plaintext port 80 looks like a legitimate internal connector — is closable
// via `require_tls` on the rule (egress.InjectionRule), which DENIES the
// request instead of silently withholding the credential, enforced a layer
// out in the plain lane before applyInjection. injectableTransport stays the
// unconditional floor under it.
func (p *Proxy) injectableTransport(scheme, host string, port int) bool {
	if strings.EqualFold(scheme, "https") {
		return true // the proxy itself runs the TLS leg
	}
	if tlsConventionalPorts[port] {
		return false // the clamp, unconditional: an authored :8443 does not re-admit it
	}
	if p.isLLMHost(host) {
		return false
	}
	// Port 80 asks the BARE question, since a bare entry is silent about the
	// port; every other port asks AuthoredPortFor, so a host authored only as
	// vendor.example:8443 is credentialed on that port and nowhere else.
	return (port == defaultPortForScheme("http") && p.policy.AllowedBareExactHost(host)) ||
		p.policy.AuthoredPortFor(host, port)
}

// tlsConventionalPorts is the set of ports the industry reads as "TLS lives
// here": 443 and the two alternates every appliance, registry and app server
// ships as its HTTPS port. Cleartext credential injection is refused to ALL of
// them regardless of authoring — 443 alone would leave the same leak one port
// over, since AuthoredPortFor reads a port-qualified entry as declared
// transport intent, and the sandbox still picks the scheme.
//
// A cleartext connector on any OTHER port is untouched, and a genuinely
// https-only vendor outside this set is served by `require_tls` on the rule,
// which refuses the request rather than silently withholding the credential.
var tlsConventionalPorts = map[int]bool{443: true, 8443: true, 9443: true}

// applyInjection is the plain forward lane's credential injection.
//
// The split is deliberate: the PROXY decides whether the TRANSPORT may
// carry a brokered credential (needs the run's policy and vendor table,
// neither of which the injector holds), and the INJECTOR decides whether a
// rule matches the host. A host with no rule is left byte-for-byte alone
// either way.
func (p *Proxy) applyInjection(req *http.Request, host string, port int) {
	if req.URL == nil || !p.injectableTransport(req.URL.Scheme, host, port) {
		return
	}
	p.inject.apply(req, host, port)
}

// apply strips the sandbox's own credential headers and sets the injected one
// if an exactly-allowed rule matches req's host. (Forward-proxy plain-HTTP
// path; a dynamic entry that fails to re-resolve simply isn't injected here —
// dynamic credentials target the TLS-MITM path, which fails closed via
// resolve.) Whether the TRANSPORT may carry the credential at all is decided
// by its ONE caller, Proxy.applyInjection/injectableTransport.
func (i *injector) apply(req *http.Request, host string, port int) {
	h, ok, err := i.resolve(host)
	if err != nil || !ok {
		return
	}
	// Strip always, inject only where the rule's PIN allows — the same two
	// decisions the MITM lane makes, made here too or the pin is bypassable
	// by simply not using TLS.
	stripSandboxCredentials(req.Header, h.name)
	if !i.allowsInjection(host, req.Method, req.URL.Path, req.URL.RawQuery) {
		return
	}
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
// token. This endpoint is the ONLY place the proxy obtains secret values;
// structurally unreachable from the sandbox. Any non-200 is a hard startup
// failure: fail closed rather than start a proxy that silently forwards
// uncredentialed requests.
func resolveInjection(ctx context.Context, base, token string, grantID uuid.UUID, client *http.Client) (types.ResolvedInjection, error) {
	return resolveInjectionQuery(ctx, base, token, grantID, nil, client)
}

// bootResolveQuery marks the sidecar's boot-time resolves. Mirrors the control
// plane's adoResolvePhase / adoResolvePhaseBoot.
var bootResolveQuery = url.Values{"phase": {"boot"}}

// resolveInjectionQuery is resolveInjection with a query — the Azure DevOps
// capability hold's per-(host, capability) ask (ado_hold.go). nil is the plain
// resolve, byte-identical on the wire.
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
		// Echo only a short slice of the upstream body: this error reaches the
		// SANDBOX on the MITM refresh-failure path, so it's an amplifier for
		// control-plane text — enough to diagnose a fail-closed startup, not a
		// 4 KiB relay (masked, see httpError). 1 KiB: the longest Azure DevOps
		// refusal plus its reason is past 256 bytes.
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		// 423 is not a refusal: the control plane is asking for a human. It
		// becomes a typed error the re-resolve path can HOLD on (credhold.go).
		// Checked BEFORE the generic status-error branch, and nowhere else —
		// every other status is byte-identical to before this existed.
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

// allowsInjection reports whether host's rule lets THIS request carry the
// credential. True for an unknown host and for every unpinned rule, so every
// lane but the captured-AWS-SSO one is unchanged.
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
// Strip and inject are two decisions, not one: a host with an injection rule
// always has the sandbox's own credential headers removed (including the
// header the rule supplies), because the rule says this host's credential is
// Wardyn's to provide. Whether one is then provided is the rule's pin — a
// request the pin doesn't cover is forwarded with NEITHER header, and the
// origin answers unauthenticated. Folding the two together (rather than
// falling through to "no rule" on a withheld injection) is what keeps the
// sandbox's own header from reaching the portal on requests the pin exists
// to narrow.
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
