// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package assistrelay

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/assistant"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// fakeWardyn stands in for the proxy and the server behind it on the brokered assistant route.
type fakeWardyn struct {
	srv   *httptest.Server
	turns chan *Turn

	mu          sync.Mutex
	events      []postedEvent
	calls       []postedCall
	afters      []string
	live        string
	toolsStatus int    // 0 means 200
	toolsBody   string // "" means one tool
	toolsDrop   bool   // close the connection with no answer
	hold        string // calls of this session wait for release
	release     chan struct{}
}

type postedEvent struct {
	Session string
	Kind    string
	Payload map[string]any
}

type postedCall struct {
	Name    string
	Session string
	Args    json.RawMessage
}

const oneTool = `[{"name":"read","description":"Read a record.","input_schema":{"type":"object","properties":{"what":{"type":"string"}}}}]`

func newFakeWardyn(t *testing.T) *fakeWardyn {
	f := &fakeWardyn{turns: make(chan *Turn, 16), release: make(chan struct{})}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeWardyn) api() *API { return &API{Base: f.srv.URL, Client: f.srv.Client()} }

func (f *fakeWardyn) serve(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, routePrefix)
	switch {
	case r.Method == http.MethodGet && path == "turns":
		f.mu.Lock()
		f.afters = append(f.afters, r.URL.Query().Get("after"))
		f.mu.Unlock()
		select {
		case t := <-f.turns:
			writeJSONBody(w, http.StatusOK, t)
		case <-time.After(20 * time.Millisecond):
			w.WriteHeader(http.StatusNoContent)
		}
	case r.Method == http.MethodPost && path == "events":
		var ev struct {
			SessionID string         `json:"session_id"`
			Kind      string         `json:"kind"`
			Payload   map[string]any `json:"payload"`
		}
		_ = json.NewDecoder(r.Body).Decode(&ev)
		f.mu.Lock()
		f.events = append(f.events, postedEvent{Session: ev.SessionID, Kind: ev.Kind, Payload: ev.Payload})
		n, live := len(f.events), f.live
		f.mu.Unlock()
		if ev.SessionID != live {
			writeJSONBody(w, http.StatusConflict, map[string]any{"error": "That session is not the live one."})
			return
		}
		writeJSONBody(w, http.StatusAccepted, map[string]any{"seq": n})
	case r.Method == http.MethodGet && path == "tools":
		f.mu.Lock()
		status, body, drop := f.toolsStatus, f.toolsBody, f.toolsDrop
		f.mu.Unlock()
		if drop {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		if status == 0 {
			status = http.StatusOK
		}
		if body == "" {
			body = oneTool
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	case r.Method == http.MethodPost && strings.HasPrefix(path, "tools/"):
		var c struct {
			SessionID string          `json:"session_id"`
			Args      json.RawMessage `json:"args"`
		}
		_ = json.NewDecoder(r.Body).Decode(&c)
		f.mu.Lock()
		f.calls = append(f.calls, postedCall{Name: strings.TrimPrefix(path, "tools/"), Session: c.SessionID, Args: c.Args})
		held := c.SessionID == f.hold
		f.mu.Unlock()
		if held {
			<-f.release
		}
		f.mu.Lock()
		live := f.live
		f.mu.Unlock()
		if c.SessionID != live {
			writeJSONBody(w, http.StatusConflict, map[string]any{"error": "That session is not the live one."})
			return
		}
		writeJSONBody(w, http.StatusOK, map[string]any{"answer": "a record"})
	default:
		http.Error(w, "unexpected", http.StatusTeapot)
	}
}

func writeJSONBody(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeWardyn) setLive(s string) {
	f.mu.Lock()
	f.live = s
	f.mu.Unlock()
}

func (f *fakeWardyn) snapshot() ([]postedEvent, []postedCall) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]postedEvent(nil), f.events...), append([]postedCall(nil), f.calls...)
}

