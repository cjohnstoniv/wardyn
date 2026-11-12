// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// Run identity for a model gateway: an admin turns it on per provider, a run's
// route and audience are sealed at dispatch, and the run's proxy asks here for a
// short-lived token naming the person, the run and the runner
// (internal/gatewayidentity signs it; internal/egress/proxy holds and sends it).
// With no provider on, the mint and the key set answer as paths no route holds.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/spiffe/go-spiffe/v2/spiffeid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/gatewayidentity"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	gatewayMintPath = "/api/v1/internal/gateway-identity"
	// gatewayMintUnheldPath is a path no route holds. An off request is routed
	// there, so its answer is the router's own, at every method and in every
	// console and base-path configuration.
	gatewayMintUnheldPath = "/api/v1/internal/gateway-identity-unheld"
	// gatewayOnWindow is how long one replica keeps its answer to "is any provider
	// on": the anonymous key set must not cost a query per request.
	gatewayOnWindow = 30 * time.Second
	// gatewayOnReadLimit bounds that read, which holds back the requests behind it.
	gatewayOnReadLimit = 5 * time.Second
	// gatewayFirstMintsKept bounds the first-mint memory. Past it the memory starts
	// again, which costs a second row for a run, never a missing one.
	gatewayFirstMintsKept = 4096
	// maxGatewayAudience bounds an audience, which is written into every token.
	maxGatewayAudience = 256

	gatewayChangedMsg = "This run's model provider was changed or turned off after the run started, so the run gets no new " +
		"gateway identity. Create a new run to use the provider as it is now."
	gatewayReviveMCPChangedMsg = "MCP server %q was changed or turned off after this run started, so nothing was revived: " +
		"the run's calls to it would be refused. Create a new run to use the server as it is now."
	gatewayReviveChangedMsg = "Model provider %q was changed or turned off after this run started, so nothing was revived: " +
		"the run's gateway calls would be refused. Create a new run to use the provider as it is now."
	gatewayUnavailableMsg    = "Wardyn couldn't check this run's gateway identity just now, so no token was issued"
	gatewayKeyUnavailableMsg = "The signing key is unavailable, so this wasn't saved."
	gatewayNotKeptMsg        = "This install keeps no run proxy configs, so a run's gateway route can't be checked and this wasn't saved."
	gatewayServiceMsg        = "The service identity that started this run is retired or suspended"
)

// gatewayIdentityState is one replica's memory of gateway identity: whether any
// provider is on, and which runs' first mints have an audit row.
type gatewayIdentityState struct {
	onMu   sync.Mutex
	onAt   time.Time
	on     bool
	onRead bool // a read has succeeded
	onErr  error

	firstMu sync.Mutex
	first   map[string]struct{}
}

// gatewayIdentityOn reports whether any enabled provider has identity on, from a
// read at most gatewayOnWindow old. It comes before the signer, which a replica
// keeps for its life: a key row left after the last provider turned off must not
// publish or sign.
//
// A failed read is an error, and is kept for the window too. The exception is an
// install last read as off, which stays off, so a database outage shows nothing
// new at these paths. Both cases fail closed.
func (s *Server) gatewayIdentityOn(ctx context.Context) (bool, error) {
	if s.cfg.Store == nil {
		return false, nil
	}
	c := &s.gatewayIdentity
	c.onMu.Lock()
	defer c.onMu.Unlock()
	now := s.cfg.Now()
	if age := now.Sub(c.onAt); !c.onAt.IsZero() && age >= 0 && age < gatewayOnWindow {
		return c.on, c.onErr
	}
	// Not the caller's cancel: one that hangs up must not become the answer the
	// next 30 seconds of callers read.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), gatewayOnReadLimit)
	defer cancel()
	sc, err := s.cfg.Store.GetSiteConfig(rctx)
	c.onAt = now
	if err != nil {
		slog.ErrorContext(ctx, "gateway identity: read the model providers", slog.Any("err", err))
		c.onErr = nil
		if !c.onRead || c.on {
			c.on, c.onErr = false, err
		}
		return c.on, c.onErr
	}
	c.on, c.onRead, c.onErr = slices.ContainsFunc(modelProviderRows(sc), gatewayProviderOn), true, nil
	return c.on, nil
}

// gatewayProviderOn reports whether p sends identity now.
func gatewayProviderOn(p types.ModelProvider) bool { return p.GatewayIdentity != nil && !p.Disabled }

