// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestDeadlineDefaultFromEnv pins RL-1: -deadline's default tracks
// WARDYN_APPROVAL_EXPIRY_AFTER (dispatch's mirror of the operator's real
// approval-expiry-after ceiling) instead of a bare 24h literal, so raising
// the ceiling actually lets a tool call wait the full budget.
func TestDeadlineDefaultFromEnv(t *testing.T) {
	t.Run("unset falls back to the 24h default", func(t *testing.T) {
		t.Setenv("WARDYN_APPROVAL_EXPIRY_AFTER", "")
		if got := deadlineDefault(); got != defaultDeadline {
			t.Fatalf("deadlineDefault() = %v, want %v", got, defaultDeadline)
		}
	})
	t.Run("a valid duration overrides it", func(t *testing.T) {
		t.Setenv("WARDYN_APPROVAL_EXPIRY_AFTER", "72h")
		if got, want := deadlineDefault(), 72*time.Hour; got != want {
			t.Fatalf("deadlineDefault() = %v, want %v", got, want)
		}
	})
	t.Run("garbage falls back rather than failing the gate", func(t *testing.T) {
		t.Setenv("WARDYN_APPROVAL_EXPIRY_AFTER", "not-a-duration")
		if got := deadlineDefault(); got != defaultDeadline {
			t.Fatalf("deadlineDefault() = %v, want %v (fallback)", got, defaultDeadline)
		}
	})
}

// TestPollFailureDenyMessageNamesTheCause is the half of F159's pin that
// compiles unchanged against the pre-fix gate struct (no new field): it only
// inspects the returned deny message, so red-here is a genuine assertion
// failure, not a compile error — the sibling test below additionally proves
// the stderr diagnostic exists, which needs the new field and so cannot
// compile pre-fix (documented there).
//
// Red-first: pre-fix, every poll failure is silently treated as PENDING, so
// the loop always exhausts the deadline and returns the generic
// "approval wait deadline reached" message even though every poll failed —
// the assertion below fails.
func TestPollFailureDenyMessageNamesTheCause(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/wardyn/v1/approvals":
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "11111111-2222-3333-4444-555555555555"})
		case r.Method == http.MethodGet:
			w.WriteHeader(http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	g := &gate{base: srv.URL, poll: 10 * time.Millisecond, deadline: 150 * time.Millisecond,
		client: &http.Client{Timeout: 2 * time.Second}}
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"approve","arguments":{"tool_name":"Bash","input":{},"tool_use_id":"t"}}}` + "\n")
	var out strings.Builder
	if err := g.serve(in, &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	if !strings.Contains(out.String(), "consecutive poll failures") {
		t.Fatalf("deny message does not name the real cause (poll failures), got %q", out.String())
	}
}

// TestPollFailuresAreLoggedAndNamedInDenyMessage covers F159: every poll
// error was treated identically to PENDING with no log anywhere (stdout,
// stderr, or the returned message), so a control-plane outage during the wait
// parked the agent for the full deadline and then denied it with a message
// that reads as "no human decided in time" when in fact every poll failed.
//
// Red-first: against the pre-fix decide() (bare `if err == nil {...}`, no
// else) stderr stays empty and the deny message is the generic deadline
// string, so both assertions below fail.
func TestPollFailuresAreLoggedAndNamedInDenyMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/wardyn/v1/approvals":
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "11111111-2222-3333-4444-555555555555"})
		case r.Method == http.MethodGet:
			// Every poll fails — the stub control plane is "up" for create but
			// broken for status, e.g. a proxy/control-plane outage mid-wait.
			w.WriteHeader(http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	var stderr strings.Builder
	g := &gate{base: srv.URL, poll: 10 * time.Millisecond, deadline: 150 * time.Millisecond,
		client: &http.Client{Timeout: 2 * time.Second}, stderr: &stderr}

	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"approve","arguments":{"tool_name":"Bash","input":{},"tool_use_id":"t"}}}` + "\n")
	var out strings.Builder
	if err := g.serve(in, &out); err != nil {
		t.Fatalf("serve: %v", err)
	}

	if stderr.Len() == 0 {
		t.Fatalf("want a poll-failure line on stderr, got none (out=%q)", out.String())
	}
	if !strings.Contains(stderr.String(), "poll approval") {
		t.Fatalf("stderr does not describe the poll failure: %q", stderr.String())
	}
	if !strings.Contains(out.String(), "consecutive poll failures") {
		t.Fatalf("deny message does not name the real cause (poll failures), got %q", out.String())
	}
}

// TestFinalPollCatchesLateDecision pins RL-1's final-poll fix: the wait loop
// exits once time.Now() no longer precedes `end`, which — with the loop's own
// last sleep landing past the deadline — can skip a state check that would
// have found a decision the operator made just before the deadline. Without
// one more check after the loop, that call is denied even though it was
// actually decided in time.
//
// Red-first: with -deadline shorter than -poll the loop body runs exactly
// once (PENDING, first GET) and then exits without ever seeing the second GET
// (APPROVED) this stub serves — pre-fix that returns the generic deadline
// deny; the final poll this test pins makes it see APPROVED instead.
func TestFinalPollCatchesLateDecision(t *testing.T) {
	var gets int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/wardyn/v1/approvals":
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "11111111-2222-3333-4444-555555555555"})
		case r.Method == http.MethodGet:
			gets++
			state := "PENDING"
			if gets > 1 {
				state = "APPROVED"
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"state": state})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	// deadline < poll: the wait loop's body runs exactly once (sees PENDING,
	// then sleeps past the deadline), so only the final poll can see the
	// second (APPROVED) GET.
	g := &gate{base: srv.URL, poll: 100 * time.Millisecond, deadline: 20 * time.Millisecond,
		client: &http.Client{Timeout: 2 * time.Second}}
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"approve","arguments":{"tool_name":"Bash","input":{},"tool_use_id":"t"}}}` + "\n")
	var out strings.Builder
	if err := g.serve(in, &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	// The text content is JSON-stringified inside the JSON-RPC envelope
	// (permissionResult), so its quotes arrive backslash-escaped — match the
	// escaped form rather than assume raw JSON.
	if !strings.Contains(out.String(), `\"behavior\":\"allow\"`) {
		t.Fatalf("final poll did not see the late APPROVED decision, got %q", out.String())
	}
}

