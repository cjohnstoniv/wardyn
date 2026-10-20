// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// The local_model route gate (#2957). A run whose model provider is a model
// server on a laptop host or an org node reaches it only on the brokered LLM
// route, and every request there passes this gate first: a default-deny
// (method, path) table, the run's exact model allowlist read from the request
// body, and bounds on the fields that let one run exhaust the server. Model
// management (pull, push, create, copy, delete, blobs, account routes) is on
// no table row, so it is refused like everything else not listed. The prompt
// is never rewritten or reordered: a bound replaces only its own value's bytes,
// so server-side prefix caching still matches.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ruleSourceLocalModel marks every refusal the local_model gate makes; the
// row's cause says which rule refused.
const ruleSourceLocalModel = "brokered:local-model"

// localModelRoutes is the (method, path) table a run may call on a local model
// server: the OpenAI-, Anthropic- and Ollama-compatible inference routes and
// the two model listings, which are filtered to the allowlist. GET
// /v1/models/<name> is allowed for an allowlisted name alone (localModelGate).
var localModelRoutes = map[string]string{
	"/v1/chat/completions": http.MethodPost,
	"/v1/completions":      http.MethodPost,
	"/v1/embeddings":       http.MethodPost,
	"/v1/responses":        http.MethodPost,
	"/v1/messages":         http.MethodPost,
	"/api/chat":            http.MethodPost,
	"/api/generate":        http.MethodPost,
	"/api/embed":           http.MethodPost,
	"/v1/models":           http.MethodGet,
	"/api/tags":            http.MethodGet,
}

// maxLocalInventory bounds a model listing the filter reads.
const maxLocalInventory = 4 << 20

// localModel is one compiled LocalModels entry.
type localModel struct {
	plaintext bool // the server speaks http://
	models    map[string]bool
	bounds    types.LocalModelSettings
}

func localModelKey(host string, port int) string {
	return strings.TrimSuffix(strings.ToLower(host), ".") + "\x00" + strconv.Itoa(port)
}

// compileLocalModels keys each entry by its base URL's host and port, the
// pair the brokered route dials (llmUpstream). An entry that does not parse is
// dropped, so its route has no gate entry and is never dialled in cleartext
// (Config.validateLocalModels refuses it at load).
func compileLocalModels(in map[string]types.LocalModelSettings) map[string]*localModel {
	out := make(map[string]*localModel, len(in))
	for raw, s := range in {
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" {
			continue
		}
		port := 443
		if u.Scheme == "http" {
			port = 80
		}
		if n, err := strconv.Atoi(u.Port()); err == nil && n > 0 {
			port = n
		}
		lm := &localModel{plaintext: u.Scheme == "http", models: map[string]bool{}, bounds: s.LocalModelBounds()}
		for _, m := range s.Models {
			if !types.LocalModelNameRefused(m) {
				lm.models[m] = true
			}
		}
		out[localModelKey(u.Hostname(), port)] = lm
	}
	return out
}

