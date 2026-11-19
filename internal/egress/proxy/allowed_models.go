// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// The allowed_models gate: with Config.AllowedModels set, a request to a hosted model provider
// reaches it only if it names one of those models exactly (case-sensitive, no folding). The model
// is read from the JSON body on the Anthropic and OpenAI routes and from the path on Bedrock. It
// FAILS CLOSED: a body the proxy cannot read whole, parse, or find a string "model" in is refused,
// as is a Bedrock model path it cannot read, and a refusal is answered here and never forwarded.
// With the list unset the gate returns before it touches the request.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// ruleSourceAllowedModels marks every refusal this gate makes; the row's cause says which rule.
const ruleSourceAllowedModels = "brokered:allowed-models"

// maxModelNameShown bounds how much of a client-chosen model string a refusal repeats.
const maxModelNameShown = 100

// anthropicModelRoutes and openaiModelRoutes are the request paths that carry a model in their JSON
// body. A path matches on whole segments from its end, so a gateway's path prefix and the OpenAI
// "v1/" or Codex "backend-api/codex/" lead-in do not matter.
//
// Every other non-GET, non-HEAD request on a model host is refused while the list is set
// (uncheckedRouteRefusal). That includes the routes that name no model, such as file and skill
// uploads, until a capture of real harness traffic backs an exact path; and the routes that
// also carry a model but are not chat models: embeddings, images, audio, moderations, assistants
// and thread runs, fine-tuning, realtime sessions, the legacy completion route, and on Bedrock
// rerank, the agent runtime and model customisation jobs.
var (
	anthropicModelRoutes = []string{"messages", "messages/count_tokens"}
	openaiModelRoutes    = []string{"completions", "responses", "responses/compact"} // "completions" is also chat/completions; "responses/compact" is Codex remote compaction
)

// The batch routes carry a model per request inside the body, or a file of requests uploaded
// beforehand, so they cannot be checked against the list and are refused while it is set. They
// are not parsed. Only the POSTs that create or fill a batch are refused: reading one back is not.
// Bedrock's async invoke and batch inference job are the same kind of request.
var (
	anthropicBatchRoutes = []string{"messages/batches"}
	openaiBatchRoutes    = []string{"batches", "files", "uploads"} // a batch is created from an uploaded file
	bedrockBatchPaths    = map[string]bool{"/async-invoke": true, "/model-invocation-job": true}
)

const (
	// batchRefusal is the sentence a refused batch request carries.
	batchRefusal = "batch requests cannot be checked against this run's allowed models"
	// uncheckedRouteRefusal is the sentence a refused route with no model to check carries.
	uncheckedRouteRefusal = "this request to a model host cannot be checked against this run's allowed models"
	// unsafePathRefusal is the sentence a refused path with a dot, empty or unreadable segment carries.
	unsafePathRefusal = "the request path has a dot, empty or unreadable segment, so it cannot be checked against this run's allowed models"
	// modelUnreadable is the sentence a refused request whose model cannot be found carries.
	modelUnreadable = "the request's model could not be read"
)

// opaqueModelTunnelRefusal is the sentence a refused opaque tunnel to a model host carries.
const opaqueModelTunnelRefusal = "this run limits the models it may use, and this connection to a model host can't be checked against them"

// bedrockModelOps are the Bedrock runtime operations whose path is /model/<id>/<op>.
var bedrockModelOps = map[string]bool{
	"invoke": true, "invoke-with-response-stream": true, "invoke-with-bidirectional-stream": true,
	"converse": true, "converse-stream": true,
}

func noopRelease() {}

// modelLimit is what the gate tests against: the run's models, and its Bedrock gateway routes.
type modelLimit struct {
	models map[string]bool
	// bedrockRoutes is Config.BedrockRoutes keyed by hostKey: a Bedrock gateway's host -> its base path.
	bedrockRoutes map[string]string
}

// compileAllowedModels is the limit the gate tests against: nil (the limit is off) for an unset
// list, and a non-nil limit whose set is empty for `[]`, which refuses every model.
func compileAllowedModels(list *[]string, routes map[string]string) *modelLimit {
	if list == nil {
		return nil
	}
	l := &modelLimit{models: make(map[string]bool, len(*list))}
	for _, m := range *list {
		l.models[m] = true
	}
	for h, base := range routes {
		if l.bedrockRoutes == nil {
			l.bedrockRoutes = make(map[string]string, len(routes))
		}
		l.bedrockRoutes[hostKey(h)] = base
	}
	return l
}

