// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
)

// These are PURE wire-contract tests for the core domain vocabulary. They lock
// down (a) JSON round-trips of every exported wire struct, (b) the exact string
// value of every enum constant (notably RunCompleted=="COMPLETED" — the
// regression for the COMPLETED critical), (c) zero-value marshalling, (d)
// forward-compat tolerance of unknown JSON fields, and (e) the helper methods on
// these types.

// ptrUUID / ptrTime / ptrBool are tiny helpers for the *optional fields.
func ptrUUID(u uuid.UUID) *uuid.UUID { return &u }
func ptrTime(t time.Time) *time.Time { return &t }
func ptrBool(b bool) *bool           { return &b }

// roundTrip marshals v to JSON, unmarshals back into a fresh value of the same
// type, and returns the decoded value so the caller can deep-equal it. It fails
// the test on any marshal/unmarshal error.
func roundTrip[T any](t *testing.T, v T) T {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Marshal(%T) error: %v", v, err)
	}
	var out T
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("Unmarshal(%T) error: %v\njson: %s", v, err, b)
	}
	return out
}

// ---- enum string-value stability ----------------------------------------

// TestRunStateValues pins the exact wire string of every RunState. The
// RunCompleted=="COMPLETED" row is the regression for the COMPLETED critical:
// if anyone renames or drops the completed string, the completion watcher /
// terminal-state logic in internal/api breaks silently. Each constant must keep
// its documented value.
func TestRunStateValues(t *testing.T) {
	tests := []struct {
		name string
		got  RunState
		want string
	}{
		{"pending", RunPending, "PENDING"},
		{"starting", RunStarting, "STARTING"},
		{"running", RunRunning, "RUNNING"},
		{"waiting", RunWaiting, "WAITING_FOR_CONFIRMATION"},
		{"stopped", RunStopped, "STOPPED"},
		{"archived", RunArchived, "ARCHIVED"},
		{"failed", RunFailed, "FAILED"},
		{"killed", RunKilled, "KILLED"},
		// COMPLETED critical regression: the terminal success state.
		{"completed", RunCompleted, "COMPLETED"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if string(tc.got) != tc.want {
				t.Errorf("RunState %s = %q, want %q", tc.name, string(tc.got), tc.want)
			}
		})
	}
}

// TestConfinementClassValues pins the CC1/CC2/CC3 wire strings. The UI and
// policy MinConfinementClass comparisons depend on these exact values.
func TestConfinementClassValues(t *testing.T) {
	tests := []struct {
		name string
		got  ConfinementClass
		want string
	}{
		{"cc1", CC1, "CC1"},
		{"cc2", CC2, "CC2"},
		{"cc3", CC3, "CC3"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if string(tc.got) != tc.want {
				t.Errorf("ConfinementClass %s = %q, want %q", tc.name, string(tc.got), tc.want)
			}
		})
	}
}

// TestConfinementClassRank pins the weakest→strongest order and the
// fail-closed rank 0 for unrecognised (incl. non-canonical case) classes that
// policy gating and the orchestrator sort depend on.
func TestConfinementClassRank(t *testing.T) {
	tests := []struct {
		c    ConfinementClass
		want int
	}{
		{CC1, 1},
		{CC2, 2},
		{CC3, 3},
		{"", 0},
		{"cc1", 0},
		{"CC4", 0},
	}
	for _, tc := range tests {
		if got := tc.c.Rank(); got != tc.want {
			t.Errorf("ConfinementClass(%q).Rank() = %d, want %d", string(tc.c), got, tc.want)
		}
	}
	if !(CC1.Rank() < CC2.Rank() && CC2.Rank() < CC3.Rank()) {
		t.Errorf("rank order broken: CC1=%d CC2=%d CC3=%d", CC1.Rank(), CC2.Rank(), CC3.Rank())
	}
}

// TestActorTypeValues pins the audit-attribution actor strings. The
// groundtruth ingest path FORCES actor_type=system, so "system" especially must
// not drift.
func TestActorTypeValues(t *testing.T) {
	tests := []struct {
		name string
		got  ActorType
		want string
	}{
		{"human", ActorHuman, "human"},
		{"agent", ActorAgent, "agent"},
		{"system", ActorSystem, "system"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if string(tc.got) != tc.want {
				t.Errorf("ActorType %s = %q, want %q", tc.name, string(tc.got), tc.want)
			}
		})
	}
}

