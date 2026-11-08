// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"
)

// The call key of one kubectl write a person approves.
//
// A kubectl row with approve_each makes the proxy hold every write its door
// admits until a person says yes to that exact request. The key below is the
// identity of that request: the cluster, the namespace, the method, the raw
// path, the canonical query, the parsed media type and the canonical body. The
// proxy that raises the approval, the control plane that checks it and the
// door that spends it all compute it here, so they cannot disagree.
//
// One exception keeps a retry spendable: the patch `kubectl rollout restart`
// sends carries the time it was run, so its body is keyed as the restart
// itself (KubeRestartPatch) and the path names what is restarted.

// KubeRestartAnnotation is the pod-template annotation `kubectl rollout
// restart` sets to the time it was run.
const KubeRestartAnnotation = "kubectl.kubernetes.io/restartedAt"

// KubeStrategicMergePatch is the media type of the patch `kubectl rollout
// restart` sends.
const KubeStrategicMergePatch = "application/strategic-merge-patch+json"

// kubeRestartKeyBody stands in the key for a restart patch's body.
const kubeRestartKeyBody = "kubectl rollout restart"

// kubeCallMethods are the methods a held kubectl write may carry.
var kubeCallMethods = []string{"POST", "PUT", "PATCH", "DELETE"}

var (
	// ErrKubeCallTooLarge means the canonical body is over MaxMCPCallBytes.
	ErrKubeCallTooLarge = errors.New("the request body is larger than 32 KiB")
	// ErrKubeCallNotJSON means the body or its media type is not JSON.
	ErrKubeCallNotJSON = errors.New("the request body is not JSON")
)

// KubeCallScope is the requested_scope of a tool_call approval for one kubectl
// write the door holds. It is known structurally by call_sha256 together with
// cluster (IsKubeCallScope), with no grant, and only the proxy writes it.
type KubeCallScope struct {
	// Cluster is the API server's host.
	Cluster   string `json:"cluster"`
	Namespace string `json:"namespace"`
	Method    string `json:"method"`
	// Path is the raw request path, /api/... or /apis/....
	Path string `json:"path"`
	// Query is KubeCanonicalQuery of the request's query; absent when it has none.
	Query string `json:"query,omitempty"`
	// MediaType is the Content-Type's media type, lower case and without
	// parameters; absent when the request names none.
	MediaType string `json:"media_type,omitempty"`
	// Body is the canonical JSON body; absent when the request has none.
	Body       json.RawMessage `json:"body,omitempty"`
	CallSHA256 string          `json:"call_sha256"`
	// HoldSeconds is how long the proxy holds the open request for the answer.
	HoldSeconds int `json:"hold_seconds,omitempty"`
}

// KubeCanonicalQuery is raw in one spelling: keys sorted, each key's values in
// the order sent, every part escaped one way. A query Go cannot parse (a bad
// escape, a semicolon) is refused.
func KubeCanonicalQuery(raw string) (string, error) {
	v, err := url.ParseQuery(raw)
	if err != nil {
		return "", fmt.Errorf("query: %w", err)
	}
	return v.Encode(), nil
}

// KubeJSONMediaType reports whether mt, a parsed media type, names JSON:
// application/json or an application/*+json type (the patch types).
func KubeJSONMediaType(mt string) bool {
	sub, ok := strings.CutPrefix(mt, "application/")
	return ok && (sub == "json" || (strings.HasSuffix(sub, "+json") && len(sub) > len("+json")))
}

// KubeCallNamespace is the namespace a namespaced path names, /api/<v>/namespaces/<ns>/...
// or /apis/<g>/<v>/namespaces/<ns>/..., and false for any other path.
func KubeCallNamespace(path string) (string, bool) {
	segs := strings.Split(strings.TrimPrefix(path, "/"), "/")
	at := -1
	switch {
	case !strings.HasPrefix(path, "/"):
	case segs[0] == "api" && len(segs) >= 4:
		at = 2
	case segs[0] == "apis" && len(segs) >= 5:
		at = 3
	}
	if at < 0 || segs[at] != "namespaces" || segs[at+1] == "" {
		return "", false
	}
	return segs[at+1], true
}

