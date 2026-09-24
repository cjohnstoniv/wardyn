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
// guarded by reMu, which also single-flights re-resolution (only one goroutine
// refreshes a given host at a time; others block on reMu and then read the fresh
// value). expiresAt == 0 marks a static credential that never re-resolves.
type injEntry struct {
	grantID uuid.UUID
	reMu    sync.Mutex
	header  injectedHeader
	// reauth is the re-auth workflow currently open for THIS entry, if any —
	// the single-flight that stops a second control-plane call for one lapse.
	// Guarded by reMu like header/expiresAt, and read only while it is held; the
	// WAIT on it happens with reMu released (see resolveCtx).
	reauth *reauthWorkflow
	// requireTLS is the rule's own transport declaration (egress.InjectionRule).
	// Immutable after buildInjector — it comes from the authored rule, never from
	// a re-resolve — so it needs no lock.
	requireTLS bool
	// rule is the authored rule itself, kept for the fields that describe WHICH
	// requests may carry the credential (PinPath/PinQuery). Immutable after
	// buildInjector for requireTLS's reason, so it needs no lock.
	rule      egress.InjectionRule
	expiresAt int64 // unix ms
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
		// No hold at boot, deliberately. This runs under the
		// proxy's 30s startupCtx, seconds after dispatch refreshed the credential
		// synchronously — a dead credential HERE is a race measured in seconds,
		// not a person who needs to sign in, and holding would fight the canary.
		// A 423 at boot is an error like any other: fail closed, exactly as today.
		// The resolve SAYS it is the boot one, so an arm that would otherwise
		// raise a sign-in request (the Azure DevOps lane) fails the run with a
		// hint instead of opening a request nothing will wait on.
		resolved, err := resolveInjectionQuery(ctx, base, token.Get(), r.GrantID, bootResolveQuery, client)
		if err != nil {
			return nil, fmt.Errorf("resolve injection for %q: %w", host, err)
		}
		// The control plane resolves header + FORMATTED value server-side
		// (it holds the secret store); the local rule is authoritative only
		// for the host binding, which the exact-allowlist check above gates.
		inj.byHost[host] = &injEntry{
			grantID:    r.GrantID,
			header:     injectedHeader{name: resolved.Header, value: resolved.Value},
			requireTLS: r.RequireTLS,
			rule:       r.InjectionRule,
			expiresAt:  resolved.ExpiresAt,
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
	return i.resolveCtx(context.Background(), host)
}

// resolveCtx is resolve with the CALLER's context, which the re-resolve's hold
// needs: the MITM request's ctx is what makes a disconnected SDK release reMu
// instead of pinning it for the whole re-auth budget. resolve() keeps the
// background ctx for the callers that have none to give (the plain lane's
// apply, headerFor), whose behaviour is unchanged.
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

	// reMu single-flights the ORDINARY re-resolve (the subscription OAuth token's
	// refresh) exactly as it always has: concurrent requests for one host make
	// ONE control-plane call and the rest read the refreshed value.
	//
	// What it no longer does is span a HOLD: holding it across a re-auth wait
	// would make every later caller queue on an uncancellable mutex for up to
	// the whole budget, so a hung-up SDK would never be released and, when the
	// budget ended, each queued caller in turn would open a NEW full-budget
	// workflow for the SAME lapse. The wait instead belongs to
	// the workflow, which owns its own goroutine and deadline; reMu is taken
	// only to read freshness, to publish or drop the in-flight workflow, and to
	// install a refreshed header — never across a network call that can block
	// for minutes.
	for {
		e.reMu.Lock()
		if e.expiresAt == 0 || time.Now().Before(time.UnixMilli(e.expiresAt).Add(-injectRefreshMargin)) {
			h := e.header
			e.reMu.Unlock()
			return h, true, nil // static, or dynamic and still fresh
		}
		if wf := e.reauth; wf != nil {
			if wf.finished() {
				// The hold is over. Drop it and re-resolve: the owner may have
				// signed in (200), or the control plane may name a NEW request.
				// The coordinator still holds the old workflow by approval id,
				// so a 423 repeating that id gets its terminal result at once
				// rather than a second hold.
				e.reauth = nil
				e.reMu.Unlock()
				continue
			}
			// A hold is open for this entry: JOIN it rather than make a second
			// control-plane call for one lapse. reMu is released first — the
			// wait is cancellable and belongs to this caller's own ctx.
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
			e.header = injectedHeader{name: resolved.Header, value: resolved.Value}
			e.expiresAt = resolved.ExpiresAt
			h := e.header
			e.reMu.Unlock()
			registerHeaderCredential(resolved.Value)
			return h, true, nil
		}
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
		// Wait here, not around the loop. Looping back would re-read the entry,
		// see a workflow that is ALREADY terminal (the sticky one this approval
		// id just returned), drop it and re-resolve — round and round until the
		// control plane's answer changed. A caller that has just been handed a
		// workflow takes ITS result, terminal or not.
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
// make the next retry call resolveInjection first — a broker mint and a
// credential.mint audit row — and only THEN join, through the coordinator,
// the very workflow it should join without asking. That is one spare mint per
// disconnect, breaking the lane's own "two hits per lapse" property.
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
	e.header = injectedHeader{name: resolved.Header, value: resolved.Value}
	e.expiresAt = resolved.ExpiresAt
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
// re-resolving anything: the plain lane asks it to decide whether to refuse the
// request, and a refusal must not touch the credential.
//
// False for an unknown host — no rule, nothing to require — so a host this run
// carries no injection for is byte-for-byte unaffected.
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
// Honest residual, narrowed but not closed: masking still catches only the
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
// (the brokered LLM route and the MITM path) has always stripped the first
// four; injector.apply — the plain forward lane's injection — only did
// Header.Set, and the two BROKER lanes (handleGitBroker, handleGitPATBroker)
// each re-spelled a narrower `Header.Del("Authorization")` at the call site, so
// the same request reached the forge carrying the brokered Basic auth AND the
// sandbox's own Private-Token/X-Api-Key/… One list, all four injecting paths.
//
// The list is every header a vendor Wardyn brokers for reads as a credential,
// verified against each vendor's own documentation rather than from memory:
//   - Authorization / X-Api-Key / Api-Key / X-Auth-Token — the generic set.
//   - Private-Token — GitLab's REST API personal/project/group access-token
//     header (docs.gitlab.com/api/rest/authentication), first-class on exactly
//     the forge kind the PAT lane exists for.
//   - X-Goog-Api-Key — Google's documented API-key header
//     (docs.cloud.google.com/docs/authentication/api-keys-use).
//   - X-Amz-Security-Token — the AWS SigV4 temporary-session-token header.
//   - X-Functions-Key — the Azure Functions access-key header
//     (learn.microsoft.com/azure/azure-functions/function-keys-how-to).
//   - X-Access-Token, Anthropic-Api-Key — credential spellings observed on the
//     brokered lanes' own upstreams.
//   - Cookie — a session credential the upstream may prefer over the header we
//     inject; on an injecting path it is the sandbox's, never the operator's.
//
// Proxy-Authorization is deliberately absent: it is hop-by-hop and already
// removed by removeHopByHop (proxy.go) on every one of these paths.
// owned is the header THIS rule supplies. It is stripped alongside the fixed
// list because the fixed list cannot know it: a rule may inject under any header
// (the captured-AWS-SSO lane uses x-amz-sso_bearer_token, which is on no generic
// credential list), and while an INJECTED request overwrites it anyway, a
// request whose injection the rule's pin withholds does not — so the sandbox's
// own value rode exactly the requests the pin exists to narrow.
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
// TRUST BOUNDARY (F110): injection keys on the lowercased hostname alone, and
// addAPIKeyGrant couples each grant to a BARE exact allowlist entry, which
// policy.go matches on ANY port. The SANDBOX therefore picks the transport:
// `POST http://<host>:443/…` on the plain lane made the proxy attach the
// operator's credential to a request it then sent in CLEARTEXT — visible to
// every on-path device and to the corporate proxy hop the deployment guide
// tells operators to put in front of egress. The no-resident-secrets invariant
// still holds (the sandbox never sees the value), but the value left the proxy
// unencrypted. Clamping the single port 443 closed one spelling of that and
// left 8443/9443/every other port open, so the clamp is stated as a rule now,
// not as a magic number:
//
//   - https: the proxy runs the TLS leg. Always injectable.
//   - cleartext to port 443: NEVER, whatever the allowlist says. This is the
//     unconditional clamp F110 named, kept unconditional on purpose: an
//     AUTHORED port cannot re-admit it. `allowed_domains: ["files.example.org:443"]`
//     is the port-scoping remedy docs/POLICIES.md recommends, and addAPIKeyGrant
//     (internal/api/llmcred.go) appends the BARE host beside whatever the
//     operator wrote — so the authored and bare entries coexist, the grant
//     resolves, and AuthoredPortFor answers true for :443. Reading that as
//     transport intent would put the credential in cleartext on the https port
//     for the very configuration the docs tell operators to write. There is no
//     plaintext connector on 443 to break.
//   - cleartext to a host the proxy itself only ever speaks TLS to (isLLMHost —
//     the vendor hosts and the operator's configured gateways, which
//     forwardInspectedLLM dials with a hardcoded https scheme): NEVER. There is
//     no plaintext connector behind those names to break.
//   - cleartext to port 80, for a host with a BARE allowlist entry: injectable.
//     That is the ONLY shape plain-lane injection has ever meaningfully worked
//     in (a CONNECT tunnel cannot be injected into) and the default port of the
//     plaintext connector an operator authors on purpose. "Bare" carries the
//     whole justification and is checked explicitly
//     (Policy.AllowedBareExactHost): the entry is SILENT about the port, so port
//     80 is the operator's default rather than the sandbox's choice.
//   - cleartext to any OTHER port: only when the operator authored that port in
//     the allowlist ("connector.internal:8080" rather than a bare
//     "connector.internal" — Policy.AuthoredPortFor). A bare entry is silent
//     about the port, so a sandbox-chosen non-default port over cleartext is
//     the sandbox choosing the transport, and the credential is withheld.
//
// Refusal HERE means NO INJECTION (the upstream answers 401), never a deny: the
// rules above are the proxy's own reading of a transport, and reading it as
// "withhold the credential" refuses nothing the operator authored.
//
// The RESIDUAL those rules left — an api_key for an https-only vendor this proxy
// has no table for, where `POST http://<that host>/…` on port 80 is
// indistinguishable from a legitimately plaintext internal connector — is now
// closable by the operator instead of hedged: `require_tls` on the rule
// (egress.InjectionRule) declares the transport intent this table cannot infer.
// That one is a DENY, not a silent withhold, and it is enforced a layer out
// where the response can be written — the plain lane's own arm, before
// applyInjection (internal/egress/proxy/plain_lane.go, rule_source
// policy:require-tls) — because an operator who says "TLS only" is refusing the
// REQUEST, not merely declining to credential it. injectableTransport stays the
// unconditional floor under it: a rule with require_tls unset is judged exactly
// as before.
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
	// Port 80 asks the BARE question. The arm's premise is an entry that
	// is silent about the port; B10-F1 made AllowedExactHost — the binding
	// question buildInjector asks — accept a port-QUALIFIED-only entry, which
	// silently turned "the operator said nothing about the port" into "the
	// operator named a DIFFERENT port". Every other port still asks
	// AuthoredPortFor, so a host authored only as vendor.example:8443 is
	// credentialed on the port its operator wrote down and nowhere else.
	return (port == defaultPortForScheme("http") && p.policy.AllowedBareExactHost(host)) ||
		p.policy.AuthoredPortFor(host, port)
}

// tlsConventionalPorts is the set of ports the industry reads as "TLS lives
// here": 443 and the two alternates every appliance, registry and app server
// ships as its HTTPS port. Cleartext credential injection is refused to ALL of
// them regardless of authoring.
//
// 443 alone was the F110 leak one port over: AuthoredPortFor deliberately reads
// a port-qualified entry as the operator declaring the transport, so
// `allowed_domains: ["vendor.example:8443"]` plus an api_key grant handed the
// operator's credential to `POST http://vendor.example:8443/…` IN CLEARTEXT —
// authored, and therefore trusted, on a port whose whole convention is TLS. The
// sandbox picks the scheme, so that is the sandbox choosing the transport.
//
// A cleartext connector on any OTHER port is untouched (port 80, or a port the
// operator authored), and the genuinely-https-only vendor on a port outside this
// set is served by `require_tls` on the rule, which refuses the request rather
// than silently withholding the credential. Refusal HERE is still a withhold,
// never a deny: the upstream answers 401.
var tlsConventionalPorts = map[int]bool{443: true, 8443: true, 9443: true}

// applyInjection is the plain forward lane's credential injection.
//
// The split is deliberate (F110): the PROXY decides whether the TRANSPORT may
// carry a brokered credential — that question needs the run's policy and the
// vendor table, neither of which the injector holds — and the INJECTOR decides
// whether a rule matches the host. A host with no rule is left byte-for-byte
// alone either way: the strip is part of injection, never a blanket header
// filter on ordinary forward egress.
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
// resolve.) Whether the TRANSPORT may carry the credential at all is decided by
// its ONE caller, Proxy.applyInjection/injectableTransport.
func (i *injector) apply(req *http.Request, host string, port int) {
	h, ok, err := i.resolve(host)
	if err != nil || !ok {
		return
	}
	// Strip always, inject only where the rule's PIN allows — the same two
	// decisions the MITM lane makes (forwardInspectedLLM), and they have to be
	// made here too or the pin is bypassable by simply not using TLS: the plain
	// lane reaches the very same portal host, and a `POST /logout` sent as an
	// ordinary absolute-URI request would have been injected while the tunnelled
	// one was not.
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
// token. This endpoint is the ONLY place the proxy obtains secret values; it
// is structurally unreachable from the sandbox (no brokered local route
// forwards it). Any non-200 (approval pending, missing secret, wrong kind) is
// a hard startup failure: we fail closed rather than start a proxy that
// silently forwards uncredentialed requests.
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
		// SANDBOX on the MITM refresh-failure path (serveMITMRequest), so it is an
		// amplifier for control-plane text. Enough to diagnose a fail-closed
		// startup, not a 4 KiB relay. (The mask still covers it — see httpError.)
		// 1 KiB, not less: the longest Azure DevOps refusal plus its reason is
		// past 256 bytes, and a cut body parses as no sentence at all.
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		// 423 is not a refusal: the control plane is asking for a human. It is
		// answered by exactly one resolve (the captured-AWS-SSO session, whose
		// owner has to sign in again) and becomes a typed error the re-resolve
		// path can HOLD on — see credhold.go. Checked BEFORE the generic
		// status-error branch, and nowhere else: every other status is
		// byte-identical to before this existed, including at boot.
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
// credential. True for an unknown host (no rule, nothing to narrow) and for
// every unpinned rule, so every lane but the captured-AWS-SSO one is unchanged.
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
// Strip and inject are two decisions, not one. A host with an injection rule
// always has the sandbox's own credential headers removed — including the header
// that rule supplies — because the rule says this host's credential is Wardyn's
// to provide. Whether one is then provided is the rule's pin: a
// request the pin does not cover is forwarded with NEITHER the sandbox's header
// nor Wardyn's, and the origin answers it unauthenticated.
//
// Folding the two together is what let a withheld injection fall through to the
// "no rule at all" branch, which PRESERVES the agent's own header — so the
// sandbox's placeholder, or anything else it chose to send, reached the portal on
// exactly the requests the pin exists to narrow.
//
// ownedHeader == "" means no rule governs this host, and then nothing is
// stripped: the agent's own resident credential is its own business.
func applyCredential(h http.Header, ownedHeader string, hdr *injectedHeader) {
	if ownedHeader == "" {
		return
	}
	stripSandboxCredentials(h, ownedHeader)
	if hdr != nil {
		h.Set(hdr.name, hdr.value)
	}
}