// gatewayMintOffGate answers the mint path as a path no route holds while no
// provider has identity on, at every method: it runs before the route and
// before internalAuth, so an install with identity off shows nothing new and an
// anonymous request costs one cached read. On, or when that read failed, the
// request goes on to internalAuth and the handler, which refuses for itself.
func (s *Server) gatewayMintOffGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == gatewayMintPath && r.URL.RawPath == "" {
			if on, err := s.gatewayIdentityOn(r.Context()); !on && err == nil {
				off := r.Clone(r.Context())
				off.URL.Path = gatewayMintUnheldPath
				next.ServeHTTP(w, off)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// mountGatewayIdentityRoute registers the mint outside the mountInternalRoutes
// group, wrapped by internalAuth itself: gatewayMintOffGate has already answered
// for an install with identity off. Relayed requests are refused
// (relayedInternalRoutes false), and the sandbox has no brokered route to it.
func (s *Server) mountGatewayIdentityRoute(r chi.Router) {
	r.With(s.internalAuth).Post("/internal/gateway-identity", s.handleInternalGatewayIdentity)
}

// handleInternalGatewayIdentity mints a run's gateway token. internalAuth has
// refused a terminal, kept, queued or wrongly relayed run. The run is the token's
// claims.RunID and the body names the provider, or the MCP server, alone. What the token
// says comes from the run token and the live record, checked against what was sealed at
// dispatch (gatewayMintAdmit), never from the request.
//
//	POST /api/v1/internal/gateway-identity
func (s *Server) handleInternalGatewayIdentity(w http.ResponseWriter, r *http.Request) {
	claims, err := claimsFromContext(r)
	if err != nil {
		writeErrorReason(w, http.StatusUnauthorized, reasonMissingRunClaims, "missing run claims")
		return
	}
	var body struct {
		ProviderID string `json:"provider_id"`
		MCPServer  string `json:"mcp_server"`
	}
	if !decodeStrict(w, r, &body) {
		return
	}
	ctx := r.Context()
	target, kind := "", "provider"
	switch {
	case body.MCPServer != "":
		kind = "mcp_server"
		if gatewayMCPKeyPattern.MatchString(body.MCPServer) {
			target = body.MCPServer
		}
	case modelProviderIDPattern.MatchString(body.ProviderID):
		target = body.ProviderID
	}
	record := func(outcome string, data map[string]any) {
		ev := s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID, "gateway_identity.mint", target, outcome, mustJSON(data))
		ev.SourceIP = r.RemoteAddr
		s.recordAudit(ctx, ev)
	}
	refuse := func(ref *ownerRefusal) {
		record("denied", map[string]any{"reason": ref.reason, kind: target})
		writeErrorReason(w, ref.status, ref.reason, ref.msg)
	}
	run, live := liveRunFromContext(ctx)
	if !live {
		refuse(gatewayUnavailable())
		return
	}
	dest, sponsor, ref := s.gatewayMintAdmit(ctx, claims, run, body.ProviderID, body.MCPServer)
	if ref != nil && ref.reason == reasonGatewayAccessCut {
		s.refuseGatewayCut(w, run, kind, target, ref, record)
		return
	}
	if ref != nil {
		refuse(ref)
		return
	}
	token, signed, err := s.signGatewayToken(ctx, claims, run, dest, sponsor)
	if err != nil {
		slog.ErrorContext(ctx, "gateway identity: sign a token", slog.String("run_id", claims.RunID.String()), slog.Any("err", err))
		refuse(gatewayUnavailable())
		return
	}
	expires := time.Unix(signed.ExpiresAt, 0).UTC().Format(time.RFC3339)
	// One row for a run's first token per provider or MCP server on this replica, not one per mint:
	// a proxy mints several times an hour for as long as the run lasts.
	if s.gatewayFirstMint(claims.RunID.String(), dest.holder()) {
		record("success", map[string]any{kind: dest.name(), "jti": signed.ID, "expires_at": expires, "first": true})
	}
	writeJSON(w, http.StatusOK, tokenRenewResponse{Token: token, ExpiresAt: expires})
}

// signGatewayToken signs for an admitted run. The signer is read, never created:
// that is the first enable's job (refuseGatewayIdentityWrite).
func (s *Server) signGatewayToken(ctx context.Context, claims *identity.Claims, run types.AgentRun, dest gatewayDest, sponsor string) (string, gatewayidentity.Claims, error) {
	if s.cfg.GatewayIdentity == nil {
		return "", gatewayidentity.Claims{}, errors.New("no signer is configured")
	}
	signer, err := s.cfg.GatewayIdentity(ctx, false)
	if err != nil {
		return "", gatewayidentity.Claims{}, err
	}
	if signer == nil {
		return "", gatewayidentity.Claims{}, errors.New("no signing key is stored")
	}
	issuer, _, ok := strings.Cut(claims.SPIFFEID, "/agent-run/")
	if !ok {
		return "", gatewayidentity.Claims{}, errors.New("the run token names no agent run")
	}
	c := gatewayidentity.Claims{
		Issuer:   issuer,
		Audience: dest.audience,
		Subject:  claims.Sub,
		Run:      claims.RunID.String(),
		Runner:   cmp.Or(run.RunnerPoolExecutor, run.RunnerTarget),
		Provider: dest.provider,
		MCP:      dest.mcp,
		Sponsor:  sponsor,
	}
	// The portal and the agent it named come from the stored run, never the request, so a revived or
	// restarted run keeps them. On by default: it grants no reach, the agent is provenance only.
	if run.CreatedVia != nil {
		c.Delegate = run.CreatedVia.String()
		if a := run.CreatedViaActor; a != nil {
			c.NamePriorActor(a.Sub, a.Iss)
		}
	}
	return signer.Sign(c, dest.lifetime)
}

// gatewayFirstMint reports whether this replica has not yet recorded a first mint
// for the run and provider, and remembers it.
func (s *Server) gatewayFirstMint(runID, providerID string) bool {
	c := &s.gatewayIdentity
	c.firstMu.Lock()
	defer c.firstMu.Unlock()
	key := runID + "/" + providerID
	if _, seen := c.first[key]; seen {
		return false
	}
	if c.first == nil || len(c.first) >= gatewayFirstMintsKept {
		c.first = map[string]struct{}{}
	}
	c.first[key] = struct{}{}
	return true
}

func gatewayUnavailable() *ownerRefusal {
	return &ownerRefusal{status: http.StatusServiceUnavailable, reason: reasonGatewayIdentityUnavailable, msg: gatewayUnavailableMsg}
}

func gatewayChanged() *ownerRefusal {
	return &ownerRefusal{status: http.StatusForbidden, reason: reasonGatewayIdentityChanged, msg: gatewayChangedMsg}
}

// gatewayDest is what an admitted mint signs for: a model provider or an MCP server, with
// the audience and lifetime its live record sets.
type gatewayDest struct {
	audience string
	lifetime time.Duration
	// provider or mcp is set, never both.
	provider, mcp string
}

// holder names the destination for the first-mint memory; name is its id in an audit row.
func (d gatewayDest) holder() string {
	if d.mcp != "" {
		return "mcp\x00" + d.mcp
	}
	return d.provider
}

func (d gatewayDest) name() string { return cmp.Or(d.provider, d.mcp) }

// gatewayMintAdmit is every check a token depends on past internalAuth: the
// person, the portal that launched the run (gatewayMintVia), then the provider
// or MCP server against what was sealed at dispatch (exactly one of providerID
// and mcpKey is named). It returns the destination the live record sets and, on
// a service run, the sponsor the token names.
func (s *Server) gatewayMintAdmit(ctx context.Context, claims *identity.Claims, run types.AgentRun, providerID, mcpKey string) (gatewayDest, string, *ownerRefusal) {
	sponsor, ref := s.gatewayMintPerson(ctx, claims, run)
	if ref == nil {
		ref = s.gatewayMintVia(ctx, run)
	}
	if ref != nil {
		return gatewayDest{}, "", ref
	}
	routes, err := s.sealedGatewayRoutes(ctx, run.ID)
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, errRunProxyConfigNotKept):
		return gatewayDest{}, "", gatewayChanged()
	case err != nil:
		slog.ErrorContext(ctx, "gateway identity: read the sealed route", slog.String("run_id", run.ID.String()), slog.Any("err", err))
		return gatewayDest{}, "", gatewayUnavailable()
	}
	if (providerID == "") == (mcpKey == "") {
		return gatewayDest{}, "", gatewayChanged()
	}
	i := slices.IndexFunc(routes, func(rt proxy.GatewayIdentityRoute) bool {
		return rt.ProviderID == providerID && rt.MCPKey == mcpKey
	})
	if i < 0 && mcpKey != "" {
		return gatewayDest{}, "", gatewayMCPChanged()
	}
	if i < 0 {
		return gatewayDest{}, "", gatewayChanged()
	}
	if mcpKey != "" {
		return s.gatewayMintAdmitMCP(ctx, run, routes[i], sponsor)
	}
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "gateway identity: read the provider", slog.String("run_id", run.ID.String()), slog.Any("err", err))
		return gatewayDest{}, "", gatewayUnavailable()
	}
	pairs, err := s.runHarnessPairs(ctx, run)
	if err != nil {
		slog.ErrorContext(ctx, "gateway identity: read the run's harnesses", slog.String("run_id", run.ID.String()), slog.Any("err", err))
		return gatewayDest{}, "", gatewayUnavailable()
	}
	p, found := modelProviderByID(sc.ModelProviders, providerID)
	served := slices.ContainsFunc(pairs, func(h types.RunHarness) bool {
		return h.ModelProviderID == providerID && p.Serves(h.Harness)
	})
	if !found || !served || !gatewayBindingHolds(routes[i], p) {
		return gatewayDest{}, "", gatewayChanged()
	}
	return gatewayDest{
		audience: p.GatewayIdentity.Audience, provider: p.ID,
		lifetime: time.Duration(p.GatewayIdentity.TokenLifetimeSeconds) * time.Second,
	}, sponsor, nil
}

