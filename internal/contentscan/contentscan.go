// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package contentscan implements Wardyn's OPTIONAL, off-by-default outbound
// content-inspection layer ("egress content inspection" / inadvertent-leak
// guardrail). It scans outbound LLM request bodies for known secret values
// before they leave the sandbox boundary and yields a CONTENT-FREE finding.
//
// HONEST FRAMING (this is load-bearing — see threatmodel/THREAT-MODEL.md):
// this is a guardrail + visibility layer, NOT exfiltration prevention. It
// catches an HONEST agent that includes a known secret value verbatim (modulo
// the bounded decode-normalization in normalize.go). A malicious / prompt-
// injected agent can encode / split-across-turns / encrypt around any scanner,
// so the encoding-evasion residual STANDS. This package never claims "DLP".
//
// Design invariants:
//   - Findings NEVER carry raw matched bytes and NEVER a reversible hash (the
//     audit log is append-only and SIEM-fanned; a finding must not become a
//     durable copy of the secret). Sample is always the masking placeholder.
//   - The engine is channel-agnostic: only the extractors (extract.go) know a
//     wire schema, so adding a provider/connector is one new extractor.
//   - A nil *Engine is a safe no-op (disabled). NewEngine returns nil when the
//     mode is off or there is nothing to detect.
package contentscan

