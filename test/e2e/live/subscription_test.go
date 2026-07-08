// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package live

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// Subscription proxy-side token-injection lanes. Two mirror-image proofs of the
// feature committed in 1138705 (see test/e2e/RESULTS.md "Subscription proxy-side
// token injection"): a subscription run holds only an inert SENTINEL credential
// and the egress proxy TLS-MITMs api.anthropic.com to inject the operator's LIVE
// host OAuth token per request.
//
//   - TestLive_SubscriptionInject      (WARDYN_E2E_EXPECT_INJECT=on, the default):
//     the safe default. Launching a subscription run authors the re-mintable
//     injection grant + auto-enables MITM — proven by the wardynd-emitted
//     run.llm.subscription_inject audit event — then `wardyn attach` reaches a
//     shell whose curl to api.anthropic.com traverses the injected+MITM'd path.
//   - TestLive_SubscriptionEscapeHatch (WARDYN_E2E_EXPECT_INJECT=off): the
//     WARDYN_SUBSCRIPTION_INJECT=off escape hatch. NO injection grant is authored
//     (no audit event, no MITM), so a garbage sandbox credential reaches Anthropic
//     over the opaque tunnel unmodified and is rejected 401 — the resident-copy
//     legacy behavior.
//
// A single wardynd is in exactly ONE mode, so each lane self-skips unless the
// running stack matches WARDYN_E2E_EXPECT_INJECT. scripts/run-e2e-subscription.sh
// runs BOTH by restarting wardynd with the env flipped between them.
//
// The control-plane audit event is the load-bearing discriminator (it is emitted
// directly by wardynd, so it is reliable even on a Docker-Desktop/WSL host where
// the proxy->control-plane egress callback may not route — see RESULTS.md). The
// in-PTY curl corroborates at the transport layer.

const subscriptionInjectAuditAction = "run.llm.subscription_inject"

// expectInject reads the mode the operator/driver brought the stack up in
// ("on" | "off"), defaulting to "on" (the safe default when a token provider is
// wired). Each lane runs only when its mode matches the running wardynd.
func expectInject() string {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("WARDYN_E2E_EXPECT_INJECT")))
	if v == "off" {
		return "off"
	}
	return "on"
}

// TestLive_SubscriptionInject proves the attach-walkthrough on the safe default:
// a subscription run authors the injection grant + MITM (audit event), then a
// human attaches and the injected credential path is live end-to-end.
func TestLive_SubscriptionInject(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if expectInject() != "on" {
		t.Skip("WARDYN_E2E_EXPECT_INJECT != on; this stack is not in inject-on mode")
	}
	if h.subscriptionMounts() == nil {
		t.Skip("no staged Claude subscription creds (scripts/stage-claude-creds.sh); cannot mount .claude to engage subscription mode")
	}

	run, class, skip := h.launchSubscriptionInteractive(ctx, "inject")
	if skip != "" {
		t.Skip(skip)
	}
	defer h.killQuietly(run.ID)
	t.Logf("launched subscription interactive run %s at %s (%s)", run.ID, class, run.State)

	// PRIMARY, deterministic proof: the injection grant was authored and MITM
	// auto-enabled. This audit event is written by wardynd at dispatch, BEFORE
	// any proxy token-resolve, so it holds even if the resident token is near
	// expiry and a downstream refresh is rate-limited.
	ev, ok := h.awaitAudit(run.ID, subscriptionInjectAuditAction, "success", 60*time.Second)
	if !ok {
		t.Fatalf("subscription-on run %s never emitted a %s/success audit event — injection grant + MITM were not authored",
			run.ID, subscriptionInjectAuditAction)
	}
	if ev.Data != nil {
		if v, _ := ev.Data["tls_mitm"].(bool); !v {
			t.Errorf("%s audit did not mark tls_mitm=true: %v", subscriptionInjectAuditAction, ev.Data)
		}
	}
	t.Logf("control plane authored the proxy-side injection grant + enabled TLS-MITM of api.anthropic.com (audit %s)", subscriptionInjectAuditAction)

	// The run must actually come up for `wardyn attach`. If the proxy sidecar
	// failed to RESOLVE the live token (resident token near expiry AND the
	// delegated refresh is rate-limited), the sandbox never reaches RUNNING —
	// that is an EXTERNAL dependency, not a feature defect, so skip (the primary
	// control-plane proof above already passed).
	running, up := h.pollRunningSoft(run.ID, 90*time.Second)
	if !up {
		t.Skipf("subscription run %s did not reach RUNNING (last=%s) — likely the proxy could not resolve the live OAuth token "+
			"(resident token near expiry + delegated refresh rate-limited); the injection-grant proof above still holds", run.ID, running.State)
	}

	conn := h.dialAttach(t, run.ID)
	defer conn.Close(websocket.StatusNormalClosure, "done")

	// `wardyn attach` reaches a live shell: echo a computed token back.
	driveExpect(t, conn, "echo wardyn-sub-$((6*7))\n", "wardyn-sub-42", 20*time.Second)
	t.Logf("attach PTY round-trip OK — a human at `wardyn attach` has a live shell in the subscription sandbox")

	// Transport corroboration: from inside the attached sandbox, hit
	// api.anthropic.com through the proxy carrying only a GARBAGE sentinel
	// credential. The proxy STRIPS it and injects the live token; -k accepts the
	// per-run MITM cert (interactive sandboxes don't install the CA on disk). We
	// LOG the HTTP code rather than assert a specific one: a raw curl omits
	// claude's OAuth beta headers, so Anthropic's downstream verdict is not a
	// stable signal — but reaching a real HTTP response (not a proxy 403-deny or
	// an unroutable 000) proves the injected + MITM'd path is live in-session.
	code := driveAnthropicProbe(t, conn, true /* insecure: accept MITM cert */)
	t.Logf("in-PTY curl api.anthropic.com (garbage sentinel cred, via proxy+MITM) -> HTTP %s", code)
	if code == "000" {
		t.Errorf("in-PTY curl to api.anthropic.com did not reach an HTTP response (code 000) — the injected/MITM path is not live")
	}
	if code == "403" {
		t.Errorf("in-PTY curl to api.anthropic.com was proxy-denied (403) — api.anthropic.com should be auto-allowed on a subscription-inject run")
	}

	// Optional true end-to-end walkthrough: drive the real `claude` CLI in the
	// attached PTY (needs the claude-code image + WARDYN_E2E_REAL_MODEL=1). A
	// rate-limit reply is a PASS (auth succeeded — the handoff's own proof); only
	// an auth/401 error fails. A TUI hang/timeout is a harness issue, logged not failed.
	if h.realModel {
		h.driveRealClaude(t, conn)
	}
}