// KubeCallBody is body in canonical form, nil for an empty body. It refuses a
// body that is not one JSON value (a repeated key included), a media type that
// is not JSON when there is a body, and a canonical body over MaxMCPCallBytes.
func KubeCallBody(mediaType string, body []byte) (json.RawMessage, error) {
	if len(body) == 0 {
		return nil, nil
	}
	if !KubeJSONMediaType(mediaType) {
		return nil, ErrKubeCallNotJSON
	}
	if len(body) > MaxMCPCallBytes {
		return nil, ErrKubeCallTooLarge
	}
	canon, err := CanonicalJSON(body)
	switch {
	case errors.Is(err, ErrCanonicalRepeatedKey):
		return nil, err
	case err != nil:
		return nil, fmt.Errorf("%w: %v", ErrKubeCallNotJSON, err)
	case len(canon) > MaxMCPCallBytes:
		return nil, ErrKubeCallTooLarge
	}
	return canon, nil
}

// KubeCallKey is the identity of one held kubectl write: the hex SHA-256 of
// its fields, each length-prefixed. query must be canonical (KubeCanonicalQuery)
// and body canonical (KubeCallBody). A restart patch is keyed as the restart,
// whatever time it carries.
func KubeCallKey(cluster, namespace, method, path, query, mediaType string, body []byte) (string, error) {
	if !slices.Contains(kubeCallMethods, method) {
		return "", fmt.Errorf("method %q is not one a held write carries", method)
	}
	keyBody := body
	if method == "PATCH" && mediaType == KubeStrategicMergePatch && KubeRestartPatch(body) {
		keyBody = []byte(kubeRestartKeyBody)
	}
	h := sha256.New()
	for _, part := range []string{"wardyn.kube-call.v1", cluster, namespace, method, path, query, mediaType, string(keyBody)} {
		fmt.Fprintf(h, "%d:", len(part))
		h.Write([]byte(part))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Validate refuses a scope a person could not read exactly: no cluster, a
// method a held write does not carry, a path that is not namespaced in the
// scope's own namespace, a query or body that is not in canonical form, or a
// media type that is not JSON (absent only with no body).
func (s KubeCallScope) Validate() error {
	if s.Cluster == "" || !CLIKubectlHost(s.Cluster) {
		return errors.New("cluster is not a host")
	}
	if !slices.Contains(kubeCallMethods, s.Method) {
		return fmt.Errorf("method %q is not one a held write carries", s.Method)
	}
	if ns, ok := KubeCallNamespace(s.Path); !ok || ns != s.Namespace || strings.ContainsAny(s.Path, "?#%") {
		return errors.New("path is not a namespaced path in the scope's namespace")
	}
	if q, err := KubeCanonicalQuery(s.Query); err != nil || q != s.Query {
		return errors.New("query is not in canonical form")
	}
	if s.MediaType != "" && !KubeJSONMediaType(s.MediaType) {
		return ErrKubeCallNotJSON
	}
	if len(s.Body) == 0 {
		return nil
	}
	canon, err := KubeCallBody(s.MediaType, s.Body)
	if err != nil {
		return err
	}
	if string(canon) != string(s.Body) {
		return errors.New("body is not in canonical form")
	}
	return nil
}

// ComputeKey recomputes the key from the scope's own fields, so what a person
// sees is what was hashed.
func (s KubeCallScope) ComputeKey() (string, error) {
	return KubeCallKey(s.Cluster, s.Namespace, s.Method, s.Path, s.Query, s.MediaType, s.Body)
}

// IsKubeCallScope reports whether raw is a kubectl call scope: an object with
// a non-empty call_sha256 and a cluster key.
func IsKubeCallScope(raw json.RawMessage) bool {
	var s struct {
		CallSHA256 string          `json:"call_sha256"`
		Cluster    json.RawMessage `json:"cluster"`
	}
	return json.Unmarshal(raw, &s) == nil && s.CallSHA256 != "" && len(s.Cluster) > 0
}

// KubeRestartPatch reports whether body is exactly
// {"spec":{"template":{"metadata":{"annotations":{"kubectl.kubernetes.io/restartedAt":"<RFC 3339 time>"}}}}}.
// A repeated key at any depth is refused by the strict canonical form.
func KubeRestartPatch(body []byte) bool {
	canon, err := CanonicalJSON(body)
	if err != nil {
		return false
	}
	var v any
	if json.Unmarshal(canon, &v) != nil {
		return false
	}
	for _, key := range []string{"spec", "template", "metadata", "annotations"} {
		m, ok := v.(map[string]any)
		if !ok || len(m) != 1 {
			return false
		}
		if v, ok = m[key]; !ok {
			return false
		}
	}
	m, ok := v.(map[string]any)
	if !ok || len(m) != 1 {
		return false
	}
	at, ok := m[KubeRestartAnnotation].(string)
	if !ok {
		return false
	}
	_, err = time.Parse(time.RFC3339, at)
	return err == nil
}