// waitFor polls until cond holds over the posted events and calls, or fails the test.
func (f *fakeWardyn) waitFor(t *testing.T, what string, cond func([]postedEvent, []postedCall) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond(f.snapshot()) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	evs, calls := f.snapshot()
	t.Fatalf("timed out waiting for %s; events %+v calls %+v", what, evs, calls)
}

func countKind(evs []postedEvent, kind string) int {
	n := 0
	for _, e := range evs {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

// fakeLauncher records each harness start and stop, and runs a script in place of the harness.
type fakeLauncher struct {
	mu        sync.Mutex
	order     []string
	launches  []*launch
	stopDelay time.Duration
	script    func(l *launch, out io.Writer)
}

// launch is one harness start as the fake saw it.
type launch struct {
	argv    []string
	env     []string
	stdin   string    // resume mode: the whole of stdin
	stdinR  io.Reader // stream mode: stdin as it arrives
	session string    // from the session's MCP config
	base    string
}

func (l *launch) envValue(name string) string {
	for _, kv := range l.env {
		if k, v, _ := strings.Cut(kv, "="); k == name {
			return v
		}
	}
	return ""
}

func (fl *fakeLauncher) record(s string) {
	fl.mu.Lock()
	fl.order = append(fl.order, s)
	fl.mu.Unlock()
}

func (fl *fakeLauncher) snapshot() ([]string, []*launch) {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	return append([]string(nil), fl.order...), append([]*launch(nil), fl.launches...)
}

func (fl *fakeLauncher) Start(_ context.Context, argv, env []string, _ string, stdin io.Reader) (Proc, error) {
	l := &launch{argv: argv, env: env}
	for i, a := range argv {
		if a == "--mcp-config" && i+1 < len(argv) {
			var cfg struct {
				MCPServers map[string]struct {
					Args []string `json:"args"`
				} `json:"mcpServers"`
			}
			body, err := os.ReadFile(argv[i+1])
			if err != nil || json.Unmarshal(body, &cfg) != nil {
				return nil, fmt.Errorf("unreadable MCP config")
			}
			args := cfg.MCPServers["wardyn"].Args
			if len(args) == 5 {
				l.session, l.base = args[2], args[4]
			}
		}
	}
	if strings.Contains(strings.Join(argv, " "), "--input-format") {
		l.stdinR = stdin
	} else {
		b, _ := io.ReadAll(stdin)
		l.stdin = string(b)
	}
	fl.mu.Lock()
	fl.launches = append(fl.launches, l)
	fl.order = append(fl.order, "start "+l.session)
	fl.mu.Unlock()
	pr, pw := io.Pipe()
	p := &fakeProc{out: pr, w: pw, fl: fl, session: l.session}
	go func() {
		if fl.script != nil {
			fl.script(l, pw)
		}
		if l.stdinR == nil {
			_ = pw.Close() // a resume-mode harness exits after its turn
		}
	}()
	return p, nil
}

type fakeProc struct {
	out     *io.PipeReader
	w       *io.PipeWriter
	fl      *fakeLauncher
	session string
	once    sync.Once
}

func (p *fakeProc) Stdout() io.Reader { return p.out }

func (p *fakeProc) Stop() error {
	p.once.Do(func() {
		time.Sleep(p.fl.stopDelay)
		_ = p.w.Close()
		_ = p.out.Close()
		p.fl.record("exit " + p.session)
	})
	return nil
}

// Harness stream lines, in the shapes the pinned harness writes (testdata/stream.jsonl).
func initLine(session string) string {
	return `{"type":"system","subtype":"init","session_id":"` + session + `"}`
}

func toolUseLine(id, name, input string) string {
	return `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"` + id + `","name":"` + name + `","input":` + input + `}]}}`
}

func toolResultLine(id string) string {
	return `{"type":"user","message":{"content":[{"tool_use_id":"` + id + `","type":"tool_result","content":[{"type":"text","text":"stub record"}]}]}}`
}

func textLine(s string) string {
	b, _ := json.Marshal(s)
	return `{"type":"assistant","message":{"content":[{"type":"text","text":` + string(b) + `}]}}`
}

const resultLine = `{"type":"result","subtype":"success","is_error":false,"usage":{"input_tokens":20,"output_tokens":10}}`

func writeLines(out io.Writer, lines ...string) {
	for _, l := range lines {
		if _, err := io.WriteString(out, l+"\n"); err != nil {
			return
		}
	}
}

// oneTurnScript answers each turn with a tool call, its result, text and the result line.
func oneTurnScript(l *launch, out io.Writer) {
	if l.stdinR == nil {
		writeLines(out, toolUseLine("t1", "mcp__wardyn__read", `{"what":"unit"}`), toolResultLine("t1"), textLine("an answer"), resultLine)
		return
	}
	sc := bufio.NewScanner(l.stdinR)
	sc.Buffer(make([]byte, 64<<10), harnessLineMax)
	first := true
	for sc.Scan() {
		if first {
			writeLines(out, initLine("harness-stream"))
			first = false
		}
		writeLines(out, toolUseLine("t1", "mcp__wardyn__read", `{"what":"unit"}`), toolResultLine("t1"), textLine("an answer"), resultLine)
	}
}

// clock is a settable time source for the idle limit.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// newTestRelay builds a relay on the fakes; harness session ids are harness-1, harness-2, ...
func newTestRelay(t *testing.T, f *fakeWardyn, fl *fakeLauncher) (*Relay, *bytes.Buffer) {
	t.Helper()
	iso, err := NewIsolation(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	log := &bytes.Buffer{}
	n := 0
	return &Relay{
		API: f.api(), Launcher: fl, Iso: iso, Claude: "claude", Self: "/usr/local/bin/wardyn-assist",
		Env: iso.Env(nil), Log: log, TextEvery: 10 * time.Millisecond, Retry: 10 * time.Millisecond,
		NewUUID: func() string { n++; return fmt.Sprintf("harness-%d", n) },
	}, log
}

// runRelay runs r in the background; the returned channel carries its exit code.
func runRelay(t *testing.T, r *Relay) <-chan int {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan int, 1)
	go func() { done <- r.Run(ctx) }()
	return done
}

func exitCode(t *testing.T, done <-chan int) int {
	t.Helper()
	select {
	case code := <-done:
		return code
	case <-time.After(5 * time.Second):
		t.Fatal("the relay did not exit")
		return -1
	}
}

func kinds(evs []postedEvent) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.Kind
	}
	return out
}