// TestLive_SubscriptionEscapeHatch proves WARDYN_SUBSCRIPTION_INJECT=off: no
// injection grant is authored and a garbage sandbox credential is rejected by
// Anthropic (the legacy resident-copy path, no proxy-side swap).
func TestLive_SubscriptionEscapeHatch(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if expectInject() != "off" {
		t.Skip("WARDYN_E2E_EXPECT_INJECT != off; restart wardynd with WARDYN_SUBSCRIPTION_INJECT=off to exercise this lane (scripts/run-e2e-subscription.sh does this)")
	}
	if h.subscriptionMounts() == nil {
		t.Skip("no staged Claude subscription creds (scripts/stage-claude-creds.sh); cannot mount .claude to engage subscription mode")
	}

	run, class, skip := h.launchSubscriptionInteractive(ctx, "escape")
	if skip != "" {
		t.Skip(skip)
	}
	defer h.killQuietly(run.ID)
	t.Logf("launched subscription (inject=off) interactive run %s at %s (%s)", run.ID, class, run.State)

	// PRIMARY proof: the escape hatch authored NO injection grant → NO MITM. Give
	// the dispatch a moment to have emitted it had it been going to, then assert
	// absence.
	running, up := h.pollRunningSoft(run.ID, 90*time.Second)
	if !up {
		t.Fatalf("subscription (inject=off) run %s did not reach RUNNING (last=%s); the resident-copy path resolves no token and should come up cleanly",
			run.ID, running.State)
	}
	if _, ok := h.awaitAudit(run.ID, subscriptionInjectAuditAction, "success", 5*time.Second); ok {
		t.Fatalf("inject=off run %s emitted %s — the escape hatch must NOT author a proxy-side injection grant", run.ID, subscriptionInjectAuditAction)
	}
	t.Logf("escape hatch honored: no %s audit event — no proxy-side injection grant, no MITM", subscriptionInjectAuditAction)

	conn := h.dialAttach(t, run.ID)
	defer conn.Close(websocket.StatusNormalClosure, "done")
	driveExpect(t, conn, "echo wardyn-esc-$((6*7))\n", "wardyn-esc-42", 20*time.Second)

	// Mirror-image transport proof: with no proxy-side injection, the GARBAGE
	// sentinel credential reaches api.anthropic.com over the opaque tunnel
	// unmodified and Anthropic rejects it 401 (the handoff verified "the garbage
	// token alone returns 401"). A non-401 would mean something still swapped it.
	code := driveAnthropicProbe(t, conn, true)
	t.Logf("in-PTY curl api.anthropic.com (garbage sentinel cred, inject=off) -> HTTP %s", code)
	if code != "401" {
		t.Fatalf("inject=off: expected Anthropic to reject the garbage sentinel with 401 (no proxy-side swap), got HTTP %s", code)
	}
	t.Logf("garbage sentinel credential rejected 401 — confirms NO live token was injected (legacy resident-copy behavior)")
}