// gatewayMintPerson refuses a suspended person independently of the kill their
// suspension runs: step one of a suspension commits the identity row and the
// cutoff before its kill step, and the kill can fail and be retried, so a run
// still alive must not go on minting as that person.
//
// Each check is the one statement an API token's own authentication makes, once
// per half-life per provider. IdentityBlocked reads the identity rows bound to the
// principal. CheckSession reads the cutoff a suspension writes for a principal no
// row binds, a person known only by an email, with epoch -1, so a routine
// sign-out's session cut does not count. A service run asks both of its sponsor
// too. A store that cannot answer fails closed.
func (s *Server) gatewayMintPerson(ctx context.Context, claims *identity.Claims, run types.AgentRun) (sponsor string, _ *ownerRefusal) {
	if isServicePrincipal(claims.Sub) {
		si, err := s.serviceRecord(ctx, strings.TrimPrefix(claims.Sub, types.ServicePrincipalPrefix))
		switch {
		case errors.Is(err, store.ErrNotFound):
			return "", &ownerRefusal{status: http.StatusForbidden, reason: reasonServiceIdentityInactive, msg: gatewayServiceMsg}
		case err != nil:
			slog.ErrorContext(ctx, "gateway identity: read the service identity", slog.Any("err", err))
			return "", gatewayUnavailable()
		case !si.Usable():
			return "", &ownerRefusal{status: http.StatusForbidden, reason: reasonServiceIdentityInactive, msg: gatewayServiceMsg}
		}
		sponsor = si.Sponsor
	}
	ib, ok := s.cfg.Store.(identityBlocker)
	if !ok {
		return "", gatewayUnavailable()
	}
	for _, who := range nonEmptyForms(claims.Sub, sponsor) {
		blocked, err := ib.IdentityBlocked(ctx, who, -1)
		live := err == nil && !blocked
		if live && s.cfg.SessionRevocations != nil {
			var status oidc.SessionStatus
			status, err = oidc.CheckSession(ctx, s.cfg.SessionRevocations, who, "", oidc.Issue{At: run.CreatedAt}, -1)
			live = err == nil && status == oidc.SessionLive
		}
		switch {
		case err != nil:
			slog.ErrorContext(ctx, "gateway identity: read the person's status", slog.Any("err", err))
			return "", gatewayUnavailable()
		case !live:
			return "", &ownerRefusal{status: http.StatusForbidden, reason: reasonIdentityDeactivated, msg: identityDeactivatedMsg}
		}
	}
	return sponsor, nil
}

