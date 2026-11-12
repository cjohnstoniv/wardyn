// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package auditreceipt

import (
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// resolveData is a run.policy.resolve row's data as dispatch writes it: the policy, envelope extras that are
// not policy, and the stamp the recorder adds.
func resolveData(t *testing.T, spec types.RunPolicySpec, stamp string) string {
	t.Helper()
	b, err := json.Marshal(struct {
		types.RunPolicySpec
		DiskMiBFilled bool   `json:"disk_mib_filled"`
		PolicyDigest  string `json:"policy_digest,omitempty"`
	}{spec, true, stamp})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// policyReceipt is a one-checkpoint receipt over a run's resolve row and two actions stamped as given.
func policyReceipt(t *testing.T, priv ed25519.PrivateKey, resolve string, actionStamps ...string) (Bundle, []Row) {
	t.Helper()
	actions, datas := []string{"run.policy.resolve"}, []string{resolve}
	for _, s := range actionStamps {
		d := `{"host": "a.example"}`
		if s != "" {
			d = `{"host": "a.example", "policy_digest": "` + s + `"}`
		}
		actions, datas = append(actions, "egress.allow"), append(datas, d)
	}
	return runReceipt(t, priv, actions, datas)
}

// runReceipt is a one-checkpoint receipt over one run's rows, the i-th with actions[i] and datas[i], seqs from 1.
func runReceipt(t *testing.T, priv ed25519.PrivateKey, actions, datas []string) (Bundle, []Row) {
	t.Helper()
	rows := testChain(1, len(actions), "")
	run := uuid.New()
	for i := range rows {
		d := datas[i]
		rows[i].Action, rows[i].RunID, rows[i].Data = actions[i], &run, &d
	}
	// Re-chain: the content changed, so every hash did.
	prev := ""
	for i := range rows {
		rows[i].PrevHash, rows[i].RowHash = nil, nil
		if prev != "" {
			p := prev
			rows[i].PrevHash = &p
		}
		h := RowHash(rows[i])
		rows[i].RowHash = &h
		prev = h
	}
	idx := make([]int, len(rows))
	for i := range idx {
		idx[i] = i
	}
	c := withEvents(testCheckpoint(priv, rows, ""), rows, idx...)
	return Bundle{Format: ReceiptFormat, Checkpoints: []Checkpoint{c}}, rows
}

func policyOf(rep Report, seq int64) (PolicyResult, bool) {
	for _, p := range rep.Policies {
		if p.Seq == seq {
			return p, true
		}
	}
	return PolicyResult{}, false
}

func TestVerifyBundle_PolicyDigestVerifiedAndLinked(t *testing.T) {
	priv, pub := testKey(1)
	spec := types.RunPolicySpec{AllowedDomains: []string{"a.example", "b.example"}, AllowedMethods: []string{"GET"}}
	d := spec.Digest()
	b, _ := policyReceipt(t, priv, resolveData(t, spec, d), d, "")
	rep := VerifyBundle(b, keysOf(pub))
	if !rep.Complete() {
		t.Fatalf("failures %+v", rep.Failures)
	}
	if p, ok := policyOf(rep, 1); !ok || p.Status != PolicyVerified || p.Digest != d {
		t.Fatalf("resolve row policy = %+v, want verified %s", p, d)
	}
	if p, ok := policyOf(rep, 2); !ok || p.Status != PolicyLinked || p.Resolve != 1 {
		t.Fatalf("stamped action policy = %+v, want linked to the resolve row at seq 1", p)
	}
	if _, ok := policyOf(rep, 3); ok {
		t.Fatalf("an unstamped action must not be reported as linked")
	}
}

func TestVerifyBundle_PolicyDigestMismatch(t *testing.T) {
	priv, pub := testKey(1)
	spec := types.RunPolicySpec{AllowedDomains: []string{"a.example"}}
	// The stamp names another policy than the one the row records.
	other := types.RunPolicySpec{AllowedDomains: []string{"evil.example"}}.Digest()
	b, _ := policyReceipt(t, priv, resolveData(t, spec, other))
	rep := VerifyBundle(b, keysOf(pub))
	if rep.OK() || !hasReason(rep, ReasonPolicyDigest) {
		t.Fatalf("failures %+v, want %s", rep.Failures, ReasonPolicyDigest)
	}
	if p, _ := policyOf(rep, 1); p.Status != PolicyMismatch {
		t.Fatalf("resolve row policy = %+v, want mismatch", p)
	}
}

// The recorder stamps the digest before the masking recorder rewrites secret-looking bytes, so a masked
// row's recomputed digest can differ for a reason that is not tampering. It reports masked, never mismatch.
func TestVerifyBundle_MaskedResolveRowIsMaskedNotMismatch(t *testing.T) {
	priv, pub := testKey(1)
	spec := types.RunPolicySpec{AllowedDomains: []string{"a.example", "tok-123.example"}}
	d := spec.Digest()
	data := strings.ReplaceAll(resolveData(t, spec, d), "tok-123", "<secret-hidden>")
	b, _ := policyReceipt(t, priv, data, d)
	rep := VerifyBundle(b, keysOf(pub))
	if !rep.Complete() || hasReason(rep, ReasonPolicyDigest) {
		t.Fatalf("a masked resolve row: failures %+v, want none", rep.Failures)
	}
	if p, _ := policyOf(rep, 1); p.Status != PolicyMasked {
		t.Fatalf("resolve row policy = %+v, want masked", p)
	}
	// The scope label the masker adds when its corpus was partial reads the same way.
	scoped := strings.Replace(resolveData(t, spec, d), `{`, `{"mask_scope":"globals_only",`, 1)
	scoped = strings.ReplaceAll(scoped, "a.example", "other.example")
	b, _ = policyReceipt(t, priv, scoped, d)
	rep = VerifyBundle(b, keysOf(pub))
	if hasReason(rep, ReasonPolicyDigest) {
		t.Fatalf("a mask_scope row: failures %+v, want no policy mismatch", rep.Failures)
	}
	if p, _ := policyOf(rep, 1); p.Status != PolicyMasked {
		t.Fatalf("mask_scope resolve row policy = %+v, want masked", p)
	}
}

// A withheld resolve row is already not complete; its unchecked content never feeds the policy link.
func TestVerifyBundle_WithheldResolveRowCarriesNoPolicy(t *testing.T) {
	priv, pub := testKey(1)
	spec := types.RunPolicySpec{AllowedDomains: []string{"a.example"}}
	d := spec.Digest()
	data := `{"redacted_fields": ["workspace_mounts"], "policy_digest": "` + d + `"}`
	b, _ := policyReceipt(t, priv, data, d)
	b.Checkpoints[0].Events[0].RedactedFields = []string{"workspace_mounts"}
	rep := VerifyBundle(b, keysOf(pub))
	if rep.Complete() || rep.Withheld() != 1 {
		t.Fatalf("withheld = %d, failures %+v; want one withheld and not complete", rep.Withheld(), rep.Failures)
	}
	if _, ok := policyOf(rep, 1); ok {
		t.Fatalf("a withheld row must not report a policy verdict")
	}
	if p, ok := policyOf(rep, 2); !ok || p.Resolve != 0 {
		t.Fatalf("action policy = %+v, %v; want a stamp not linked to any resolve row", p, ok)
	}
}

// generationData is a run.policy.generation row's data as a revive writes it: the generation and its policy
// nested under "policy", stamped with that generation's digest.
func generationData(t *testing.T, gen int, spec types.RunPolicySpec) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"generation": gen, "policy": spec, "policy_digest": spec.Digest(), "policy_generation": gen})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A revived run's rows stamped with generation 2's digest link to the generation row, which verifies from its
// nested policy; rows stamped before the revive still link to dispatch's resolve row.
func TestVerifyBundle_RevivedRunLinksToGenerationRow(t *testing.T) {
	priv, pub := testKey(1)
	gen1 := types.RunPolicySpec{AllowedDomains: []string{"a.example"}}
	gen2 := types.RunPolicySpec{AllowedDomains: []string{"a.example", "b.example"}}
	d1, d2 := gen1.Digest(), gen2.Digest()
	act := func(d string) string { return `{"host": "a.example", "policy_digest": "` + d + `"}` }
	b, _ := runReceipt(t, priv,
		[]string{"run.policy.resolve", "egress.allow", "run.policy.generation", "egress.allow"},
		[]string{resolveData(t, gen1, d1), act(d1), generationData(t, 2, gen2), act(d2)})
	rep := VerifyBundle(b, keysOf(pub))
	if !rep.Complete() {
		t.Fatalf("failures %+v", rep.Failures)
	}
	if p, ok := policyOf(rep, 3); !ok || p.Status != PolicyVerified || p.Digest != d2 {
		t.Fatalf("generation row policy = %+v, want verified %s", p, d2)
	}
	if p, _ := policyOf(rep, 4); p.Status != PolicyLinked || p.Resolve != 3 {
		t.Fatalf("revived row policy = %+v, want linked to the generation row at seq 3", p)
	}
	if p, _ := policyOf(rep, 2); p.Status != PolicyLinked || p.Resolve != 1 {
		t.Fatalf("pre-revive row policy = %+v, want linked to the resolve row at seq 1", p)
	}
	// A generation row whose nested policy is not the one its stamp names fails like a resolve row.
	tampered := strings.Replace(generationData(t, 2, gen2), "b.example", "evil.example", 1)
	b, _ = runReceipt(t, priv, []string{"run.policy.generation"}, []string{tampered})
	if rep := VerifyBundle(b, keysOf(pub)); !hasReason(rep, ReasonPolicyDigest) {
		t.Fatalf("tampered generation row: failures %+v, want %s", rep.Failures, ReasonPolicyDigest)
	}
}

// policy_digest "unknown" names no policy: a row stamped so links nothing, even beside a masked policy row
// stamped unknown, which would otherwise pass as matching content.
func TestVerifyBundle_UnknownStampLinksNothing(t *testing.T) {
	priv, pub := testKey(1)
	spec := types.RunPolicySpec{AllowedDomains: []string{"a.example"}}
	unknown := types.PolicyDigestUnknown
	masked := strings.Replace(resolveData(t, spec, unknown), `{`, `{"mask_scope":"globals_only",`, 1)
	b, _ := policyReceipt(t, priv, masked, unknown, spec.Digest())
	rep := VerifyBundle(b, keysOf(pub))
	if p, ok := policyOf(rep, 2); !ok || p.Status != PolicyUnlinked || p.Resolve != 0 {
		t.Fatalf("unknown-stamped row policy = %+v, %v; want unlinked", p, ok)
	}
	if p, _ := policyOf(rep, 1); p.Status != PolicyUnlinked {
		t.Fatalf("unknown-stamped resolve row policy = %+v, want unlinked and no verdict", p)
	}
	if p, _ := policyOf(rep, 3); p.Status != PolicyUnlinked {
		t.Fatalf("row stamped with a digest no policy row carries = %+v, want unlinked", p)
	}
}
