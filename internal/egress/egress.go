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
	RuleSource string     `json:"rule_source"` // "policy" | "approval:<id>" | "builtin:private-ip" | "builtin:resolve-failed" | ...
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
	Action     string        `json:"action"`                // "alert" | "block" | "skipped" | "blind" | "error"
	Channel    string        `json:"channel,omitempty"`     // e.g. "anthropic.messages"
	Skipped    bool          `json:"skipped,omitempty"`     // a span/the body was not fully scanned
	SkipReason string        `json:"skip_reason,omitempty"` // "span_oversize" | "parse_error" | "sidecar_error" | "body_oversize" | "uninspected_channel" | "findings_capped"
	Findings   []ScanFinding `json:"findings,omitempty"`
	// FindingsCapped and FindingsPastCap put the per-request findings cap ON
	// THE WIRE (F075).
	//
	// Findings above carries at most the cap's worth of rows, so a truncated
	// scan used to be indistinguishable in the audit from one that happened to
	// find exactly that many — and when an earlier skip reason (span_oversize,
	// scan_budget) had already claimed SkipReason, nothing said the result was
	// truncated at all. FindingsCapped is that flag; FindingsPastCap is the
	// count of findings examined past the cap (an upper bound on how many were
	// pushed out, since severity keep-backs are counted too — see
	// contentscan.Result.FindingsDropped). FindingsTotal is what an auditor
	// comparing "how much was found" against "how much was reported" needs: the
	// number of findings the detectors PRODUCED before the cap truncated the
	// list, copied from contentscan.Result.FindingsSeen, which COUNTS them as
	// they are produced. It is deliberately not FindingsPastCap + the reported
	// rows: block mode keeps a past-cap finding back into the report while
	// still counting it past the cap, so that sum double-counts every keep-back.
	FindingsCapped  bool `json:"findings_capped,omitempty"`
	FindingsPastCap int  `json:"findings_past_cap,omitempty"`
	FindingsTotal   int  `json:"findings_total,omitempty"`
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

// ValidHeaderName reports whether name is a legal HTTP field-name — an RFC 9110
// token, capped at maxHeaderNameLen.
//
// An InjectionRule's Header is OPERATOR-AUTHORED (an integration's credential
// header, an api_key grant scope in a stored or inline policy) and is written
// verbatim onto a forwarded request by the proxy, so it is a trust boundary:
// a name carrying CR or LF is a header-splitting shape, and one carrying ':'
// or a space is a malformed field-name the peer may parse as something else.
// The token charset excludes all of those by construction — this function is
// the single definition of "a header name Wardyn will put on the wire", run at
// every write boundary AND at the injection sink so no authoring path can
// reach the proxy with one.
//
// Go's own Transport also rejects an invalid field name at request-write time,
// which makes this defense-in-depth rather than the only thing standing
// between an authored header and the wire — but that backstop fails the
// request at run time, after a run has already started. Rejecting at write
// time turns the same mistake into a 400 the operator reads immediately.
func ValidHeaderName(name string) bool {
	if name == "" || len(name) > maxHeaderNameLen {
		return false
	}
	for i := 0; i < len(name); i++ {
		if !headerNameByte[name[i]] {
			return false
		}
	}
	return true
}

// maxHeaderNameLen bounds an authored header name. No real field name comes
// close; the cap exists so a pathological value cannot ride into logs, audit
// details and proxy config.
const maxHeaderNameLen = 128

// headerNameByte is the RFC 9110 `tchar` set: ALPHA / DIGIT / "!#$%&'*+-.^_`|~".
// Note what it excludes — CR, LF, NUL, space, ':' and every other separator.
var headerNameByte = func() [256]bool {
	var ok [256]bool
	for _, c := range []byte("!#$%&'*+-.^_`|~") {
		ok[c] = true
	}
	for c := byte('0'); c <= '9'; c++ {
		ok[c] = true
	}
	for c := byte('a'); c <= 'z'; c++ {
		ok[c] = true
	}
	for c := byte('A'); c <= 'Z'; c++ {
		ok[c] = true
	}
	return ok
}()
