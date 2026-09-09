// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// F6 PROBE — record → promote → confined verify (lane F6-record-promote-verify).
//
// INTENDED DESTINATION: internal/api/record_probe_f6_test.go
// (package api — it reuses record_test.go's recordStore / importStateFake /
// egressAllowEvent / newTestSrv helpers, so it MUST live in that directory).
//
// RUN (no Postgres needed — every store touch is the record_test.go fake):
//
//	cd <repo root> && \
//	cp local/review-0.7/deep/F6-record-promote-verify/record_probe_f6_test.go internal/api/ && \
//	nice -n 10 GOMAXPROCS=8 go test ./internal/api/ -run 'TestF6' -count=1 -v ; \
//	rm -f internal/api/record_probe_f6_test.go
//
// Two families:
//
//	TestF6Probe_*  pin invariants the code ENFORCES today — expected GREEN on
//	               fa910735 (base 80538b10); a RED here is a regression.
//	TestF6Gap_*    pin invariants the trace doc says the code does NOT enforce
//	               (hypotheses H1..H6 in F6-record-promote-verify.md) — expected
//	               RED on fa910735; a GREEN here means the hypothesis is wrong
//	               (or the gap was closed) and the doc must be corrected.
//
// Run them separately (-run 'TestF6Probe' / -run 'TestF6Gap') if a mixed
// verdict is confusing.
package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/recordmode"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// f6Workspace is a settled OPEN recording ("build", status recorded) over obs,
// on a plain local-dir workspace with no legacy ApprovedEgress rows.
func f6Workspace(wsID, runID uuid.UUID, task string, res RecordTaskResult) types.Workspace {
	return types.Workspace{
		ID:      wsID,
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: "/w", Target: "/home/agent/work"}},
		Status:  types.WorkspaceScanned,
		RecordResults: mustJSON(map[string]RecordTaskResult{
			task: res,
		}),
	}
}

func f6RecordedOpen(runID uuid.UUID, obs *recordmode.Observations) RecordTaskResult {
	return RecordTaskResult{RunID: runID, Mode: recordModeInteractive, Status: recordStatusRecorded, Observations: obs}
}

func f6PromoteURL(wsID uuid.UUID, task string) string {
	return "/api/v1/workspaces/" + wsID.String() + "/record/" + task + "/promote-egress"
}

// ─────────────────────────────────────────────────────────────────────────────
// PROBE family — expected GREEN.
// ─────────────────────────────────────────────────────────────────────────────

// TestF6Probe_ControlPlaneHostNeverPromotable: the control plane's own host is
// never promotable — neither wholesale nor by explicit {"hosts":[...]} — even
// when it is a DOTTED name that passes hostrules.ValidApprovedHost (so the
// ONLY thing excluding it is record.go's selfHost match), even when it was
// observed+allowed more than any real host, and even when ControlPlaneURL is
// spelled with mixed case, a scheme, a port and a trailing slash
// (controlPlaneHost must normalize all of that).
func TestF6Probe_ControlPlaneHostNeverPromotable(t *testing.T) {
	runID, wsID := uuid.New(), uuid.New()
	const cp = "wardyn.example.internal"
	obs := recordmode.Observations{Domains: []recordmode.DomainObservation{
		{Host: "api.stripe.com", AllowCount: 1},
		{Host: cp, AllowCount: 7}, // the sandbox's brokered uploads — plumbing
	}}
	fake := &recordStore{importStateFake: importStateFake{ws: f6Workspace(wsID, runID, "build", f6RecordedOpen(runID, &obs))}}
	h := newHarness(t)
	cfg := baseTestConfig(h, fake)
	cfg.ControlPlaneURL = " HTTPS://Wardyn.Example.Internal:8443/ " // un-normalized on purpose
	srv := New(cfg)

	// Pure hop: promotableHosts with the resolved selfHost.
	if self := controlPlaneHost(cfg.ControlPlaneURL); self != cp {
		t.Fatalf("controlPlaneHost(%q) = %q, want %q", cfg.ControlPlaneURL, self, cp)
	}
	if p := promotableHosts(&obs, controlPlaneHost(cfg.ControlPlaneURL), srv.promoteSkipHosts(context.Background(), fake.ws)); len(p) != 1 {
		t.Fatalf("promotableHosts = %v, want exactly {api.stripe.com}", p)
	} else if _, bad := p[cp]; bad {
		t.Fatalf("promotableHosts offered the control plane host %q", cp)
	}

	// Wholesale click.
	w := do(t, srv, http.MethodPost, f6PromoteURL(wsID, "build"), adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("wholesale promote: code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if _, leaked := fake.ws.Requirements["egress:"+cp]; leaked {
		t.Fatalf("control plane host landed as a required egress row: %v", fake.ws.Requirements)
	}
	if len(fake.ws.Requirements) != 1 || fake.ws.Requirements["egress:api.stripe.com"].Level != "required" {
		t.Fatalf("requirements = %v, want exactly egress:api.stripe.com", fake.ws.Requirements)
	}

	// Explicit narrowing that names the control plane host must be refused
	// outright (422), and must widen nothing.
	w = do(t, srv, http.MethodPost, f6PromoteURL(wsID, "build"), adminToken, `{"hosts":["`+cp+`"]}`)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("explicit control-plane host: code = %d, want 422; body=%s", w.Code, w.Body.String())
	}
	if _, leaked := fake.ws.Requirements["egress:"+cp]; leaked || len(fake.ws.Requirements) != 1 {
		t.Fatalf("explicit control-plane host widened the contract: %v", fake.ws.Requirements)
	}
}

