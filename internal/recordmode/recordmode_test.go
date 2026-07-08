// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package recordmode

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/groundtruth"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ── audit-event builders (hand-built []types.AuditEvent, parsed via encoding/json) ──

// egressEvent builds an egress.<decision> audit event whose Data matches the map
// handlePostDecision marshals ({host,port,method,path,rule_source,approval_id}).
func egressEvent(decision egress.Decision, host, method, ruleSource string) types.AuditEvent {
	data, _ := json.Marshal(map[string]any{
		"host":        host,
		"port":        443,
		"method":      method,
		"path":        "/",
		"rule_source": ruleSource,
	})
	outcome := outcomeSuccess
	if decision == egress.Deny {
		outcome = "denied"
	}
	return types.AuditEvent{
		ID:        uuid.New(),
		ActorType: types.ActorAgent,
		Actor:     "spiffe://wardyn/agent-run/x",
		Action:    "egress." + string(decision),
		Target:    host,
		Outcome:   outcome,
		Data:      data,
	}
}

// mintEvent builds a credential.mint audit event (broker.auditMint shape).
func mintEvent(grantID uuid.UUID, outcome string) types.AuditEvent {
	data, _ := json.Marshal(map[string]any{
		"grant_id": grantID.String(),
		"scope":    json.RawMessage(`{"repos":["org/x"]}`),
		"jti":      "jti-" + grantID.String()[:8],
	})
	return types.AuditEvent{
		ID:        uuid.New(),
		ActorType: types.ActorAgent,
		Actor:     "spiffe://wardyn/agent-run/x",
		Action:    actionCredentialMint,
		Target:    grantID.String(),
		Outcome:   outcome,
		Data:      data,
	}
}

// kernelEvent builds a kernel.* ground-truth audit event from an EventData.
func kernelEvent(action, outcome string, d groundtruth.EventData) types.AuditEvent {
	data, _ := json.Marshal(d)
	return types.AuditEvent{
		ID:        uuid.New(),
		ActorType: types.ActorSystem,
		Actor:     groundtruth.SensorActor,
		Action:    action,
		Outcome:   outcome,
		Data:      data,
	}
}

func execEvent(argv []string, loader bool) types.AuditEvent {
	return kernelEvent(groundtruth.ActionProcessExec, outcomeSuccess, groundtruth.EventData{
		Stream: groundtruth.Stream, Subtype: groundtruth.SubtypeProcessExec,
		Argv: argv, Loader: loader, Correlation: groundtruth.CorrelationMapped,
	})
}

func connectEvent(dst string, corr groundtruth.Correlation, outcome string) types.AuditEvent {
	return kernelEvent(groundtruth.ActionNetworkConnect, outcome, groundtruth.EventData{
		Stream: groundtruth.Stream, Subtype: groundtruth.SubtypeNetworkConnect,
		Dst: dst, Correlation: corr,
	})
}

func fileWriteEvent(path string) types.AuditEvent {
	return kernelEvent(groundtruth.ActionFileWrite, outcomeSuccess, groundtruth.EventData{
		Stream: groundtruth.Stream, Subtype: groundtruth.SubtypeFileWrite,
		Path: path, Correlation: groundtruth.CorrelationMapped,
	})
}

// ── TestCapture: table-driven over hand-built event slices ──

