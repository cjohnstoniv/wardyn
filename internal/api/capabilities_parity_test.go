// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The capability matcher ships TWICE — capValueMatches /
// capValueOverlaps here, and capabilityValueMatches / capabilityValueOverlaps in
// ui/src/app/lib/capabilities.ts, which the member "why was this denied" surface
// reads. Two matchers that disagree is the bug the Go side's own comment refuses
// to have inside one language ("two host matchers that disagree is how a deny
// gets bypassed by a port suffix"); across two languages nothing stopped it.
//
// The console already keeps the table: capabilities.test.ts EXPORTS
// CAP_MATCH_CASES and CAP_OVERLAP_CASES precisely so a second consumer could
// read them. This is that consumer. One table, two readers, so the next change
// to either matcher reds on the side that was NOT edited.
//
// ponytail: the Go side PARSES the TS literal rather than either side reading a
// shared JSON fixture. The rows are already written down in the file whose own
// vitest suite runs them, a fixture would be a third artifact to keep in step,
// and the shape is four fixed columns. If a row ever needs to carry anything
// but (string, string, string, bool), move both sides onto a fixture then.
const capParityTableFile = "../../ui/src/app/lib/capabilities.test.ts"

var (
	capParityArray = regexp.MustCompile(`(?s)export const (CAP_MATCH_CASES|CAP_OVERLAP_CASES): Array<\[string, string, string, boolean\]> = \[(.*?)\n\];`)
	capParityRow   = regexp.MustCompile(`\["([^"]*)",\s*"([^"]*)",\s*"([^"]*)",\s*(true|false)\]`)
)

func TestCapabilityMatcherParityWithConsole(t *testing.T) {
	b, err := os.ReadFile(capParityTableFile)
	if err != nil {
		t.Fatalf("read %s: %v", capParityTableFile, err)
	}
	blocks := capParityArray.FindAllStringSubmatch(string(b), -1)
	if len(blocks) != 2 {
		t.Fatalf("parsed %d exported case tables from %s, want CAP_MATCH_CASES and CAP_OVERLAP_CASES — "+
			"the table moved or changed shape, which is exactly what this test exists to notice", len(blocks), capParityTableFile)
	}
	for _, block := range blocks {
		name, body := block[1], block[2]
		answer := capValueMatches
		if name == "CAP_OVERLAP_CASES" {
			answer = capValueOverlaps
		}
		rows := capParityRow.FindAllStringSubmatch(body, -1)
		if len(rows) == 0 {
			t.Fatalf("%s parsed to zero rows — this guard would pass vacuously", name)
		}
		// A row the regexp cannot read is a row this guard silently does not
		// check, which is the failure mode a parser-based pin has and a fixture
		// does not: a formatter wrapping one row across lines, or a row written
		// with a template literal, would shrink the table without failing.
		// `["` opens a row and nothing else in these two literals, so counting
		// it is the cheap independent census.
		if opens := strings.Count(body, `["`); opens != len(rows) {
			t.Fatalf("%s has %d rows but %d parsed — a row this test cannot read is a row it does not check; "+
				"re-read %s and teach the row regexp its shape", name, opens, len(rows), capParityTableFile)
		}
		for i, row := range rows {
			kind, grant, want, expected := row[1], row[2], row[3], row[4] == "true"
			if got := answer(kind, grant, want); got != expected {
				t.Errorf("capability matcher parity: %s row %d (%s,%q,%q) expects %v, Go answered %v — "+
					"a change to one matcher belongs in the other in the same commit",
					name, i, kind, grant, want, expected, got)
			}
		}
		t.Logf("%s: %d rows agree", name, len(rows))
	}
	// The console's list of kinds is the other half of the mirror, and it has no
	// Go pin anywhere else (TestCapabilityKindsAreTheClosedSet compares
	// capabilityKinds to a hard-coded Go want list, and only NAMES the TS file).
	copyTS, err := os.ReadFile("../../ui/src/app/lib/permissions-copy.ts")
	if err != nil {
		t.Fatalf("read permissions-copy.ts: %v", err)
	}
	for _, kind := range capabilityKinds {
		if !strings.Contains(string(copyTS), `"`+kind+`"`) {
			t.Errorf("capability kind %q ships in capabilityKinds but ui/src/app/lib/permissions-copy.ts "+
				"names it nowhere — CAPABILITY_KINDS and its KindCopy row land with the Go constant", kind)
		}
	}
}