// sealedGatewayRoutes is the routes a run was dispatched with, from its sealed
// config: never from the request or the run token.
func (s *Server) sealedGatewayRoutes(ctx context.Context, runID uuid.UUID) ([]proxy.GatewayIdentityRoute, error) {
	raw, err := s.loadRunProxyConfig(ctx, runID)
	if err != nil {
		return nil, err
	}
	var sealed struct {
		GatewayIdentity []proxy.GatewayIdentityRoute `json:"gateway_identity"`
	}
	if err := json.Unmarshal(raw, &sealed); err != nil {
		return nil, fmt.Errorf("read the sealed gateway routes: %w", err)
	}
	return sealed.GatewayIdentity, nil
}

// inspectionLeftTo names each provider whose sealed route leaves prompt inspection
// to its gateway: the run's coverage line. The name is the provider's now, or its id
// once it is gone. A run with no sealed config, or one this deployment does not keep,
// names none; the line then reads as the policy says.
func (s *Server) inspectionLeftTo(ctx context.Context, runID uuid.UUID, sc types.SiteConfig) []string {
	routes, err := s.sealedGatewayRoutes(ctx, runID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) && !errors.Is(err, errRunProxyConfigNotKept) {
			slog.WarnContext(ctx, "api: run policy view without the sealed gateway routes", "run_id", runID, "err", err)
		}
		return nil
	}
	var out []string
	for _, rt := range routes {
		if !rt.InspectionDeferred || rt.ProviderID == "" {
			continue
		}
		name := rt.ProviderID
		if p, ok := modelProviderByID(sc.ModelProviders, rt.ProviderID); ok && p.Name != "" {
			name = p.Name
		}
		out = append(out, name)
	}
	return out
}

