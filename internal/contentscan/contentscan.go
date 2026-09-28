// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package contentscan implements Wardyn's OPTIONAL, off-by-default outbound
// content-inspection layer: scans outbound LLM request bodies for known
// secret values before they leave the sandbox boundary and yields a
// CONTENT-FREE finding.
//
// HONEST FRAMING (see threatmodel/THREAT-MODEL.md): a guardrail + visibility
// layer, NOT exfiltration prevention. It catches an HONEST agent including a
// known secret verbatim; a malicious/prompt-injected agent can
// encode/split/encrypt around any scanner, and that residual stands. This
// package never claims "DLP".
//
// Design invariants:
//   - Findings NEVER carry raw matched bytes or a reversible hash; Sample is
//     always the masking placeholder.
//   - The engine is channel-agnostic: only the extractors (extract.go) know
//     a wire schema.
//   - A nil *Engine is a safe no-op; NewEngine returns nil when the mode is
//     off or there's nothing to detect.
package contentscan

import (
	"fmt"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// maskedPlaceholder is the content-free sample emitted for every finding,
// byte-identical to secretmask's placeholder. Deliberately no surrounding
// context and no hash of the secret (a crackable oracle); location fields
// alone prove the hit.
const maskedPlaceholder = "<secret-hidden>"

// defaultMaxScanBytes caps a single extracted text span the engine will
// scan. Over-cap is skipped (fail-open) and recorded, so a multi-MB file
// paste can't turn every model turn into an unbounded regex run.
const defaultMaxScanBytes = 1 << 20 // 1 MiB

// defaultMaxTotalScanBytes bounds the TOTAL bytes scanned across every span
// of ONE request, on top of the per-span defaultMaxScanBytes cap: extractors
// impose no bound on span COUNT, so a body split into many sub-cap spans
// burns CPU proportional to the whole body (measured 8-15s of proxy CPU for
// a 22-31 MiB body of thousands of ~1 KiB spans). Once the running total
// crosses this budget the scan stops for the rest of the request (fail-open,
// Skipped{scan_budget}).
const defaultMaxTotalScanBytes = 4 << 20 // 4 MiB

// defaultMaxFindings bounds the number of findings a single ScanRequest
// REPORTS to the audit. Nothing else bounds this: a body split into many
// spans can fan out into hundreds of thousands of findings (measured
// 600,000 for one 22.4 MiB request). Past the cap, findings are dropped
// (Result.FindingsDropped) and marked Skipped{findings_capped} — EXCEPT a
// finding at or above blockMin, kept up to a hard ceiling so a request can't
// buy a quiet forward by fanning out cheap noise ahead of the real secret.
//
// Severity priority applies in every mode by two mechanisms: block mode
// GROWS the report to a hard 2*maxFindings ceiling (enforcement evidence
// must survive), while every other mode DISPLACES a retained below-blockMin
// finding instead, keeping the report exactly maxFindings long (measured:
// arrival-order dropping let 900 cheap entropy findings evict the one
// high-severity key a human needed to see). Scanning itself is NOT stopped
// by this cap — scan_budget bounds CPU; this cap bounds only the audit
// stream's size.
const defaultMaxFindings = 500

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

	// matchID is a content-free per-corpus-secret discriminator (1-based
	// corpus INDEX of the matched secret; 0 for non-corpus detectors),
	// unexported so it never enters the audit log. Sole use is dedup: two
	// distinct corpus secrets of identical length in the same decoded variant
	// would otherwise collapse and undercount the leak.
	matchID int
}