// TestF6Probe_TruncatedCaptureIsNeverClean pins the truncation half of the
// invariant end to end: the pure verdict (recordmode.CleanReplay) is false for
// ANY truncated capture — even an empty one, even an all-allow one — and
// reconcileRecordRun actually threads `len(events) >= maxCaptureAuditEvents`
// into that verdict for a CONFINED entry (Clean=false, the truncation caveat
// stamped), while one event under the ceiling stays clean.
func TestF6Probe_TruncatedCaptureIsNeverClean(t *testing.T) {
	// Pure hop.
	if recordmode.CleanReplay(nil, true) {
		t.Fatal("CleanReplay(nil, truncated=true) = true, want false (empty-but-truncated is NOT clean)")
	}
	if recordmode.CleanReplay([]recordmode.DomainObservation{{Host: "pypi.org", AllowCount: 3}}, true) {
		t.Fatal("CleanReplay(all-allow, truncated=true) = true, want false")
	}
	if !recordmode.CleanReplay([]recordmode.DomainObservation{{Host: "pypi.org", AllowCount: 3}}, false) {
		t.Fatal("CleanReplay(all-allow, truncated=false) = false, want true (control)")
	}

	// Server hop: reconcileRecordRun over a confined entry.
	run := func(t *testing.T, n int) RecordTaskResult {
		t.Helper()
		h := newHarness(t)
		runID, wsID := uuid.New(), uuid.New()
		ws := recordingWorkspace(wsID, runID, "verify:build")
		ws.RecordResults = mustJSON(map[string]RecordTaskResult{
			"verify:build": {RunID: runID, Mode: recordModeInteractive, Confined: true, Status: recordStatusRecording},
		})
		events := make([]types.AuditEvent, 0, n)
		for i := 0; i < n; i++ {
			events = append(events, egressAllowEvent(runID, "registry.npmjs.org"))
		}
		fake := &recordStore{
			run:             types.AgentRun{ID: runID, WorkspaceID: &wsID, Task: "workspace record", State: types.RunStopped, SandboxRef: "sb"},
			importStateFake: importStateFake{ws: ws},
			events:          events,
		}
		srv := New(baseTestConfig(h, fake))
		srv.reconcileRecordRun(context.Background(), runID)
		res := fake.savedResult(t, "verify:build")
		if res.Status != recordStatusRecorded {
			t.Fatalf("status = %q, want recorded (hint=%s)", res.Status, res.FailureHint)
		}
		return res
	}

	t.Run("at the ceiling: truncated, never clean", func(t *testing.T) {
		res := run(t, maxCaptureAuditEvents)
		if res.Clean == nil || *res.Clean {
			t.Fatalf("clean = %v, want a stamped false for a truncated confined capture", res.Clean)
		}
		found := false
		for _, c := range res.Caveats {
			if strings.Contains(c, captureAuditTruncatedNote) {
				found = true
			}
		}
		if !found {
			t.Fatalf("caveats = %v, want the truncation note stamped", res.Caveats)
		}
	})
	t.Run("one under the ceiling: complete, clean", func(t *testing.T) {
		res := run(t, maxCaptureAuditEvents-1)
		if res.Clean == nil || !*res.Clean {
			t.Fatalf("clean = %v, want true for a complete all-allow confined capture", res.Clean)
		}
		for _, c := range res.Caveats {
			if strings.Contains(c, captureAuditTruncatedNote) {
				t.Fatalf("truncation note stamped on a capture under the ceiling: %v", res.Caveats)
			}
		}
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// GAP family — expected RED on fa910735. Each names the hypothesis it probes.
// ─────────────────────────────────────────────────────────────────────────────

// TestF6Gap_TruncatedOpenCapturePromotesNothing — H1. handlePromoteRecordEgress
// never reads the truncation caveat; a truncated OPEN recording promotes its
// (incomplete) allowed set wholesale. The coordinator's stated invariant is
// "a truncated observation set promotes NO host"; the code has no such gate.
func TestF6Gap_TruncatedOpenCapturePromotesNothing(t *testing.T) {
	runID, wsID := uuid.New(), uuid.New()
	obs := recordmode.Observations{Domains: []recordmode.DomainObservation{{Host: "api.stripe.com", AllowCount: 1}}}
	res := f6RecordedOpen(runID, &obs)
	res.Caveats = []string{recordMaskingCaveat, captureAuditTruncatedNote}
	fake := &recordStore{importStateFake: importStateFake{ws: f6Workspace(wsID, runID, "build", res)}}
	srv := newTestSrv(t, fake)

	w := do(t, srv, http.MethodPost, f6PromoteURL(wsID, "build"), adminToken, "")
	if w.Code == http.StatusOK || len(fake.ws.Requirements) != 0 {
		t.Fatalf("H1: a TRUNCATED capture promoted wholesale (code=%d, rows=%v); want a refusal (4xx) and zero rows",
			w.Code, fake.ws.Requirements)
	}
}

// TestF6Gap_ConfinedVerifyEntryIsNotPromotable — H5. The promote route accepts
// the "verify:<key>" entry of a CONFINED replay; an allow released there by a
// live first-use approval (rule_source approval:<id> → ApprovalCount>0) is
// promotable, even though learnVerifyEgress deliberately refuses to durably
// learn a once/until-scoped approval.
func TestF6Gap_ConfinedVerifyEntryIsNotPromotable(t *testing.T) {
	runID, wsID := uuid.New(), uuid.New()
	obs := recordmode.Observations{Domains: []recordmode.DomainObservation{
		{Host: "api.stripe.com", AllowCount: 1, ApprovalCount: 1}, // released by a live approval (scope once)
	}}
	res := f6RecordedOpen(runID, &obs)
	res.Confined = true
	clean := false
	res.Clean = &clean
	fake := &recordStore{importStateFake: importStateFake{ws: f6Workspace(wsID, runID, "verify:build", res)}}
	srv := newTestSrv(t, fake)

	w := do(t, srv, http.MethodPost, f6PromoteURL(wsID, "verify:build"), adminToken, "")
	if w.Code == http.StatusOK || len(fake.ws.Requirements) != 0 {
		t.Fatalf("H5: a CONFINED verify entry promoted an approval-released host (code=%d, rows=%v); want refusal + zero rows",
			w.Code, fake.ws.Requirements)
	}
}

// TestF6Gap_WildcardCeilingPlumbingIsSkipped — H3. promoteSkipHosts keys on the
// ceiling's entries VERBATIM; a "*.anthropic.com" ceiling (the canonical
// spelling llmcred.go documents) does not skip api.anthropic.com, and the
// "*.githubusercontent.com" broker entry does not skip raw.githubusercontent.com.
func TestF6Gap_WildcardCeilingPlumbingIsSkipped(t *testing.T) {
	runID, wsID := uuid.New(), uuid.New()
	obs := recordmode.Observations{Domains: []recordmode.DomainObservation{
		{Host: "api.stripe.com", AllowCount: 1},
		{Host: "api.anthropic.com", AllowCount: 9},         // harness plumbing under a wildcard ceiling
		{Host: "raw.githubusercontent.com", AllowCount: 2}, // broker-managed by wildcard → dead row
	}}
	fake := &recordStore{importStateFake: importStateFake{ws: f6Workspace(wsID, runID, "build", f6RecordedOpen(runID, &obs))}}
	h := newHarness(t)
	cfg := baseTestConfig(h, fake)
	cfg.DefaultPolicy = types.RunPolicySpec{AllowedDomains: []string{"*.anthropic.com"}}
	srv := New(cfg)

	w := do(t, srv, http.MethodPost, f6PromoteURL(wsID, "build"), adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var leaked []string
	for _, k := range []string{"egress:api.anthropic.com", "egress:raw.githubusercontent.com"} {
		if _, ok := fake.ws.Requirements[k]; ok {
			leaked = append(leaked, k)
		}
	}
	if len(leaked) > 0 {
		t.Fatalf("H3: plumbing promoted under a wildcard skip entry: %v (all rows: %v)", leaked, fake.ws.Requirements)
	}
}

// TestF6Gap_DeniedEgressHostIsNotPromotable — H6. A host the operator has
// since put on deny·always (ws.DeniedEgress) is still promotable from an
// older recording, producing a contract that declares egress:X required while
// X is permanently denied — the exact contradiction denyAlwaysReject's M1 rule
// refuses in the other direction.
func TestF6Gap_DeniedEgressHostIsNotPromotable(t *testing.T) {
	runID, wsID := uuid.New(), uuid.New()
	obs := recordmode.Observations{Domains: []recordmode.DomainObservation{{Host: "api.stripe.com", AllowCount: 1}}}
	ws := f6Workspace(wsID, runID, "build", f6RecordedOpen(runID, &obs))
	ws.DeniedEgress = []string{"api.stripe.com"} // denied AFTER the recording
	fake := &recordStore{importStateFake: importStateFake{ws: ws}}
	srv := newTestSrv(t, fake)

	w := do(t, srv, http.MethodPost, f6PromoteURL(wsID, "build"), adminToken, "")
	if _, contradicted := fake.ws.Requirements["egress:api.stripe.com"]; w.Code == http.StatusOK && contradicted {
		t.Fatalf("H6: promoted a deny·always host into a required row (code=%d, rows=%v, denied=%v)",
			w.Code, fake.ws.Requirements, fake.ws.DeniedEgress)
	}
}

// TestF6Gap_EmptyControlPlaneURLStillExcludesSelf — H2. controlPlaneHost("")
// is "", and promotableHosts' guard is `selfHost != "" && host == selfHost`, so
// an unset/unparseable ControlPlaneURL disables the control-plane exclusion
// entirely (zero-value fails open). Only the undotted default "wardynd" is
// saved by the host-shape rule; a dotted control-plane name is not.
func TestF6Gap_EmptyControlPlaneURLStillExcludesSelf(t *testing.T) {
	runID, wsID := uuid.New(), uuid.New()
	const cp = "wardynd.wardyn.svc.cluster.local"
	obs := recordmode.Observations{Domains: []recordmode.DomainObservation{{Host: cp, AllowCount: 3}}}
	fake := &recordStore{importStateFake: importStateFake{ws: f6Workspace(wsID, runID, "build", f6RecordedOpen(runID, &obs))}}
	h := newHarness(t)
	cfg := baseTestConfig(h, fake)
	cfg.ControlPlaneURL = "" // zero value
	srv := New(cfg)

	w := do(t, srv, http.MethodPost, f6PromoteURL(wsID, "build"), adminToken, "")
	if _, leaked := fake.ws.Requirements["egress:"+cp]; leaked {
		t.Fatalf("H2: with ControlPlaneURL unset, the control-plane host was promoted (code=%d, rows=%v)", w.Code, fake.ws.Requirements)
	}
}

// TestF6Gap_PublicIPLiteralIsNotPromotable — H4. record.go's comment says an
// IP literal fails the approve-lane shape; hostrules.ValidApprovedHost's regex
// accepts dotted digits, so a PUBLIC IP literal reached under allow-all is
// promotable (private/metadata literals are deny-only by the builtin guard and
// so are excluded by AllowCount, not by shape).
func TestF6Gap_PublicIPLiteralIsNotPromotable(t *testing.T) {
	runID, wsID := uuid.New(), uuid.New()
	obs := recordmode.Observations{Domains: []recordmode.DomainObservation{{Host: "93.184.216.34", AllowCount: 1}}}
	fake := &recordStore{importStateFake: importStateFake{ws: f6Workspace(wsID, runID, "build", f6RecordedOpen(runID, &obs))}}
	srv := newTestSrv(t, fake)

	w := do(t, srv, http.MethodPost, f6PromoteURL(wsID, "build"), adminToken, "")
	if _, leaked := fake.ws.Requirements["egress:93.184.216.34"]; leaked {
		t.Fatalf("H4: a public IP literal was promoted as a required egress row (code=%d, rows=%v)", w.Code, fake.ws.Requirements)
	}
}
