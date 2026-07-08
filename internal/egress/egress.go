// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package egress defines the L2 proxy decision model shared by
// cmd/wardyn-proxy and the control plane's policy/approval wiring.
//
// INVARIANTS:
//   - Default deny. An empty policy allows nothing.
//   - DeniedDomains always beats AllowedDomains.
//   - Private/link-local/metadata IPs are unconditionally denied (DNS
//     rebinding guard), regardless of policy.
//   - Every decision (allow/deny/pending) emits a structured decision log.
//   - Credential injection happens here, proxy-side, and injection rules
//     never widen egress: a host must independently pass the allowlist.
package egress

import (
	"time"

	"github.com/google/uuid"
)

// Decision is the outcome of one egress policy evaluation.
type Decision string

const (
	Allow Decision = "allow"
	Deny  Decision = "deny"
	// Pending means the request was held and an egress_domain ApprovalRequest
	// was raised (first-use approval flow).
	Pending Decision = "pending"
)

// Request is one normalized outbound attempt observed at the proxy.
type Request struct {
	RunID  uuid.UUID
	Host   string // lowercased hostname, no port
	Port   int
	Method string // HTTP method, or "CONNECT" for tunneled TLS
	Path   string // empty for CONNECT (hostname-only visibility)
	Time   time.Time
}

// DecisionLog is the structured record streamed to the control plane for
// every decision. It feeds both audit and the first-use approval queue.
type DecisionLog struct {
	Request    Request    `json:"request"`
	Decision   Decision   `json:"decision"`
	RuleSource string     `json:"rule_source"` // "policy" | "approval:<id>" | "builtin:private-ip" | ...
	ApprovalID *uuid.UUID `json:"approval_id,omitempty"`
	// Scan, when non-nil, carries the OUTBOUND content-inspection summary for an
	// LLM route decision (off-by-default; nil when inspection is disabled). It
	// makes per-decision coverage honest: a tunneled-opaque LLM CONNECT is
	// recorded as scanned=false ("blind") so audit cannot imply coverage it does
	// not have. The control plane turns it into an llm.scan.* audit event.
	Scan *ScanSummary `json:"scan,omitempty"`
}

// ScanSummary is the CONTENT-FREE summary of one LLM content-inspection pass.
// It NEVER carries raw matched bytes; ScanFinding.Sample is always a masking
// placeholder. This is what lands in the append-only, SIEM-fanned audit log, so
// it must not become a durable copy of a secret.
type ScanSummary struct {
	Scanned    bool          `json:"scanned"`
	Coverage   string        `json:"coverage"`              // "inspectable" | "tunneled-opaque"
	Mode       string        `json:"mode,omitempty"`        // "alert" | "block"
	Action     string        `json:"action"`                // "alert" | "block" | "skipped" | "blind"
	Channel    string        `json:"channel,omitempty"`     // e.g. "anthropic.messages"
	Skipped    bool          `json:"skipped,omitempty"`     // a span/the body was not fully scanned
	SkipReason string        `json:"skip_reason,omitempty"` // "span_oversize" | "parse_error" | "sidecar_error" | "body_oversize" | "uninspected_channel"
	Findings   []ScanFinding `json:"findings,omitempty"`
}

// ScanFinding is one content-free detection record (detector + location only).
type ScanFinding struct {
	Detector  string `json:"detector"`
	Category  string `json:"category"`
	FieldPath string `json:"field_path"`
	Offset    int    `json:"offset"`
	Length    int    `json:"length"`
	Severity  string `json:"severity"`
	Sample    string `json:"sample"` // masking placeholder only — never raw bytes
}

// InjectionRule rewrites matching outbound requests to carry a credential
// that never existed inside the sandbox.
type InjectionRule struct {
	Host   string `json:"host"`   // exact host this rule applies to
	Header string `json:"header"` // e.g. "Authorization"
	// SecretName resolves via the broker at injection time (late binding).
	SecretName string `json:"secret_name"`
	// Format wraps the secret, e.g. "Bearer %s".
	Format string `json:"format"`
}