// Result summarizes a scan of one request body.
type Result struct {
	Scanned    bool      `json:"scanned"`
	Findings   []Finding `json:"findings,omitempty"`
	Skipped    bool      `json:"skipped,omitempty"`     // at least one span/the body was not fully scanned
	SkipReason string    `json:"skip_reason,omitempty"` // "span_oversize" | "parse_error" | "sidecar_error" | "attachment_decode_error" | "scan_budget" | "findings_capped"

	// FindingsDropped counts every finding examined past the per-request cap
	// once exceeded, including a block-relevant finding kept back rather than
	// dropped, so this is an UPPER BOUND on what was pushed out, not an exact
	// count. No JSON tag; the proxy copies it onto
	// egress.ScanSummary.FindingsPastCap, the wire field.
	FindingsDropped int `json:"-"`

	// FindingsSeen is the number of findings the detectors PRODUCED across
	// every scanned span, counted before the per-request cap trimmed the
	// list — a COUNTED fact, since len(Findings)+FindingsDropped
	// double-counts every block-mode keep-back (measured: reported=800
	// dropped=700 for a true total of 1,200, not the sum's 1,500).
	// Pre-dedupe. No JSON tag; the proxy copies it onto
	// egress.ScanSummary.FindingsTotal, the wire field.
	FindingsSeen int `json:"-"`

	// FindingsCapped records that the per-request cap TRUNCATED this result,
	// independent of SkipReason: SkipReason holds a single value that the
	// first writer keeps, so a body whose first span was oversize would
	// otherwise say nothing about the truncation that followed.
	FindingsCapped bool `json:"-"`
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
	Scan(span Span, dst *[]Finding)
}

// Engine runs the enabled detectors over the spans an extractor yields. It holds
// no per-request state and is safe for concurrent use. A nil *Engine is disabled.
type Engine struct {
	mode      Mode
	detectors []Detector
	maxBytes  int
	// maxTotalScanBytes bounds total scanned bytes across every span of ONE
	// request; maxFindings bounds the number of findings ONE request may
	// return. Both are built-in safety limits, not policy-configurable.
	maxTotalScanBytes int
	maxFindings       int
	failOpen          bool // on a scanner ERROR (parse_error): allow (true) vs block (false)
	blockMin          Severity
	// corpus is the filtered operator-declared secret set, held so EVERY
	// finding's FieldPath can be corpus-masked centrally (sanitizeFieldPath)
	// regardless of which detector produced it.
	corpus [][]byte
	// scanAttachments decodes+scans base64 image/document attachment bytes.
	scanAttachments bool
	// inspectForward extends inspection to the generic plaintext-HTTP forward path
	// (the proxy consults this; the engine itself scans whatever channel it's given).
	inspectForward bool
}