// TestGrantKindValues pins the broker-mintable credential-kind strings.
func TestGrantKindValues(t *testing.T) {
	tests := []struct {
		name string
		got  GrantKind
		want string
	}{
		{"github_token", GrantGitHubToken, "github_token"},
		{"cloud_sts", GrantCloudSTS, "cloud_sts"},
		{"api_key", GrantAPIKey, "api_key"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if string(tc.got) != tc.want {
				t.Errorf("GrantKind %s = %q, want %q", tc.name, string(tc.got), tc.want)
			}
		})
	}
}

// TestApprovalKindValues pins what a human can be asked to approve.
func TestApprovalKindValues(t *testing.T) {
	tests := []struct {
		name string
		got  ApprovalKind
		want string
	}{
		{"credential", ApprovalCredential, "credential"},
		{"egress_domain", ApprovalEgressDomain, "egress_domain"},
		{"tool_call", ApprovalToolCall, "tool_call"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if string(tc.got) != tc.want {
				t.Errorf("ApprovalKind %s = %q, want %q", tc.name, string(tc.got), tc.want)
			}
		})
	}
}

// TestApprovalStateValues pins the approval lifecycle strings, including EXPIRED
// (the timed-out terminal state the broker rejects mints against).
func TestApprovalStateValues(t *testing.T) {
	tests := []struct {
		name string
		got  ApprovalState
		want string
	}{
		{"pending", ApprovalPending, "PENDING"},
		{"approved", ApprovalApproved, "APPROVED"},
		{"denied", ApprovalDenied, "DENIED"},
		{"expired", ApprovalExpired, "EXPIRED"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if string(tc.got) != tc.want {
				t.Errorf("ApprovalState %s = %q, want %q", tc.name, string(tc.got), tc.want)
			}
		})
	}
}

// ---- JSON round-trips of every wire struct (representative non-zero) -----

// fixedTime is a stable, sub-second-truncated timestamp. RFC3339 JSON encoding
// preserves nanoseconds, but using a clean value keeps deep-equal robust across
// the marshal/unmarshal of time.Time (which is location-sensitive); UTC avoids
// monotonic-clock and zone surprises.
var fixedTime = time.Date(2026, 6, 28, 12, 30, 45, 0, time.UTC)

func TestAgentRunRoundTrip(t *testing.T) {
	polID := uuid.New()
	in := AgentRun{
		ID:               uuid.New(),
		CreatedAt:        fixedTime,
		UpdatedAt:        fixedTime.Add(time.Minute),
		CreatedBy:        "user:alice",
		Agent:            "claude-code",
		Repo:             "org/name",
		Task:             "refactor the auth package",
		PolicyID:         ptrUUID(polID),
		ConfinementClass: CC2,
		State:            RunCompleted,
		SPIFFEID:         "spiffe://wardyn.example/agent-run/abc",
		RunnerTarget:     "docker",
		SandboxRef:       "container-xyz",
	}
	got := roundTrip(t, in)
	if !reflect.DeepEqual(in, got) {
		t.Errorf("AgentRun round-trip mismatch:\n got: %+v\nwant: %+v", got, in)
	}
}

func TestRunPolicyRoundTrip(t *testing.T) {
	in := RunPolicy{
		ID:        uuid.New(),
		Name:      "default-policy",
		CreatedAt: fixedTime,
		UpdatedAt: fixedTime.Add(time.Hour),
		Spec: RunPolicySpec{
			AllowedDomains:      []string{"github.com", "*.githubusercontent.com"},
			DeniedDomains:       []string{"evil.example"},
			AllowAllEgress:      true,
			FirstUseApproval:    FirstUseDenyWithReview,
			AllowedMethods:      []string{"GET", "POST"},
			MinConfinementClass: CC1,
			EligibleGrants: []GrantSpec{
				{
					Kind:             GrantGitHubToken,
					Scope:            json.RawMessage(`{"repos":["org/name"]}`),
					TTLSeconds:       3600,
					RequiresApproval: true,
				},
			},
			AutoStopAfterSec: -1,
			WorkspaceMounts: []WorkspaceMount{
				{Source: "/home/host/work", Target: "/work", ReadOnly: ptrBool(false)},
			},
		},
	}
	got := roundTrip(t, in)
	if !reflect.DeepEqual(in, got) {
		t.Errorf("RunPolicy round-trip mismatch:\n got: %+v\nwant: %+v", got, in)
	}
}

