// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	claudeCred = "claude-credential-plaintext"
	codexCred  = "codex-credential-plaintext"
)

func credDigest(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// perHarnessProxy is a proxy on the per-harness regime: claude-code may Read and
// is denied WebFetch, codex-cli is denied Read, and a third credentialed harness
// has no entry at all. cpCalls counts the control-plane raises a hold makes.
func perHarnessProxy(t *testing.T) (p *Proxy, cpCalls *int) {
	t.Helper()
	n := 0
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(types.ApprovalRequest{ID: uuid.New(), RunID: uuid.New(), Kind: types.ApprovalToolCall, State: types.ApprovalPending})
	}))
	t.Cleanup(cp.Close)
	p, _ = newLocalRouteProxy(t, "http://wardynd.test:8080", "RUNTOK", upstreamAddr(cp), nil, nil)
	p.harnessRules = []types.HarnessToolRules{
		{Harness: "claude-code", Rules: []types.ToolRule{{Tool: "Read", Effect: types.ToolAllow}, {Tool: "WebFetch", Effect: types.ToolDeny}}},
		{Harness: "codex-cli", Rules: []types.ToolRule{{Tool: "Read", Effect: types.ToolDeny}}},
	}
	p.harnessCreds = []types.HarnessCredential{
		{Harness: "claude-code", SHA256: credDigest(claudeCred)},
		{Harness: "codex-cli", SHA256: credDigest(codexCred)},
		{Harness: "gemini-cli", SHA256: credDigest("gemini-credential-plaintext")},
	}
	return p, &n
}

func raiseTool(t *testing.T, p *Proxy, tool, bearer, extraBody string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"kind":"tool_call","payload":{"tool":"` + tool + `","cmd":"x"}` + extraBody + `}`
	req := mustLocalReq(t, http.MethodPost, routeApprovalsCreate, strings.NewReader(body))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

// A harness the rules do not list, a call with no credential and a call with one
// the proxy never issued are all held for a human; none borrows another
// harness's allow.
func TestToolRules_UnlistedHarnessHeldNeverInherits(t *testing.T) {
	p, cpCalls := perHarnessProxy(t)
	if rec := raiseTool(t, p, "Read", claudeCred, ""); rec.Code != http.StatusOK || *cpCalls != 0 || !strings.Contains(rec.Body.String(), "APPROVED") {
		t.Fatalf("the listed harness's own allow: %d %s (cp %d), want APPROVED with no raise", rec.Code, rec.Body.String(), *cpCalls)
	}
	for name, bearer := range map[string]string{
		"a credentialed harness with no entry": "gemini-credential-plaintext",
		"no credential":                        "",
		"a credential nobody issued":           "guessed",
	} {
		before := *cpCalls
		rec := raiseTool(t, p, "Read", bearer, "")
		if rec.Code != http.StatusCreated || *cpCalls != before+1 || strings.Contains(rec.Body.String(), "APPROVED") {
			t.Errorf("%s: %d %s (cp %d->%d), want a held approval", name, rec.Code, rec.Body.String(), before, *cpCalls)
		}
	}
}

// The harness is the one the proxy-issued credential names. A body that claims
// another harness is ignored, in both directions.
func TestToolRules_HarnessFromTheProxyIssuedCredential(t *testing.T) {
	p, cpCalls := perHarnessProxy(t)
	// codex's credential, a body claiming claude-code (whose rules allow Read): codex's deny applies.
	rec := raiseTool(t, p, "Read", codexCred, `,"harness":"claude-code","payload_harness":"claude-code"`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "DENIED") || *cpCalls != 0 {
		t.Fatalf("codex claiming claude-code: %d %s (cp %d), want its own DENIED", rec.Code, rec.Body.String(), *cpCalls)
	}
	// No credential, a body claiming claude-code: held, not allowed.
	rec = raiseTool(t, p, "Read", "", `,"harness":"claude-code"`)
	if rec.Code != http.StatusCreated || *cpCalls != 1 {
		t.Fatalf("an uncredentialed claim: %d %s (cp %d), want held", rec.Code, rec.Body.String(), *cpCalls)
	}
	// The same body field inside the payload.
	req := mustLocalReq(t, http.MethodPost, routeApprovalsCreate,
		strings.NewReader(`{"kind":"tool_call","payload":{"tool":"Read","cmd":"x","harness":"claude-code"}}`))
	req.Header.Set("Authorization", "Bearer "+codexCred)
	out := httptest.NewRecorder()
	p.ServeHTTP(out, req)
	if !strings.Contains(out.Body.String(), "DENIED") {
		t.Errorf("a payload claim moved the harness: %s", out.Body.String())
	}
}

// Under per-harness rules a run-wide policy rule is not consulted, and a run on
// the older regime (no harness rules) is unchanged by a credential header.
func TestToolRules_RegimesDoNotMix(t *testing.T) {
	p, cpCalls := perHarnessProxy(t)
	p.policy = CompilePolicy(types.RunPolicySpec{ToolRules: []types.ToolRule{{Tool: "Bash", Effect: types.ToolAllow}}})
	if rec := raiseTool(t, p, "Bash", claudeCred, ""); rec.Code != http.StatusCreated || *cpCalls != 1 {
		t.Errorf("a run-wide allow answered under per-harness rules: %d %s", rec.Code, rec.Body.String())
	}
	p.harnessRules, p.harnessCreds = nil, nil
	if rec := raiseTool(t, p, "Bash", "anything", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "APPROVED") {
		t.Errorf("the older regime changed: %d %s", rec.Code, rec.Body.String())
	}
}

// The sidecar refuses a malformed carrier at start, and the digests are all it
// is ever given.
func TestConfigHarnessCredentialsAreValidated(t *testing.T) {
	base := `{"run_id":"` + uuid.NewString() + `","control_plane_url":"http://127.0.0.1:1","run_token":"t","policy":{},"harness_credentials":`
	good := `[{"harness":"claude-code","sha256":"` + credDigest("x") + `"}]`
	if _, err := LoadConfigBytes([]byte(base + good + `}`)); err != nil {
		t.Fatalf("a digest was refused: %v", err)
	}
	for name, bad := range map[string]string{
		"a plaintext in the digest slot": `[{"harness":"claude-code","sha256":"claude-credential-plaintext"}]`,
		"upper-case digest":              `[{"harness":"claude-code","sha256":"` + strings.ToUpper(credDigest("x")) + `"}]`,
		"a harness twice":                `[{"harness":"a","sha256":"` + credDigest("x") + `"},{"harness":"a","sha256":"` + credDigest("y") + `"}]`,
		"one credential for two":         `[{"harness":"a","sha256":"` + credDigest("x") + `"},{"harness":"b","sha256":"` + credDigest("x") + `"}]`,
		"an unknown key":                 `[{"harness":"a","sha256":"` + credDigest("x") + `","token":"t"}]`,
	} {
		if _, err := LoadConfigBytes([]byte(base + bad + `}`)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}
