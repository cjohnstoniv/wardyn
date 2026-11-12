// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// Run identity for a model gateway: on a provider's route or an organisation MCP
// server's, and only there, the proxy sends a short-lived token the control plane signed, naming the person,
// the run and the runner. The sandbox never holds it, and nothing the sandbox
// sends under these header names reaches a gateway on a run with a route. No
// route means none of this runs. The token lives in this process's memory: the
// config carries routes, never a token or a key.

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/contentscan"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	// gatewayIdentityHeader carries the token. Never Authorization, which holds
	// the person's own model credential on these routes.
	gatewayIdentityHeader = "X-Wardyn-Identity"
	// gatewayCallIDHeader carries the id both logs share. An MCP server gets this, and a token only on an organisation server with an audience.
	gatewayCallIDHeader = "X-Wardyn-Call-Id"
)

// gatewayCutSentence is what the sandbox reads once the control plane has cut
// the run's gateway access. It carries nothing the control plane sent.
const gatewayCutSentence = "This run's gateway access was cut when the portal that started it was revoked; the call was not sent. Start a new run if it is still needed."

// gatewayCutCause is the decision row's cause for a call refused by the cut.
const gatewayCutCause = "gateway access cut: the run's portal was revoked"

// errGatewayAccessCut is a mint refused with the control plane's permanent
// gateway_access_cut reason, the only reason a refusal body is read for. It
// makes the holder terminal.
var errGatewayAccessCut = errors.New("the control plane cut this run's gateway access")

// maxCutReasonBody bounds how much of a refusal is read for its reason.
const maxCutReasonBody = 1 << 10

// GatewayIdentityRoute is one route that carries the run's signed identity,
// fixed at dispatch. A request matches on host, exact port and path prefix.
type GatewayIdentityRoute struct {
	Host string `json:"host"`
	Port int    `json:"port"`
	// PathPrefix is the provider's base URL path, "" for the whole host and port.
	PathPrefix string `json:"path_prefix,omitempty"`
	// ProviderID names the model provider the route serves, or MCPKey the MCP server:
	// exactly one is set.
	ProviderID string `json:"provider_id,omitempty"`
	MCPKey     string `json:"mcp_key,omitempty"`
	// Audience is what this route expects in a token's aud. A token for any
	// other audience is refused here, whatever the control plane minted.
	Audience string `json:"audience"`
	// ProviderUID is the provider's UID at dispatch, or an MCP route's component id and
	// version. The proxy never reads it: it is here because this config is what is
	// sealed, and a mint compares it to the live row's, so a provider deleted and
	// recreated under the same id, or a component edited, fails closed.
	ProviderUID string `json:"provider_uid,omitempty"`
	// InspectionDeferred leaves the provider's prompts on this route to the gateway: the proxy
	// checks them for the run's own secret values only (deferredScan). A provider route only.
	InspectionDeferred bool `json:"inspection_deferred,omitempty"`
}

// holderKey is what names the route's token holder and its mint: the provider's id,
// or the MCP server's key under a prefix no provider id can have.
func (rt GatewayIdentityRoute) holderKey() string {
	if rt.MCPKey != "" {
		return "mcp\x00" + rt.MCPKey
	}
	return rt.ProviderID
}

// validate checks a route against the ones before it. A provider or an MCP server
// has one audience, and two destinations on one host and port need paths apart (Clashes).
func (rt GatewayIdentityRoute) validate(earlier []GatewayIdentityRoute) error {
	switch {
	case rt.Host == "" || strings.ContainsAny(rt.Host, "/ "):
		return fmt.Errorf("host %q is not a host name", rt.Host)
	case rt.Port < 1 || rt.Port > 65535:
		return fmt.Errorf("port %d is out of range", rt.Port)
	case (rt.ProviderID == "") == (rt.MCPKey == ""):
		return errors.New("exactly one of provider_id and mcp_key is required")
	case rt.Audience == "":
		return errors.New("audience is required")
	case rt.PathPrefix != "" && (!strings.HasPrefix(rt.PathPrefix, "/") || strings.ContainsAny(rt.PathPrefix, "?#\\")):
		return fmt.Errorf("path_prefix %q is not a path", rt.PathPrefix)
	case rt.InspectionDeferred && rt.ProviderID == "":
		return errors.New("inspection_deferred is for a model provider's route only")
	}
	for _, o := range earlier {
		if o.holderKey() == rt.holderKey() && o.Audience != rt.Audience {
			return fmt.Errorf("provider or MCP server %q has two audiences", cmp.Or(rt.ProviderID, rt.MCPKey))
		}
		if rt.Clashes(o) {
			return fmt.Errorf("%s:%d serves two destinations under one path", rt.Host, rt.Port)
		}
	}
	return nil
}