// gatewayDestination is where a provider's calls go, in the form two spellings of
// one address compare equal in.
type gatewayDestination struct {
	host string
	port int
	path string
}

// gatewayDestinationOf normalizes a provider's base URL. ok is false for an address
// the proxy's route could not hold, or that its path match could never cover,
// which would send every call unsigned.
func gatewayDestinationOf(baseURL string) (_ gatewayDestination, ok bool) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Hostname() == "" {
		return gatewayDestination{}, false
	}
	d := gatewayDestination{host: strings.TrimSuffix(strings.ToLower(u.Hostname()), "."), port: 443, path: strings.TrimSuffix(u.Path, "/")}
	if raw := u.Port(); raw != "" {
		if d.port, err = strconv.Atoi(raw); err != nil || d.port < 1 || d.port > 65535 {
			return gatewayDestination{}, false
		}
	}
	if strings.ContainsAny(d.path, "?#\\") || slices.ContainsFunc(strings.Split(d.path, "/"), func(seg string) bool {
		before, _, _ := strings.Cut(seg, ";")
		return before == "." || before == ".."
	}) {
		return gatewayDestination{}, false
	}
	return d, true
}

// A Bedrock row names its gateway in bedrock.base_url. A sign-in row has rules of its own besides
// (internal_gateway_identity_bedrock_sso.go).
const (
	mp400GatewayBedrockBaseURL = "model_providers: %q: gateway_identity needs a bedrock.base_url, or the token would go to AWS"
	mp400GatewayBedrockAddress = "model_providers: %q: gateway_identity needs a bedrock.base_url with a valid port and no dot segment in its path"
)

// gatewayAddressOf is the address p sends its calls to when it has a gateway: a
// Bedrock row's bedrock.base_url, any other row's base_url.
func gatewayAddressOf(p types.ModelProvider) string {
	if p.Kind.IsBedrock() {
		return providerBedrockSettings(p).BaseURL
	}
	return p.BaseURL
}

// gatewayRouteOf is the route p would be dispatched with now. ok is false when p
// has identity off or an address no route can hold.
func gatewayRouteOf(p types.ModelProvider) (_ proxy.GatewayIdentityRoute, ok bool) {
	if p.GatewayIdentity == nil {
		return proxy.GatewayIdentityRoute{}, false
	}
	d, ok := gatewayDestinationOf(gatewayAddressOf(p))
	if !ok {
		return proxy.GatewayIdentityRoute{}, false
	}
	return proxy.GatewayIdentityRoute{
		Host: d.host, Port: d.port, PathPrefix: d.path,
		ProviderID: p.ID, ProviderUID: p.UID, Audience: p.GatewayIdentity.Audience,
		// Only the exact value defers; anything else leaves Wardyn inspecting. A Bedrock
		// call is scanned by the generic forward check, which a route never defers.
		InspectionDeferred: p.GatewayIdentity.Inspection == types.GatewayInspectionGateway && !p.Kind.IsBedrock(),
	}, true
}

// gatewayBindingHolds is the one predicate a mint and a revive share: the live
// provider is on, is the provider that was dispatched (the UID, so one deleted and
// recreated under the same id fails, as a provider key's UID check does), and would
// be dispatched today with the sealed route. So a provider edited and edited back
// passes, and neither a base_url nor an audience edit can send a token for one
// gateway to another: the proxy's route is fixed for the run's life.
func gatewayBindingHolds(sealed proxy.GatewayIdentityRoute, p types.ModelProvider) bool {
	live, ok := gatewayRouteOf(p)
	return ok && !p.Disabled && sealed.ProviderUID != "" && live == sealed
}