func mcpPathOf(argv []string) string {
	for i, a := range argv {
		if a == "--mcp-config" && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
}

var testRecords = []types.AssistantSection{
	{Label: "Unit", Text: "unit: payments", Untrusted: false},
	{Label: "Proposer's note", Text: "a note", Untrusted: true},
}

// TestRelayTurnLoop: a first turn with its records, a second turn that resumes, and an end.
func TestRelayTurnLoop(t *testing.T) {
	f, fl := newFakeWardyn(t), &fakeLauncher{script: oneTurnScript}
	s1 := uuid.NewString()
	f.setLive(s1)
	r, _ := newTestRelay(t, f, fl)
	done := runRelay(t, r)
	f.turns <- &Turn{Seq: 1, Kind: "user", SessionID: s1, Model: "stub-model", System: "the prompt", Records: testRecords, Text: "first question"}
	f.waitFor(t, "turn 1", func(e []postedEvent, _ []postedCall) bool { return countKind(e, kindTurnEnd) == 1 })
	f.turns <- &Turn{Seq: 3, Kind: "user", SessionID: s1, Text: "second question"}
	f.waitFor(t, "turn 2", func(e []postedEvent, _ []postedCall) bool { return countKind(e, kindTurnEnd) == 2 })
	f.turns <- &Turn{Seq: 5, Kind: "end", SessionID: s1}
	if code := exitCode(t, done); code != ExitOK {
		t.Fatalf("exit %d, want 0", code)
	}
	evs, _ := f.snapshot()
	want := []string{kindState, kindToolCall, kindToolResult, kindText, kindTurnEnd, kindToolCall, kindToolResult, kindText, kindTurnEnd}
	if !reflect.DeepEqual(kinds(evs), want) {
		t.Fatalf("events %q, want %q", kinds(evs), want)
	}
	for _, e := range evs {
		if e.Session != s1 {
			t.Fatalf("event %+v is not tagged with the session", e)
		}
	}
	if evs[0].Payload["state"] != "ready" || evs[3].Payload["text"] != "an answer" || evs[1].Payload["name"] != "read" {
		t.Fatalf("payloads: %+v", evs)
	}
	if u, _ := evs[4].Payload["usage"].(map[string]any); u["input_tokens"] != float64(20) || u["output_tokens"] != float64(10) {
		t.Fatalf("turn_end usage %+v", evs[4].Payload)
	}
	order, launches := fl.snapshot()
	if len(launches) != 2 || !reflect.DeepEqual(order, []string{"start " + s1, "exit " + s1, "start " + s1, "exit " + s1}) {
		t.Fatalf("launcher order %q", order)
	}
	mcp := mcpPathOf(launches[0].argv)
	if want := ResumeArgs("claude", "stub-model", "the prompt", mcp, "harness-1", false); !reflect.DeepEqual(launches[0].argv, want) {
		t.Fatalf("turn 1 argv %q\nwant %q", launches[0].argv, want)
	}
	if want := ResumeArgs("claude", "stub-model", "the prompt", mcp, "harness-1", true); !reflect.DeepEqual(launches[1].argv, want) {
		t.Fatalf("turn 2 argv %q\nwant %q", launches[1].argv, want)
	}
	if want := assistant.RecordBytes(testRecords) + "first question"; launches[0].stdin != want {
		t.Fatalf("turn 1 stdin %q, want the records then the text", launches[0].stdin)
	}
	if launches[1].stdin != "second question" {
		t.Fatalf("turn 2 stdin %q", launches[1].stdin)
	}
	for _, l := range launches {
		for _, a := range l.argv {
			if strings.Contains(a, "question") {
				t.Fatalf("turn text in argv: %q", l.argv)
			}
		}
		if l.envValue("HOME") != r.Iso.Dir || l.envValue("CLAUDE_CONFIG_DIR") != r.Iso.Config || l.session != s1 {
			t.Fatalf("launch env/session: HOME %q CONFIG %q session %q", l.envValue("HOME"), l.envValue("CLAUDE_CONFIG_DIR"), l.session)
		}
	}
	if b, _ := os.ReadFile(r.Iso.Prompt); string(b) != "the prompt" {
		t.Fatalf("prompt file %q", b)
	}
	f.mu.Lock()
	afters := strings.Join(f.afters, ",")
	f.mu.Unlock()
	if !strings.Contains(afters, ",1,") || !strings.HasSuffix(afters, ",3") {
		t.Fatalf("polls asked after %s; want 1 after the first turn and 3 last", afters)
	}
}

// TestRelayNeverLogsTurnText: the person's text and the model's output appear in no log line.
func TestRelayNeverLogsTurnText(t *testing.T) {
	const canary = "CANARY-TURN-5d1e"
	f := newFakeWardyn(t)
	fl := &fakeLauncher{script: func(l *launch, out io.Writer) {
		writeLines(out, toolUseLine("t1", "mcp__wardyn__read", `{"what":"`+canary+`"}`), toolResultLine("t1"), textLine("model says "+canary), "not json "+canary, resultLine)
	}}
	s1 := uuid.NewString()
	f.setLive(uuid.NewString()) // every post is refused, so the refusal path logs too
	r, log := newTestRelay(t, f, fl)
	done := runRelay(t, r)
	f.turns <- &Turn{Seq: 1, Kind: "user", SessionID: s1, Model: "stub-model", System: "p", Records: []types.AssistantSection{{Label: "x", Text: canary}}, Text: "person says " + canary}
	f.waitFor(t, "the turn", func(e []postedEvent, _ []postedCall) bool { return countKind(e, kindTurnEnd) == 1 })
	f.turns <- &Turn{Seq: 2, Kind: "end", SessionID: s1}
	exitCode(t, done)
	if strings.Contains(log.String(), canary) {
		t.Fatalf("the log carries turn or model text:\n%s", log.String())
	}
	if !strings.Contains(log.String(), "refused") {
		t.Fatalf("the refusal path was not exercised:\n%s", log.String())
	}
}

func TestRelayIdleExit(t *testing.T) {
	f, fl := newFakeWardyn(t), &fakeLauncher{script: oneTurnScript}
	s1 := uuid.NewString()
	f.setLive(s1)
	r, _ := newTestRelay(t, f, fl)
	c := &clock{now: time.Unix(1_000_000, 0)}
	r.Now = c.Now
	done := runRelay(t, r)
	f.turns <- &Turn{Seq: 1, Kind: "user", SessionID: s1, Model: "m", System: "p", Text: "hi"}
	f.waitFor(t, "the turn", func(e []postedEvent, _ []postedCall) bool { return countKind(e, kindTurnEnd) == 1 })
	c.advance(defaultIdleAfter - time.Second)
	time.Sleep(60 * time.Millisecond)
	if evs, _ := f.snapshot(); countKind(evs, kindNotice) != 0 {
		t.Fatal("idle before 15 minutes")
	}
	c.advance(time.Second)
	if code := exitCode(t, done); code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	evs, _ := f.snapshot()
	last := evs[len(evs)-1]
	if last.Kind != kindNotice || last.Payload["notice"] != "idle" || last.Session != s1 {
		t.Fatalf("last event %+v, want the idle notice", last)
	}
}

// TestRelayTurnLimits: past 12 tool calls, or past the time cap, the harness stops, the notice is posted and
// the next turn resumes.
func TestRelayTurnLimits(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script func(l *launch, out io.Writer)
		calls  int
	}{
		{"tool calls", func(l *launch, out io.Writer) {
			if strings.Contains(strings.Join(l.argv, " "), "--resume") {
				writeLines(out, textLine("ok"), resultLine)
				return
			}
			for i := 0; i < 13; i++ {
				writeLines(out, toolUseLine(fmt.Sprintf("t%d", i), "mcp__wardyn__read", `{}`))
			}
			select {}
		}, 12},
		{"time", func(l *launch, out io.Writer) {
			if strings.Contains(strings.Join(l.argv, " "), "--resume") {
				writeLines(out, textLine("ok"), resultLine)
				return
			}
			select {}
		}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, fl := newFakeWardyn(t), &fakeLauncher{script: tc.script}
			s1 := uuid.NewString()
			f.setLive(s1)
			r, _ := newTestRelay(t, f, fl)
			r.TurnTime = 200 * time.Millisecond
			done := runRelay(t, r)
			f.turns <- &Turn{Seq: 1, Kind: "user", SessionID: s1, Model: "m", System: "p", Text: "go"}
			f.waitFor(t, "the limit notice", func(e []postedEvent, _ []postedCall) bool { return countKind(e, kindNotice) == 1 })
			f.turns <- &Turn{Seq: 2, Kind: "user", SessionID: s1, Text: "again"}
			f.waitFor(t, "turn 2", func(e []postedEvent, _ []postedCall) bool { return countKind(e, kindTurnEnd) == 1 })
			f.turns <- &Turn{Seq: 3, Kind: "end", SessionID: s1}
			exitCode(t, done)
			evs, _ := f.snapshot()
			if countKind(evs, kindToolCall) != tc.calls {
				t.Fatalf("%d tool_call events, want %d", countKind(evs, kindToolCall), tc.calls)
			}
			for _, e := range evs {
				if e.Kind == kindNotice && e.Payload["notice"] != "turn_limit" {
					t.Fatalf("notice %+v", e)
				}
			}
			order, launches := fl.snapshot()
			if len(launches) != 2 || order[1] != "exit "+s1 || launches[1].argv[len(launches[1].argv)-2] != "--resume" {
				t.Fatalf("order %q; turn 2 argv %q", order, launches[1].argv)
			}
		})
	}
}