// Clashes reports whether rt and o are two destinations (providers or MCP servers) a
// request could match both of: one host and port, and a path under both prefixes. The
// proxy could not tell which token to send, so a run with both is refused.
func (rt GatewayIdentityRoute) Clashes(o GatewayIdentityRoute) bool {
	if hostKey(o.Host) != hostKey(rt.Host) || o.Port != rt.Port || o.holderKey() == rt.holderKey() {
		return false
	}
	a, b := strings.TrimSuffix(rt.PathPrefix, "/"), strings.TrimSuffix(o.PathPrefix, "/")
	return a == "" || b == "" || a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

// covers reports whether a request for path falls under the route's prefix. The
// prefix ends at a segment boundary, and a path with a dot segment covers
// nothing: the gateway may normalize it to somewhere outside the prefix.
func (rt GatewayIdentityRoute) covers(path string) bool {
	if strings.Contains(path, "\\") || slices.ContainsFunc(strings.Split(path, "/"), isDotSegment) {
		return false
	}
	prefix := strings.TrimSuffix(rt.PathPrefix, "/")
	return prefix == "" || path == prefix || strings.HasPrefix(path, prefix+"/")
}

// gatewayRouteFor is the route a request to host, port and path carries the
// token on, or nil. The upstream round trip never follows a redirect
// (roundTripUpstream), so a 3xx goes back to the sandbox and its next request is
// matched here afresh.
func (p *Proxy) gatewayRouteFor(host string, port int, path string) *GatewayIdentityRoute {
	for i := range p.gatewayRoutes {
		rt := &p.gatewayRoutes[i]
		if hostKey(rt.Host) == hostKey(host) && rt.Port == port && rt.covers(path) {
			return rt
		}
	}
	return nil
}

// deferredScan is what a route that leaves prompt inspection to its gateway still
// runs: eng checks the run's own secret values, exact match, and nothing else. A
// nil eng is a policy with no such check, so nothing runs there.
type deferredScan struct{ eng *contentscan.Engine }

// deferredScanFor builds the cut engine when a route leaves inspection to its
// gateway and the run inspects at all (full is the run's engine); nil otherwise,
// and nil defers nothing.
func deferredScanFor(full *contentscan.Engine, spec types.LLMInspectionSpec, corpus [][]byte, routes []GatewayIdentityRoute) (*deferredScan, error) {
	if full == nil || !slices.ContainsFunc(routes, func(rt GatewayIdentityRoute) bool { return rt.InspectionDeferred }) {
		return nil, nil
	}
	eng, err := contentscan.NewSecretsOnlyEngine(spec, corpus)
	if err != nil {
		return nil, err
	}
	return &deferredScan{eng: eng}, nil
}

// inspectionDeferredTo is the route a request to host, port and path carries the
// token on, when that route leaves prompt inspection to its gateway; nil means the
// run's whole policy applies. It reads the same match the token is attached by
// (gatewayRouteFor), so only a call that goes to that gateway is left to it. A proxy
// that never built the cut engine defers nothing: an unknown state is inspected.
func (p *Proxy) inspectionDeferredTo(host string, port int, path string) *GatewayIdentityRoute {
	if p.deferredScan == nil {
		return nil
	}
	rt := p.gatewayRouteFor(host, port, path)
	if rt == nil || !rt.InspectionDeferred || rt.ProviderID == "" {
		return nil
	}
	return rt
}

// stripGatewayHeaders deletes what the sandbox sent under the gateway's header
// names, on every lane of a run with a route. An opaque tunnel is out of reach;
// a forged token fails the gateway's signature check there. No route: nothing
// changes, and a sandbox's own header passes as any other does.
func (p *Proxy) stripGatewayHeaders(h http.Header) {
	if len(p.gatewayRoutes) == 0 {
		return
	}
	h.Del(gatewayIdentityHeader)
	h.Del(gatewayCallIDHeader)
}

// attachGatewayIdentity sets the route's token on an outbound request, or
// answers the sandbox with a refusal and returns false: a route configured to
// sign never sends unsigned, since a lenient gateway would take the call as
// anonymous. Accept-Encoding goes too, so an echo arrives in bytes the masker
// can read: Go's transport then negotiates gzip and decompresses by itself. Range
// and If-Range go first: a gateway that echoes headers and honours a range could
// otherwise split the token across responses, and masking matches exact bytes.
func (p *Proxy) attachGatewayIdentity(w http.ResponseWriter, r *http.Request, h http.Header, rt *GatewayIdentityRoute, host string, port int, ruleSource, callID string) bool {
	h.Del("Range")
	h.Del("If-Range")
	token, err := p.gatewayHolders[rt.holderKey()].current(r.Context(), p)
	if err != nil {
		if r.Context().Err() != nil {
			return false // the caller hung up during the mint and was never refused
		}
		if errors.Is(err, errGatewayAccessCut) {
			if p.sink != nil {
				log := decisionLog(p.reqOf(r, host, port), egress.Deny, ruleSource)
				log.GatewayCallID = callID
				log.Cause = gatewayCutCause
				p.sink.emit(log)
			}
			if shape := p.shapeOf(r, host); shape != shapePlain {
				p.writeBrokeredFault(w, shape, host, brokeredFault{port: port, ruleSource: ruleSource, reason: reasonGatewayAccessCut}, err)
			} else {
				w.Header().Set(egressHeaderWardynReason, reasonGatewayAccessCut)
				http.Error(w, gatewayCutSentence, http.StatusForbidden)
			}
			return false
		}
		if p.sink != nil {
			log := decisionLog(p.reqOf(r, host, port), egress.Deny, ruleSource)
			log.GatewayCallID = callID
			log.Cause = "gateway identity unavailable: " + p.dialFailureCause(err)
			p.sink.emit(log)
		}
		p.httpErrorShaped(w, p.shapeOf(r, host), host, "gateway identity unavailable, the call was not sent", err, http.StatusBadGateway,
			brokeredFault{port: port, ruleSource: ruleSource, reason: reasonCredentialUnavailable})
		return false
	}
	h.Set(gatewayIdentityHeader, token)
	h.Del("Accept-Encoding")
	rearmBodyDeadline(w, r) // the mint may have held the request
	return true
}

// relayGatewayRoute is the response tail of a route carrying the token: always
// scrubbed, whatever Config.ScrubResponses says, because a token echoed to the
// sandbox defeats the point of holding it here, and always flushed, so forcing
// the scrub leaves a stream's timing alone. It refuses bytes the masker cannot
// read rather than relay unmasked. Every end
// of the answer, clean or failed, is a cut: a trailing run of gatewayCutFloor bytes or
// more that begins a held value is replaced too, because a token missing its last
// characters can be recovered, and a short Content-Length ends cleanly with no read
// error to tell it from a whole answer.
func (p *Proxy) relayGatewayRoute(w http.ResponseWriter, r *http.Request, host string, port int, resp *http.Response, ruleSource, sentToken, callID string) {
	refuse := func(cause string) {
		if p.sink != nil {
			log := decisionLog(p.reqOf(r, host, port), egress.Deny, ruleSource)
			log.Cause = cause
			log.GatewayCallID = callID
			p.sink.emit(log)
		}
		p.httpErrorShaped(w, p.shapeOf(r, host), host, "the gateway's answer was not relayed", errors.New(cause), http.StatusBadGateway,
			brokeredFault{port: port, ruleSource: ruleSource, reason: reasonUpstreamError})
	}
	if enc := strings.Join(resp.Header.Values("Content-Encoding"), ","); enc != "" && !strings.EqualFold(enc, "identity") {
		refuse("the gateway answered with a Content-Encoding the proxy cannot scrub")
		return
	}
	// A call can wait longer than the holder keeps a value in the mask set, and no
	// timeout bounds how long, so this answer's masker holds the token the call sent
	// whatever a refresh has dropped by now or drops while the answer is read, and the
	// shared set does not keep it.
	values := slices.Clone(procRegistry.Masker(p.maskKey).Secrets())
	m := secretmask.NewMasker(append(values, gatewayMaskValues(sentToken)...))
	out := &scrubbedWriter{ResponseWriter: w, m: m, mw: secretmask.NewMaskingWriter(w, m), cutHeaders: true}
	defer func() { _ = out.mw.CloseCutFrom(gatewayCutFloor) }()
	flushRelay(out, resp)
}

// gatewaySignatureHeadLen is how much of a token's signature the mask set holds as a
// value of its own: 120 bits. An echo of the token cut short and followed by more bytes
// matches no whole token and is no end of an answer, but if it holds the signature from
// its first character through this far, these characters go and it is 120 bits short.
// Short of that, the claims and under this many signature characters pass. An echo that
// starts inside the head matches neither value and passes whole: a gateway slicing a
// token, like one transforming it, can show all of its signature across two answers.
const gatewaySignatureHeadLen = 20

// gatewayCutFloor is the shortest end of an answer or header value that counts as a held
// value's start. A JWT starts "e", so with no floor "true", "no-cache" and "Service
// Unavailable" would each lose a last letter, and a client reading x-should-retry would
// misread it. What passes is under eight bytes of a held value's start at the very end.
const gatewayCutFloor = 8

// gatewayMaskValues is what the mask set holds for one token, which check has accepted.
// The head lies inside the token, and overlapping matches join, so a whole token is
// still one placeholder.
func gatewayMaskValues(token string) [][]byte {
	signature := token[strings.LastIndexByte(token, '.')+1:]
	return [][]byte{[]byte(token), []byte(signature[:gatewaySignatureHeadLen])}
}

// maskValuesCut masks one header name's values on a route. A value's end that begins
// a held value is replaced as a body's is. If a held value crosses from one value to
// the next, every value of the name becomes the placeholder.
func (s *scrubbedWriter) maskValuesCut(vs []string) {
	each := make([]string, len(vs))
	for i, v := range vs {
		each[i] = s.maskValueCut(v)
	}
	if len(vs) > 1 && s.maskValueCut(strings.Join(vs, "")) != strings.Join(each, "") {
		for i := range each {
			each[i] = string(s.m.Mask(s.m.Secrets()[0]))
		}
	}
	copy(vs, each)
}

// maskValueCut masks one header value, ending as an answer on the route does.
func (s *scrubbedWriter) maskValueCut(v string) string {
	var out strings.Builder
	mw := secretmask.NewMaskingWriter(&out, s.m)
	_, _ = mw.Write([]byte(v))
	_ = mw.CloseCutFrom(gatewayCutFloor)
	return out.String()
}

// gatewayTokenHolder is one provider's or MCP server's current token. Requests share one mint
// at a time. From half-life on, the next request waits for a mint before it is forwarded, so a
// cut is seen by the first call after it; a mint that fails but may pass keeps the held token
// to exp, with no grace past that. A permanent cut ends the holder: it mints no more. The mask
// set holds the two most recent tokens, each with its signature head: a new one
// is registered before it is used, then the one before last is dropped, so the
// set does not grow for the run's life.
type gatewayTokenHolder struct {
	providerID, mcpKey, audience string
	// timeout bounds one mint. A request with no usable token waits at most this long.
	timeout time.Duration

	mu                      sync.Mutex
	token                   string
	expires                 time.Time
	refreshAt               time.Time
	nextAsk                 time.Time // paces mints during an outage, while a held token still serves
	lastMinted, priorMinted string    // the values in the mask set
	lastErr                 error
	flight                  chan struct{} // non-nil while a mint is out
	cut                     bool          // the control plane cut this run's gateway access; terminal
}

// current is the token to send, or why there is none. A held token serves
// until half-life; from then on the call waits for a refresh (the mint is
// bounded by h.timeout), so a refusal is seen before the call is forwarded. A
// refresh that fails but may pass leaves the held token serving to exp, and
// is asked again after the retry interval, or after half of what is left of
// the token's life when that is shorter (settleLocked).
func (h *gatewayTokenHolder) current(ctx context.Context, p *Proxy) (string, error) {
	h.mu.Lock()
	if h.cut {
		err := h.lastErr
		h.mu.Unlock()
		return "", err
	}
	now := p.now()
	held := h.token != "" && now.Before(h.expires)
	if held && (now.Before(h.refreshAt) || (h.flight == nil && now.Before(h.nextAsk))) {
		token := h.token
		h.mu.Unlock()
		return token, nil
	}
	// A refused or failed mint is asked again once per interval, not once per
	// request: a sandbox retrying must not turn into a mint, and an audit row, each time.
	if !held && h.flight == nil && h.lastErr != nil && now.Before(h.nextAsk) {
		err := h.lastErr
		h.mu.Unlock()
		return "", err
	}
	flight := h.startMintLocked(p)
	h.mu.Unlock()
	select {
	case <-flight:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cut {
		return "", h.lastErr
	}
	if h.token != "" && p.now().Before(h.expires) {
		return h.token, nil
	}
	return "", cmp.Or(h.lastErr, errors.New("the run's gateway identity expired as it arrived"))
}

// startMintLocked joins the mint that is out, or starts one. h.mu is held. The
// mint runs on its own context: a request hanging up must not cancel what the
// next request is waiting for.
func (h *gatewayTokenHolder) startMintLocked(p *Proxy) <-chan struct{} {
	if h.flight != nil {
		return h.flight
	}
	flight := make(chan struct{})
	h.flight = flight
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), h.timeout)
		defer cancel()
		token, exp, drop, err := h.mint(ctx, p)
		h.mu.Lock()
		defer h.mu.Unlock()
		h.settleLocked(p, token, exp, drop, err)
		h.flight = nil
		close(flight)
	}()
	return flight
}