// gatewayIdentityRefusal is a revive's check of the same binding: a run that would
// refuse every gateway call is not revived, and the refusal says why and what to do
// instead. A run with no route reads nothing.
func (s *Server) gatewayIdentityRefusal(ctx context.Context, cfg *proxy.Config) (*ownerRefusal, error) {
	if len(cfg.GatewayIdentity) == 0 {
		return nil, nil
	}
	if ref, err := s.gatewayReviveCut(ctx, cfg); ref != nil || err != nil {
		return ref, err
	}
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the model providers: %w", err)
	}
	for _, rt := range cfg.GatewayIdentity {
		if rt.MCPKey != "" {
			held, err := s.gatewayMCPBindingHolds(ctx, cfg.RunID, rt)
			if err != nil {
				return nil, fmt.Errorf("read the run's MCP server: %w", err)
			}
			if !held {
				return &ownerRefusal{status: http.StatusConflict, reason: reasonGatewayIdentityChanged, msg: fmt.Sprintf(gatewayReviveMCPChangedMsg, rt.MCPKey)}, nil
			}
			continue
		}
		p, found := modelProviderByID(sc.ModelProviders, rt.ProviderID)
		if !found || !gatewayBindingHolds(rt, p) {
			return &ownerRefusal{status: http.StatusConflict, reason: reasonGatewayIdentityChanged, msg: fmt.Sprintf(gatewayReviveChangedMsg, rt.ProviderID)}, nil
		}
	}
	return nil, nil
}

// gatewayRouteClash is the id of a provider already on the run whose route a call
// to l's could also match, or "" (proxy.GatewayIdentityRoute.Clashes).
func (d *providerDispatch) gatewayRouteClash(p types.ModelProvider) string {
	rt, ok := gatewayRouteOf(p)
	if !ok {
		return ""
	}
	for _, o := range d.gatewayRoutes {
		if rt.Clashes(o) {
			return o.ProviderID
		}
	}
	return ""
}

// addGatewayRoute records a provider's route. A provider two harnesses share has one.
// ok is false when identity is on and no route can be written: such a run is
// refused, because it would otherwise call the gateway unsigned.
func (d *providerDispatch) addGatewayRoute(p types.ModelProvider) (ok bool) {
	if p.GatewayIdentity == nil {
		return true
	}
	rt, ok := gatewayRouteOf(p)
	if !ok {
		return false
	}
	if !slices.Contains(d.gatewayRoutes, rt) {
		d.gatewayRoutes = append(d.gatewayRoutes, rt)
	}
	return true
}

// takeProviderGatewayRoute records the gateway route of a lane's provider on prov, for the kinds that may
// carry one: the key and endpoint kinds, a Bedrock key row, whose call reaches forwardInspectedLLM through
// the bearer's MITM host, and a Bedrock sign-in row, whose calls the proxy re-signs for the gateway (llm is
// the lane's transport). ok is false when the run was refused and is already FAILED: the route clashes with
// one already on the run, its stored address can hold none, so the call would otherwise go unsigned, or a
// sign-in lane is not re-signed, so the call would carry no identity.
func (s *Server) takeProviderGatewayRoute(ctx context.Context, run types.AgentRun, prov *providerDispatch, p types.ModelProvider, llm llmTransport) (ok bool) {
	sso := p.Kind == types.ModelProviderBedrockSSO
	if !providerKeyKind(p.Kind) && p.Kind != types.ModelProviderBedrockBearer && !sso {
		return true
	}
	if other := prov.gatewayRouteClash(p); other != "" {
		s.refuseProviderDispatch(ctx, run, p.Kind, providerDenial{msg: fmt.Sprintf(mpRunGatewayClash, p.ID, other)}, nil)
		return false
	}
	if !prov.addGatewayRoute(p) {
		s.refuseProviderDispatch(ctx, run, p.Kind, providerDenial{msg: fmt.Sprintf(mpRunGatewayAddress, p.ID)}, nil)
		return false
	}
	if why := llm.bedrock.sigv4Why; sso && p.GatewayIdentity != nil && (why.Code != "" || llm.bedrock.sigv4Gateway == nil) {
		s.refuseProviderDispatch(ctx, run, p.Kind, providerDenial{msg: fmt.Sprintf(mpRunGatewaySigV4, p.ID, why.String())},
			map[string]any{"aws_signing_reason": why.Code})
		return false
	}
	return true
}