// drive runs one full MCP conversation (initialize → tools/list → tools/call)
// against a gate pointed at the given fake control plane, and returns the
// PermissionResult the tools/call answer carried.
func drive(t *testing.T, base string, callArguments string) map[string]any {
	t.Helper()
	g := &gate{base: base, poll: 10 * time.Millisecond, deadline: 2 * time.Second,
		client: &http.Client{Timeout: 5 * time.Second}}

	in := strings.NewReader(strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"approve","arguments":` + callArguments + `}}`,
	}, "\n") + "\n")
	var out strings.Builder
	if err := g.serve(in, &out); err != nil {
		t.Fatalf("serve: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 responses (initialize, tools/list, tools/call), got %d: %q", len(lines), out.String())
	}
	var resp struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[2]), &resp); err != nil {
		t.Fatalf("tools/call response: %v", err)
	}
	if len(resp.Result.Content) != 1 {
		t.Fatalf("want one content block, got %+v", resp.Result)
	}
	var pr map[string]any
	if err := json.Unmarshal([]byte(resp.Result.Content[0].Text), &pr); err != nil {
		t.Fatalf("PermissionResult not JSON: %q", resp.Result.Content[0].Text)
	}
	return pr
}

// fakePlane serves the two brokered routes: POST create (returns a fixed id,
// capturing the payload) and GET poll (returns states in sequence, holding the
// last one).
func fakePlane(t *testing.T, states ...string) (*httptest.Server, *map[string]any) {
	t.Helper()
	captured := map[string]any{}
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/wardyn/v1/approvals":
			_ = json.NewDecoder(r.Body).Decode(&captured)
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "11111111-2222-3333-4444-555555555555"})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/wardyn/v1/approvals/"):
			s := states[min(polls, len(states)-1)]
			polls++
			_ = json.NewEncoder(w).Encode(map[string]string{"state": s})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &captured
}

func TestApproveAfterPendingAllows(t *testing.T) {
	srv, captured := fakePlane(t, "PENDING", "PENDING", "APPROVED")
	args := `{"tool_name":"Bash","input":{"command":"date > x.txt"},"tool_use_id":"toolu_1"}`
	pr := drive(t, srv.URL, args)
	if pr["behavior"] != "allow" {
		t.Fatalf("want allow after PENDING→APPROVED, got %+v", pr)
	}
	// updatedInput must echo the original input — an allow that drops it is
	// treated by claude as a rewrite to nothing. Compare decoded (Marshal
	// escapes '>' as >; the JSON is identical, the bytes are not).
	ui, _ := pr["updatedInput"].(map[string]any)
	if ui["command"] != "date > x.txt" {
		t.Fatalf("updatedInput not echoed: %+v", pr["updatedInput"])
	}
	// The approval payload is the UI's existing tool_call shape.
	payload := (*captured)["payload"].(map[string]any)
	if (*captured)["kind"] != "tool_call" || payload["tool"] != "Bash" || payload["cmd"] != "date > x.txt" {
		t.Fatalf("payload wrong: %+v", *captured)
	}
}

func TestDenyAndExpireBothDeny(t *testing.T) {
	for _, state := range []string{"DENIED", "EXPIRED"} {
		srv, _ := fakePlane(t, state)
		pr := drive(t, srv.URL, `{"tool_name":"Edit","input":{"file_path":"a"},"tool_use_id":"t"}`)
		if pr["behavior"] != "deny" {
			t.Fatalf("state %s: want deny, got %+v", state, pr)
		}
	}
}

func TestUnreachablePlaneDeniesAtDeadline(t *testing.T) {
	// A gate that cannot raise its approval holds the door shut.
	g := &gate{base: "http://127.0.0.1:1", poll: 10 * time.Millisecond, deadline: 100 * time.Millisecond,
		client: &http.Client{Timeout: 200 * time.Millisecond}}
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"approve","arguments":{"tool_name":"Bash","input":{},"tool_use_id":"t"}}}` + "\n")
	var out strings.Builder
	if err := g.serve(in, &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	if !strings.Contains(out.String(), `\"behavior\":\"deny\"`) && !strings.Contains(out.String(), `"behavior":"deny"`) {
		t.Fatalf("want deny from unreachable plane, got %q", out.String())
	}
}

func TestSummarizeCapsAndFallsBack(t *testing.T) {
	if s := summarize("Bash", json.RawMessage(`{"command":"ls -la"}`)); s != "ls -la" {
		t.Fatalf("bash command not extracted: %q", s)
	}
	if s := summarize("Edit", json.RawMessage(`{"file_path":"a.go"}`)); s != `{"file_path":"a.go"}` {
		t.Fatalf("non-bash fallback wrong: %q", s)
	}
	long := strings.Repeat("x", maxCmdBytes+100)
	if s := summarize("Bash", json.RawMessage(`{"command":"`+long+`"}`)); len(s) > maxCmdBytes+20 || !strings.HasSuffix(s, "…[truncated]") {
		t.Fatalf("cap not applied: len=%d", len(s))
	}
}
