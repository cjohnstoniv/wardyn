// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package contentscan

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestFieldPathIsBoundedInBytes pins the F075 fix-up's first half: the
// per-request cap bounds the NUMBER of findings, and nothing bounded their
// SIZE.
//
// A Finding's FieldPath is built by walkValue (extract.go) as `path + "." +
// key` out of AGENT-CONTROLLED JSON keys — sanitized for secret formats, never
// truncated — and the scan budget counts span TEXT (values), so a body of
// enormous KEYS burns neither limit. Measured on the shipped code: a
// 324,654-byte body of 301 long-key leaves produced 301 findings (cap 500 never
// fires, Skipped=false, SkipReason="") whose marshalled findings JSON was
// 46,323,600 bytes — 143x the body, and 44x internal/api/helpers.go's 1 MiB
// maxJSONBody, so the audit POST is refused and the whole line is still
// mirrored to the proxy's stdout.
func TestFieldPathIsBoundedInBytes(t *testing.T) {
	eng := newTestEngine(t, "alert", testSecret)

	// 301 leaves, each under a ~1 KiB key: keys the scan budget never counts.
	const leaves = 301
	longKey := strings.Repeat("k", 1024)
	var sb strings.Builder
	sb.WriteString("{")
	for i := range leaves {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `"%s%d":{"%s%d":"leak %s"}`, longKey, i, longKey, i, testSecret)
	}
	sb.WriteString("}")
	body := sb.String()

	res, _, err := eng.ScanRequest(ChannelGeneric, []byte(body))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(res.Findings) == 0 {
		t.Fatal("no findings — the probe body must actually produce them")
	}
	for _, f := range res.Findings {
		if len(f.FieldPath) > maxFieldPathBytes {
			t.Fatalf("a finding's FieldPath is %d bytes (cap %d): the audit stream has to be bounded in "+
				"BYTES, not only in rows — the agent authors these keys", len(f.FieldPath), maxFieldPathBytes)
		}
	}
	marshalled, err := json.Marshal(res.Findings)
	if err != nil {
		t.Fatalf("marshal findings: %v", err)
	}
	// maxJSONBody, internal/api/helpers.go — the ceiling the control plane's
	// MaxBytesReader enforces on the decision POST. Cited, not imported: the
	// scanner must not depend on the API package.
	const maxJSONBody = 1 << 20
	if len(marshalled) >= maxJSONBody {
		t.Fatalf("marshalled findings = %d bytes for a %d-byte body (%.0fx): over the control plane's "+
			"%d-byte limit, so the decision POST is refused and the audit record is LOST while the proxy "+
			"still mirrors the whole line to stdout", len(marshalled), len(body),
			float64(len(marshalled))/float64(len(body)), maxJSONBody)
	}
}

// TestFieldPathTruncationKeepsBothEndsAndIsIdempotent: a truncated path must
// still read as the path it came from (root and leaf), say that it was cut, and
// never carry a split UTF-8 sequence.
func TestFieldPathTruncationKeepsBothEndsAndIsIdempotent(t *testing.T) {
	long := "root." + strings.Repeat("é", 4000) + ".leaf"
	got := sanitizePath(long)
	if len(got) > maxFieldPathBytes {
		t.Fatalf("sanitizePath returned %d bytes, want <= %d", len(got), maxFieldPathBytes)
	}
	if !strings.HasPrefix(got, "root.") || !strings.HasSuffix(got, ".leaf") {
		t.Fatalf("truncated path = %q, want both ends kept so an operator can still navigate it", got)
	}
	if !strings.Contains(got, "B]") {
		t.Fatalf("truncated path = %q, want it to STATE that it was cut", got)
	}
	if !isValidUTF8(got) {
		t.Fatalf("truncated path %q is not valid UTF-8 — the audit stream must never carry a split rune", got)
	}
	if again := sanitizePath(got); again != got {
		t.Fatalf("sanitizePath is not idempotent on a truncated path: %q -> %q", got, again)
	}
}

func isValidUTF8(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}