// bedrockRoute is the base path of h's configured Bedrock gateway route, when the limit is set and h
// has one.
func (p *Proxy) bedrockRoute(h string) (string, bool) {
	if p.allowedModels == nil {
		return "", false
	}
	base, ok := p.allowedModels.bedrockRoutes[h]
	return base, ok
}

// underBase is cleaned with base taken off, when cleaned is base or a path under it.
func underBase(cleaned, base string) (string, bool) {
	switch {
	case base == "":
		return cleaned, true
	case cleaned == base:
		return "/", true
	case strings.HasPrefix(cleaned, base+"/"):
		return cleaned[len(base):], true
	}
	return "", false
}

// validateBedrockRoutes refuses a route whose key is not a bare hostname or a bare IPv6 address, or
// whose base path is not "" or a clean absolute path with no escapes, query or trailing slash.
func (c *Config) validateBedrockRoutes() error {
	for host, base := range c.BedrockRoutes {
		switch {
		case strings.TrimSpace(host) == "" || (strings.ContainsAny(host, ":/@ ") && !bareIPv6(host)):
			return fmt.Errorf("config: bedrock_routes[%q]: the key must be a bare hostname or a bare IPv6 address", host)
		case base != "" && (!strings.HasPrefix(base, "/") || path.Clean(base) != base || base == "/" || strings.ContainsAny(base, "%?#\\")):
			return fmt.Errorf("config: bedrock_routes[%q]: base path %q must be empty or a clean absolute path", host, base)
		}
	}
	return nil
}

// validateAllowedModels refuses a list with an empty entry, an entry with leading or trailing
// space, or a repeated entry, rather than quietly repairing it: the control plane authors the
// list and a malformed one is a bug to see at load. Unset (nil) is off; `[]` is set and empty,
// which refuses every model.
func (c *Config) validateAllowedModels() error {
	if c.AllowedModels == nil {
		return nil
	}
	seen := map[string]bool{}
	for i, m := range *c.AllowedModels {
		switch {
		case m == "":
			return fmt.Errorf("config: allowed_models[%d]: must not be empty", i)
		case strings.TrimSpace(m) != m:
			return fmt.Errorf("config: allowed_models[%d]: %q has leading or trailing space", i, m)
		case seen[m]:
			return fmt.Errorf("config: allowed_models[%d]: %q is listed twice", i, m)
		}
		seen[m] = true
	}
	return nil
}

// routeCarriesModel reports whether the cleaned path ends, on whole segments, in one of routes.
func routeCarriesModel(path string, routes []string) bool {
	for _, route := range routes {
		if path == route || strings.HasSuffix(path, "/"+route) {
			return true
		}
	}
	return false
}

// cleanModelPath returns path.Clean of p with its leading slash, or false when p holds a segment
// the route match must not be left to guess at: a dot segment, an empty one (a doubled or
// trailing slash; the root path alone is fine), or one that does not unescape or holds a control
// character or backslash. A server may resolve any of these differently than a suffix match reads
// them. p is the decoded path, or with escaped set the escaped one, where an escaped "/" must stay
// inside one segment and each segment is unescaped here to look for a dot behind an escape.
func cleanModelPath(p string, escaped bool) (string, bool) {
	trimmed := strings.TrimPrefix(p, "/")
	segs := strings.Split(trimmed, "/")
	for _, seg := range segs {
		dec, err := seg, error(nil)
		if escaped {
			dec, err = url.PathUnescape(seg)
		}
		if err != nil || isDotSegment(seg) || isDotSegment(dec) ||
			strings.ContainsFunc(dec, func(c rune) bool { return c == '\\' || c < 0x20 || c == 0x7f }) ||
			(seg == "" && len(segs) > 1) {
			return "", false
		}
	}
	return path.Clean("/" + trimmed), true
}

// modelCheck is what the gate does with a request on a model host.
type modelCheck int

const (
	checkPass       modelCheck = iota // not a request the limit governs
	checkBatch                        // a batch route: refused
	checkUnchecked                    // a request with no model to check: refused
	checkUnreadable                   // a model route whose model cannot be found: refused
	checkPathModel                    // a model named in the path
	checkBodyModel                    // a model named in the JSON body
)

func bodiless(method string) bool { return method == http.MethodGet || method == http.MethodHead }