// TestRelayEndConversationEndsTheTurnOnly: an EndConversation tool use posts turn_end, the session goes on,
// and the next turn resumes the harness session.
func TestRelayEndConversationEndsTheTurnOnly(t *testing.T) {
	f := newFakeWardyn(t)
	fl := &fakeLauncher{script: func(l *launch, out io.Writer) {
		writeLines(out, textLine("bye"), toolUseLine("e1", "EndConversation", `{}`))
		select {}
	}}
	s1 := uuid.NewString()
	f.setLive(s1)
	r, _ := newTestRelay(t, f, fl)
	done := runRelay(t, r)
	f.turns <- &Turn{Seq: 1, Kind: "user", SessionID: s1, Model: "m", System: "p", Text: "one"}
	f.waitFor(t, "turn 1", func(e []postedEvent, _ []postedCall) bool { return countKind(e, kindTurnEnd) == 1 })
	f.turns <- &Turn{Seq: 2, Kind: "user", SessionID: s1, Text: "two"}
	f.waitFor(t, "turn 2", func(e []postedEvent, _ []postedCall) bool { return countKind(e, kindTurnEnd) == 2 })
	select {
	case code := <-done:
		t.Fatalf("the relay exited (%d) on EndConversation", code)
	default:
	}
	f.turns <- &Turn{Seq: 3, Kind: "end", SessionID: s1}
	exitCode(t, done)
	_, launches := fl.snapshot()
	if len(launches) != 2 || launches[1].argv[len(launches[1].argv)-2] != "--resume" || launches[1].stdin != "two" {
		t.Fatalf("turn 2 did not resume: %q", launches[1].argv)
	}
	if evs, _ := f.snapshot(); countKind(evs, kindToolCall) != 0 {
		t.Fatal("EndConversation was shown as a tool call")
	}
}