// validateLocalModels: each entry is an http(s) base URL this run's brokered
// route also dials (an LLMUpstreams value), with at least one model no rule
// refuses.
func (c *Config) validateLocalModels() error {
	for raw, s := range c.LocalModels {
		u, err := url.Parse(raw)
		switch {
		case err != nil:
			return fmt.Errorf("config: local_models[%q]: %w", raw, err)
		case (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "":
			return fmt.Errorf("config: local_models[%q]: want an http:// or https:// base URL", raw)
		case !slices.Contains(slices.Collect(maps.Values(c.LLMUpstreams)), raw):
			return fmt.Errorf("config: local_models[%q]: not an llm_upstreams address", raw)
		case len(s.Models) == 0 || slices.ContainsFunc(s.Models, types.LocalModelNameRefused):
			return fmt.Errorf("config: local_models[%q]: models must list at least one model, none with a \"/\" or a cloud tag", raw)
		}
	}
	return nil
}

// localModelAt is the gate for the brokered route's dial target, or nil when
// host:port is no local model server.
func (p *Proxy) localModelAt(host string, port int) *localModel {
	return p.localModels[localModelKey(host, port)]
}

// localModelHost reports whether host is a local model server's, on any port:
// evaluate refuses it on every lane but the brokered route.
func (p *Proxy) localModelHost(host string) bool {
	prefix := strings.TrimSuffix(strings.ToLower(host), ".") + "\x00"
	for k := range p.localModels {
		if strings.HasPrefix(k, prefix) {
			return true
		}
	}
	return false
}

// refuseLocalModel records the refusal (the cause says which rule) and answers
// in the route's provider shape. It returns true, for the gate's callers.
func (p *Proxy) refuseLocalModel(w http.ResponseWriter, r *http.Request, host string, port int, status int, reason, cause string) bool {
	if p.sink != nil {
		log := decisionLog(p.reqOf(r, host, port), egress.Deny, ruleSourceLocalModel)
		log.Cause = cause
		p.sink.emit(log)
	}
	shape := p.shapeOf(r, host)
	writeProviderError(w, shape, status, reason,
		brokeredMessage(reason, p.hostPortLabel(shape, host, port), ruleSourceLocalModel, cause, ""), nil)
	return true
}

// localModelRefused is the gate: true when it refused (and answered) the
// request. A request it lets through carries its bounded body.
func (p *Proxy) localModelRefused(w http.ResponseWriter, r *http.Request, host string, port int, rest string, lm *localModel) bool {
	path := "/" + rest
	if name, ok := strings.CutPrefix(path, "/v1/models/"); ok && r.Method == http.MethodGet {
		if !lm.models[name] {
			return p.refuseLocalModel(w, r, host, port, http.StatusForbidden, reasonEgressDenied,
				fmt.Sprintf("model %q is not on this run's model allowlist", name))
		}
		return false
	}
	method, ok := localModelRoutes[path]
	if !ok || r.Method != method {
		return p.refuseLocalModel(w, r, host, port, http.StatusForbidden, reasonEgressDenied,
			fmt.Sprintf("%s %s is not a route a run may call on a local model server", r.Method, path))
	}
	if method == http.MethodGet {
		r.Header.Del("Accept-Encoding") // the listing filter reads the answer
		return false
	}
	if ce := r.Header.Get("Content-Encoding"); ce != "" && !strings.EqualFold(ce, "identity") {
		return p.refuseLocalModel(w, r, host, port, http.StatusBadRequest, reasonNotInspected, "an encoded request body cannot be checked")
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, int64(maxLLMScanBody)+1))
	if err != nil || len(body) > maxLLMScanBody {
		return p.refuseLocalModel(w, r, host, port, http.StatusBadRequest, reasonNotInspected, "the request body could not be read whole")
	}
	out, model, err := boundLocalRequest(body, lm.bounds)
	if err != nil {
		return p.refuseLocalModel(w, r, host, port, http.StatusBadRequest, reasonNotInspected, err.Error())
	}
	if !lm.models[model] {
		return p.refuseLocalModel(w, r, host, port, http.StatusForbidden, reasonEgressDenied,
			fmt.Sprintf("model %q is not on this run's model allowlist", model))
	}
	r.Body, r.ContentLength = io.NopCloser(bytes.NewReader(out)), int64(len(out))
	return false
}

// jsonField is one member of a JSON object: its key and its value's byte span.
type jsonField struct {
	key        string
	start, end int
}

var errLocalBody = errors.New("the request body must be one JSON object with a \"model\" string")

// objectFields lists b's members with their value spans offset by base. Keys
// are compared case-insensitively, as a Go server matches them, so a second
// spelling of "model" can never pick another model than the one checked: a
// duplicate under that comparison refuses the body.
func objectFields(b []byte, base int) ([]jsonField, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, errLocalBody
	}
	var out []jsonField
	for dec.More() {
		t, err := dec.Token()
		key, ok := t.(string)
		if err != nil || !ok {
			return nil, errLocalBody
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, errLocalBody
		}
		end := int(dec.InputOffset())
		start := end - len(raw)
		if start < 0 || !bytes.Equal(b[start:end], raw) {
			return nil, errLocalBody
		}
		if slices.ContainsFunc(out, func(f jsonField) bool { return strings.EqualFold(f.key, key) }) {
			return nil, fmt.Errorf("the request body names %q twice", key)
		}
		out = append(out, jsonField{key: key, start: base + start, end: base + end})
	}
	if t, err := dec.Token(); err != nil || t != json.Delim('}') {
		return nil, errLocalBody
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errLocalBody
	}
	return out, nil
}

