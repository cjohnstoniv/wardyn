// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"strings"
	"time"

	"github.com/google/uuid"
)

// MCPOperationKeyMeta is the params._meta member an approve_each MCP call carries its operation key in
// (MCPCallScope.OperationKey). The run proxy reads it and never sends it to the server.
const MCPOperationKeyMeta = "wardyn.dev/operation-key"

// MaxOperationKeyLen is the longest operation key, in bytes: the limit of a run's correlation ID.
const MaxOperationKeyLen = 128

// The reasons of the control plane's 409 for a keyed MCP call: its key was already sent in its
// namespace, with this call (MCPOperationAlreadySent) or with another (MCPOperationKeyReused).
// internal/api's reason constants carry the same values.
const (
	MCPOperationAlreadySent = "operation_already_sent"
	MCPOperationKeyReused   = "operation_key_reused"
)

// ValidOperationKey reports whether s is an operation key: 1 to MaxOperationKeyLen ASCII letters, digits
// or any of . _ : / -, the characters a run's correlation ID may hold, so a correlation ID joined to a
// step name is a key.
func ValidOperationKey(s string) bool {
	if len(s) == 0 || len(s) > MaxOperationKeyLen {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("._:/-", c) >= 0) {
			return false
		}
	}
	return true
}

// DerivedOperationKeyPrefix begins an operation key a run's proxy makes from a call under send once: the prefix,
// the call's hash, a colon and its occurrence. A caller's own key never starts with it (the proxy refuses one).
const DerivedOperationKeyPrefix = "call:"

// DerivedOperationKeyFits reports whether key is either not of the made form, or exactly a key made from
// callSHA256: DerivedOperationKeyPrefix, callSHA256, ":" and a positive number with no leading zero.
func DerivedOperationKeyFits(key, callSHA256 string) bool {
	rest, ok := strings.CutPrefix(key, DerivedOperationKeyPrefix)
	if !ok {
		return true
	}
	n, ok := strings.CutPrefix(rest, callSHA256+":")
	if !ok || callSHA256 == "" || n == "" || n[0] == '0' {
		return false
	}
	for _, c := range n {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// MCPCallSentAnswer is the control plane's answer about a keyed MCP call's send. On a 200 to the send
// record it names the send just recorded; on a 409 operation_already_sent, the earlier send, with
// Answered set once its server's answer was recorded. A 409 operation_key_reused names none.
type MCPCallSentAnswer struct {
	Error      string    `json:"error,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	ApprovalID uuid.UUID `json:"approval_id"`
	SentAt     time.Time `json:"sent_at"`
	Answered   bool      `json:"answered"`
}

// MCPCallSendWindow is how long a keyed MCP call's send answers for its key: until then a later call with
// the key in its namespace is answered with the send, and from then on it is a new call that a person is
// asked about. A constant, stated in the docs.
const MCPCallSendWindow = 24 * time.Hour

// MCPCallSendLive reports whether a send recorded at sentAt still answers for its key at now, both read
// on the database's clock: less than MCPCallSendWindow has passed, so at exactly the window it no longer
// does. A now before sentAt, a clock that stepped back, still answers.
func MCPCallSendLive(sentAt, now time.Time) bool { return now.Sub(sentAt) < MCPCallSendWindow }
