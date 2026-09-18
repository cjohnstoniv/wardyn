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
	"net/http"
	"net/url"
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
	// Repeat, when non-zero, marks this row as the SUMMARY of a streak of
	// IDENTICAL refusals the proxy answered from a memo instead of re-deciding
	// (today only builtin:private-ip — see privateIPMemo in
	// internal/egress/proxy). It counts the attempts that were refused WITHOUT a
	// row of their own, so the trail says "this happened N more times" rather
	// than either flooding with duplicates or under-reporting silently.
	//
	// It always arrives as a NEW row, never as an update of the row that opened
	// the streak: the audit chain is append-only (migration 0047) and a decision
	// already recorded is not rewritten because it happened again.
	Repeat int `json:"repeat,omitempty"`
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
	// RequireTLS declares that this rule's credential may ride ONLY a transport
	// the proxy runs TLS on (F110's residual half). It is the operator's
	// transport intent, which no other field could carry: injectableTransport
	// (internal/egress/proxy/inject.go) can rule out cleartext to :443 and to a
	// host the proxy itself only ever speaks TLS to, but a plaintext connector on
	// :80 is indistinguishable from an https-only vendor the proxy has no table
	// for — so a `POST http://<that host>/…` was injected. Setting this refuses
	// such a request outright (403, rule_source policy:require-tls) rather than
	// silently withholding the credential.
	//
	// Default false = today's behaviour, so no shipped policy changes meaning.
	RequireTLS bool `json:"require_tls,omitempty"`
	// PinPath and PinQuery narrow this rule's credential to ONE request shape:
	// a GET of PinPath whose query carries exactly these key=value pairs. Any
	// other request to the same host is forwarded WITHOUT the header, and the
	// origin answers it however it answers an unauthenticated call — nothing
	// Wardyn holds is exposed either way.
	//
	// It exists because an injected credential otherwise rides EVERY request the
	// sandbox makes to that host. For the captured-AWS-SSO lane that includes
	// `POST /logout`, which AWS documents as invalidating the owner's server-side
	// sign-in session, and a GetRoleCredentials for any other account/role the
	// session holds. In 0.7.5 the token was resident in the sandbox, so its reach
	// was the same and nothing could narrow it; proxy-side injection is the first
	// point at which an admin-asserted identity can become an enforced one.
	//
	// Both empty = unpinned = today's behaviour, which is every other rule.
	PinPath  string            `json:"pin_path,omitempty"`
	PinQuery map[string]string `json:"pin_query,omitempty"`
}

// Pinned reports whether this rule narrows its credential to one request shape.
func (r InjectionRule) Pinned() bool { return r.PinPath != "" }

// AllowsInjection reports whether a request may carry this rule's credential.
// An UNPINNED rule allows every request, which is what every rule but the
// captured-AWS-SSO one does today.
func (r InjectionRule) AllowsInjection(method, path string, query url.Values) bool {
	if !r.Pinned() {
		return true
	}
	if method != http.MethodGet || path != r.PinPath {
		return false
	}
	for k, want := range r.PinQuery {
		if query.Get(k) != want {
			return false
		}
	}
	return true
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