import (
	"fmt"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// maskedPlaceholder is the content-free sample emitted for every finding. It is
// byte-identical to secretmask's placeholder so masked output is uniform across
// Wardyn. We deliberately do NOT include any surrounding context (which could leak
// adjacent sensitive data) nor any hash of the secret (crackable confirmation
// oracle) — the location fields (field path + offset + length) prove the hit.
const maskedPlaceholder = "<secret-hidden>"

// defaultMaxScanBytes caps the size of a single extracted text span the engine
// will scan. A span larger than this is skipped (fail-open) and recorded, so a
// multi-MB file paste cannot turn every model turn into an unbounded regex run.
const defaultMaxScanBytes = 1 << 20 // 1 MiB

// Mode is the inspection action. Off => the engine is never constructed.
type Mode string

const (
	ModeOff   Mode = "off"
	ModeAlert Mode = "alert" // scan + audit, forward unchanged
	ModeBlock Mode = "block" // scan; a qualifying finding => the request is refused
)

// Category groups findings for audit aggregation.
type Category string

const (
	CategorySecret     Category = "secret"
	CategoryPII        Category = "pii"        // phase 4
	CategoryClassified Category = "classified" // phase 5 (walled-garden)
)

// Severity orders findings for the block threshold.
type Severity string

const (
	SevLow      Severity = "low"
	SevMedium   Severity = "medium"
	SevHigh     Severity = "high"
	SevCritical Severity = "critical"
)

func severityRank(s Severity) int {
	switch s {
	case SevLow:
		return 1
	case SevMedium:
		return 2
	case SevHigh:
		return 3
	case SevCritical:
		return 4
	default:
		return 0
	}
}

// Finding is one detection. It is CONTENT-FREE by construction: Sample is always
// maskedPlaceholder, and only the detector name + location are recorded.
type Finding struct {
	Detector  string   `json:"detector"`
	Category  Category `json:"category"`
	FieldPath string   `json:"field_path"`
	Offset    int      `json:"offset"` // byte offset within the field's text; -1 if from a decoded variant
	Length    int      `json:"length"`
	Severity  Severity `json:"severity"`
	Sample    string   `json:"sample"` // ALWAYS masked; never raw bytes, never a hash

	// matchID is a content-free per-corpus-secret discriminator (the 1-based
	// corpus INDEX of the matched known secret; 0 for non-corpus detectors). It
	// is UNEXPORTED and never JSON-marshaled, so it never enters the audit log —
	// its sole use is dedup: two DISTINCT corpus secrets of identical length in
	// the same decoded variant (same detector/field/offset/length) would
	// otherwise collapse and undercount the leak. It carries the secret's
	// position, never its bytes.
	matchID int
}

// Result summarizes a scan of one request body.
type Result struct {
	Scanned    bool      `json:"scanned"`
	Findings   []Finding `json:"findings,omitempty"`
	Skipped    bool      `json:"skipped,omitempty"`     // at least one span/the body was not fully scanned
	SkipReason string    `json:"skip_reason,omitempty"` // "span_oversize" | "parse_error" | "sidecar_error"
}

// Span is one (field path, text) pair yielded by an extractor.
type Span struct {
	FieldPath string
	Text      string
}

// Detector scans one text span and appends findings. Implementations must be
// safe for concurrent use (the engine is shared across the proxy's request
// handlers) and must never place raw matched bytes into a Finding.
type Detector interface {
	Name() string
	Scan(span Span, dst *[]Finding)
}

// Engine runs the enabled detectors over the spans an extractor yields. It holds
// no per-request state and is safe for concurrent use. A nil *Engine is disabled.
type Engine struct {
	mode      Mode
	detectors []Detector
	maxBytes  int
	failOpen  bool // on a scanner ERROR (parse_error): allow (true) vs block (false)
	blockMin  Severity
	// corpus is the filtered operator-declared secret set, held on the engine so
	// EVERY finding's FieldPath can be corpus-masked centrally (see
	// sanitizeFieldPath) regardless of which detector produced it or whether the
	// known-secret detector is even enabled.
	corpus [][]byte
	// scanAttachments decodes+scans base64 image/document attachment bytes.
	scanAttachments bool
	// inspectForward extends inspection to the generic plaintext-HTTP forward path
	// (the proxy consults this; the engine itself scans whatever channel it's given).
	inspectForward bool
}

// NewEngine builds an Engine from a policy spec and a corpus of known secret
// values (operator-declared workspace secrets plus any proxy-registered
// credentials). It returns (nil, nil) — a disabled no-op — when the mode is off
// or when no detector ends up with anything to match, so callers can treat a nil
// engine as "scanning disabled" exactly like a nil recording store.
func NewEngine(spec types.LLMInspectionSpec, corpus [][]byte) (*Engine, error) {
	mode := Mode(strings.ToLower(strings.TrimSpace(spec.Mode)))
	if mode == "" {
		mode = ModeOff
	}
	switch mode {
	case ModeOff:
		return nil, nil
	case ModeAlert, ModeBlock:
		// ok
	default:
		return nil, fmt.Errorf("contentscan: invalid mode %q", spec.Mode)
	}

	// Fail CLOSED on an invalid detector combination, because the proxy builds
	// the engine from WARDYN_PROXY_CONFIG_JSON WITHOUT re-running validatePolicySpec
	// (the control plane validates, but a malformed spec reaching the sidecar must
	// not silently disable scanning). These mirror validateLLMInspection.
	if !spec.DetectSecrets && !spec.DetectSecretPatterns && !spec.DetectEntropy &&
		!spec.DetectPII && spec.DetectorSidecarURL == "" && len(spec.ClassifiedMarkers) == 0 {
		return nil, fmt.Errorf("contentscan: mode %q requires at least one detector", mode)
	}

	// Filter ONCE and hold on the engine: used both to build the known-secret
	// detector and to corpus-mask every finding's FieldPath centrally, so a corpus
	// secret used as an agent-controlled JSON key is masked even when only a
	// non-corpus detector (pii/regex/entropy/classify/sidecar) is enabled.
	filtered := filterCorpus(corpus)

	var dets []Detector
	if spec.DetectSecrets {
		// Known-secret exact match needs a non-empty corpus to do anything; if it
		// is empty, this detector is simply omitted (other detectors may still run).
		if len(filtered) > 0 {
			dets = append(dets, &knownSecretDetector{secrets: filtered, normalize: true})
		}
	}
	if spec.DetectSecretPatterns {
		dets = append(dets, regexSecretDetector{})
	}
	if spec.DetectEntropy {
		dets = append(dets, entropyDetector{})
	}
	if spec.DetectPII {
		dets = append(dets, piiDetector{})
	}
	if cd, ok := newClassifyDetector(spec.ClassifiedMarkers); ok {
		dets = append(dets, cd)
	}
	if spec.DetectorSidecarURL != "" {
		dets = append(dets, newSidecarDetector(spec.DetectorSidecarURL, nil))
	}
	if len(dets) == 0 {
		// Configured WITH a detector but nothing active (e.g. only detect_secrets
		// and an empty corpus). No-op rather than error; NewServer logs it so the
		// operator sees inspection is effectively disabled.
		return nil, nil
	}

	maxBytes := spec.MaxScanBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxScanBytes
	}
	blockMin := Severity(strings.ToLower(strings.TrimSpace(spec.BlockMinSeverity)))
	if blockMin == "" {
		blockMin = SevLow
	}
	return &Engine{
		mode:      mode,
		detectors: dets,
		maxBytes:  maxBytes,
		corpus:    filtered,
		// OnScannerError defaults to "pass" (fail-open) — a guardrail must not
		// brick the agent's only path to the model on a scan hiccup.
		failOpen:        !strings.EqualFold(strings.TrimSpace(spec.OnScannerError), "block"),
		blockMin:        blockMin,
		scanAttachments: spec.ScanAttachments,
		inspectForward:  spec.InspectForwardEgress,
	}, nil
}