// settleLocked applies a mint's answer. A failure that may pass keeps the held
// token to its exp; a refusal drops it at once. h.mu is held.
func (h *gatewayTokenHolder) settleLocked(p *Proxy, token string, exp time.Time, drop bool, err error) {
	now := p.now()
	if err != nil {
		slog.Warn("proxy: gateway identity mint failed", "provider", h.providerID, "mcp_server", h.mcpKey, "err", err)
		if drop {
			h.token = ""
		}
		h.cut = h.cut || errors.Is(err, errGatewayAccessCut)
		// A held token is asked again within half of what is left of its life, so a short-lived
		// token's next mint still lands before its exp; with none held, or one past its exp, the
		// usual interval, so an outage longer than a token's life is not asked once a second.
		wait := lastGoodRetry
		if left := h.expires.Sub(now); h.token != "" && left > 0 && left/2 < wait {
			wait = max(left/2, time.Second)
		}
		h.lastErr, h.nextAsk = err, now.Add(wait)
		return
	}
	for _, v := range gatewayMaskValues(token) {
		procMaskFor(p.maskKey, v)
	}
	if h.priorMinted != "" {
		for _, v := range gatewayMaskValues(h.priorMinted) {
			procRegistry.DropRunValue(p.maskKey, v)
		}
	}
	h.priorMinted, h.lastMinted = h.lastMinted, token
	h.token, h.expires, h.refreshAt, h.lastErr, h.nextAsk = token, exp, now.Add(exp.Sub(now)/2), nil, time.Time{}
}