// TestRelayRefusesToRunWithoutTools: any answer but a 200 with a non-empty tool list gives one
// tools_unavailable error with the status, no ready state, no harness, and exit 3.
func TestRelayRefusesToRunWithoutTools(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		drop   bool
		want   float64
	}{
		{"404", http.StatusNotFound, `{"error":"not found"}`, false, 404},
		{"500", http.StatusInternalServerError, `{}`, false, 500},
		{"no answer", 0, "", true, 0},
		{"empty list", http.StatusOK, `[]`, false, 200},
		{"not the list", http.StatusOK, `{"tools":[{"name":"read"}]}`, false, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, fl := newFakeWardyn(t), &fakeLauncher{script: oneTurnScript}
			f.toolsStatus, f.toolsBody, f.toolsDrop = tc.status, tc.body, tc.drop
			s1 := uuid.NewString()
			f.setLive(s1)
			r, _ := newTestRelay(t, f, fl)
			done := runRelay(t, r)
			f.turns <- &Turn{Seq: 1, Kind: "user", SessionID: s1, Model: "m", System: "p", Text: "hi"}
			if code := exitCode(t, done); code != ExitToolsUnavailable {
				t.Fatalf("exit %d, want %d", code, ExitToolsUnavailable)
			}
			evs, _ := f.snapshot()
			if len(evs) != 1 || evs[0].Kind != kindError || evs[0].Payload["code"] != "tools_unavailable" ||
				evs[0].Payload["status"] != tc.want || evs[0].Session != s1 {
				t.Fatalf("events %+v; want exactly one tools_unavailable error with status %v", evs, tc.want)
			}
			if _, launches := fl.snapshot(); len(launches) != 0 {
				t.Fatalf("the harness started %d times", len(launches))
			}
		})
	}
	t.Run("one tool", func(t *testing.T) {
		f, fl := newFakeWardyn(t), &fakeLauncher{script: oneTurnScript}
		s1 := uuid.NewString()
		f.setLive(s1)
		r, _ := newTestRelay(t, f, fl)
		done := runRelay(t, r)
		f.turns <- &Turn{Seq: 1, Kind: "user", SessionID: s1, Model: "m", System: "p", Text: "hi"}
		f.waitFor(t, "the turn", func(e []postedEvent, _ []postedCall) bool { return countKind(e, kindTurnEnd) == 1 })
		f.turns <- &Turn{Seq: 2, Kind: "end", SessionID: s1}
		exitCode(t, done)
		if _, launches := fl.snapshot(); len(launches) != 1 {
			t.Fatalf("the harness started %d times, want 1", len(launches))
		}
	})
}