func TestCredentialGrantRoundTrip(t *testing.T) {
	in := CredentialGrant{
		ID:        uuid.New(),
		RunID:     uuid.New(),
		CreatedAt: fixedTime,
		Spec: GrantSpec{
			Kind:             GrantAPIKey,
			Scope:            json.RawMessage(`{"host":"api.example.com","header":"X-Api-Key"}`),
			TTLSeconds:       1800,
			RequiresApproval: false,
		},
	}
	got := roundTrip(t, in)
	if !reflect.DeepEqual(in, got) {
		t.Errorf("CredentialGrant round-trip mismatch:\n got: %+v\nwant: %+v", got, in)
	}
}

func TestApprovalRequestRoundTrip(t *testing.T) {
	grantID := uuid.New()
	decided := fixedTime.Add(2 * time.Minute)
	in := ApprovalRequest{
		ID:             uuid.New(),
		RunID:          uuid.New(),
		GrantID:        ptrUUID(grantID),
		Kind:           ApprovalCredential,
		RequestedScope: json.RawMessage(`{"repos":["org/name"],"permissions":{"contents":"read"}}`),
		State:          ApprovalApproved,
		RequestedAt:    fixedTime,
		DecidedAt:      ptrTime(decided),
		DecidedBy:      "user:bob",
		MintedJTI:      "jti-12345",
		Reason:         "looks fine",
	}
	got := roundTrip(t, in)
	if !reflect.DeepEqual(in, got) {
		t.Errorf("ApprovalRequest round-trip mismatch:\n got: %+v\nwant: %+v", got, in)
	}
}

func TestAuditEventRoundTrip(t *testing.T) {
	runID := uuid.New()
	in := AuditEvent{
		ID:        uuid.New(),
		Time:      fixedTime,
		RunID:     ptrUUID(runID),
		ActorType: ActorSystem,
		Actor:     "wardyn-tetragon-ingest",
		Action:    "kernel.network.connect",
		Target:    "1.2.3.4:443",
		Outcome:   "success",
		SourceIP:  "10.0.0.5",
		Data:      json.RawMessage(`{"stream":"ebpf","subtype":"network_connect","dst":"1.2.3.4:443"}`),
	}
	got := roundTrip(t, in)
	if !reflect.DeepEqual(in, got) {
		t.Errorf("AuditEvent round-trip mismatch:\n got: %+v\nwant: %+v", got, in)
	}
}

func TestGrantSpecRoundTrip(t *testing.T) {
	in := GrantSpec{
		Kind:             GrantCloudSTS,
		Scope:            json.RawMessage(`{"role":"arn:aws:iam::123:role/x"}`),
		TTLSeconds:       3600,
		RequiresApproval: true,
	}
	got := roundTrip(t, in)
	if !reflect.DeepEqual(in, got) {
		t.Errorf("GrantSpec round-trip mismatch:\n got: %+v\nwant: %+v", got, in)
	}
}

func TestRunPolicySpecRoundTrip(t *testing.T) {
	in := RunPolicySpec{
		AllowedDomains:      []string{"example.com"},
		FirstUseApproval:    FirstUseAlwaysDeny,
		MinConfinementClass: CC3,
	}
	got := roundTrip(t, in)
	if !reflect.DeepEqual(in, got) {
		t.Errorf("RunPolicySpec round-trip mismatch:\n got: %+v\nwant: %+v", got, in)
	}
}

// TestFirstUseModeDecode is the load-bearing backward-compat check: every
// pre-enum policy stored first_use_approval as a JSON boolean. Those rows MUST
// still decode (true=>deny_with_review, false=>always_deny) or GetPolicy/every
// run dispatch breaks. The new string form and garbage/empty are covered too.
func TestFirstUseModeDecode(t *testing.T) {
	cases := []struct {
		json string
		want FirstUseMode
		// wantNorm is the fail-closed runtime value (Normalize).
		wantNorm FirstUseMode
		valid    bool
	}{
		{`true`, FirstUseDenyWithReview, FirstUseDenyWithReview, true}, // legacy
		{`false`, FirstUseAlwaysDeny, FirstUseAlwaysDeny, true},        // legacy
		{`"always_deny"`, FirstUseAlwaysDeny, FirstUseAlwaysDeny, true},
		{`"deny_with_review"`, FirstUseDenyWithReview, FirstUseDenyWithReview, true},
		{`"wait_for_review"`, FirstUseWaitForReview, FirstUseWaitForReview, true},
		{`""`, FirstUseMode(""), FirstUseAlwaysDeny, true}, // empty => normalizes to default
		{`null`, FirstUseAlwaysDeny, FirstUseAlwaysDeny, true},
		{`"bogus"`, FirstUseMode("bogus"), FirstUseAlwaysDeny, false}, // verbatim, fails closed
	}
	for _, c := range cases {
		var m FirstUseMode
		if err := json.Unmarshal([]byte(c.json), &m); err != nil {
			t.Fatalf("decode %s: %v", c.json, err)
		}
		if m != c.want {
			t.Errorf("decode %s = %q, want %q", c.json, m, c.want)
		}
		if m.Normalize() != c.wantNorm {
			t.Errorf("decode %s normalize = %q, want %q", c.json, m.Normalize(), c.wantNorm)
		}
		if m.Valid() != c.valid {
			t.Errorf("decode %s valid = %v, want %v", c.json, m.Valid(), c.valid)
		}
	}
	// A legacy boolean nested in a full spec must still decode.
	var spec RunPolicySpec
	if err := json.Unmarshal([]byte(`{"first_use_approval":true,"min_confinement_class":"CC2"}`), &spec); err != nil {
		t.Fatalf("legacy spec decode: %v", err)
	}
	if spec.FirstUseApproval != FirstUseDenyWithReview {
		t.Errorf("legacy spec first_use_approval = %q, want deny_with_review", spec.FirstUseApproval)
	}
}

