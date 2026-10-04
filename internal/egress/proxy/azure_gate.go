// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// The Azure Foundry route gate: on an azure_foundry host the proxy attaches a
// person's Entra token only to the row's listed inference calls, for the row's
// pinned deployments, on a connection the proxy terminates.
//
// THIS IS THE BOUNDARY, not a second opinion: the person's token is scoped to
// one resource audience and is resource-wide inside it, and the Azure data
// plane also serves files, fine-tuning and images and picks the deployment from
// the URL path. So the gate is a method-and-path allowlist, then a deployment
// identity check, then fail-closed body handling, all BEFORE the injector
// resolves (a refused request never redeems the token). The plain forward lane
// and an unterminated CONNECT never run it and are refused outright; the
// brokered /wardyn/llm/* route honours the rule-level pin (allowsInjection).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"

	"golang.org/x/sync/semaphore"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	// ruleSourceAzure marks an inference request the gate passed; ruleSourceAzureDenied is ANY refusal
	// by the gate, on any of the three doors.
	ruleSourceAzure       = "brokered:azure"
	ruleSourceAzureDenied = "brokered:azure:denied"

	// azureRouteRefused and azureInflightBudgetRefused are the two answers' `wardyn` codes.
	azureRouteRefused          = "azure_route_refused"
	azureInflightBudgetRefused = "azure_inflight_budget"

	// azureBodyCap* is the per-request inference-body cap by route. 32 MiB is the vendor maximum of the
	// Anthropic dialect; the Azure OpenAI v1 figure is the same bound until the A0 record states its own.
	// Never adoscope.MaxBodyPeek: a 256 KiB peek refuses ordinary prompts.
	azureBodyCapAnthropic int64 = 32 << 20
	azureBodyCapOpenAI    int64 = 32 << 20

	// azureInflightBudget bounds the body bytes every Azure gate holds at once, per proxy. It is charged
	// BEFORE a body is read. Sizing (the memory arithmetic the proxy's cgroup envelope is checked against,
	// runner.AzureProxyMemoryMiB): this budget + maxRetainedScanBytes (64 MiB) + one extraction at the
	// largest body (5.3 x 32 MiB, about 170 MiB) is 362 MiB, which does not fit the default 256 MiB
	// envelope, so a run with an Azure gate gets a larger envelope and no other run changes.
	azureInflightBudget int64 = 128 << 20
)

// AzureGateConfig is one azure_foundry row's gate on one host: the route it serves (types.AzureRoute*),
// the deployments it pins, and the largest body it admits (zero = the route's default).
type AzureGateConfig struct {
	Host    string   `json:"host"`
	Route   string   `json:"route"`
	Models  []string `json:"models"`
	BodyCap int64    `json:"body_cap,omitempty"`
}

// azureRoutePins is the method-and-path set each route's harness needs. WebSocket is never listed.
var azureRoutePins = map[string][]egress.PinRoute{
	types.AzureRouteAnthropic: {
		{Method: http.MethodPost, Path: "/anthropic/v1/messages"},
		{Method: http.MethodPost, Path: "/anthropic/v1/messages/count_tokens"},
	},
	types.AzureRouteOpenAIV1: {
		{Method: http.MethodPost, Path: "/openai/v1/responses"},
		{Method: http.MethodPost, Path: "/openai/v1/responses/compact"},
	},
}

// AzureRoutePins is the method-and-path set the gate admits on route, for the injection rule that
// carries the same pin (egress.InjectionRule.PinRoutes) so the token is withheld on every door.
// Nil for a route the gate does not know.
func AzureRoutePins(route string) []egress.PinRoute {
	return append([]egress.PinRoute(nil), azureRoutePins[route]...)
}

func azureDefaultBodyCap(route string) int64 {
	if route == types.AzureRouteOpenAIV1 {
		return azureBodyCapOpenAI
	}
	return azureBodyCapAnthropic
}

// azureGate is one row's compiled gate.
type azureGate struct {
	route  string
	pins   []egress.PinRoute
	models map[string]struct{}
	cap    int64
}

// azureGates is the proxy's compiled gate set, nil when no row carries one.
type azureGates struct {
	byHost map[string][]*azureGate
	budget *semaphore.Weighted
}

func newAzureGates(cfgs []AzureGateConfig) *azureGates {
	if len(cfgs) == 0 {
		return nil
	}
	g := &azureGates{byHost: map[string][]*azureGate{}, budget: semaphore.NewWeighted(azureInflightBudget)}
	for _, c := range cfgs {
		gate := &azureGate{route: c.Route, pins: azureRoutePins[c.Route], models: map[string]struct{}{}, cap: c.BodyCap}
		if gate.cap <= 0 {
			gate.cap = azureDefaultBodyCap(c.Route)
		}
		for _, m := range c.Models {
			gate.models[m] = struct{}{}
		}
		key := azureHostKey(c.Host)
		g.byHost[key] = append(g.byHost[key], gate)
	}
	return g
}

