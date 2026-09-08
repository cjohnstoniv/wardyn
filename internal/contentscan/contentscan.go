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

// defaultMaxTotalScanBytes bounds the TOTAL bytes scanned across every span of
// ONE request, on top of the per-span defaultMaxScanBytes cap above. The
// extractors impose no bound on span COUNT (walkTextOrBlocks/walkJSONStrings
// walk every content block / string leaf), so a body split into many sub-cap
// spans burns CPU proportional to the whole body regardless of the per-span
// cap — measured at 8-15s of proxy CPU for a 22-31 MiB body of thousands of
// ~1 KiB spans, each three orders of magnitude under the per-span cap so
// span_oversize never fires (F073). Once the running total crosses this
// budget the scan stops for the rest of the request (fail-open, recorded as
// Skipped{scan_budget}) exactly like an oversize span.
const defaultMaxTotalScanBytes = 4 << 20 // 4 MiB

// defaultMaxFindings bounds the number of findings a single ScanRequest
// REPORTS (the decision log copied verbatim to stdout and POSTed to the
// control-plane audit). Nothing else bounds this: each detector appends per
// span with no cross-span limit, so a body split into many spans can fan out
// into hundreds of thousands of findings (F075; measured 600,000 findings /
// ~104 MiB of decision-log output for one 22.4 MiB request). Once the cap is
// hit, findings past it are dropped (counted in Result.FindingsDropped) and
// the result is marked Skipped{findings_capped} — EXCEPT a finding whose
// Severity is already at or above the engine's blockMin, which is kept (up to
// a hard ceiling) so a request cannot buy a quiet forward by fanning out cheap
// noise ahead of the real secret (B5).
//
// Severity priority applies in EVERY mode (F075 fix-up), by two mechanisms:
// block mode GROWS the report to a hard 2*maxFindings ceiling (enforcement
// evidence must survive), while every other mode DISPLACES a retained
// below-blockMin finding instead, so the report stays exactly maxFindings long.
// Alert mode's only product IS the alert, and dropping by arrival order there
// let 900 cheap entropy findings evict the AKIA key from the row a human reads
// (measured: base alert mode reported the high-severity finding; the capped
// tree reported findings=500 dropped=401, high-or-critical-present=false). The
// decision log is still a truncated view once the cap fires (B7); what the
// truncation keeps is now the part that matters.
// Scanning itself is NOT stopped by this cap — the scan_budget above is what bounds CPU;
// this cap bounds only the size of the audit stream.
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
	SkipReason string    `json:"skip_reason,omitempty"` // "span_oversize" | "parse_error" | "sidecar_error" | "attachment_decode_error" | "scan_budget" | "findings_capped"

	// FindingsDropped counts every finding examined past the per-request cap
	// (defaultMaxFindings) once it was exceeded — this includes a
	// block-relevant finding (severity >= blockMin) that was then kept back
	// (up to a hard ceiling) rather than dropped. The truncation arm in
	// ScanRequest examines each such finding exactly once (B6), so this is an
	// EXACT count of findings EXAMINED past the cap — an UPPER BOUND on how
	// many were actually pushed out of the decision log, exceeding it by
	// however many were kept back (measured, block mode + default
	// block_min_severity: 100 examined with 0 actually dropped for a
	// 600-finding body; 700 examined with 200 dropped for a 1,200-finding
	// one). See B5/F075/B6. No JSON tag here — Result is the in-process shape;
	// the proxy copies this onto egress.ScanSummary.FindingsPastCap, which IS
	// the wire field (F075 fix-up: a capped scan used to be indistinguishable
	// on the wire from a scan that happened to find exactly maxFindings).
	FindingsDropped int `json:"-"`

	// FindingsSeen is the number of findings the detectors PRODUCED across
	// every scanned span, counted once each as they were produced and before
	// the per-request cap trimmed the list. It is the honest answer to "how
	// much did this scan find", and it is a COUNTED fact rather than an
	// inferred one on purpose.
	//
	// The inference it replaces — len(Findings) + FindingsDropped — is wrong in
	// the mode that matters. FindingsDropped counts every finding EXAMINED past
	// the cap, and under ModeBlock capFindings then keeps a block-relevant one
	// back INTO Findings (up to the 2*maxFindings ceiling), so each keep-back is
	// counted on both sides of that sum: measured, a body of 900 entropy
	// findings followed by 300 AKIA keys under block_min_severity=high reported
	// reported=800 dropped=700 for a true total of 1,200 — the sum said 1,500.
	// Block mode is precisely where the audit row is enforcement evidence.
	//
	// PRE-DEDUPE, and that is what "before the cap" means: the cap fires DURING
	// scanning while dedupeFindings runs once at the end, so the population the
	// cap truncated is the pre-dedupe one. An uncapped control engine's reported
	// count therefore equals this exactly for a body with no duplicate
	// (detector, field path, offset, length) positions, and is lower by however
	// many exact duplicates the body contained otherwise. No JSON tag: Result is
	// the in-process shape; the proxy copies this onto
	// egress.ScanSummary.FindingsTotal, which is the wire field.
	FindingsSeen int `json:"-"`

	// FindingsCapped records that the per-request cap TRUNCATED this result,
	// independently of SkipReason. SkipReason holds a single value and the
	// first writer keeps it, so a body whose first span was oversize reported
	// span_oversize and said nothing at all about the truncation that followed
	// (F075 fix-up).
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
	// request (F073); maxFindings bounds the number of findings ONE request may
	// return (F075). Both are built-in safety limits, not policy-configurable.
	maxTotalScanBytes int
	maxFindings       int
	failOpen          bool // on a scanner ERROR (parse_error): allow (true) vs block (false)
	blockMin          Severity
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
		mode:              mode,
		detectors:         dets,
		maxBytes:          maxBytes,
		maxTotalScanBytes: defaultMaxTotalScanBytes,
		maxFindings:       defaultMaxFindings,
		corpus:            filtered,
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