func TestSiteConfigRoundTrip(t *testing.T) {
	in := SiteConfig{
		UpstreamProxySecretRef: "corp-proxy-url",
		ArtifactOverrides: map[string]ArtifactOverride{
			"npm": {BaseURL: "https://artifactory.corp/api/npm/npm-remote/", TokenSecretRef: "npm-token"},
			"go":  {BaseURL: "https://artifactory.corp/api/go/go-remote"},
		},
		ScmHosts: []string{"dev.azure.com", "github.example.com"},
	}
	got := roundTrip(t, in)
	if !reflect.DeepEqual(in, got) {
		t.Errorf("SiteConfig round-trip mismatch:\n got: %+v\nwant: %+v", got, in)
	}
}

func TestWorkspaceMountRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		in   WorkspaceMount
	}{
		{"explicit_rw", WorkspaceMount{Source: "/a", Target: "/work", ReadOnly: ptrBool(false)}},
		{"explicit_ro", WorkspaceMount{Source: "/a", Target: "/work", ReadOnly: ptrBool(true)}},
		{"omitted_ro", WorkspaceMount{Source: "/a", Target: "/work"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := roundTrip(t, tc.in)
			if !reflect.DeepEqual(tc.in, got) {
				t.Errorf("WorkspaceMount round-trip mismatch:\n got: %+v\nwant: %+v", got, tc.in)
			}
		})
	}
}

// ---- zero-value structs marshal + round-trip ----------------------------

// TestZeroValueRoundTrips asserts every exported wire struct marshals from its
// zero value without error and round-trips back deep-equal. This guards the
// omitempty contract (optional pointers stay nil, optional slices stay nil).
//
// NOTE: structs carrying a NON-omitempty json.RawMessage (GrantSpec.Scope,
// ApprovalRequest.RequestedScope, and anything embedding them) are tested
// separately below: a nil RawMessage marshals to the JSON literal null and
// decodes back to the 4-byte []byte("null"), so the zero value is intentionally
// NOT byte-identical after a round-trip. That asymmetry is a documented property
// of encoding/json, not a contract bug.
func TestZeroValueRoundTrips(t *testing.T) {
	t.Run("AgentRun", func(t *testing.T) {
		var z AgentRun
		got := roundTrip(t, z)
		if !reflect.DeepEqual(z, got) {
			t.Errorf("zero AgentRun mismatch:\n got: %+v\nwant: %+v", got, z)
		}
	})
	t.Run("RunPolicy", func(t *testing.T) {
		var z RunPolicy
		got := roundTrip(t, z)
		if !reflect.DeepEqual(z, got) {
			t.Errorf("zero RunPolicy mismatch:\n got: %+v\nwant: %+v", got, z)
		}
	})
	t.Run("RunPolicySpec", func(t *testing.T) {
		var z RunPolicySpec
		got := roundTrip(t, z)
		if !reflect.DeepEqual(z, got) {
			t.Errorf("zero RunPolicySpec mismatch:\n got: %+v\nwant: %+v", got, z)
		}
	})
	t.Run("WorkspaceMount", func(t *testing.T) {
		var z WorkspaceMount
		got := roundTrip(t, z)
		if !reflect.DeepEqual(z, got) {
			t.Errorf("zero WorkspaceMount mismatch:\n got: %+v\nwant: %+v", got, z)
		}
	})
	t.Run("AuditEvent", func(t *testing.T) {
		// AuditEvent.Data IS omitempty, so a nil RawMessage drops out entirely
		// and round-trips back to nil — the zero value is byte-stable here.
		var z AuditEvent
		got := roundTrip(t, z)
		if !reflect.DeepEqual(z, got) {
			t.Errorf("zero AuditEvent mismatch:\n got: %+v\nwant: %+v", got, z)
		}
	})
	t.Run("SiteConfig", func(t *testing.T) {
		var z SiteConfig
		got := roundTrip(t, z)
		if !reflect.DeepEqual(z, got) {
			t.Errorf("zero SiteConfig mismatch:\n got: %+v\nwant: %+v", got, z)
		}
	})
}