// InspectForwardEgress reports whether the proxy should also scan the generic
// plaintext-HTTP forward path (not just the LLM routes). Nil engine => false.
func (e *Engine) InspectForwardEgress() bool {
	return e != nil && e.inspectForward
}

// Mode reports the engine's configured mode (ModeOff for a nil engine).
func (e *Engine) Mode() Mode {
	if e == nil {
		return ModeOff
	}
	return e.mode
}

// sanitizeFieldPath is the single content-free guarantee for a Finding's
// FieldPath: it masks well-known secret FORMATS (sanitizePath) and the
// operator-declared corpus (maskCorpus) so NO detector can carry a raw secret —
// used as an agent-controlled JSON key — into the append-only audit. Idempotent.
func (e *Engine) sanitizeFieldPath(path string) string {
	return maskCorpus(sanitizePath(path), e.corpus)
}

// ScanRequest extracts spans for channel from body, runs the detectors, and
// returns the Result. The second return value is the (possibly-rewritten) body —
// in this phase it is always body unchanged (redaction is a later phase). A nil
// or off engine returns an unscanned, empty Result.
//
// A malformed/unknown body is reported as Skipped{parse_error} plus the error;
// the caller decides allow-vs-block via ShouldBlock (honoring fail-open).
func (e *Engine) ScanRequest(channel Channel, body []byte) (res Result, out []byte, err error) {
	if e == nil || e.mode == ModeOff {
		return Result{Scanned: false}, body, nil
	}
	// CENTRAL content-free chokepoint. EVERY finding's FieldPath — no matter which
	// detector produced it (pii, regex, entropy, classify, sidecar, known-secret)
	// or on which return path — is masked here for well-known secret FORMATS *and*
	// the operator corpus, so a secret used as an agent-controlled JSON key can
	// never ride into the append-only, SIEM-fanned audit. Idempotent: an
	// already-masked path is unchanged (the secret bytes are gone). Deferred so no
	// current or future return path can bypass it. Only reached for a non-nil
	// engine (registered after the nil/off early return above).
	defer func() {
		for i := range res.Findings {
			res.Findings[i].FieldPath = e.sanitizeFieldPath(res.Findings[i].FieldPath)
		}
	}()
	res = Result{Scanned: true}
	scanSpan := func(s Span) {
		if e.maxBytes > 0 && len(s.Text) > e.maxBytes {
			res.Skipped = true
			res.SkipReason = "span_oversize"
			return // skip this oversize span; keep scanning the rest (partial coverage)
		}
		for _, d := range e.detectors {
			// The sidecar is a NETWORK detector that can fail (build/timeout/non-200/
			// decode). Unlike the in-process detectors its failure must be VISIBLE:
			// record it as a degraded scan so an audit can tell "inspected clean" from
			// "scanner never ran" (FIX #24). Whether that skip BLOCKS is decided later
			// by ShouldBlock per OnScannerError (fail-open by default). The !res.Skipped
			// guard is load-bearing: SkipReason is shared across spans, so a sidecar
			// outage on a later span must NOT clobber an earlier span's block-eligible
			// skip (e.g. span_oversize) and silently downgrade it to fail-open.
			if sc, ok := d.(*sidecarDetector); ok {
				if serr := sc.scanReport(s, &res.Findings); serr != nil && !res.Skipped {
					res.Skipped = true
					res.SkipReason = "sidecar_error"
				}
				continue
			}
			d.Scan(s, &res.Findings)
		}
	}
	if err := Extract(channel, body, scanSpan); err != nil {
		res.Skipped = true
		res.SkipReason = "parse_error"
		return res, body, err
	}
	// Opt-in: also decode+scan base64 attachment bytes (Anthropic only). Best-
	// effort — undecodable blocks are simply skipped.
	if e.scanAttachments && channel == ChannelAnthropicMessages {
		extractAnthropicAttachments(body, scanSpan)
	}
	res.Findings = dedupeFindings(res.Findings)
	return res, body, nil
}