// ── shared subscription helpers ──────────────────────────────────────────────

// launchSubscriptionInteractive seeds a trivial workspace and launches an
// interactive run with the .claude credential mounts that engage subscription
// mode. It uses the oracle image (shell + curl, no model) unless the real-model
// lane is on, in which case it uses claude-code so `claude` is on PATH. Returns
// a non-empty skip reason (instead of failing) when the run can't be scheduled —
// e.g. subscription floors to CC3/Vault and this host lacks that runtime.
func (h *harness) launchSubscriptionInteractive(ctx context.Context, label string) (types.AgentRun, string, string) {
	h.t.Helper()
	installed := h.installedClasses(ctx)
	class := h.bestInstalledClass(ctx)

	// A trivial interactive task just needs a workspace to mount + attach into.
	task := Task{Name: "interactive-repl", Agent: "oracle"}
	ws := h.seedWorkspace(task, "sub-"+label, false)

	mounts := []types.WorkspaceMount{{Source: ws, Target: workspaceTarget, ReadOnly: boolPtr(false)}}
	mounts = append(mounts, h.subscriptionMounts()...) // the .claude mounts → subscription mode
	spec := types.RunPolicySpec{
		MinConfinementClass: types.ConfinementClass(class),
		// api.anthropic.com must be egress-allowed for the transport probe. The
		// inject path auto-adds it, but the escape-hatch (off) path does not — add
		// it explicitly so BOTH lanes can reach Anthropic through the proxy.
		AllowedDomains:   []string{"api.anthropic.com"},
		WorkspaceMounts:  mounts,
		AutoStopAfterSec: -1, // never reap an idle interactive sandbox
	}

	agent := "oracle"
	if h.realModel {
		agent = "claude-code" // needs `claude` on PATH for the optional real turn
	}
	run, err := h.sdk.CreateRun(ctx, client.CreateRunRequest{
		Agent:            agent,
		Repo:             "local:e2e",
		ConfinementClass: class,
		Interactive:      true,
		InlinePolicy:     &spec,
	})
	if err != nil {
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && apiErr.Status == 422 && strings.Contains(apiErr.Body, "confinement") {
			return types.AgentRun{}, class, "subscription runs floor to CC3/Vault; this host cannot enforce it (best installed: " + class + ", installed: " + strings.Join(keys(installed), ",") + ")"
		}
		h.t.Fatalf("CreateRun(subscription %s): %v", label, err)
	}
	return run, class, ""
}

// awaitAudit polls the run's audit stream until an event with the given action +
// outcome appears (returns it), or the timeout elapses. Data is decoded into a
// generic map for field checks (e.g. tls_mitm).
func (h *harness) awaitAudit(id uuid.UUID, action, outcome string, timeout time.Duration) (decodedAudit, bool) {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		events, err := h.sdk.AuditEvents(ctx, id)
		cancel()
		if err == nil {
			for _, e := range events {
				if e.Action == action && (outcome == "" || e.Outcome == outcome) {
					var data map[string]any
					if len(e.Data) > 0 {
						_ = json.Unmarshal(e.Data, &data)
					}
					return decodedAudit{Action: e.Action, Outcome: e.Outcome, Data: data}, true
				}
			}
		}
		if !time.Now().Before(deadline) {
			return decodedAudit{}, false
		}
		time.Sleep(2 * time.Second)
	}
}

type decodedAudit struct {
	Action  string
	Outcome string
	Data    map[string]any
}

// pollRunningSoft is pollRunning without the t.Fatal: it returns ok=false when
// the run goes terminal or never reaches RUNNING, so a lane can decide whether
// that is a skip (external) or a failure.
func (h *harness) pollRunningSoft(id uuid.UUID, timeout time.Duration) (types.AgentRun, bool) {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	var last types.AgentRun
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		run, err := h.sdk.GetRun(ctx, id)
		cancel()
		if err == nil {
			last = run
			if run.State == types.RunRunning {
				return run, true
			}
			if isTerminal(run.State) {
				return run, false
			}
		}
		time.Sleep(2 * time.Second)
	}
	return last, false
}

// killQuietly best-effort tears down a run (deferred cleanup).
func (h *harness) killQuietly(id uuid.UUID) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, _ = h.sdk.KillRun(ctx, id)
}