// TestZeroValueRawMessageRoundTrips covers the wire structs whose NON-omitempty
// json.RawMessage field makes the zero value round-trip to []byte("null") rather
// than nil. We assert exactly that documented shape: marshal succeeds, the round
// trip does not error, the RawMessage decodes to the literal null, and every
// OTHER field is preserved. This pins the real contract instead of a false
// byte-identity expectation.
func TestZeroValueRawMessageRoundTrips(t *testing.T) {
	t.Run("GrantSpec", func(t *testing.T) {
		got := roundTrip(t, GrantSpec{})
		if string(got.Scope) != "null" {
			t.Errorf("zero GrantSpec.Scope = %q, want %q", got.Scope, "null")
		}
		if got.Kind != "" || got.TTLSeconds != 0 || got.RequiresApproval {
			t.Errorf("zero GrantSpec scalar fields drifted: %+v", got)
		}
	})
	t.Run("CredentialGrant", func(t *testing.T) {
		got := roundTrip(t, CredentialGrant{})
		if string(got.Spec.Scope) != "null" {
			t.Errorf("zero CredentialGrant.Spec.Scope = %q, want %q", got.Spec.Scope, "null")
		}
		if got.ID != (uuid.UUID{}) || got.RunID != (uuid.UUID{}) {
			t.Errorf("zero CredentialGrant id fields drifted: %+v", got)
		}
	})
	t.Run("ApprovalRequest", func(t *testing.T) {
		got := roundTrip(t, ApprovalRequest{})
		if string(got.RequestedScope) != "null" {
			t.Errorf("zero ApprovalRequest.RequestedScope = %q, want %q", got.RequestedScope, "null")
		}
		// Optional pointers must stay nil through the zero-value round-trip.
		if got.GrantID != nil || got.DecidedAt != nil {
			t.Errorf("zero ApprovalRequest optional pointers should stay nil: %+v", got)
		}
	})
}

// TestZeroValueOmitemptyFields spot-checks that omitempty actually drops the
// optional fields from the zero-value wire form (so a minimal record stays
// minimal). If an omitempty tag is dropped these keys would appear.
func TestZeroValueOmitemptyFields(t *testing.T) {
	t.Run("AgentRun_omits_policy_id_and_sandbox_ref", func(t *testing.T) {
		b, err := json.Marshal(AgentRun{})
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		for _, k := range []string{"policy_id", "sandbox_ref"} {
			if _, ok := m[k]; ok {
				t.Errorf("zero AgentRun should omit %q, but it is present: %s", k, b)
			}
		}
		// state (no omitempty) must always be present, even empty.
		if _, ok := m["state"]; !ok {
			t.Errorf("zero AgentRun should always carry \"state\": %s", b)
		}
	})
	t.Run("ApprovalRequest_omits_optionals", func(t *testing.T) {
		b, err := json.Marshal(ApprovalRequest{})
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		for _, k := range []string{"grant_id", "decided_at", "decided_by", "minted_jti", "reason"} {
			if _, ok := m[k]; ok {
				t.Errorf("zero ApprovalRequest should omit %q, but it is present: %s", k, b)
			}
		}
	})
}

// ---- forward-compat: unknown / extra JSON fields tolerated --------------

