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
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// ruleSourceAllowedModels marks every refusal this gate makes; the row's cause says which rule.
const ruleSourceAllowedModels = "brokered:allowed-models"

// maxModelNameShown bounds how much of a client-chosen model string a refusal repeats.
const maxModelNameShown = 100

// anthropicModelRoutes and openaiModelRoutes are the request paths, trimmed of slashes, that carry
// a model in their JSON body. A path matches on whole segments from its end, so a gateway's path
// prefix and the OpenAI "v1/" or Codex "backend-api/codex/" lead-in do not matter.
//
// Embeddings, image and audio routes also carry a model but are deliberately out of scope: they
// are not the chat models the limit governs, and are left ungated.
var (
	anthropicModelRoutes = []string{"messages", "messages/count_tokens"}
	openaiModelRoutes    = []string{"completions", "responses"} // "completions" is also chat/completions
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

// batchRefusal is the sentence a refused batch request carries.
const batchRefusal = "batch requests cannot be checked against this run's allowed models"

// opaqueModelTunnelRefusal is the sentence a refused opaque tunnel to a model host carries.
const opaqueModelTunnelRefusal = "this run limits the models it may use, and a tunnel to a model host cannot be checked against them; " +
	"the run needs TLS interception (mitm_llm and a CA, plus a mitm_hosts entry for a Bedrock or gateway host) to reach it"

// bedrockModelOps are the Bedrock runtime operations whose path is /model/<id>/<op>.
var bedrockModelOps = map[string]bool{
	"invoke": true, "invoke-with-response-stream": true, "converse": true, "converse-stream": true,
}

func noopRelease() {}

// compileAllowedModels is the set the gate tests against; nil (off) for an empty list.
func compileAllowedModels(list []string) map[string]bool {
	if len(list) == 0 {
		return nil
	}
	out := make(map[string]bool, len(list))
	for _, m := range list {
		out[m] = true
	}
	return out
}

// validateAllowedModels refuses a list with an empty entry, an entry with leading or trailing
// space, or a repeated entry, rather than quietly repairing it: the control plane authors the
// list and a malformed one is a bug to see at load. Unset (nil) is off; a list that is set but
// empty is refused, since it would have to mean either deny-all or no gate and neither is safe
// to guess.
func (c *Config) validateAllowedModels() error {
	if c.AllowedModels != nil && len(c.AllowedModels) == 0 {
		return fmt.Errorf("config: allowed_models: set but empty; omit it to turn the limit off")
	}
	seen := map[string]bool{}
	for i, m := range c.AllowedModels {
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

// routeCarriesModel reports whether the trimmed path ends, on whole segments, in one of routes.
func routeCarriesModel(path string, routes []string) bool {
	for _, route := range routes {
		if path == route || strings.HasSuffix(path, "/"+route) {
			return true
		}
	}
	return false
}

// bedrockModelPath reads the model id out of a Bedrock runtime path /model/<id>/<op>, from the
// escaped path so an id carrying an escaped "/" (an inference-profile ARN) stays one segment.
// gated is false for any other path, which names no model. ok is false for a model path whose id
// cannot be read.
func bedrockModelPath(escapedPath string) (model string, gated, ok bool) {
	segs := strings.Split(escapedPath, "/")
	if len(segs) != 4 || segs[0] != "" || segs[1] != "model" || !bedrockModelOps[segs[3]] {
		return "", false, true
	}
	id, err := url.PathUnescape(segs[2])
	if err != nil || id == "" {
		return "", true, false
	}
	return id, true, true
}

// requestModel reads the model the request names: from the path on a Bedrock host, from the JSON
// body on an Anthropic or OpenAI route. gated is false for a request that names none by design
// (another route, another method). It restores the body it read, so what is forwarded is the
// bytes the caller sent, and returns the release of the body's retained-budget charge.
// ok is false when the request is gated and its model could not be read.
func (p *Proxy) requestModel(r *http.Request, host, rest string) (model string, gated, ok bool, release func(), err error) {
	if isBedrockHost(hostKey(host)) {
		model, gated, ok = bedrockModelPath(r.URL.EscapedPath())
		return model, gated, ok, noopRelease, nil
	}
	if r.Method != http.MethodPost {
		return "", false, true, noopRelease, nil
	}
	path := strings.Trim(rest, "/")
	var routes []string
	switch p.shapeOf(r, host) {
	case shapeAnthropic:
		routes = anthropicModelRoutes
	case shapeOpenAI:
		routes = openaiModelRoutes
	default: // a gateway both vendors share has no shape of its own
		routes = append(append([]string{}, anthropicModelRoutes...), openaiModelRoutes...)
	}
	if !routeCarriesModel(path, routes) {
		return "", false, true, noopRelease, nil
	}
	if ce := r.Header.Get("Content-Encoding"); ce != "" && !strings.EqualFold(ce, "identity") {
		return "", true, false, noopRelease, nil
	}
	// A body a door already holds charged is judged as it is: a second charge would wait on this
	// request's own bytes.
	body, held := r.Context().Value(chargedBodyKey{}).([]byte)
	release = noopRelease
	if !held {
		body, release, err = p.scan.readBody(r.Context(), r.Body, r.ContentLength, int64(maxLLMScanBody))
		if errors.Is(err, errScanBusy) {
			return "", true, false, noopRelease, err
		}
		if err != nil || len(body) > maxLLMScanBody {
			release()
			return "", true, false, noopRelease, nil
		}
		holdChargedBody(r, body)
	}
	fields, ferr := objectFields(body, 0)
	if ferr != nil {
		release()
		return "", true, false, noopRelease, nil
	}
	for _, f := range fields {
		if f.key != "model" {
			continue
		}
		raw := body[f.start:f.end]
		if jerr := json.Unmarshal(raw, &model); jerr != nil || len(raw) == 0 || raw[0] != '"' { // null decodes to "" without error
			release()
			return "", true, false, noopRelease, nil
		}
		return model, true, true, release, nil
	}
	release()
	return "", true, false, noopRelease, nil
}

// isBatchRequest reports whether r is a POST to one of the host's batch routes (see above).
func (p *Proxy) isBatchRequest(r *http.Request, host, rest string) bool {
	if r.Method != http.MethodPost {
		return false
	}
	if isBedrockHost(hostKey(host)) {
		return bedrockBatchPaths[r.URL.Path]
	}
	path := strings.Trim(rest, "/")
	switch p.shapeOf(r, host) {
	case shapeAnthropic:
		return routeCarriesModel(path, anthropicBatchRoutes)
	case shapeOpenAI:
		return openaiBatchRequest(path)
	}
	return routeCarriesModel(path, anthropicBatchRoutes) || openaiBatchRequest(path)
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
	if len(p.allowedModels) == 0 {
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
// release. rest is the request path after the host, as proxyLLMRequest has it.
func (p *Proxy) allowedModelRefused(w http.ResponseWriter, r *http.Request, host string, port int, rest string) (func(), bool) {
	if len(p.allowedModels) == 0 || !p.isLLMHost(host) {
		return noopRelease, false
	}
	if p.isBatchRequest(r, host, rest) {
		return noopRelease, p.refuseAllowedModel(w, r, host, port, http.StatusForbidden, reasonEgressDenied, batchRefusal)
	}
	model, gated, ok, release, err := p.requestModel(r, host, rest)
	switch {
	case err != nil: // the retained budget had no room: the same 502 the local gate answers
		p.emitLLMDecision(r, host, port, egress.Deny, ruleSourceLLM, nil)
		p.scanFault(w, p.shapeOf(r, host), "read llm body", err, http.StatusBadGateway)
		return noopRelease, true
	case !gated:
		return release, false
	case !ok:
		return noopRelease, p.refuseAllowedModel(w, r, host, port, http.StatusBadRequest, reasonNotInspected,
			"the request's model could not be read")
	case !p.allowedModels[model]:
		release()
		return noopRelease, p.refuseAllowedModel(w, r, host, port, http.StatusForbidden, reasonEgressDenied,
			fmt.Sprintf("model %q is not on this run's allowed models list", shownModel(model)))
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
