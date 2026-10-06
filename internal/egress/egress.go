// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package egress defines the proxy decision model shared by
// cmd/wardyn-proxy and the control plane's policy/approval wiring.
//
// Invariants:
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
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Decision is the outcome of one egress policy evaluation.
type Decision string

const (
	Allow Decision = "allow"
	Deny  Decision = "deny"
	// Pending means the request was held and an egress_domain ApprovalRequest
	// was raised (first-use approval flow). Its wire value is the verb the
	// audit action carries (egress.hold), not the approval's state.
	Pending Decision = "hold"
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
	// Cause is the masked, topology-redacted sentence naming WHY a builtin
	// dial-shaped refusal happened and at what STAGE (TCP dial, TLS
	// handshake, upstream-proxy CONNECT). It runs through the same masking
	// passes a sandbox-facing error body does before reaching this field.
	// Three other rows carry one, each a fixed sentence and not a stage plus
	// an error text: builtin:upstream-protocol-mismatch (that round trip
	// COMPLETED, so it names the protocol answered), builtin:tunnel-failed
	// (what ended a CONNECT tunnel after its 200) and a
	// builtin:resolve-failed written with an upstream proxy configured (the
	// name is bypassed and did not resolve at the proxy). Empty on every
	// other decision.
	Cause string `json:"cause,omitempty"`
	// Via names the CLASS of hop that carried the request, or that a refusal
	// attempted ("upstream-proxy" or "direct"), never an address — Cause's
	// redaction pass exists to strip that, and Via must not reopen the hole.
	// It rides every row that has a Cause, and every allow that followed a
	// forward dial. An allow that dialled nothing (the CONNECT allow of a
	// tunnel the proxy terminates itself) has none.
	Via string `json:"via,omitempty"`
	// UpstreamFault names the AWS error class when Bedrock's data plane
	// refused a call the proxy ALLOWED and relayed, or "recovered" on the
	// first clean call after such a refusal. Read off the response status
	// and x-amzn-ErrorType header only, never a body.
	UpstreamFault string `json:"upstream_fault,omitempty"`
	// Scan, when non-nil, carries the OUTBOUND content-inspection summary for
	// an LLM route decision. A tunneled-opaque LLM CONNECT is recorded as
	// scanned=false ("bypass") so audit cannot imply coverage it lacks.
	Scan *ScanSummary `json:"scan,omitempty"`
	// Repeat, when non-zero, marks this row as the SUMMARY of a streak of
	// IDENTICAL refusals the proxy answered from a memo instead of
	// re-deciding, counting attempts refused without a row of their own. It
	// always arrives as a NEW row: the audit chain is append-only and a
	// recorded decision is never rewritten.
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
	Action     string        `json:"action"`                // "alert" | "block" | "skip" | "bypass" | "fail"
	Channel    string        `json:"channel,omitempty"`     // e.g. "anthropic.messages"
	Skipped    bool          `json:"skipped,omitempty"`     // a span/the body was not fully scanned
	SkipReason string        `json:"skip_reason,omitempty"` // "span_oversize" | "parse_error" | "sidecar_error" | "body_oversize" | "uninspected_channel" | "findings_capped"
	Findings   []ScanFinding `json:"findings,omitempty"`
	// FindingsCapped and FindingsPastCap put the per-request findings cap ON
	// THE WIRE, since Findings above carries at most the cap's worth of rows
	// and a truncated scan would otherwise be indistinguishable from one that
	// found exactly that many. FindingsPastCap is an upper bound on findings
	// pushed out past the cap. FindingsTotal is the number the detectors
	// PRODUCED before truncation (contentscan.Result.FindingsSeen) — not
	// FindingsPastCap plus the reported rows, since block mode's keep-backs
	// would double-count.
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
	// RequireTLS declares that this rule's credential may ride ONLY a
	// transport the proxy runs TLS on: a plaintext connector on :80 is
	// indistinguishable from an https-only vendor the proxy has no table
	// for, so setting this refuses such a request outright (403) rather
	// than silently withholding the credential. Default false = today's
	// behaviour.
	RequireTLS bool `json:"require_tls,omitempty"`
	// PinPath and PinQuery narrow this rule's credential to ONE request
	// shape: a GET of PinPath whose query carries exactly these key=value
	// pairs. Any other request to the same host is forwarded WITHOUT the
	// header. Exists because an injected credential otherwise rides EVERY
	// request to that host — for captured-AWS-SSO that includes `POST
	// /logout` and a GetRoleCredentials for any other account/role the
	// session holds. Both empty = unpinned = every other rule.
	PinPath  string            `json:"pin_path,omitempty"`
	PinQuery map[string]string `json:"pin_query,omitempty"`
	// PinRoutes narrows this rule's credential to a SET of method-and-path
	// pairs: a request carries the credential only when its method and its
	// decoded path equal one entry exactly (no prefix match; the query string
	// is not looked at). Unlike PinPath it admits methods other than GET.
	// When PinPath is also set, a request must satisfy both.
	PinRoutes []PinRoute `json:"pin_routes,omitempty"`
}