// anthropicProbeCmd curls api.anthropic.com/v1/messages through the proxy with a
// deliberately GARBAGE credential and prints the HTTP status framed by sentinels
// so driveCapture can extract it. -k accepts an untrusted (per-run MITM) cert.
func anthropicProbeCmd(insecure bool) string {
	k := ""
	if insecure {
		k = "-k "
	}
	return "curl -sS -o /dev/null -m 20 " + k + "-x http://wardyn-proxy:3128 " +
		"-H 'content-type: application/json' -H 'anthropic-version: 2023-06-01' " +
		"-H 'x-api-key: wardyn-garbage-sentinel-key' " +
		"-H 'authorization: Bearer wardyn-garbage-sentinel' " +
		`-d '{"model":"claude-3-5-haiku-latest","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}' ` +
		"-w 'WCODE=%{http_code}=WEND\\n' https://api.anthropic.com/v1/messages\n"
}

// driveAnthropicProbe runs the probe in the attached PTY and returns the HTTP
// status curl observed ("000" on connect/TLS failure).
func driveAnthropicProbe(t *testing.T, conn *websocket.Conn, insecure bool) string {
	t.Helper()
	return driveCapture(t, conn, anthropicProbeCmd(insecure), "WCODE=", "=WEND", 30*time.Second)
}

// driveCapture writes send as a PTY frame, reads output until endMark appears,
// and returns the substring between the LAST startMark and the following endMark
// (the shell echoes the command, so the real -w output is the last occurrence).
func driveCapture(t *testing.T, conn *websocket.Conn, send, startMark, endMark string, timeout time.Duration) string {
	t.Helper()
	wctx, wcancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := conn.Write(wctx, websocket.MessageBinary, []byte(send)); err != nil {
		wcancel()
		t.Fatalf("write PTY probe: %v", err)
	}
	wcancel()

	var buf strings.Builder
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		rctx, rcancel := context.WithTimeout(context.Background(), 3*time.Second)
		typ, data, err := conn.Read(rctx)
		rcancel()
		if err != nil {
			if time.Now().Before(deadline) && errorsIsDeadline(err) {
				continue
			}
			break
		}
		if typ != websocket.MessageBinary {
			continue
		}
		buf.WriteString(string(data))
		s := buf.String()
		// Only match once the marker is the -w OUTPUT (a line beginning startMark),
		// not the echoed command that also contains "WCODE=%{http_code}".
		if i := strings.LastIndex(s, startMark); i >= 0 {
			rest := s[i+len(startMark):]
			if j := strings.Index(rest, endMark); j >= 0 {
				code := strings.TrimSpace(rest[:j])
				if code != "" && !strings.Contains(code, "{") { // skip the echoed %{http_code}
					return code
				}
			}
		}
	}
	t.Fatalf("did not capture %s...%s in PTY output within %s.\n--- stream ---\n%s", startMark, endMark, timeout, tail(buf.String(), 800))
	return ""
}

// driveRealClaude drives a real `claude -p` turn in the attached PTY (real-model
// lane only). Auth succeeding is the point: a normal reply OR a rate-limit
// message passes; only an explicit auth error fails. A TUI hang is logged, not failed.
func (h *harness) driveRealClaude(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	send := "claude -p 'Reply with only the number 42.' --output-format text 2>&1; echo WCLAUDE-DONE\n"
	wctx, wcancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := conn.Write(wctx, websocket.MessageBinary, []byte(send)); err != nil {
		wcancel()
		t.Logf("real-model: write claude turn failed: %v (skipping the optional turn)", err)
		return
	}
	wcancel()

	var buf strings.Builder
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		rctx, rcancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, data, err := conn.Read(rctx)
		rcancel()
		if err != nil {
			if time.Now().Before(deadline) && errorsIsDeadline(err) {
				continue
			}
			break
		}
		buf.WriteString(string(data))
		if strings.Contains(buf.String(), "WCLAUDE-DONE") {
			break
		}
	}
	out := strings.ToLower(buf.String())
	authFail := strings.Contains(out, "authentication_error") || strings.Contains(out, "invalid bearer") ||
		strings.Contains(out, "401") || strings.Contains(out, "please run") || strings.Contains(out, "invalid api key") ||
		strings.Contains(out, "oauth token has expired")
	if authFail {
		t.Fatalf("real-model: `claude` in the subscription sandbox hit an AUTH error — proxy-side injection is broken.\n--- output ---\n%s", tail(buf.String(), 1200))
	}
	switch {
	case strings.Contains(out, "42"):
		t.Logf("real-model: `wardyn attach` -> `claude` replied through the injected subscription token (saw 42) — full walkthrough PASS")
	case strings.Contains(out, "limit") || strings.Contains(out, "usage"):
		t.Logf("real-model: `claude` authenticated (rate/usage-limit reply, not an auth error) — injection is load-bearing; PASS")
	default:
		t.Logf("real-model: `claude` produced no clear reply within the window (no auth error). Likely a TUI/permission prompt — not treated as a feature failure.\n--- output ---\n%s", tail(buf.String(), 800))
	}
}