// mint asks the control plane for a token. drop says the answer was definite,
// so a held token must go too.
func (h *gatewayTokenHolder) mint(ctx context.Context, p *Proxy) (token string, exp time.Time, drop bool, err error) {
	ask := map[string]string{"provider_id": h.providerID}
	if h.mcpKey != "" {
		ask = map[string]string{"mcp_server": h.mcpKey}
	}
	body, err := json.Marshal(ask)
	if err != nil {
		return "", time.Time{}, false, err
	}
	resp, err := p.forwardToControlPlane(ctx, http.MethodPost, "/api/v1/internal/gateway-identity", body, "application/json")
	if err != nil {
		return "", time.Time{}, false, fmt.Errorf("the control plane could not be reached for this run's gateway identity: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests:
		return "", time.Time{}, false, fmt.Errorf("the control plane answered %d for this run's gateway identity", resp.StatusCode)
	case resp.StatusCode == http.StatusForbidden && refusedAsCut(resp.Body):
		return "", time.Time{}, true, fmt.Errorf("the control plane refused this run's gateway identity (HTTP %d): %w", resp.StatusCode, errGatewayAccessCut)
	default:
		return "", time.Time{}, true, fmt.Errorf("the control plane refused this run's gateway identity (HTTP %d); if the provider was changed or turned off, start a new run", resp.StatusCode)
	}
	// A refused body is read only by refusedAsCut, and no error below quotes an
	// answer: what it carries must not reach a decision row, the sandbox or a log.
	var answer struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if resp.Header.Get("Content-Encoding") != "" || json.NewDecoder(io.LimitReader(resp.Body, maxBrokeredBody)).Decode(&answer) != nil {
		return "", time.Time{}, true, errors.New("the control plane's gateway identity answer could not be read")
	}
	exp, err = h.check(answer.Token, answer.ExpiresAt, p.now())
	if err != nil {
		return "", time.Time{}, true, err
	}
	return answer.Token, exp, false, nil
}

// refusedAsCut reports whether a refusal body names the one closed reason that
// ends the holder. It reads at most maxCutReasonBody bytes and keeps nothing
// else of them: any other body, or one that does not parse in that much, is not it.
func refusedAsCut(body io.Reader) bool {
	var refusal struct {
		Reason string `json:"reason"`
	}
	if json.NewDecoder(io.LimitReader(body, maxCutReasonBody)).Decode(&refusal) != nil {
		return false
	}
	return refusal.Reason == reasonGatewayAccessCut
}

// check reads a minted token's aud and exp, and refuses one for any audience but
// this route's: the second layer of audience binding, independent of the mint's.
// The signature is the gateway's to verify; the holder has no key. The token's life
// is judged against the control plane's expires_at, or its exp if that is earlier.
// A clock ahead of the control plane's drops the token early; one behind holds it
// no longer than ten minutes, and the gateway refuses what its own clock has passed:
// wrong either way, it fails closed. Ten minutes is the cap on a token's life (the
// provider may set a shorter one); half the life, the refresh point, is how long an
// outage is tolerated.
func (h *gatewayTokenHolder) check(token string, expiresAt, now time.Time) (time.Time, error) {
	const lifetime = 10 * time.Minute
	unusable := errors.New("the control plane's gateway identity token is malformed")
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || len(parts[2]) < gatewaySignatureHeadLen {
		return time.Time{}, unusable
	}
	for _, part := range []string{parts[0], parts[2]} {
		if _, err := base64.RawURLEncoding.DecodeString(part); err != nil {
			return time.Time{}, unusable
		}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, unusable
	}
	var claims struct {
		Aud json.RawMessage `json:"aud"`
		Exp float64         `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return time.Time{}, unusable
	}
	var one string
	var many []string
	if json.Unmarshal(claims.Aud, &one) != nil && json.Unmarshal(claims.Aud, &many) == nil && len(many) == 1 {
		one = many[0]
	}
	if one != h.audience {
		return time.Time{}, errors.New("the control plane's gateway identity token is for another audience than this run's route, so the call was not sent; start a new run")
	}
	exp := time.UnixMilli(int64(claims.Exp * 1000))
	if expiresAt.Before(exp) {
		exp = expiresAt
	}
	if !now.Before(exp) {
		return time.Time{}, errors.New("the control plane's gateway identity token has already expired by this proxy's clock")
	}
	if limit := now.Add(lifetime); exp.After(limit) {
		exp = limit
	}
	return exp, nil
}
