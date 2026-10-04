// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestSign_Vector pins the X-Wardyn-Signature format. The expected value was computed independently
// (openssl dgst -sha256 -hmac) over `<t>.<body>`; docs/OPERATIONS.md publishes the same vector.
func TestSign_Vector(t *testing.T) {
	body := []byte(`{"schema":"wardyn.approval.v1","delivery_id":"d"}`)
	got := Sign("whsec_test_vector", 1700000000, body)
	const want = "t=1700000000,v1=b851b43234ba1d1386179e9f8785cf6c37337c6dee0791f8479579e0eb1e097a"
	if got != want {
		t.Fatalf("Sign = %q, want %q", got, want)
	}
}

func facts() approvalFacts {
	pid := uuid.MustParse("00000000-0000-4000-8000-0000000000aa")
	return approvalFacts{
		ApprovalID:  uuid.MustParse("00000000-0000-4000-8000-000000000001"),
		State:       types.ApprovalPending,
		Kind:        types.ApprovalEgressDomain,
		RequestedAt: time.Now().UTC().Truncate(time.Second),
		RunID:       uuid.MustParse("00000000-0000-4000-8000-000000000002"),
		Principal:   "alice",
		Email:       "alice@example.com",
		ProfileID:   &pid,
		ProfileName: "payments",
	}
}

// TestBuildPayload_Golden pins the allowlisted body, byte for byte.
func TestBuildPayload_Golden(t *testing.T) {
	f := facts()
	got, err := buildPayload(uuid.MustParse("00000000-0000-4000-8000-0000000000dd"), 0, f, "https://wardyn.example.com/", secretmask.Masker{})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schema":"wardyn.approval.v1","delivery_id":"00000000-0000-4000-8000-0000000000dd","event":"raised","tier":0,` +
		`"approval":{"id":"00000000-0000-4000-8000-000000000001","kind":"egress_domain","requested_at":"` + f.RequestedAt.Format(time.RFC3339) + `"},` +
		`"run":{"id":"00000000-0000-4000-8000-000000000002"},` +
		`"profile":{"id":"00000000-0000-4000-8000-0000000000aa","name":"payments"},` +
		`"requester":{"principal":"alice","email":"alice@example.com"},` +
		`"console_url":"https://wardyn.example.com/approvals"}`
	if string(got) != want {
		t.Fatalf("payload\n got %s\nwant %s", got, want)
	}
}

// An approval with a later tier scheduled names its due time in approval.sla_due_at.
func TestBuildPayload_SLADueAt(t *testing.T) {
	f := facts()
	due := f.RequestedAt.Add(30 * time.Minute)
	f.NextTierDueAt = &due
	got, err := buildPayload(uuid.MustParse("00000000-0000-4000-8000-0000000000dd"), 0, f, "", secretmask.Masker{})
	if err != nil {
		t.Fatal(err)
	}
	want := `"approval":{"id":"00000000-0000-4000-8000-000000000001","kind":"egress_domain","requested_at":"` +
		f.RequestedAt.Format(time.RFC3339) + `","sla_due_at":"` + due.Format(time.RFC3339) + `"},`
	if !strings.Contains(string(got), want) {
		t.Fatalf("payload\n got %s\nwant it to contain %s", got, want)
	}
}

// TestBuildPayload_NeverCarriesSandboxText: the facts the worker loads have no field for the scope, the
// reason or the title, so a secret written into any of them cannot reach the body. The test asserts that
// at the seam: it renders a payload for an approval whose scope and reason hold a registered secret and
// checks neither the secret nor the scope's host appears.
func TestBuildPayload_NeverCarriesSandboxText(t *testing.T) {
	const secret = "ghp_REGISTEREDSECRETVALUE0123456789"
	reg := secretmask.NewRegistry()
	runID := uuid.New()
	reg.Add(runID, []byte(secret))
	approval := types.ApprovalRequest{
		RequestedScope: json.RawMessage(`{"host":"evil.example.com","note":"` + secret + `"}`),
		Reason:         "please approve, token " + secret,
	}
	f := facts()
	f.RunID = runID
	body, err := buildPayload(uuid.New(), 1, f, "https://wardyn.example.com", reg.Masker(runID))
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{secret, "evil.example.com", approval.Reason, string(approval.RequestedScope)} {
		if strings.Contains(string(body), leak) {
			t.Fatalf("payload carries %q: %s", leak, body)
		}
	}
	if !strings.Contains(string(body), `"event":"escalated"`) {
		t.Fatalf("tier 1 must be an escalated event: %s", body)
	}
}

// TestBuildPayload_MasksIdentifiersThatEqualASecret is the defence in depth: an allowlisted string that
// happens to equal a registered secret is masked before encoding, so the JSON stays well formed.
func TestBuildPayload_MasksIdentifiersThatEqualASecret(t *testing.T) {
	const secret = "alice-the-secret"
	reg := secretmask.NewRegistry()
	f := facts()
	reg.Add(f.RunID, []byte(secret))
	f.Principal = secret
	body, err := buildPayload(uuid.New(), 0, f, "", reg.Masker(f.RunID))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), secret) {
		t.Fatalf("a registered secret survived masking: %s", body)
	}
	if !json.Valid(body) {
		t.Fatalf("masked payload is not valid JSON: %s", body)
	}
}

func TestBuildPayload_StripsControlCharactersAndCaps(t *testing.T) {
	f := facts()
	f.ProfileName = "pay\x00ments\n\x1b[31m" + strings.Repeat("é", 300)
	body, err := buildPayload(uuid.New(), 0, f, "", secretmask.Masker{})
	if err != nil {
		t.Fatal(err)
	}
	var p payload
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatal(err)
	}
	name := p.Profile.Name
	if strings.ContainsAny(name, "\x00\n\x1b") || len(name) > maxFieldBytes || !strings.HasPrefix(name, "payments[31m") {
		t.Fatalf("profile name %q (len %d) was not stripped and capped", name, len(name))
	}
}