// validateProviderGatewayIdentity holds one row's opt-in to the kinds that may carry
// a token: a key row with a base_url, a Bedrock row with a bedrock.base_url (a sign-in
// row also needs a gateway on a name AWS does not own and a region: validateBedrockSSOGateway),
// or an endpoint row. Every other kind, a subscription row included, is refused: it has
// no gateway of an admin's to name.
func validateProviderGatewayIdentity(mp types.ModelProvider) error {
	gi := mp.GatewayIdentity
	if gi == nil {
		return nil
	}
	field, noAddress, badAddress := "base_url", mp400GatewayBaseURL, mp400GatewayAddress
	switch mp.Kind {
	case types.ModelProviderAnthropicAPIKey, types.ModelProviderOpenAIAPIKey, types.ModelProviderCustomEndpoint:
	case types.ModelProviderBedrockBearer, types.ModelProviderBedrockSSO:
		field, noAddress, badAddress = "bedrock.base_url", mp400GatewayBedrockBaseURL, mp400GatewayBedrockAddress
	default:
		return fmt.Errorf(mp400GatewayKind, mp.ID, string(mp.Kind))
	}
	addr := gatewayAddressOf(mp)
	if addr == "" {
		return atField(field, fmt.Errorf(noAddress, mp.ID))
	}
	d, ok := gatewayDestinationOf(addr)
	if !ok {
		return atField(field, fmt.Errorf(badAddress, mp.ID))
	}
	if mp.Kind == types.ModelProviderBedrockSSO {
		if err := validateBedrockSSOGateway(mp, d); err != nil {
			return err
		}
	}
	if gi.Audience == "" {
		return atField("gateway_identity.audience", fmt.Errorf(mp400GatewayAudience, mp.ID))
	}
	if len(gi.Audience) > maxGatewayAudience || strings.ContainsFunc(gi.Audience, unicode.IsControl) {
		return atField("gateway_identity.audience", fmt.Errorf(mp400GatewayAudienceForm, mp.ID, maxGatewayAudience))
	}
	if n := gi.TokenLifetimeSeconds; n != 0 && (n < types.GatewayTokenLifetimeMin || n > types.GatewayTokenLifetimeMax) {
		return atField("gateway_identity.token_lifetime_seconds",
			fmt.Errorf(mp400GatewayLifetime, mp.ID, types.GatewayTokenLifetimeMin, types.GatewayTokenLifetimeMax))
	}
	switch {
	case gi.Inspection != "" && gi.Inspection != types.GatewayInspectionGateway:
		return atField("gateway_identity.inspection", fmt.Errorf(mp400GatewayInspection, mp.ID, types.GatewayInspectionGateway))
	case gi.Inspection != "" && mp.Kind.IsBedrock():
		return atField("gateway_identity.inspection", fmt.Errorf(mp400GatewayInspectionBR, mp.ID))
	}
	return nil
}

// validateGatewayAudiences refuses one audience on two destinations: a token for
// it would be good at both, so one gateway could replay it at the other, and a
// model route's token could be replayed on an MCP route or the reverse. Rows the
// same destination serves may share one. Every row carrying the field counts,
// turned off or not, since turning one on is no edit of the other. mcp is the
// organisation's remote MCP servers with an audience (gatewayMCPAudienceUses).
func validateGatewayAudiences(p *types.ModelProviders, mcp []types.Component) error {
	type holder struct {
		id    string // the provider's id, for a provider
		label string
		dest  gatewayDestination
	}
	seen := map[string]holder{}
	var providers []types.ModelProvider
	if p != nil {
		providers = p.Providers
	}
	for _, mp := range providers {
		if mp.GatewayIdentity == nil {
			continue
		}
		dest, _ := gatewayDestinationOf(gatewayAddressOf(mp)) // validateProviderGatewayIdentity has passed it
		first, ok := seen[mp.GatewayIdentity.Audience]
		if ok && first.dest != dest {
			return atField("gateway_identity.audience", fmt.Errorf(mp400GatewayAudienceDup, mp.ID, mp.GatewayIdentity.Audience, first.id))
		}
		if !ok {
			seen[mp.GatewayIdentity.Audience] = holder{id: mp.ID, label: "provider " + strconv.Quote(mp.ID), dest: dest}
		}
	}
	for _, c := range mcp {
		aud := c.Definition.MCP.GatewayAudience
		dest, _ := gatewayDestinationOf(c.Definition.MCP.URL) // the component save has passed it
		label := "MCP server " + strconv.Quote(c.Name)
		first, ok := seen[aud]
		if ok && first.dest != dest {
			return atField("mcp.gateway_audience", fmt.Errorf(gatewayMCPAudienceDup, aud, label, first.label))
		}
		if !ok {
			seen[aud] = holder{label: label, dest: dest}
		}
	}
	return nil
}