// NewEngine builds an Engine from a policy spec and a corpus of known secret
// values. It returns (nil, nil) — a disabled no-op — when the mode is off or
// no detector ends up with anything to match.
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

	// SECURITY (fail-closed): the proxy builds the engine from
	// WARDYN_PROXY_CONFIG_JSON without re-running validatePolicySpec, so a
	// malformed spec reaching the sidecar must not silently disable scanning.
	if !spec.DetectSecrets && !spec.DetectSecretPatterns && !spec.DetectEntropy &&
		!spec.DetectPII && spec.DetectorSidecarURL == "" && len(spec.ClassifiedMarkers) == 0 {
		return nil, fmt.Errorf("contentscan: mode %q requires at least one detector", mode)
	}

	// Filter ONCE and hold on the engine: used both to build the known-secret
	// detector and to corpus-mask every finding's FieldPath centrally, so a
	// corpus secret used as a JSON key is masked even with no corpus detector enabled.
	filtered := filterCorpus(corpus)

	var dets []Detector
	if spec.DetectSecrets {
		// Known-secret exact match needs a non-empty corpus; if empty, this
		// detector is simply omitted (others may still run).
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
		// Configured WITH a detector but nothing active (e.g. detect_secrets
		// with an empty corpus). No-op; NewServer logs it.
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
		mode:              mode,
		detectors:         dets,
		maxBytes:          maxBytes,
		maxTotalScanBytes: defaultMaxTotalScanBytes,
		maxFindings:       defaultMaxFindings,
		corpus:            filtered,
		// Defaults to "pass" (fail-open): a guardrail must not brick the
		// agent's only model path on a scan hiccup.
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
// FieldPath: masks well-known secret FORMATS (sanitizePath) and the
// operator-declared corpus (maskCorpus) so NO detector can carry a raw
// secret, used as an agent-controlled JSON key, into the append-only audit.
// Idempotent.
func (e *Engine) sanitizeFieldPath(path string) string {
	return maskCorpus(sanitizePath(path), e.corpus)
}

// capFindings enforces the per-request findings cap on res, in place. vetted
// and displace are the arm's cursors, carried across spans by ScanRequest
// and returned updated.
func (e *Engine) capFindings(res *Result, vetted, displace int) (int, int) {
	// Caps what this request REPORTS, never what block mode ENFORCES
	// (scanning continues; scan_budget is the CPU bound). Findings past the
	// cap are dropped UNLESS at or above blockMin, kept up to a hard ceiling
	// so an agent can't fan out cheap noise to suppress the finding that
	// matters. Not conditioned on ModeBlock: alert mode's only product is
	// the alert, and without the keep-back 900 low-severity findings could
	// evict the one high-severity key from it.
	//
	// vetted tracks findings this arm already examined across EARLIER spans,
	// or every later span above maxFindings re-walks the already-vetted tail
	// from scratch (O(span_count x maxFindings)). Starting at vetted (never
	// below maxFindings) means each finding is looked at exactly once.
	start := e.maxFindings
	if vetted > start {
		start = vetted
	}
	// Severity priority applies in every mode, by a different mechanism per
	// mode: ModeBlock GROWS the report to a hard 2*maxFindings ceiling
	// (enforcement evidence must survive); every other mode DISPLACES a
	// retained below-blockMin finding instead, keeping the report exactly
	// maxFindings long — appending as block mode does would widen the very
	// cap this arm is.
	kept := res.Findings[:start]
	for _, f := range res.Findings[start:] {
		res.FindingsDropped++
		if severityRank(f.Severity) < severityRank(e.blockMin) {
			continue
		}
		if e.mode == ModeBlock {
			if len(kept) < 2*e.maxFindings {
				kept = append(kept, f)
			}
			continue
		}
		for displace < len(kept) && severityRank(kept[displace].Severity) >= severityRank(e.blockMin) {
			displace++
		}
		if displace < len(kept) {
			kept[displace] = f
			displace++
		}
	}
	res.Findings = kept
	// A FLAG, not a reason, so a leading span_oversize or scan_budget can't
	// hide the truncation (SkipReason holds one value, first writer keeps it).
	res.FindingsCapped = true
	if !res.Skipped {
		res.Skipped = true
		res.SkipReason = "findings_capped"
	}
	return len(kept), displace
}

// ScanRequest extracts spans for channel from body, runs the detectors, and
// returns the Result. The second return value is the (possibly-rewritten)
// body — in this phase always body unchanged (redaction is a later phase).
// A nil or off engine returns an unscanned, empty Result.
//
// A malformed/unknown body is reported as Skipped{parse_error} plus the
// error; the caller decides allow-vs-block via ShouldBlock (honoring fail-open).
func (e *Engine) ScanRequest(channel Channel, body []byte) (res Result, out []byte, err error) {
	if e == nil || e.mode == ModeOff {
		return Result{Scanned: false}, body, nil
	}
	// CENTRAL content-free chokepoint: EVERY finding's FieldPath, whichever
	// detector produced it, is masked here for well-known secret formats and
	// the operator corpus, so a secret used as a JSON key never rides into
	// the audit. Deferred so no return path can bypass it.
	defer func() {
		for i := range res.Findings {
			res.Findings[i].FieldPath = e.sanitizeFieldPath(res.Findings[i].FieldPath)
		}
	}()
	res = Result{Scanned: true}
	scannedBytes := 0
	vetted := 0 // findings already examined by the truncation arm below
	// displace is that arm's cursor into the RETAINED head, carried across
	// spans so each position is examined at most once.
	displace := 0
	scanSpan := func(s Span) {
		if e.maxBytes > 0 && len(s.Text) > e.maxBytes {
			res.Skipped = true
			res.SkipReason = "span_oversize"
			return // skip this oversize span; keep scanning the rest (partial coverage)
		}
		if e.maxTotalScanBytes > 0 && scannedBytes >= e.maxTotalScanBytes {
			// Budget exhausted: stop running detectors over the rest of the
			// body. !res.Skipped guards a later hit from clobbering an
			// earlier skip's reason.
			if !res.Skipped {
				res.Skipped = true
				res.SkipReason = "scan_budget"
			}
			return
		}
		scannedBytes += len(s.Text)
		// The PRE-CAP total, counted as produced (Result.FindingsSeen): the
		// cap below trims res.Findings in place, so no later arithmetic can
		// recover how many there were.
		beforeDetectors := len(res.Findings)
		for _, d := range e.detectors {
			// The sidecar is a NETWORK detector that can fail; unlike
			// in-process detectors its failure must be VISIBLE, so an audit
			// can tell "inspected clean" from "scanner never ran". !res.Skipped
			// guards a later outage from clobbering an earlier skip reason.
			if sc, ok := d.(*sidecarDetector); ok {
				if serr := sc.scanReport(s, &res.Findings); serr != nil && !res.Skipped {
					res.Skipped = true
					res.SkipReason = "sidecar_error"
				}
				continue
			}
			d.Scan(s, &res.Findings)
		}
		res.FindingsSeen += len(res.Findings) - beforeDetectors
		if e.maxFindings > 0 && len(res.Findings) > e.maxFindings {
			vetted, displace = e.capFindings(&res, vetted, displace)
		}
	}
	if err := Extract(channel, body, scanSpan); err != nil {
		res.Skipped = true
		res.SkipReason = "parse_error"
		return res, body, err
	}
	// Opt-in: also decode+scan base64 attachment bytes (Anthropic only).
	// Best-effort, but a genuine decode failure is recorded honestly, not a
	// clean scan.
	if e.scanAttachments && channel == ChannelAnthropicMessages {
		if extractAnthropicAttachments(body, scanSpan) && !res.Skipped {
			res.Skipped = true
			res.SkipReason = "attachment_decode_error"
		}
	}
	res.Findings = dedupeFindings(res.Findings)
	return res, body, nil
}

// ShouldBlock reports whether a Result must cause the request to be
// refused. Only ModeBlock ever blocks. A qualifying finding (severity >=
// blockMin) blocks. A SKIP (uninspectable content) blocks ONLY when
// fail-open is disabled (OnScannerError=block); by default a skip never
// blocks, since oversize/odd bodies are normal for coding agents and
// bricking the only model path would be a self-DoS. "sidecar_error" is
// treated the same: fail-open by default, fail-CLOSED only when the
// operator has explicitly set OnScannerError=block.
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
			res.SkipReason == "sidecar_error" || res.SkipReason == "attachment_decode_error" ||
			res.SkipReason == "scan_budget" || res.SkipReason == "findings_capped") {
		return true
	}
	return false
}

// BlocksOnError reports whether the engine is configured to fail CLOSED on
// content it can't inspect (block mode with OnScannerError=block). The proxy
// uses this for the body-too-large-to-buffer path, which never produces a
// Result to pass to ShouldBlock.
func (e *Engine) BlocksOnError() bool {
	return e != nil && e.mode == ModeBlock && !e.failOpen
}

// filterCorpus copies the corpus, dropping values shorter than
// secretmask.MinLen (the masking layer's own low-false-positive floor) and
// exact dupes.
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

// dedupeFindings removes findings identical in (detector, field path,
// offset, length, matchID), which the raw + normalized passes can otherwise
// both emit. Length and matchID are both in the key because decoded-variant
// hits all carry Offset == -1: without a discriminator, two DISTINCT
// secrets matched inside the same decoded variant would collapse to one and
// undercount the leak. Both content-free: matchID is a position, never the
// matched bytes.
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