// PinRoute is one method-and-path pair an InjectionRule's PinRoutes admits.
type PinRoute struct {
	Method string `json:"method"`
	Path   string `json:"path"`
}

// Pinned reports whether this rule narrows its credential to one request shape.
func (r InjectionRule) Pinned() bool { return r.PinPath != "" || len(r.PinRoutes) > 0 }

// AllowsInjection reports whether a request may carry this rule's credential.
// An UNPINNED rule allows every request, which is what every rule but the
// captured-AWS-SSO one does today.
//
// It takes the raw query, not url.Values: matching on a parsed
// url.Values.Get would accept a second, differently-spelled or
// ';'-separated account/role parameter riding alongside the pinned one,
// forwarded verbatim with the credential attached, since how an origin
// resolves a duplicate parameter is undocumented and unmeasurable offline.
// So the query must be unambiguous by construction: no ';' in the raw query
// at all, every pinned key present EXACTLY once equal to the pin, and no key
// whose literal spelling differs from its decoded one. Other keys are left
// alone — the pin narrows WHICH account and role the session may be spent
// on, not what else a caller may ask for.
func (r InjectionRule) AllowsInjection(method, path, rawQuery string) bool {
	if !r.Pinned() {
		return true
	}
	if len(r.PinRoutes) > 0 {
		if !slices.Contains(r.PinRoutes, PinRoute{Method: method, Path: path}) {
			return false
		}
		if r.PinPath == "" {
			return true
		}
	}
	if method != http.MethodGet || path != r.PinPath {
		return false
	}
	if len(r.PinQuery) == 0 {
		return true
	}
	// ';' is refused on the RAW query, before any parse — defence in depth,
	// since the refusal should not depend on whichever parser is linked (Go
	// 1.17 changed ParseQuery to also reject it, previously a bypass).
	if strings.Contains(rawQuery, ";") {
		return false
	}
	vals, err := url.ParseQuery(rawQuery)
	if err != nil {
		return false
	}
	// One spelling per key: a key whose literal bytes differ from its decoded
	// form (account%5Fid, account+id, ...) is a second way to write a name
	// this rule pins. This is independent of ParseQuery's own decoding, so
	// the refusal covers any encoding trick, not only ones Go normalises.
	for _, pair := range strings.Split(rawQuery, "&") {
		if pair == "" {
			continue
		}
		literal, _, _ := strings.Cut(pair, "=")
		decoded, derr := url.QueryUnescape(literal)
		if derr != nil || decoded != literal {
			return false
		}
	}
	for k, want := range r.PinQuery {
		got := vals[k]
		if len(got) != 1 || got[0] != want {
			return false
		}
	}
	return true
}

// ValidHeaderName reports whether name is a legal HTTP field-name — an RFC
// 9110 token, capped at maxHeaderNameLen.
//
// An InjectionRule's Header is OPERATOR-AUTHORED and written verbatim onto a
// forwarded request, so it is a trust boundary: a name carrying CR/LF is a
// header-splitting shape, and one carrying ':' or a space is malformed. The
// token charset excludes all of those by construction, checked at every
// write boundary and the injection sink. Go's Transport also rejects an
// invalid field name at request-write time, but only at run time after a run
// has started; rejecting here turns the mistake into an immediate 400.
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