// capFindings enforces the per-request findings cap on res, in place. vetted and
// displace are the arm's cursors, carried across spans by ScanRequest and
// returned updated.
func (e *Engine) capFindings(res *Result, vetted, displace int) (int, int) {
	// F075/B5: cap what this request REPORTS, never what block mode
	// ENFORCES. The CPU bound is scan_budget above, so scanning
	// CONTINUES for the rest of the body (this cap only trims the
	// decision log). Findings past the cap are dropped UNLESS they are
	// at or above blockMin, which are kept up to a hard ceiling —
	// otherwise an agent could fan out cheap noise ahead of its real
	// secret and suppress the finding that matters (B5).
	//
	// The keep-back is NOT conditioned on ModeBlock (F075 fix-up).
	// blockMin is not an enforcement threshold in alert mode, but it is
	// still the operator's statement of "this severity is what I care
	// about", and alert mode's only product is the alert: with the
	// keep-back off, 900 low-severity entropy findings evicted the
	// high-severity key from the alert entirely. Severity priority in
	// every mode; enforcement (ShouldBlock) is untouched.
	//
	// vetted tracks how many findings this arm has already examined
	// across EARLIER spans (B6): without it, every later span whose
	// finding count is still above maxFindings (true on every span
	// once the retained tail sits at maxFindings..2*maxFindings)
	// re-slices and re-walks the SAME already-vetted tail from
	// scratch, making the arm's own cost O(span_count x maxFindings)
	// with span_count unbounded by the scan budget (which counts
	// scanned TEXT, not span count). Starting the walk at vetted
	// (never below maxFindings, so the retained head is never
	// re-examined) means each finding is looked at exactly once.
	start := e.maxFindings
	if vetted > start {
		start = vetted
	}
	// SEVERITY PRIORITY, in every mode (F075 fix-up), but by a different
	// mechanism per mode because the two modes are protecting different
	// things:
	//
	//   - ModeBlock GROWS the report up to a hard 2*maxFindings ceiling
	//     for findings at or above blockMin. What must survive here is
	//     the ENFORCEMENT evidence, and the base's own pins fix that
	//     shape (B5).
	//   - Every other mode DISPLACES instead: a block-relevant finding
	//     past the cap takes the place of a retained finding BELOW
	//     blockMin, so the report stays exactly maxFindings long and
	//     still contains what matters. Alert mode's only product is the
	//     alert, and dropping strictly by arrival order let 900 cheap
	//     entropy findings evict the AKIA key from it entirely
	//     (measured: high-or-critical-present=false on a body the base
	//     alerted on). Appending as block mode does would instead widen
	//     the very cap this arm is.
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
	// FindingsCapped is a FLAG, not a reason, so a leading span_oversize or
	// scan_budget cannot hide the truncation (F075 fix-up): SkipReason holds one
	// value and the first writer keeps it, which made a capped scan behind an
	// earlier skip indistinguishable from a complete one.
	res.FindingsCapped = true
	if !res.Skipped {
		res.Skipped = true
		res.SkipReason = "findings_capped"
	}
	return len(kept), displace
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
	scannedBytes := 0
	vetted := 0 // findings already examined by the truncation arm below
	// displace is that arm's cursor into the RETAINED head: the next position
	// that may still hold a finding below blockMin for a later block-relevant
	// one to take over (see the arm). Carried across spans so each retained
	// position is examined at most once, the same reason vetted exists.
	displace := 0
	scanSpan := func(s Span) {
		if e.maxBytes > 0 && len(s.Text) > e.maxBytes {
			res.Skipped = true
			res.SkipReason = "span_oversize"
			return // skip this oversize span; keep scanning the rest (partial coverage)
		}
		if e.maxTotalScanBytes > 0 && scannedBytes >= e.maxTotalScanBytes {
			// F073: the per-request scan budget is exhausted — stop running
			// detectors over the rest of the body. !res.Skipped mirrors the
			// sidecar guard below: a later budget hit must not clobber an
			// earlier span_oversize/sidecar_error skip's reason.
			if !res.Skipped {
				res.Skipped = true
				res.SkipReason = "scan_budget"
			}
			return
		}
		scannedBytes += len(s.Text)
		// The PRE-CAP total, counted as it is produced (see Result.FindingsSeen):
		// the cap below trims res.Findings in place and block mode keeps some of
		// the trimmed findings back, so after the fact no arithmetic over the
		// surviving slice can recover how many there were.
		beforeDetectors := len(res.Findings)
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
	// Opt-in: also decode+scan base64 attachment bytes (Anthropic only). Best-
	// effort, but a genuine decode failure is recorded honestly (F056) — NOT
	// as a clean scan — so ShouldBlock/on_scanner_error still governs whether
	// an undecodable attachment refuses the request under block mode.
	if e.scanAttachments && channel == ChannelAnthropicMessages {
		if extractAnthropicAttachments(body, scanSpan) && !res.Skipped {
			res.Skipped = true
			res.SkipReason = "attachment_decode_error"
		}
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
			res.SkipReason == "sidecar_error" || res.SkipReason == "attachment_decode_error" ||
			res.SkipReason == "scan_budget" || res.SkipReason == "findings_capped") {
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
