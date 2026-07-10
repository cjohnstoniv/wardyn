// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package live

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestLive_Interactive proves the interactive lane: an interactive run comes up
// idle, a client attaches over the WS-PTY, drives a shell, and — critically —
// the sandbox's egress boundary is ENFORCED during the interactive session (a
// denied host is held with a 403 from inside the attached shell). Interactive
// runs never exec agent-run, so this is inherently a stream assertion (the
// acknowledged exception to the state-grader rule).
func TestLive_Interactive(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	installed := h.installedClasses(ctx)
	if !installed["CC1"] {
		t.Fatal("stack does not advertise CC1 — check /healthz")
	}

	var task Task
	for _, tk := range h.loadTasks() {
		if tk.Name == "interactive-repl" {
			task = tk
		}
	}
	if task.Name == "" {
		t.Skip("interactive-repl task not found in the corpus")
	}

	// Prove the interactive PTY + in-session egress boundary under each installed
	// confinement substrate (attach works, and the boundary holds, at CC1/CC2/CC3).
	for _, class := range sortedInstalled(installed) {
		class := class
		t.Run(class, func(t *testing.T) {
			hh := h.forT(t)
			ws := hh.seedWorkspace(task, "interactive-"+class, false)
			// Oracle image gives a shell + curl (no model); interactive runs open a
			// fresh shell via Runner.Attach (the agent task is never exec'd).
			spec := hh.buildManualPolicy(task, class, ws, false, true /* interactive */)
			run := hh.launchManual(ctx, "oracle", "", class, spec, true)
			t.Logf("launched interactive run %s at %s (%s)", run.ID, class, run.State)
			defer func() {
				kctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				_, _ = hh.sdk.KillRun(kctx, run.ID)
			}()

			// Interactive stays RUNNING/idle — it never goes terminal.
			running := hh.pollRunning(run.ID, 90*time.Second)
			if running.State != types.RunRunning {
				t.Fatalf("interactive run at %s never reached RUNNING: state=%s", class, running.State)
			}

			conn := hh.dialAttach(t, run.ID)
			defer conn.Close(websocket.StatusNormalClosure, "done")

			// 1) PTY round-trips bytes: echo a computed token and read it back.
			driveExpect(t, conn, "echo wardyn-pty-$((6*7))\n", "wardyn-pty-42", 20*time.Second)
			t.Logf("%s PTY echo round-trip OK (wardyn-pty-42)", class)

			// 2) SELECTIVITY positive control: an ALLOWED host succeeds through the
			//    SAME proxy (curl exits 0 => print WOK), so the block below can't be
			//    a deny-EVERYTHING proxy.
			driveExpect(t,
				conn,
				"curl -sS -o /dev/null -m 15 -x http://wardyn-proxy:3128 https://github.com/ && printf 'W''OK\\n'\n",
				"WOK",
				25*time.Second)
			t.Logf("%s in-PTY allowed host reachable via proxy (github.com) — proxy up + selective", class)

			// 3) CONFINEMENT during the session: a denied host is held by the proxy
			//    (403) from inside the attached shell — the point of a governed box.
			driveExpect(t,
				conn,
				"curl -sS -o /dev/null -m 12 -w '%{http_code}' -x http://wardyn-proxy:3128 https://evil.example.com/\n",
				"403",
				25*time.Second)
			t.Logf("%s in-PTY egress boundary enforced: evil.example.com held (403)", class)
		})
	}
}

// pollRunning waits until the run is RUNNING (or a terminal state, which is a
// failure for an interactive run).
func (h *harness) pollRunning(id uuid.UUID, timeout time.Duration) types.AgentRun {
	h.t.Helper()
	run, ok := h.pollState(id, timeout, runningPred)
	if !ok {
		if isTerminal(run.State) {
			h.t.Fatalf("interactive run went terminal (%s) before RUNNING; it should idle awaiting attach", run.State)
		}
		h.t.Fatalf("run %s did not reach RUNNING within %s (last=%s)", id, timeout, run.State)
	}
	return run
}

// runningPred is the pollState predicate shared by pollRunning/pollRunningSoft:
// done+good on RUNNING, done+!good on any terminal state, else keep polling.
func runningPred(s types.RunState) (done, good bool) {
	if s == types.RunRunning {
		return true, true
	}
	return isTerminal(s), false
}

// dialAttach opens the attach WS-PTY as a non-browser client (no Origin header,
// so the server's same-origin check does not apply — only the bearer matters).
func (h *harness) dialAttach(t *testing.T, id uuid.UUID) *websocket.Conn {
	t.Helper()
	wsURL := strings.Replace(h.base, "http", "ws", 1) + "/api/v1/runs/" + id.String() + "/attach?cols=120&rows=40"
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + h.token}},
	})
	if err != nil {
		t.Fatalf("dial attach WS %s: %v", wsURL, err)
	}
	conn.SetReadLimit(1 << 20)
	return conn
}

// driveUntil writes send as a binary PTY frame (skipped when send == ""), then
// reads binary output frames — tolerating read-deadline errs between bursts —
// accumulating them until matchFn reports a match or the timeout elapses.
// Returns the accumulated stream and whether matchFn ever matched.
func driveUntil(t *testing.T, conn *websocket.Conn, send string, timeout time.Duration, matchFn func(buf string) bool) (string, bool) {
	t.Helper()
	if send != "" {
		wctx, wcancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := conn.Write(wctx, websocket.MessageBinary, []byte(send))
		wcancel()
		if err != nil {
			t.Fatalf("write PTY input %q: %v", send, err)
		}
	}

	var buf strings.Builder
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		rctx, rcancel := context.WithTimeout(context.Background(), 3*time.Second)
		typ, data, err := conn.Read(rctx)
		rcancel()
		if err != nil {
			// A read timeout is expected between bursts of output; keep polling
			// until the overall deadline. Any other error ends the attempt.
			if time.Now().Before(deadline) && errorsIsDeadline(err) {
				continue
			}
			break
		}
		if typ == websocket.MessageBinary {
			buf.Write(data)
			if matchFn(buf.String()) {
				return buf.String(), true
			}
		}
	}
	return buf.String(), false
}

// driveExpect writes input bytes as a binary PTY frame, then reads binary output
// frames until `want` appears in the accumulated stream or the deadline passes.
func driveExpect(t *testing.T, conn *websocket.Conn, send, want string, timeout time.Duration) {
	t.Helper()
	buf, ok := driveUntil(t, conn, send, timeout, func(b string) bool { return strings.Contains(b, want) })
	if !ok {
		t.Fatalf("did not see %q in PTY output within %s after sending %q.\n--- stream ---\n%s",
			want, timeout, strings.TrimSpace(send), tail(buf, 800))
	}
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