// bedrockCheck classifies a request on a Bedrock host by its cleaned escaped path. Every path whose
// first segment is "model" is gated: it must be /model/<id>/<op> with a known op, and the id is
// returned unescaped. Anything else is refused unless it is a read.
func bedrockCheck(r *http.Request, cleaned string) (modelCheck, string) {
	segs := strings.Split(strings.TrimPrefix(cleaned, "/"), "/")
	switch {
	case segs[0] == "model":
		if len(segs) != 3 || !bedrockModelOps[segs[2]] {
			return checkUnreadable, ""
		}
		id, err := url.PathUnescape(segs[1])
		if err != nil || id == "" {
			return checkUnreadable, ""
		}
		return checkPathModel, id
	case r.Method == http.MethodPost && bedrockBatchPaths[cleaned]:
		return checkBatch, ""
	case bodiless(r.Method):
		return checkPass, ""
	}
	return checkUnchecked, ""
}

// hostedCheck classifies a request on an Anthropic- or OpenAI-style host by its cleaned path. A
// gateway both vendors share has no shape of its own, so it takes both vendors' routes.
func (p *Proxy) hostedCheck(r *http.Request, host, cleaned string) modelCheck {
	path := strings.TrimPrefix(cleaned, "/")
	shape := p.shapeOf(r, host)
	anthropic, openai := shape != shapeOpenAI, shape != shapeAnthropic
	post := r.Method == http.MethodPost
	switch {
	case post && ((anthropic && routeCarriesModel(path, anthropicBatchRoutes)) || (openai && openaiBatchRequest(path))):
		return checkBatch
	case post && ((anthropic && routeCarriesModel(path, anthropicModelRoutes)) || (openai && routeCarriesModel(path, openaiModelRoutes))):
		return checkBodyModel
	case bodiless(r.Method):
		return checkPass
	}
	return checkUnchecked
}

// bodyModel reads the model the JSON body names. It restores the body it read, so what is forwarded
// is the bytes the caller sent, and returns the release of the body's retained-budget charge. ok is
// false when the model could not be read; err is set only when the retained budget had no room.
func (p *Proxy) bodyModel(r *http.Request) (model string, ok bool, release func(), err error) {
	if ce := r.Header.Get("Content-Encoding"); ce != "" && !strings.EqualFold(ce, "identity") {
		return "", false, noopRelease, nil
	}
	// A body a door already holds charged is judged as it is: a second charge would wait on this
	// request's own bytes.
	body, held := r.Context().Value(chargedBodyKey{}).([]byte)
	release = noopRelease
	if !held {
		body, release, err = p.scan.readBody(r.Context(), r.Body, r.ContentLength, int64(maxLLMScanBody))
		if errors.Is(err, errScanBusy) {
			return "", false, noopRelease, err
		}
		if err != nil || len(body) > maxLLMScanBody {
			release()
			return "", false, noopRelease, nil
		}
		holdChargedBody(r, body)
	}
	fields, ferr := objectFields(body, 0)
	if ferr == nil {
		for _, f := range fields {
			raw := body[f.start:f.end]
			if f.key == "model" && len(raw) > 0 && raw[0] == '"' && json.Unmarshal(raw, &model) == nil { // null decodes to "" without error
				return model, true, release, nil
			}
		}
	}
	release()
	return "", false, noopRelease, nil
}

// openaiBatchRequest matches the OpenAI batch create and its file uploads. An upload part is
// /uploads/<id>/parts and its completion /uploads/<id>/complete, so those end in a fixed word
// after an upload id.
func openaiBatchRequest(path string) bool {
	if routeCarriesModel(path, openaiBatchRoutes) {
		return true
	}
	segs := strings.Split(path, "/")
	n := len(segs)
	return n >= 3 && segs[n-3] == "uploads" && (segs[n-1] == "parts" || segs[n-1] == "complete")
}

// refuseOpaqueModelTunnel refuses a CONNECT to a model host that the proxy would pass as an opaque
// tunnel, which the gate could not see into, while the allowed_models list is set. The caller
// asks only after every terminating branch has declined the host. Unset, it does nothing.
func (p *Proxy) refuseOpaqueModelTunnel(w http.ResponseWriter, r *http.Request, host string, port int) bool {
	if p.allowedModels == nil {
		return false
	}
	if p.sink != nil {
		log := decisionLog(p.reqOf(r, host, port), egress.Deny, ruleSourceAllowedModels)
		log.Cause = opaqueModelTunnelRefusal
		p.sink.emit(log)
	}
	setEgressRefusalHeadersWithReason(w, egressRefusalDenied, host, ruleSourceAllowedModels)
	w.Header().Set(egressHeaderDetail, opaqueModelTunnelRefusal)
	w.Header().Set(egressHeaderWardynReason, reasonEgressDenied)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	out, _ := json.Marshal(map[string]any{"wardyn": map[string]string{
		"reason": reasonEgressDenied, "host": host, "rule_source": ruleSourceAllowedModels, "detail": opaqueModelTunnelRefusal}})
	_, _ = w.Write(out)
	return true
}