// mcpClient drives an in-process `wardyn-assist tools` the way the harness drives its stdio child.
type mcpClient struct {
	in  *io.PipeWriter
	out *bufio.Scanner
}

func startTools(f *fakeWardyn, session string) *mcpClient {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ts := &ToolServer{API: f.api(), Session: session, Log: io.Discard}
	go func() { _ = ts.Serve(context.Background(), inR, outW); _ = outW.Close() }()
	sc := bufio.NewScanner(outR)
	sc.Buffer(make([]byte, 64<<10), mcpLineMax)
	return &mcpClient{in: inW, out: sc}
}

func (c *mcpClient) send(id int, method, params string) {
	_, _ = fmt.Fprintf(c.in, `{"jsonrpc":"2.0","id":%d,"method":%q,"params":%s}`+"\n", id, method, params)
}

// next reads the next answer: whether it is an error result, and its text; ok is false when none came.
func (c *mcpClient) next() (isErr bool, text string, ok bool) {
	if !c.out.Scan() {
		return false, "", false
	}
	var m struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if json.Unmarshal(c.out.Bytes(), &m) != nil || len(m.Result.Content) == 0 {
		return false, "", false
	}
	return m.Result.IsError, m.Result.Content[0].Text, true
}

// reply is next on the test goroutine.
func (c *mcpClient) reply(t *testing.T) (bool, string) {
	t.Helper()
	isErr, text, ok := c.next()
	if !ok {
		t.Fatal("no MCP reply")
	}
	return isErr, text
}

