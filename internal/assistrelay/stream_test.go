// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package assistrelay

import (
	"bufio"
	"os"
	"reflect"
	"strings"
	"testing"
)

// parseFile runs every line of a captured harness stream through one parser.
func parseFile(t *testing.T, path string) []Step {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	p := NewParser()
	var steps []Step
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), harnessLineMax)
	for sc.Scan() {
		st, err := p.Line(sc.Bytes())
		if err != nil {
			t.Fatalf("line %q: %v", sc.Text(), err)
		}
		steps = append(steps, st)
	}
	return steps
}

// TestParseCapturedStream reads the two-turn stream the pinned harness wrote in stream-json mode.
func TestParseCapturedStream(t *testing.T) {
	steps := parseFile(t, "testdata/stream.jsonl")
	if len(steps) != 10 {
		t.Fatalf("got %d steps, want the sample's 10 lines", len(steps))
	}
	var events []Event
	var text []string
	ends, calls := 0, 0
	for _, st := range steps {
		for _, ev := range st.Events {
			if ev.Kind == kindText {
				text = append(text, ev.Payload["text"].(string))
				continue
			}
			events = append(events, ev)
		}
		calls += st.ToolCalls
		if st.EndTurn {
			ends++
			if st.Usage == nil || st.Usage.InputTokens != 20 || st.Usage.OutputTokens != 10 || st.Failed {
				t.Fatalf("result step = %+v usage %+v; want usage 20 in, 10 out, not failed", st, st.Usage)
			}
		}
	}
	if steps[0].HarnessSession != "d86bec66-1a0f-4dda-9def-dcadb7d95d23" {
		t.Fatalf("init session = %q", steps[0].HarnessSession)
	}
	want := []Event{
		{Kind: kindToolCall, Payload: map[string]any{"name": "read", "summary": `{"what":"unit"}`}},
		{Kind: kindToolResult, Payload: map[string]any{"name": "read", "ok": true, "summary": "stub record"}},
		{Kind: kindToolCall, Payload: map[string]any{"name": "read", "summary": `{"what":"unit"}`}},
		{Kind: kindToolResult, Payload: map[string]any{"name": "read", "ok": true, "summary": "stub record"}},
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %#v\nwant %#v", events, want)
	}
	if !reflect.DeepEqual(text, []string{"stub text 2", "stub text 4"}) || ends != 2 || calls != 2 {
		t.Fatalf("text %q, ends %d, calls %d; want two texts, two ends, two calls", text, ends, calls)
	}
}

// TestParseCapturedDenial reads a stream where the harness refused the MCP call (no allow-list).
func TestParseCapturedDenial(t *testing.T) {
	var results []Event
	for _, st := range parseFile(t, "testdata/denied.jsonl") {
		for _, ev := range st.Events {
			if ev.Kind == kindToolResult {
				results = append(results, ev)
			}
		}
	}
	if len(results) != 1 || results[0].Payload["ok"] != false || results[0].Payload["name"] != "read" ||
		!strings.HasPrefix(results[0].Payload["summary"].(string), "Claude requested permissions") {
		t.Fatalf("results = %#v; want one failed read", results)
	}
}

func TestParseEndConversationEndsTheTurnOnly(t *testing.T) {
	st, err := NewParser().Line([]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"bye"},` +
		`{"type":"tool_use","id":"t1","name":"EndConversation","input":{}}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	want := []Event{{Kind: kindText, Payload: map[string]any{"text": "bye"}}}
	if !st.EndTurn || !st.EndConversation || st.ToolCalls != 0 || !reflect.DeepEqual(st.Events, want) {
		t.Fatalf("step = %+v; want the turn ended, no tool call counted or shown", st)
	}
}

func TestParseSummariesAreCapped(t *testing.T) {
	long := strings.Repeat("é", summaryMax+50)
	p := NewParser()
	st, err := p.Line([]byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"mcp__wardyn__read","input":{"q":"` + long + `"}}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := []rune(st.Events[0].Payload["summary"].(string)); len(got) != summaryMax {
		t.Fatalf("call summary has %d characters, want %d", len(got), summaryMax)
	}
	st, err = p.Line([]byte(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"` + long + `","is_error":true}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	ev := st.Events[0]
	if got := []rune(ev.Payload["summary"].(string)); len(got) != summaryMax || ev.Payload["ok"] != false || ev.Payload["name"] != "read" {
		t.Fatalf("result = %#v; want a %d-character summary of a failed read", ev, summaryMax)
	}
}

func TestParseOtherLines(t *testing.T) {
	p := NewParser()
	for _, line := range []string{"", "  ", `{"type":"system","subtype":"permission_denied"}`, `{"type":"user","message":{"content":"plain"}}`} {
		st, err := p.Line([]byte(line))
		if err != nil || !reflect.DeepEqual(st, Step{}) {
			t.Fatalf("line %q = %+v, %v; want an empty step", line, st, err)
		}
	}
	if _, err := p.Line([]byte("not json")); err == nil {
		t.Fatal("a line that is not JSON parsed")
	}
	st, err := p.Line([]byte(`{"type":"result","subtype":"error_max_turns","is_error":true}`))
	if err != nil || !st.EndTurn || !st.Failed {
		t.Fatalf("failed result = %+v, %v; want an end that failed", st, err)
	}
}

// TestParseKeepsLineOrder: text and tool uses come out in the order the message gives them.
func TestParseKeepsLineOrder(t *testing.T) {
	st, err := NewParser().Line([]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"A"},` +
		`{"type":"tool_use","id":"t1","name":"mcp__wardyn__read","input":{}},{"type":"text","text":"B"}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, ev := range st.Events {
		kinds = append(kinds, ev.Kind)
	}
	if !reflect.DeepEqual(kinds, []string{kindText, kindToolCall, kindText}) || st.Events[0].Payload["text"] != "A" ||
		st.Events[2].Payload["text"] != "B" || st.ToolCalls != 1 {
		t.Fatalf("events %+v; want text A, the tool call, text B", st.Events)
	}
}