// TestUnknownFieldsTolerated decodes wire payloads that carry extra, unknown
// keys (as a newer producer would emit) and asserts the known fields still
// populate and no error is returned. This is the forward-compatibility contract:
// an older consumer must not break when a newer producer adds fields.
func TestUnknownFieldsTolerated(t *testing.T) {
	t.Run("AgentRun", func(t *testing.T) {
		raw := []byte(`{
			"state": "RUNNING",
			"agent": "claude-code",
			"confinement_class": "CC2",
			"future_field": {"nested": [1,2,3]},
			"another_unknown": "ignored"
		}`)
		var ar AgentRun
		if err := json.Unmarshal(raw, &ar); err != nil {
			t.Fatalf("Unmarshal with unknown fields errored: %v", err)
		}
		if ar.State != RunRunning {
			t.Errorf("State = %q, want %q", ar.State, RunRunning)
		}
		if ar.Agent != "claude-code" {
			t.Errorf("Agent = %q, want %q", ar.Agent, "claude-code")
		}
		if ar.ConfinementClass != CC2 {
			t.Errorf("ConfinementClass = %q, want %q", ar.ConfinementClass, CC2)
		}
	})
	t.Run("ApprovalRequest", func(t *testing.T) {
		raw := []byte(`{
			"kind": "credential",
			"state": "EXPIRED",
			"unexpected": true,
			"v2_only": {"x": 1}
		}`)
		var req ApprovalRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Fatalf("Unmarshal with unknown fields errored: %v", err)
		}
		if req.Kind != ApprovalCredential {
			t.Errorf("Kind = %q, want %q", req.Kind, ApprovalCredential)
		}
		if req.State != ApprovalExpired {
			t.Errorf("State = %q, want %q", req.State, ApprovalExpired)
		}
	})
	t.Run("AuditEvent", func(t *testing.T) {
		raw := []byte(`{
			"actor_type": "system",
			"action": "kernel.process.exec",
			"outcome": "success",
			"brand_new_top_level": [1,2,3]
		}`)
		var ev AuditEvent
		if err := json.Unmarshal(raw, &ev); err != nil {
			t.Fatalf("Unmarshal with unknown fields errored: %v", err)
		}
		if ev.ActorType != ActorSystem {
			t.Errorf("ActorType = %q, want %q", ev.ActorType, ActorSystem)
		}
		if ev.Action != "kernel.process.exec" {
			t.Errorf("Action = %q, want %q", ev.Action, "kernel.process.exec")
		}
	})
	t.Run("RunPolicySpec", func(t *testing.T) {
		raw := []byte(`{
			"allowed_domains": ["github.com"],
			"min_confinement_class": "CC1",
			"v3_feature_flag": true
		}`)
		var spec RunPolicySpec
		if err := json.Unmarshal(raw, &spec); err != nil {
			t.Fatalf("Unmarshal with unknown fields errored: %v", err)
		}
		if len(spec.AllowedDomains) != 1 || spec.AllowedDomains[0] != "github.com" {
			t.Errorf("AllowedDomains = %v, want [github.com]", spec.AllowedDomains)
		}
		if spec.MinConfinementClass != CC1 {
			t.Errorf("MinConfinementClass = %q, want %q", spec.MinConfinementClass, CC1)
		}
	})
}

// ---- helper methods on the types ----------------------------------------

// TestWorkspaceMountReadOnlyOrDefault exercises the ReadOnlyOrDefault resolver
// for every input shape. The SAFE DEFAULT (nil read_only => read-only=true) is
// the security-relevant case: an omitted flag must never silently mean
// read-write, which is the unsafe direction.
func TestWorkspaceMountReadOnlyOrDefault(t *testing.T) {
	tests := []struct {
		name string
		mnt  WorkspaceMount
		want bool
	}{
		{"nil_defaults_readonly", WorkspaceMount{Source: "/a", Target: "/work"}, true},
		{"explicit_true_readonly", WorkspaceMount{Source: "/a", Target: "/work", ReadOnly: ptrBool(true)}, true},
		{"explicit_false_readwrite", WorkspaceMount{Source: "/a", Target: "/work", ReadOnly: ptrBool(false)}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.mnt.ReadOnlyOrDefault(); got != tc.want {
				t.Errorf("ReadOnlyOrDefault() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestReadOnlyOrDefaultMatchesWireDefault is a defense-in-depth check that the
// resolver agrees with how an omitted read_only key decodes from the wire: a
// JSON object with NO read_only key must resolve to read-only=true.
func TestReadOnlyOrDefaultMatchesWireDefault(t *testing.T) {
	var m WorkspaceMount
	if err := json.Unmarshal([]byte(`{"source":"/a","target":"/work"}`), &m); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if m.ReadOnly != nil {
		t.Fatalf("read_only omitted on wire should decode to nil pointer, got %v", *m.ReadOnly)
	}
	if !m.ReadOnlyOrDefault() {
		t.Errorf("omitted read_only must resolve read-only=true (safe default), got false")
	}
}