// ShouldBlock reports whether a Result must cause the request to be refused.
// Only ModeBlock ever blocks. A qualifying finding (severity >= blockMin) blocks.
// A SKIP (parse_error or span_oversize — content that could not be inspected)
// blocks ONLY when fail-open is disabled (OnScannerError=block); by default
// (fail-open) a skip never blocks, since oversize/odd bodies are normal for
// coding agents and bricking the only model path would be a self-DoS.
//
// A "sidecar_error" skip (FIX #24) is treated like parse_error/span_oversize: a
// sidecar outage IS "the scanner errored", so it respects OnScannerError — fail-open
// by DEFAULT (never bricks the model path on a sidecar hiccup), but fail-CLOSED when
// the operator has explicitly set OnScannerError=block (they bought that availability
// loss deliberately, and the network sidecar is exactly the detector they set it for).
// Code and the OnScannerError contract (types.go) must agree; an asymmetry here would
// be code weaker than the documented claim.
func (e *Engine) ShouldBlock(res Result) bool {
	if e == nil || e.mode != ModeBlock {
		return false
	}
	for _, f := range res.Findings {
		if severityRank(f.Severity) >= severityRank(e.blockMin) {
			return true
		}
	}
	if res.Skipped && !e.failOpen &&
		(res.SkipReason == "parse_error" || res.SkipReason == "span_oversize" ||
			res.SkipReason == "sidecar_error") {
		return true
	}
	return false
}

// BlocksOnError reports whether the engine is configured to fail CLOSED on
// content it cannot inspect (block mode with OnScannerError=block). The proxy
// uses this for the body-too-large-to-buffer path, which never produces a
// Result to pass to ShouldBlock.
func (e *Engine) BlocksOnError() bool {
	return e != nil && e.mode == ModeBlock && !e.failOpen
}

// filterCorpus copies the corpus, dropping values shorter than secretmask.MinLen
// (the same low-false-positive floor the masking layer uses) and exact dupes.
func filterCorpus(corpus [][]byte) [][]byte {
	seen := make(map[string]struct{}, len(corpus))
	out := make([][]byte, 0, len(corpus))
	for _, v := range corpus {
		if len(v) < secretmask.MinLen {
			continue
		}
		if _, ok := seen[string(v)]; ok {
			continue
		}
		seen[string(v)] = struct{}{}
		cp := make([]byte, len(v))
		copy(cp, v)
		out = append(out, cp)
	}
	return out
}

// dedupeFindings removes findings identical in (detector, field path, offset,
// length), which the raw + normalized passes can otherwise both emit.
//
// Length and matchID are part of the key because decoded-variant hits all carry
// Offset == -1 (offsets are meaningless in the decoded space, detectors.go):
// without a discriminator, two DISTINCT secrets matched inside the SAME decoded
// variant (same detector chain, same field) would collapse to one and UNDERCOUNT
// the leak. matchID (the corpus index of the matched known secret) fully
// separates distinct CORPUS secrets even when they share a length — same-provider
// API keys, the common multi-secret case. Length remains in the key for
// non-corpus detectors (matchID == 0). Both are content-free: matchID is a
// position, never the matched bytes; the Sample is always the masked placeholder.
func dedupeFindings(in []Finding) []Finding {
	if len(in) < 2 {
		return in
	}
	type key struct {
		d string
		f string
		o int
		l int
		m int
	}
	seen := make(map[key]struct{}, len(in))
	out := in[:0]
	for _, f := range in {
		k := key{f.Detector, f.FieldPath, f.Offset, f.Length, f.matchID}
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, f)
	}
	return out
}