// validateGatewayIdentityEdit refuses a save that re-points a provider with identity
// on and keeps its audience: the audience names one gateway, so a new address needs
// its own. A retyped value can't be told from a kept one, so the same audience is
// refused however it arrived; saving with identity off first is the way to keep it.
// It needs the stored block, so it is not in validateModelProviders.
func validateGatewayIdentityEdit(block, stored *types.ModelProviders) error {
	if block == nil {
		return nil
	}
	for _, p := range block.Providers {
		prior, ok := modelProviderByID(stored, p.ID)
		if !ok || p.GatewayIdentity == nil || prior.GatewayIdentity == nil ||
			prior.GatewayIdentity.Audience != p.GatewayIdentity.Audience {
			continue
		}
		before, _ := gatewayDestinationOf(gatewayAddressOf(prior))
		after, _ := gatewayDestinationOf(gatewayAddressOf(p))
		if before != after {
			return atField("gateway_identity.audience", fmt.Errorf(mp400GatewayAudienceMoved, p.ID))
		}
	}
	return nil
}

// introducedGatewayIdentity reports whether block turns identity on for a provider
// that was not sending it. Only that makes or reads the key, the reason
// introducedSubscription gives: a stored provider and a damaged key row must not
// wedge every later save of this document, the one that turns identity off included.
func introducedGatewayIdentity(block, stored *types.ModelProviders) bool {
	if block == nil {
		return false
	}
	return slices.ContainsFunc(block.Providers, func(p types.ModelProvider) bool {
		prior, ok := modelProviderByID(stored, p.ID)
		return gatewayProviderOn(p) && (!ok || !gatewayProviderOn(prior))
	})
}

// refuseGatewayIdentityWrite is a save door's stored-block half of the identity
// rules, and makes the signing key at the first save that turns identity on, so
// the key set can serve it before any run starts and an install with identity off
// has no row. A failed create refuses the save. It answers for itself and returns
// true when it refused; prefix is the door's own sentence opening.
func (s *Server) refuseGatewayIdentityWrite(w http.ResponseWriter, r *http.Request, block, stored *types.ModelProviders, prefix string) bool {
	if err := validateGatewayIdentityEdit(block, stored); err != nil {
		writeErrorReasonField(w, http.StatusBadRequest, reasonSiteConfigInvalid, prefix+err.Error(), refusalField(err))
		return true
	}
	if s.refuseGatewayAudienceWrite(w, r, block, prefix) {
		return true
	}
	if err := s.bedrockSSOGatewayWriteRefusal(block, stored); err != nil {
		writeErrorReason(w, http.StatusBadRequest, reasonSiteConfigInvalid, prefix+err.Error())
		return true
	}
	if !introducedGatewayIdentity(block, stored) {
		return false
	}
	if _, kept := s.runProxyConfigStore(); !kept {
		writeErrorReason(w, http.StatusServiceUnavailable, reasonGatewayIdentityUnavailable, gatewayNotKeptMsg)
		return true
	}
	if s.cfg.GatewayIdentity != nil {
		signer, err := s.cfg.GatewayIdentity(r.Context(), true)
		if err == nil && signer != nil {
			return false
		}
		slog.ErrorContext(r.Context(), "gateway identity: make the signing key", slog.Any("err", err))
	}
	writeErrorReason(w, http.StatusServiceUnavailable, reasonGatewayIdentityUnavailable, gatewayKeyUnavailableMsg)
	return true
}

// gatewayIdentityIssuer is every token's iss, as the run token spells it: the
// trust domain's SPIFFE address, not the bare name /healthz reports. It is what the
// console shows and a gateway must be told. "" when the name cannot be read.
func (s *Server) gatewayIdentityIssuer() string {
	td, err := spiffeid.TrustDomainFromString(s.cfg.TrustDomain)
	if err != nil {
		return ""
	}
	return td.IDString()
}