// TestAlertModeKeepsTheHighSeverityFindingBehindCheapNoise pins the F075
// fix-up's second half: severity priority in EVERY mode.
//
// The keep-back used to be `e.mode == ModeBlock && …`, so in alert mode —
// whose ONLY product is the alert — findings were dropped strictly by arrival
// order. An agent that fans out cheap low-severity noise ahead of its real
// secret evicted the high-severity finding from the row a human reads:
// measured findings=500 dropped=401 with high-or-critical-present=false, on a
// body the pre-cap base alerted on.
//
// The report must stay bounded (the cap is the point) AND contain the finding
// that matters.
func TestAlertModeKeepsTheHighSeverityFindingBehindCheapNoise(t *testing.T) {
	// An operator who said "high severity is what I care about", alert mode, and
	// a noise detector that produces something BELOW that — the shape the
	// keep-back exists for. (With the default block_min_severity of low, every
	// finding qualifies and there is nothing to prioritise.)
	eng, err := NewEngine(types.LLMInspectionSpec{
		Mode:             "alert",
		DetectSecrets:    true,
		DetectPII:        true,
		BlockMinSeverity: string(SevHigh),
	}, [][]byte{[]byte(testSecret)})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	var sb strings.Builder
	sb.WriteString(`{"noise":[`)
	for i := range 900 {
		if i > 0 {
			sb.WriteString(",")
		}
		// Email addresses: the PII detector reports them SevLow, i.e. below
		// the operator's threshold — cheap noise, exactly what an agent can
		// fan out at will.
		fmt.Fprintf(&sb, `"noise%04d@example.test"`, i)
	}
	sb.WriteString(`],"z_last":"leak `)
	sb.WriteString(testSecret)
	sb.WriteString(`"}`)

	res, _, err := eng.ScanRequest(ChannelGeneric, []byte(sb.String()))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(res.Findings) > eng.maxFindings {
		t.Fatalf("alert-mode report = %d findings, want at most the cap (%d): severity priority must "+
			"REPLACE, not widen the cap", len(res.Findings), eng.maxFindings)
	}
	var kept bool
	for _, f := range res.Findings {
		if severityRank(f.Severity) >= severityRank(eng.blockMin) {
			kept = true
			break
		}
	}
	if !kept {
		t.Fatalf("no finding at or above block_min_severity (%s) survived the cap in ALERT mode "+
			"(findings=%d dropped=%d): cheap noise fanned out ahead of the real secret evicted the only "+
			"finding the alert exists for", eng.blockMin, len(res.Findings), res.FindingsDropped)
	}
}

// TestFindingsCappedIsRecordedBehindAnEarlierSkipReason pins the F075 fix-up's
// fourth item: SkipReason holds ONE value and the first writer keeps it, so a
// body whose first span was oversize reported span_oversize and said nothing at
// all about the truncation that followed. The flag has to be separate.
func TestFindingsCappedIsRecordedBehindAnEarlierSkipReason(t *testing.T) {
	eng := newTestEngine(t, "alert", testSecret)

	// One span over the per-span cap (sets span_oversize first), then enough
	// leaves to blow the findings cap.
	var sb strings.Builder
	sb.WriteString(`{"a_huge":"`)
	sb.WriteString(strings.Repeat("A", eng.maxBytes+1))
	sb.WriteString(`"`)
	for i := range 700 {
		fmt.Fprintf(&sb, `,"k%d":"leak %s"`, i, testSecret)
	}
	sb.WriteString("}")

	res, _, err := eng.ScanRequest(ChannelGeneric, []byte(sb.String()))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if res.SkipReason != "span_oversize" {
		t.Fatalf("SkipReason = %q, want span_oversize (the premise: an EARLIER reason claimed the field)",
			res.SkipReason)
	}
	if !res.FindingsCapped {
		t.Fatalf("FindingsCapped = false while %d findings were dropped past the cap: a leading skip "+
			"reason must not hide the truncation from the audit", res.FindingsDropped)
	}
}