func TestCapture(t *testing.T) {
	tests := []struct {
		name   string
		events []types.AuditEvent
		check  func(t *testing.T, obs Observations)
	}{
		{
			name:   "empty input yields zero-value observations",
			events: nil,
			check: func(t *testing.T, obs Observations) {
				if len(obs.Domains) != 0 || len(obs.MintedGrantIDs) != 0 || len(obs.ExecArgv0s) != 0 ||
					len(obs.FileWrites) != 0 || len(obs.Connects) != 0 || len(obs.Anomalies) != 0 {
					t.Fatalf("expected empty observations, got %+v", obs)
				}
			},
		},
		{
			name: "domain dedup and method union",
			events: []types.AuditEvent{
				egressEvent(egress.Allow, "api.github.com", "GET", "policy"),
				egressEvent(egress.Allow, "api.github.com", "POST", "policy"),
				egressEvent(egress.Allow, "API.GitHub.com", "get", "policy"), // case-folds to same host+method
			},
			check: func(t *testing.T, obs Observations) {
				if len(obs.Domains) != 1 {
					t.Fatalf("want 1 domain, got %d: %+v", len(obs.Domains), obs.Domains)
				}
				d := obs.Domains[0]
				if d.Host != "api.github.com" {
					t.Errorf("host = %q", d.Host)
				}
				if !reflect.DeepEqual(d.Methods, []string{"GET", "POST"}) {
					t.Errorf("methods = %v, want [GET POST]", d.Methods)
				}
				if d.AllowCount != 3 {
					t.Errorf("allow_count = %d, want 3", d.AllowCount)
				}
			},
		},
		{
			name: "deny captures anomaly and deny count, no allow",
			events: []types.AuditEvent{
				egressEvent(egress.Deny, "evil.example", "GET", "builtin:private-ip"),
			},
			check: func(t *testing.T, obs Observations) {
				if len(obs.Domains) != 1 || obs.Domains[0].DenyCount != 1 || obs.Domains[0].AllowCount != 0 {
					t.Fatalf("domain agg wrong: %+v", obs.Domains)
				}
				if !containsSubstr(obs.Anomalies, "egress.deny to evil.example") {
					t.Errorf("missing deny anomaly: %v", obs.Anomalies)
				}
				if !containsSubstr(obs.Anomalies, "rule_source=builtin:private-ip") {
					t.Errorf("anomaly missing rule_source: %v", obs.Anomalies)
				}
			},
		},
		{
			name: "successful mint captured, denied/failed mint ignored",
			events: []types.AuditEvent{
				mintEvent(fixedGrantA, outcomeSuccess),
				mintEvent(fixedGrantA, outcomeSuccess), // dup → one id
				mintEvent(fixedGrantB, "denied"),       // never minted
				mintEvent(fixedGrantC, outcomeFailure), // never minted
			},
			check: func(t *testing.T, obs Observations) {
				if !reflect.DeepEqual(obs.MintedGrantIDs, []uuid.UUID{fixedGrantA}) {
					t.Errorf("minted = %v, want [%s]", obs.MintedGrantIDs, fixedGrantA)
				}
			},
		},
		{
			name: "exec argv0 dedup; loader exec flagged anomaly",
			events: []types.AuditEvent{
				execEvent([]string{"/usr/bin/git", "status"}, false),
				execEvent([]string{"/usr/bin/git", "log"}, false), // dup argv0
				execEvent([]string{"/bin/sh", "-c", "x"}, false),
				execEvent([]string{"/lib64/ld-linux-x86-64.so.2", "./payload"}, true),
			},
			check: func(t *testing.T, obs Observations) {
				want := []string{"/bin/sh", "/lib64/ld-linux-x86-64.so.2", "/usr/bin/git"}
				if !reflect.DeepEqual(obs.ExecArgv0s, want) {
					t.Errorf("argv0s = %v, want %v", obs.ExecArgv0s, want)
				}
				if !containsSubstr(obs.Anomalies, "dynamic-linker exec") {
					t.Errorf("missing loader anomaly: %v", obs.Anomalies)
				}
			},
		},
		{
			name: "loader detected from path even when sensor loader flag is false",
			events: []types.AuditEvent{
				execEvent([]string{"/lib/ld-musl-x86_64.so.1", "./x"}, false), // flag false, path is a loader
			},
			check: func(t *testing.T, obs Observations) {
				if !containsSubstr(obs.Anomalies, "dynamic-linker exec") {
					t.Errorf("expected loader anomaly via IsDynamicLinker fallback: %v", obs.Anomalies)
				}
			},
		},
		{
			name: "connect dedup; unmapped and failure connects flagged",
			events: []types.AuditEvent{
				connectEvent("10.0.0.5:443", groundtruth.CorrelationMapped, outcomeSuccess),
				connectEvent("10.0.0.5:443", groundtruth.CorrelationMapped, outcomeSuccess), // dup
				connectEvent("169.254.169.254:80", groundtruth.CorrelationUnmapped, outcomeFailure),
			},
			check: func(t *testing.T, obs Observations) {
				want := []string{"10.0.0.5:443", "169.254.169.254:80"}
				if !reflect.DeepEqual(obs.Connects, want) {
					t.Errorf("connects = %v, want %v", obs.Connects, want)
				}
				if !containsSubstr(obs.Anomalies, "unmapped kernel connect to 169.254.169.254:80") {
					t.Errorf("missing unmapped anomaly: %v", obs.Anomalies)
				}
				if !containsSubstr(obs.Anomalies, "outcome=failure") {
					t.Errorf("missing failure anomaly: %v", obs.Anomalies)
				}
			},
		},
		{
			name: "sensitive file writes deduped and sorted",
			events: []types.AuditEvent{
				fileWriteEvent("/etc/passwd"),
				fileWriteEvent("/root/.ssh/authorized_keys"),
				fileWriteEvent("/etc/passwd"), // dup
			},
			check: func(t *testing.T, obs Observations) {
				want := []string{"/etc/passwd", "/root/.ssh/authorized_keys"}
				if !reflect.DeepEqual(obs.FileWrites, want) {
					t.Errorf("file_writes = %v, want %v", obs.FileWrites, want)
				}
				if len(obs.Anomalies) != 0 {
					t.Errorf("file writes must not produce anomalies, got %v", obs.Anomalies)
				}
			},
		},
		{
			name: "unrelated actions are ignored",
			events: []types.AuditEvent{
				{Action: "identity.issue", Outcome: outcomeSuccess, Data: json.RawMessage(`{"x":1}`)},
				{Action: "policy.update", Outcome: outcomeSuccess, Data: json.RawMessage(`{}`)},
				egressEvent(egress.Allow, "pypi.org", "GET", "policy"),
			},
			check: func(t *testing.T, obs Observations) {
				if len(obs.Domains) != 1 || obs.Domains[0].Host != "pypi.org" {
					t.Fatalf("expected only pypi.org domain, got %+v", obs.Domains)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.check(t, Capture(tt.events))
		})
	}
}

// TestCaptureOrderIndependent asserts the captured Observations are identical
// regardless of the order the events were recorded in (determinism guarantee).
func TestCaptureOrderIndependent(t *testing.T) {
	events := []types.AuditEvent{
		egressEvent(egress.Allow, "api.github.com", "GET", "policy"),
		egressEvent(egress.Deny, "evil.example", "POST", "policy"),
		egressEvent(egress.Allow, "api.github.com", "POST", "policy"),
		mintEvent(fixedGrantB, outcomeSuccess),
		mintEvent(fixedGrantA, outcomeSuccess),
		execEvent([]string{"/usr/bin/git"}, false),
		connectEvent("1.2.3.4:443", groundtruth.CorrelationUnmapped, outcomeSuccess),
		fileWriteEvent("/etc/hosts"),
	}
	forward := Capture(events)

	reversed := make([]types.AuditEvent, len(events))
	for i := range events {
		reversed[i] = events[len(events)-1-i]
	}
	backward := Capture(reversed)

	if !reflect.DeepEqual(forward, backward) {
		t.Fatalf("Capture is order-dependent:\n forward=%+v\nbackward=%+v", forward, backward)
	}
}

// ── TestSynthesize ──

func TestSynthesize(t *testing.T) {
	run := types.AgentRun{ID: uuid.New(), ConfinementClass: types.CC2}

	t.Run("forced invariants always hold", func(t *testing.T) {
		// Even with allow-all-looking evidence, allow_all_egress must be false
		// and first_use_approval must be true.
		obs := Capture([]types.AuditEvent{
			egressEvent(egress.Allow, "api.anthropic.com", "POST", "policy"),
		})
		spec, _ := Synthesize(obs, nil, run)
		if spec.AllowAllEgress {
			t.Error("allow_all_egress must be forced false")
		}
		if !spec.FirstUseApproval.RaisesApproval() {
			t.Error("first_use_approval must be forced true")
		}
		if len(spec.AllowedMethods) != 0 {
			t.Errorf("allowed_methods must be empty, got %v", spec.AllowedMethods)
		}
		if spec.MinConfinementClass != types.CC2 {
			t.Errorf("min_confinement_class = %q, want CC2", spec.MinConfinementClass)
		}
	})

	t.Run("allowed domains are exact, deduped, sorted; denied/pending excluded", func(t *testing.T) {
		obs := Capture([]types.AuditEvent{
			egressEvent(egress.Allow, "pypi.org", "GET", "policy"),
			egressEvent(egress.Allow, "api.github.com", "GET", "policy"),
			egressEvent(egress.Deny, "denied.example", "GET", "policy"),
			egressEvent(egress.Pending, "pending.example", "GET", "policy"),
		})
		spec, warns := Synthesize(obs, nil, run)
		want := []string{"api.github.com", "pypi.org"}
		if !reflect.DeepEqual(spec.AllowedDomains, want) {
			t.Errorf("allowed_domains = %v, want %v", spec.AllowedDomains, want)
		}
		for _, d := range spec.AllowedDomains {
			if strings.Contains(d, "*") {
				t.Errorf("allowed_domains must never auto-wildcard, got %q", d)
			}
		}
		if !containsSubstr(warns, "denied.example") || !containsSubstr(warns, "pending.example") {
			t.Errorf("expected exclusion warnings for denied/pending hosts: %v", warns)
		}
	})

	t.Run("empty input denies all egress with warning", func(t *testing.T) {
		spec, warns := Synthesize(Capture(nil), nil, run)
		if spec.AllowAllEgress || len(spec.AllowedDomains) != 0 {
			t.Errorf("empty recording must produce empty default-deny egress: %+v", spec)
		}
		if !spec.FirstUseApproval.RaisesApproval() {
			t.Error("first_use_approval must be true even on empty input")
		}
		if !containsSubstr(warns, "no allowed egress observed") {
			t.Errorf("expected no-egress warning: %v", warns)
		}
	})

	t.Run("grant lookup: minted grants included by id, github scope warned", func(t *testing.T) {
		ghSpec := types.GrantSpec{
			Kind:  types.GrantGitHubToken,
			Scope: json.RawMessage(`{"repos":["org/x"],"permissions":{"contents":"write","pull_requests":"write"}}`),
		}
		apiSpec := types.GrantSpec{
			Kind:  types.GrantAPIKey,
			Scope: json.RawMessage(`{"host":"api.example","secret_name":"k"}`),
		}
		runGrants := []types.CredentialGrant{
			{ID: fixedGrantA, RunID: run.ID, Spec: ghSpec},
			{ID: fixedGrantB, RunID: run.ID, Spec: apiSpec},
			{ID: fixedGrantC, RunID: run.ID, Spec: apiSpec}, // never minted → excluded
		}
		obs := Capture([]types.AuditEvent{
			mintEvent(fixedGrantA, outcomeSuccess),
			mintEvent(fixedGrantB, outcomeSuccess),
		})
		spec, warns := Synthesize(obs, runGrants, run)
		if len(spec.EligibleGrants) != 2 {
			t.Fatalf("want 2 eligible grants (only the minted ones), got %d: %+v", len(spec.EligibleGrants), spec.EligibleGrants)
		}
		// Order follows sorted MintedGrantIDs; assert the set of kinds is present.
		kinds := map[types.GrantKind]bool{}
		for _, g := range spec.EligibleGrants {
			kinds[g.Kind] = true
		}
		if !kinds[types.GrantGitHubToken] || !kinds[types.GrantAPIKey] {
			t.Errorf("eligible grant kinds = %v, want github_token+api_key", kinds)
		}
		if !containsSubstr(warns, "contents:write") || !containsSubstr(warns, "pull_requests:write") {
			t.Errorf("expected github scope intersection warning with permissions: %v", warns)
		}
	})

	t.Run("minted grant absent from catalog warns and is omitted", func(t *testing.T) {
		obs := Capture([]types.AuditEvent{mintEvent(fixedGrantA, outcomeSuccess)})
		spec, warns := Synthesize(obs, nil, run) // empty catalog
		if len(spec.EligibleGrants) != 0 {
			t.Errorf("unknown grant must be omitted, got %+v", spec.EligibleGrants)
		}
		if !containsSubstr(warns, "not found among run grants") {
			t.Errorf("expected not-found warning: %v", warns)
		}
	})

	t.Run("confinement class left empty and warns when run has none", func(t *testing.T) {
		spec, warns := Synthesize(Capture(nil), nil, types.AgentRun{})
		if spec.MinConfinementClass != "" {
			t.Errorf("min_confinement_class = %q, want empty", spec.MinConfinementClass)
		}
		if !containsSubstr(warns, "left empty") {
			t.Errorf("expected left-empty confinement warning: %v", warns)
		}
	})

	t.Run("workspace mounts not synthesized; mount-target write warns", func(t *testing.T) {
		obs := Capture([]types.AuditEvent{
			fileWriteEvent("/workspace/app/secret.txt"),
			fileWriteEvent("/etc/passwd"),
		})
		spec, warns := Synthesize(obs, nil, run)
		if len(spec.WorkspaceMounts) != 0 {
			t.Errorf("workspace_mounts must never be synthesized, got %+v", spec.WorkspaceMounts)
		}
		if !containsSubstr(warns, "host-mount target prefix") {
			t.Errorf("expected mount-usage warning: %v", warns)
		}
	})

	t.Run("anomalies and kernel surface flow into warnings", func(t *testing.T) {
		obs := Capture([]types.AuditEvent{
			egressEvent(egress.Deny, "evil.example", "GET", "policy"),
			execEvent([]string{"/usr/bin/curl"}, false),
			connectEvent("8.8.8.8:53", groundtruth.CorrelationUnmapped, outcomeSuccess),
		})
		_, warns := Synthesize(obs, nil, run)
		if !containsSubstr(warns, "anomaly: egress.deny to evil.example") {
			t.Errorf("expected deny anomaly surfaced as warning: %v", warns)
		}
		if !containsSubstr(warns, "no exec-allowlist") {
			t.Errorf("expected exec-surface warning: %v", warns)
		}
		if !containsSubstr(warns, "connect destination(s)") {
			t.Errorf("expected connect-surface warning: %v", warns)
		}
	})
}

// TestSynthesizeDeterministic asserts the same Observations yield byte-identical
// spec + warnings on repeated calls (no map-iteration nondeterminism).
func TestSynthesizeDeterministic(t *testing.T) {
	run := types.AgentRun{ID: uuid.New(), ConfinementClass: types.CC1}
	runGrants := []types.CredentialGrant{
		{ID: fixedGrantA, Spec: types.GrantSpec{Kind: types.GrantGitHubToken, Scope: json.RawMessage(`{"permissions":{"contents":"write","issues":"read"}}`)}},
		{ID: fixedGrantB, Spec: types.GrantSpec{Kind: types.GrantAPIKey, Scope: json.RawMessage(`{"host":"h"}`)}},
	}
	obs := Capture([]types.AuditEvent{
		egressEvent(egress.Allow, "b.example", "GET", "policy"),
		egressEvent(egress.Allow, "a.example", "POST", "policy"),
		egressEvent(egress.Deny, "c.example", "GET", "policy"),
		mintEvent(fixedGrantA, outcomeSuccess),
		mintEvent(fixedGrantB, outcomeSuccess),
		execEvent([]string{"/usr/bin/git"}, false),
		fileWriteEvent("/work/out"),
	})

	spec1, warns1 := Synthesize(obs, runGrants, run)
	for i := 0; i < 25; i++ {
		spec2, warns2 := Synthesize(obs, runGrants, run)
		if !reflect.DeepEqual(spec1, spec2) {
			t.Fatalf("spec not deterministic on iter %d:\n%+v\n%+v", i, spec1, spec2)
		}
		if !reflect.DeepEqual(warns1, warns2) {
			t.Fatalf("warnings not deterministic on iter %d:\n%v\n%v", i, warns1, warns2)
		}
	}
	// Sanity: the github permission summary is itself sorted.
	if got := githubPermSummary(runGrants[0].Spec.Scope); got != "{contents:write, issues:read}" {
		t.Errorf("githubPermSummary = %q, want sorted {contents:write, issues:read}", got)
	}
}

// ── helpers + fixed ids (kept stable so sorted-order assertions are reproducible) ──

var (
	fixedGrantA = uuid.MustParse("11111111-1111-1111-1111-111111111111")
	fixedGrantB = uuid.MustParse("22222222-2222-2222-2222-222222222222")
	fixedGrantC = uuid.MustParse("33333333-3333-3333-3333-333333333333")
)

// containsSubstr reports whether any element of ss contains sub.
func containsSubstr(ss []string, sub string) bool {
	for _, s := range ss {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// TestMintedGrantIDsSorted pins the by-string sort order of minted grant ids.
func TestMintedGrantIDsSorted(t *testing.T) {
	obs := Capture([]types.AuditEvent{
		mintEvent(fixedGrantC, outcomeSuccess),
		mintEvent(fixedGrantA, outcomeSuccess),
		mintEvent(fixedGrantB, outcomeSuccess),
	})
	got := make([]string, len(obs.MintedGrantIDs))
	for i, id := range obs.MintedGrantIDs {
		got[i] = id.String()
	}
	want := []string{fixedGrantA.String(), fixedGrantB.String(), fixedGrantC.String()}
	if !sort.StringsAreSorted(got) || !reflect.DeepEqual(got, want) {
		t.Errorf("minted ids = %v, want sorted %v", got, want)
	}
}