func azureHostKey(host string) string { return strings.ToLower(strings.TrimSuffix(host, ".")) }

func (g *azureGates) forHost(host string) []*azureGate {
	if g == nil {
		return nil
	}
	return g.byHost[azureHostKey(host)]
}

// match is the gate whose route admits exactly this method and path, or nil.
func (g *azureGates) match(host, method, path string) *azureGate {
	for _, gate := range g.forHost(host) {
		for _, pin := range gate.pins {
			if pin.Method == method && pin.Path == path {
				return gate
			}
		}
	}
	return nil
}

// isAzureLane reports whether host carries an Azure gate for this run.
func (p *Proxy) isAzureLane(host string) bool { return len(p.azure.forHost(host)) > 0 }

// azurePathUnsafe reports a raw path the gate will not even compare: a parent or empty segment, a
// backslash, a semicolon, any percent-encoding or a control character, following adoCheck. The allowlist
// below is over literal bytes, so every spelling trick is refused rather than decoded.
func azurePathUnsafe(path string) bool {
	return strings.Contains(path, "..") || strings.Contains(path, "//") ||
		strings.ContainsFunc(path, func(c rune) bool { return c == '\\' || c == ';' || c == '%' || c < 0x20 || c == 0x7f })
}

// azureDeploymentOutside reports a path naming a deployment outside the pinned set. It is defence in
// depth behind the allowlist: it never admits a path the allowlist refuses.
func azureDeploymentOutside(path string, models map[string]struct{}) bool {
	const marker = "/openai/deployments/"
	i := strings.Index(path, marker)
	if i < 0 {
		return false
	}
	seg, _, _ := strings.Cut(path[i+len(marker):], "/")
	_, ok := models[seg]
	return !ok
}

// gateAzure is the hook serveMITMRequest calls, before the injector resolves. It returns the rule
// source to forward under (brokered:azure for a gated host, src unchanged for any other), a release
// for the in-flight bytes it charged (call it after the upstream round trip), and false when it has
// already refused the request.
func (p *Proxy) gateAzure(w http.ResponseWriter, r *http.Request, host string, port int, src string) (string, func(), bool) {
	noop := func() {}
	if !p.isAzureLane(host) {
		return src, noop, true
	}
	refuse := func(msg string) (string, func(), bool) {
		p.refuseAzure(w, r, host, port, http.StatusForbidden, azureRouteRefused, msg)
		return "", noop, false
	}
	path := adoRawPath(r)
	if azurePathUnsafe(path) {
		return refuse("the path carries a character or segment this gate does not compare")
	}
	gate := p.azure.match(host, r.Method, path)
	if gate == nil {
		return refuse("this request is not one of the inference calls this run may make to this host")
	}
	if azureDeploymentOutside(path, gate.models) {
		return refuse("the path names a deployment this run is not pinned to")
	}
	for _, enc := range r.Header.Values("Content-Encoding") {
		if !strings.EqualFold(strings.TrimSpace(enc), "identity") {
			return refuse("an encoded body cannot be checked")
		}
	}
	if r.ContentLength > gate.cap {
		return refuse("the body is larger than this gate admits")
	}

	// Charge the budget BEFORE reading: by the declared length, else by the whole cap (a chunked or
	// undeclared body may be any size up to it). No queueing on sandbox-controlled input.
	charged := gate.cap
	if r.ContentLength >= 0 {
		charged = r.ContentLength
	}
	if !p.azure.budget.TryAcquire(charged) {
		p.refuseAzure(w, r, host, port, http.StatusServiceUnavailable, azureInflightBudgetRefused,
			"too many large requests are in flight on this run; retry shortly")
		return "", noop, false
	}
	var once sync.Once
	release := func() { once.Do(func() { p.azure.budget.Release(charged) }) }

	if r.Body == nil {
		r.Body = http.NoBody
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, gate.cap+1))
	_ = r.Body.Close()
	if err != nil || int64(len(body)) > gate.cap {
		release()
		return refuse("the body could not be read within the cap")
	}
	// The real size is known: return the over-charge (a body shorter than the cap it was charged at).
	if over := charged - int64(len(body)); over > 0 {
		p.azure.budget.Release(over)
		charged -= over
	}
	model, err := adoscope.TopLevelString(body, "model")
	if err != nil {
		release()
		return refuse("the body does not name exactly one model")
	}
	if _, ok := gate.models[model]; !ok {
		release()
		return refuse("the body's model is not one of this run's pinned deployments")
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	r.ContentLength = int64(len(body))
	return ruleSourceAzure, release, true
}