// TestRelayResetWithADelayedOldCall: session 1's tools process has a call held at the server and a second
// call still in its stdin when the reset to session 2 comes. Session 1's tree exits before session 2's
// harness starts; both old calls arrive tagged with session 1 and come back as errors (409); session 2's first
// call carries session 2; session 1's harness session store is gone.
func TestRelayResetWithADelayedOldCall(t *testing.T) {
	f := newFakeWardyn(t)
	s1, s2 := uuid.NewString(), uuid.NewString()
	f.setLive(s1)
	f.hold = s1
	old := make(chan *mcpClient, 1)
	var oldStore string
	fl := &fakeLauncher{stopDelay: 300 * time.Millisecond, script: func(l *launch, out io.Writer) {
		c := startTools(f, l.session)
		c.send(1, "tools/call", `{"name":"read","arguments":{"what":"unit"}}`)
		if l.session == s1 {
			for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
				if _, calls := f.snapshot(); len(calls) == 1 {
					break
				}
			}
			oldStore = filepath.Join(l.envValue("CLAUDE_CONFIG_DIR"), "projects", "-work", "harness-1.jsonl")
			_ = os.MkdirAll(filepath.Dir(oldStore), 0o700)
			_ = os.WriteFile(oldStore, []byte("turn one"), 0o600)
			old <- c
			writeLines(out, resultLine)
			return
		}
		if isErr, text, ok := c.next(); !ok || isErr {
			t.Errorf("session 2's call failed: %s", text)
		}
		writeLines(out, resultLine)
	}}
	r, _ := newTestRelay(t, f, fl)
	done := runRelay(t, r)
	f.turns <- &Turn{Seq: 1, Kind: "user", SessionID: s1, Model: "m", System: "p", Text: "one"}
	f.waitFor(t, "session 1's turn", func(e []postedEvent, _ []postedCall) bool { return countKind(e, kindTurnEnd) == 1 })
	f.setLive(s2)
	f.turns <- &Turn{Seq: 2, Kind: "reset", SessionID: s2, Model: "m", System: "p2", Records: testRecords}
	f.waitFor(t, "session 2 ready", func(e []postedEvent, _ []postedCall) bool { return countKind(e, kindState) == 2 })
	oc := <-old
	oc.send(2, "tools/call", `{"name":"read","arguments":{"what":"late"}}`) // delayed in session 1's stdin
	f.turns <- &Turn{Seq: 3, Kind: "user", SessionID: s2, Text: "two"}
	f.waitFor(t, "session 2's turn", func(e []postedEvent, _ []postedCall) bool { return countKind(e, kindTurnEnd) == 2 })
	close(f.release)
	for i := 0; i < 2; i++ {
		if isErr, text := oc.reply(t); !isErr || text != "That session is not the live one." {
			t.Fatalf("an old call answered isError=%v %q; want the server's 409 as an error", isErr, text)
		}
	}
	f.turns <- &Turn{Seq: 4, Kind: "end", SessionID: s2}
	exitCode(t, done)

	order, launches := fl.snapshot()
	exit1, start2 := -1, -1
	for i, o := range order {
		if o == "exit "+s1 && exit1 < 0 {
			exit1 = i
		}
		if o == "start "+s2 && start2 < 0 {
			start2 = i
		}
	}
	if exit1 < 0 || start2 < 0 || exit1 > start2 {
		t.Fatalf("launcher order %q: session 1's tree must exit before session 2's harness starts", order)
	}
	_, calls := f.snapshot()
	var bySession = map[string]int{}
	for _, c := range calls {
		bySession[c.Session]++
	}
	if bySession[s1] != 2 || bySession[s2] != 1 || len(calls) != 3 {
		t.Fatalf("calls %+v; want two from session 1, one from session 2", calls)
	}
	first2 := -1
	for i, c := range calls {
		if c.Session == s2 {
			first2 = i
			break
		}
	}
	if first2 < 0 || calls[first2].Session != s2 {
		t.Fatalf("session 2's first call: %+v", calls)
	}
	if _, err := os.Stat(oldStore); !os.IsNotExist(err) {
		t.Fatalf("session 1's harness session store survived the reset: %v", err)
	}
	if launches[1].session != s2 || launches[1].argv[len(launches[1].argv)-2] != "--session-id" ||
		launches[1].argv[len(launches[1].argv)-1] != "harness-2" || launches[1].stdin != assistant.RecordBytes(testRecords)+"two" {
		t.Fatalf("session 2's harness: session %q argv %q stdin %q", launches[1].session, launches[1].argv, launches[1].stdin)
	}
}