// boundLocalRequest returns body with each bounded field held to its cap, and
// the model it names. Only a bounded value's own bytes change.
func boundLocalRequest(body []byte, b types.LocalModelSettings) ([]byte, string, error) {
	fields, err := objectFields(body, 0)
	if err != nil {
		return nil, "", err
	}
	type splice struct {
		start, end int
		val        string
	}
	var edits []splice
	var model string
	var seenModel bool
	bound := func(f jsonField, limit int) error {
		v, err := boundValue(f.key, body[f.start:f.end], limit)
		if err == nil && v != "" {
			edits = append(edits, splice{f.start, f.end, v})
		}
		return err
	}
	for _, f := range fields {
		var err error
		switch foldKey(f.key, "model", "max_tokens", "max_completion_tokens", "max_output_tokens", "keep_alive", "options") {
		case "model":
			seenModel = json.Unmarshal(body[f.start:f.end], &model) == nil
		case "max_tokens", "max_completion_tokens", "max_output_tokens":
			err = bound(f, b.MaxOutput)
		case "keep_alive":
			v, kerr := boundKeepAlive(body[f.start:f.end], b.KeepAliveSeconds)
			if kerr == nil && v != "" {
				edits = append(edits, splice{f.start, f.end, v})
			}
			err = kerr
		case "options":
			raw := body[f.start:f.end]
			if string(raw) == "null" {
				continue
			}
			opts, oerr := objectFields(raw, f.start)
			if errors.Is(oerr, errLocalBody) {
				return nil, "", errors.New("the request body's options must be a JSON object")
			}
			if oerr != nil {
				return nil, "", oerr
			}
			for _, o := range opts {
				switch foldKey(o.key, "num_ctx", "num_predict") {
				case "num_ctx":
					err = errors.Join(err, bound(o, b.MaxContext))
				case "num_predict":
					err = errors.Join(err, bound(o, b.MaxOutput))
				}
			}
		}
		if err != nil {
			return nil, "", err
		}
	}
	if !seenModel {
		return nil, "", errLocalBody
	}
	if len(edits) == 0 {
		return body, model, nil
	}
	slices.SortFunc(edits, func(x, y splice) int { return x.start - y.start })
	out := make([]byte, 0, len(body))
	at := 0
	for _, e := range edits {
		out = append(append(out, body[at:e.start]...), e.val...)
		at = e.end
	}
	return append(out, body[at:]...), model, nil
}

// foldKey is the name in names that key matches the way a Go server's JSON
// decoder matches a field (Unicode case folding), or "".
func foldKey(key string, names ...string) string {
	for _, n := range names {
		if strings.EqualFold(key, n) {
			return n
		}
	}
	return ""
}

// boundValue is the replacement for a numeric field over limit ("" to keep
// it). A negative value is over it too: Ollama reads num_predict -1 as no
// limit and -2 as fill the context, and maps max_tokens onto num_predict.
// null is kept; anything but a number is refused, since a lenient server
// would read a numeric string past the bound.
func boundValue(key string, raw []byte, limit int) (string, error) {
	if string(raw) == "null" {
		return "", nil
	}
	n, err := strconv.ParseFloat(string(raw), 64)
	if err != nil {
		return "", fmt.Errorf("the request body's %s must be a number", key)
	}
	if n < 0 || n > float64(limit) {
		return strconv.Itoa(limit), nil
	}
	return "", nil
}

// boundKeepAlive holds keep_alive (seconds, or a duration string; negative is
// "forever") to limit seconds, so no run pins a model in the server's memory.
func boundKeepAlive(raw []byte, limit int) (string, error) {
	if string(raw) == "null" {
		return "", nil
	}
	var secs float64
	var s string
	switch {
	case json.Unmarshal(raw, &secs) == nil:
	case json.Unmarshal(raw, &s) == nil:
		d, err := time.ParseDuration(s)
		if err != nil {
			return "", errors.New("the request body's keep_alive must be a number of seconds or a duration")
		}
		secs = d.Seconds()
	default:
		return "", errors.New("the request body's keep_alive must be a number of seconds or a duration")
	}
	if secs < 0 || secs > float64(limit) {
		return strconv.Itoa(limit), nil
	}
	return "", nil
}

// filterLocalInventory narrows a model listing's answer to the run's
// allowlist, so a run learns of no other model on the server. Any answer it
// cannot read is an error: the listing is never relayed unfiltered.
func filterLocalInventory(resp *http.Response, rest string, lm *localModel) error {
	var list, name string
	switch "/" + rest {
	case "/v1/models":
		list, name = "data", "id"
	case "/api/tags":
		list, name = "models", "name"
	default:
		return nil
	}
	if resp.StatusCode/100 != 2 {
		return nil
	}
	if ce := resp.Header.Get("Content-Encoding"); ce != "" && !strings.EqualFold(ce, "identity") {
		return errors.New("an encoded model listing cannot be filtered")
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxLocalInventory+1))
	if err != nil || len(raw) > maxLocalInventory {
		return errors.New("the model listing could not be read whole")
	}
	var doc map[string]json.RawMessage
	var entries []map[string]json.RawMessage
	if json.Unmarshal(raw, &doc) != nil || json.Unmarshal(doc[list], &entries) != nil {
		return errors.New("the model listing is not the shape the filter reads")
	}
	kept := slices.DeleteFunc(entries, func(e map[string]json.RawMessage) bool {
		var n string
		return json.Unmarshal(e[name], &n) != nil || !lm.models[n]
	})
	if kept == nil {
		kept = []map[string]json.RawMessage{}
	}
	doc[list], _ = json.Marshal(kept)
	out, _ := json.Marshal(doc)
	resp.Body = io.NopCloser(bytes.NewReader(out))
	resp.ContentLength = int64(len(out))
	resp.Header.Set("Content-Length", strconv.Itoa(len(out)))
	return nil
}