// allowedModelRefused is the gate: true when it refused (and answered) the request. A request it
// lets through keeps its body, charged to the retained budget until the caller's deferred
// release. rest is the request path after the host, as proxyLLMRequest has it. With the list set,
// a non-read request on a model host is refused unless its model is read and listed; with the list
// unset (nil) it returns before touching the request.
func (p *Proxy) allowedModelRefused(w http.ResponseWriter, r *http.Request, host string, port int, rest string) (func(), bool) {
	if p.allowedModels == nil || !p.isLLMHost(host) {
		return noopRelease, false
	}
	base, routed := p.bedrockRoute(hostKey(host))
	bedrock := routed || isBedrockHost(hostKey(host))
	raw := rest
	if bedrock {
		raw = r.URL.EscapedPath() // an escaped "/" in a model id stays inside its segment
	}
	cleaned, ok := cleanModelPath(raw, bedrock)
	if !ok {
		return noopRelease, p.refuseAllowedModel(w, r, host, port, http.StatusBadRequest, reasonUnsupportedPath, unsafePathRefusal)
	}
	if routed { // a Bedrock gateway: its Bedrock API is under the base path, and nothing else on it is read
		if cleaned, ok = underBase(cleaned, base); !ok {
			if bodiless(r.Method) {
				return noopRelease, false
			}
			return noopRelease, p.refuseAllowedModel(w, r, host, port, http.StatusForbidden, reasonEgressDenied, uncheckedRouteRefusal)
		}
	}
	check, model := checkPass, ""
	if bedrock {
		check, model = bedrockCheck(r, cleaned)
	} else {
		check = p.hostedCheck(r, host, cleaned)
	}
	release := noopRelease
	switch check {
	case checkPass:
		return noopRelease, false
	case checkBatch:
		return noopRelease, p.refuseAllowedModel(w, r, host, port, http.StatusForbidden, reasonEgressDenied, batchRefusal)
	case checkUnchecked:
		return noopRelease, p.refuseAllowedModel(w, r, host, port, http.StatusForbidden, reasonEgressDenied, uncheckedRouteRefusal)
	case checkBodyModel:
		var err error
		model, ok, release, err = p.bodyModel(r)
		if err != nil { // the retained budget had no room: the same 502 the local gate answers
			p.emitLLMDecision(r, host, port, egress.Deny, ruleSourceLLM, nil)
			p.scanFault(w, p.shapeOf(r, host), "read llm body", err, http.StatusBadGateway)
			return noopRelease, true
		}
		if !ok {
			check = checkUnreadable
		}
	}
	switch {
	case check == checkUnreadable:
		return noopRelease, p.refuseAllowedModel(w, r, host, port, http.StatusBadRequest, reasonNotInspected, modelUnreadable)
	case !p.allowedModels.models[model]:
		release()
		return noopRelease, p.refuseAllowedModel(w, r, host, port, http.StatusForbidden, reasonEgressDenied,
			fmt.Sprintf("model %q is not on this run's allowed models list", shownModel(model)))
	}
	// A listed model is not enough: a rerouting key in the body can send the call to another
	// (reroute_keys.go). The bytes bodyModel read are held, so this reads nothing again.
	if check == checkBodyModel && p.rerouteGateRefused(w, r, host, port, release) {
		return noopRelease, true
	}
	return release, false
}

// shownModel is a model name cut to what a refusal repeats.
func shownModel(m string) string {
	if len(m) > maxModelNameShown {
		return m[:maxModelNameShown] + "..."
	}
	return m
}

// refuseAllowedModel records the refusal (the cause says which rule) and answers in the route's
// provider shape, as refuseLocalModel does. It returns true, for the gate's callers.
func (p *Proxy) refuseAllowedModel(w http.ResponseWriter, r *http.Request, host string, port int, status int, reason, cause string) bool {
	if p.sink != nil {
		log := decisionLog(p.reqOf(r, host, port), egress.Deny, ruleSourceAllowedModels)
		log.Cause = cause
		p.sink.emit(log)
	}
	shape := p.shapeOf(r, host)
	writeProviderError(w, shape, status, reason,
		brokeredMessage(reason, p.hostPortLabel(shape, host, port), ruleSourceAllowedModels, cause, ""), nil)
	return true
}