// TestRelayStreamMode: one harness per session, each turn written to its stdin.
func TestRelayStreamMode(t *testing.T) {
	f, fl := newFakeWardyn(t), &fakeLauncher{script: oneTurnScript}
	s1 := uuid.NewString()
	f.setLive(s1)
	r, _ := newTestRelay(t, f, fl)
	r.Stream = true
	done := runRelay(t, r)
	f.turns <- &Turn{Seq: 1, Kind: "user", SessionID: s1, Model: "m", System: "p", Records: testRecords, Text: "one"}
	f.waitFor(t, "turn 1", func(e []postedEvent, _ []postedCall) bool { return countKind(e, kindTurnEnd) == 1 })
	f.turns <- &Turn{Seq: 2, Kind: "user", SessionID: s1, Text: "two"}
	f.waitFor(t, "turn 2", func(e []postedEvent, _ []postedCall) bool { return countKind(e, kindTurnEnd) == 2 })
	f.turns <- &Turn{Seq: 3, Kind: "end", SessionID: s1}
	exitCode(t, done)
	order, launches := fl.snapshot()
	if len(launches) != 1 || !reflect.DeepEqual(order, []string{"start " + s1, "exit " + s1}) {
		t.Fatalf("order %q", order)
	}
	if want := StreamArgs("claude", "m", "p", mcpPathOf(launches[0].argv)); !reflect.DeepEqual(launches[0].argv, want) {
		t.Fatalf("argv %q\nwant %q", launches[0].argv, want)
	}
}
