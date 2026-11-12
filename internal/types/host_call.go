// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// The call key of one host rule hold a person decides.
//
// A host rule with effect hold makes the proxy hold a request to that host,
// method and path until a person decides. The approval names the host, the
// method and the path in normal form (NormalizeRequestPath), never the query or
// the body: a yes covers every request with that host, method and path for the
// rest of the run in that proxy, and a no refuses them all. The proxy that
// raises the approval and the control plane that checks it compute the key
// here, so they cannot disagree.

// HostCallScope is the requested_scope of a tool_call approval for one host
// rule hold. It is known structurally by call_sha256 together with host
// (IsHostCallScope), with no grant, and only the proxy writes it.
type HostCallScope struct {
	// Host is the request's host, lowercase and without a port.
	Host string `json:"host"`
	// Method is the request's method, uppercase.
	Method string `json:"method"`
	// Path is the request's path in normal form, without its query.
	Path       string `json:"path"`
	CallSHA256 string `json:"call_sha256"`
	// HoldSeconds is how long the proxy holds the open request for the answer.
	HoldSeconds int `json:"hold_seconds,omitempty"`
}

// HostCallKey is the identity of one held host, method and path: the hex
// SHA-256 of the three, each length-prefixed.
func HostCallKey(host, method, path string) string {
	h := sha256.New()
	for _, part := range []string{"wardyn.host-call.v1", host, method, path} {
		fmt.Fprintf(h, "%d:", len(part))
		h.Write([]byte(part))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Validate refuses a scope a host rule could not have held: a host, method or
// path not written the way a rule names them (lowercase host with no port, an
// uppercase method token, a path in normal form), or a negative hold.
func (s HostCallScope) Validate() error {
	if err := validHostRule(HostRule{Host: s.Host, Methods: []string{s.Method}, Paths: []string{s.Path}, Effect: HostRuleAllow}); err != nil {
		return err
	}
	if s.HoldSeconds < 0 {
		return fmt.Errorf("hold_seconds: %d is negative", s.HoldSeconds)
	}
	return nil
}

// ComputeKey recomputes the key from the scope's own fields, so what a person
// sees is what was hashed.
func (s HostCallScope) ComputeKey() string { return HostCallKey(s.Host, s.Method, s.Path) }

// IsHostCallScope reports whether raw is a host call scope: an object with a
// non-empty call_sha256 and a host key, and no cluster key (IsKubeCallScope).
func IsHostCallScope(raw json.RawMessage) bool {
	var s struct {
		CallSHA256 string          `json:"call_sha256"`
		Host       json.RawMessage `json:"host"`
	}
	return json.Unmarshal(raw, &s) == nil && s.CallSHA256 != "" && len(s.Host) > 0 && !IsKubeCallScope(raw)
}
