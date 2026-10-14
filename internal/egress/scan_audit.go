// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package egress

import "encoding/json"

const maxAuditFindings = 100

// ScanAudit builds the shared content-free scan event for direct decisions and
// durable runner replay. Findings stay capped at 100; the total stays honest.
func ScanAudit(sc *ScanSummary, host string) (action, outcome string, data json.RawMessage) {
	findings := sc.Findings
	if len(findings) > maxAuditFindings {
		findings = findings[:maxAuditFindings]
	}
	outcome = "success"
	switch sc.Action {
	case "block":
		outcome = "denied"
	case "fail":
		outcome = "failure"
	}
	// finding_count is the number of findings the scan PRODUCED before the cap
	// truncated the list, which the proxy counts as it produces them and
	// sends as findings_total: the proxy caps how many findings it REPORTS, so
	// len(sc.Findings) is the reported count, and an audit row that states it as
	// the finding count makes a truncated scan indistinguishable from one that
	// found exactly the cap. findings_capped / findings_past_cap carry the
	// truncation itself, and findings_past_cap is an UPPER bound on what was
	// pushed out (block mode's severity keep-backs are counted past the cap and
	// still reported), which is why finding_count is not derived from it.
	findingCount := len(sc.Findings)
	if sc.FindingsTotal > findingCount {
		findingCount = sc.FindingsTotal
	}
	data, _ = json.Marshal(map[string]any{
		"host":              host,
		"channel":           sc.Channel,
		"mode":              sc.Mode,
		"coverage":          sc.Coverage,
		"scanned":           sc.Scanned,
		"skipped":           sc.Skipped,
		"skip_reason":       sc.SkipReason,
		"finding_count":     findingCount,
		"findings_reported": len(sc.Findings),
		"findings_capped":   sc.FindingsCapped,
		"findings_past_cap": sc.FindingsPastCap,
		"findings":          findings,
	})
	return "llm.scan." + sc.Action, outcome, data
}