// refuseAzure is the ONE refusal point of the gate: a decision row under brokered:azure:denied and a
// JSON answer carrying the code. Never 401, which clients read as "try another credential".
func (p *Proxy) refuseAzure(w http.ResponseWriter, r *http.Request, host string, port int, status int, code, msg string) {
	if p.sink != nil {
		p.sink.emit(decisionLog(p.reqOf(r, host, port), egress.Deny, ruleSourceAzureDenied))
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"wardyn": code, "detail": "Wardyn refused this Azure request: " + msg + "."})
}

// refuseAzurePlain refuses, on the plain forward lane, every request to a host the run's Azure gate
// covers, and reports whether it did. That lane never runs gateAzure, so an absolute-form request
// there would otherwise reach the rule-level pin alone; Azure traffic flows only through the
// terminated tunnel.
func (p *Proxy) refuseAzurePlain(w http.ResponseWriter, r *http.Request) bool {
	if r.URL == nil {
		return false
	}
	host, port := splitHostPort(r.URL.Host, defaultPortForScheme(r.URL.Scheme))
	if !p.isAzureLane(host) {
		return false
	}
	p.refuseAzure(w, r, host, port, http.StatusForbidden, azureRouteRefused, "this host is reached only through Wardyn's checked HTTPS tunnel (CONNECT), not as a plain proxy request")
	return true
}

// refuseAzureTunnel refuses a CONNECT to a host the run's Azure gate covers, and reports whether it
// did. The caller reaches it only for a CONNECT it will not terminate: a blind tunnel would carry the
// request past the gate.
func (p *Proxy) refuseAzureTunnel(w http.ResponseWriter, r *http.Request, host string, port int) bool {
	if !p.isAzureLane(host) {
		return false
	}
	p.refuseAzure(w, r, host, port, http.StatusForbidden, azureRouteRefused, "this run reaches this host only through Wardyn's checked Azure door")
	return true
}

// validateAzureGates refuses a config whose gates the proxy could not honour, at boot rather than as a
// credential riding an ungated path: an unknown route, no pinned deployment, a cap the budget cannot
// charge, the same (host, route) twice, no MITM CA (the gate runs only on a terminated connection), or
// an injection rule on a gate host that is not TLS-only and pinned inside the gate's own allowlist.
func (c *Config) validateAzureGates() error {
	if len(c.AzureGates) == 0 {
		return nil
	}
	if c.MITMCACertPEM == "" || c.MITMCAKeyPEM == "" {
		return fmt.Errorf("config: azure_gates requires mitm_ca_cert_pem and mitm_ca_key_pem — the Azure gate runs only on a terminated connection")
	}
	type hostRoute struct{ host, route string }
	seen := map[hostRoute]bool{}
	allowed := map[string][]egress.PinRoute{} // host -> every method-and-path its gates admit
	for i, g := range c.AzureGates {
		pins, known := azureRoutePins[g.Route]
		switch {
		case strings.TrimSpace(g.Host) == "":
			return fmt.Errorf("config: azure_gates[%d]: host is required", i)
		case !known:
			return fmt.Errorf("config: azure_gates[%d]: route %q is not one of %q, %q", i, g.Route, types.AzureRouteAnthropic, types.AzureRouteOpenAIV1)
		case len(g.Models) == 0 || slices.Contains(g.Models, ""):
			return fmt.Errorf("config: azure_gates[%d]: models must name at least one deployment, none empty", i)
		case g.BodyCap < 0 || g.BodyCap > azureInflightBudget:
			return fmt.Errorf("config: azure_gates[%d]: body_cap %d is outside 0..%d", i, g.BodyCap, azureInflightBudget)
		}
		k := hostRoute{azureHostKey(g.Host), g.Route}
		if seen[k] {
			return fmt.Errorf("config: azure_gates[%d]: host %q route %q appears twice", i, g.Host, g.Route)
		}
		seen[k] = true
		allowed[k.host] = append(allowed[k.host], pins...)
	}
	for i, inj := range c.Injection {
		pins, gated := allowed[azureHostKey(inj.Host)]
		if !gated {
			continue
		}
		if !inj.RequireTLS || len(inj.PinRoutes) == 0 || inj.PinPath != "" {
			return fmt.Errorf("config: injection[%d]: a rule on the Azure gate host %q must set require_tls and pin_routes (and no pin_path)", i, inj.Host)
		}
		for _, pin := range inj.PinRoutes {
			if !slices.Contains(pins, pin) {
				return fmt.Errorf("config: injection[%d]: pin_routes entry %s %s is outside the Azure gate's allowlist for %q", i, pin.Method, pin.Path, inj.Host)
			}
		}
	}
	return nil
}
