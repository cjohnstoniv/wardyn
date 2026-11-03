// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package contentscan

import (
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// everyDetector turns on every detector a policy can name, so a test can tell
// which of them a cut engine kept.
func everyDetector(mode string) types.LLMInspectionSpec {
	return types.LLMInspectionSpec{
		Mode:                 mode,
		DetectSecrets:        true,
		DetectSecretPatterns: true,
		DetectEntropy:        true,
		DetectPII:            true,
		ClassifiedMarkers:    []string{"INTERNAL ONLY"},
		ScanAttachments:      true,
		OnScannerError:       "block",
		BlockMinSeverity:     "high",
	}
}

// The secrets-only engine keeps the run's own secret values, exact match, and
// nothing that overlaps what a gateway runs: no format pattern, entropy, PII or
// marker finding, while the same prompt under the full engine has each.
func TestGatewayInspectionEngine_KeepsOnlyTheRunsSecretValues(t *testing.T) {
	corpus := [][]byte{[]byte(testSecret)}
	full, err := NewEngine(everyDetector("block"), corpus)
	if err != nil {
		t.Fatal(err)
	}
	cut, err := NewSecretsOnlyEngine(everyDetector("block"), corpus)
	if err != nil || cut == nil {
		t.Fatalf("NewSecretsOnlyEngine = %v, %v; want an engine", cut, err)
	}
	if len(cut.detectors) != 1 {
		t.Fatalf("detectors = %d, want the known-secret check alone", len(cut.detectors))
	}
	if _, ok := cut.detectors[0].(*knownSecretDetector); !ok {
		t.Fatalf("detector = %T, want *knownSecretDetector", cut.detectors[0])
	}
	if cut.scanAttachments || cut.Mode() != ModeBlock || !cut.BlocksOnError() || cut.blockMin != SevHigh {
		t.Fatalf("cut engine lost the run's mode, error policy or severity floor, or kept attachments")
	}

	overlap := anthropicBody(t, "", "INTERNAL ONLY ghp_"+strings.Repeat("a", 36)+" mail me at a.person@example.com 4111 1111 1111 1111")
	if res, _, _ := full.ScanRequest(ChannelAnthropicMessages, overlap); len(res.Findings) == 0 {
		t.Fatal("the full engine finds nothing in the overlap prompt; the test proves nothing")
	}
	if res, _, _ := cut.ScanRequest(ChannelAnthropicMessages, overlap); len(res.Findings) != 0 {
		t.Fatalf("secrets-only engine found %+v in a prompt with none of the run's secrets", res.Findings)
	}
	res, _, _ := cut.ScanRequest(ChannelAnthropicMessages, anthropicBody(t, "", "the key is "+testSecret))
	if len(res.Findings) != 1 || res.Findings[0].Detector != "known-secret" || !cut.ShouldBlock(res) {
		t.Fatalf("findings = %+v; the run's own secret must still be found and blocked", res.Findings)
	}
}

// No DetectSecrets on the policy means nothing is left to run on the route, and an
// empty corpus means the check has nothing to match: both are a nil engine.
func TestGatewayInspectionEngine_NothingLeft(t *testing.T) {
	spec := everyDetector("alert")
	spec.DetectSecrets = false
	if eng, err := NewSecretsOnlyEngine(spec, [][]byte{[]byte(testSecret)}); eng != nil || err != nil {
		t.Fatalf("no DetectSecrets: got %v, %v; want nil, nil", eng, err)
	}
	if eng, err := NewSecretsOnlyEngine(everyDetector("alert"), nil); eng != nil || err != nil {
		t.Fatalf("empty corpus: got %v, %v; want nil, nil", eng, err)
	}
}
