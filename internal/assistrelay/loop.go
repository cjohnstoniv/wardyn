// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package assistrelay

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/assistant"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// routePrefix is the proxy's brokered assistant route.
const routePrefix = "/wardyn/v1/assistant/"

// Exit codes of `wardyn-assist relay`.
const (
	ExitOK = 0
	// ExitFailed is any failure but the two below.
	ExitFailed = 1
	// ExitToolsUnavailable is the exit when the server answers no usable tool list: the relay never offers a
	// model an empty one.
	ExitToolsUnavailable = 3
)

// Defaults of a Relay's limits.
const (
	defaultIdleAfter = 15 * time.Minute
	defaultTurnTools = 12
	defaultTurnTime  = 180 * time.Second
	defaultTextEvery = 250 * time.Millisecond
	defaultRetry     = 2 * time.Second
	// maxPollFails is how many polls in a row may fail before the relay gives up.
	maxPollFails = 5
	// harnessLineMax bounds one line of the harness stream.
	harnessLineMax = 8 << 20
	// textPostMax bounds one text event's encoded text, well under the proxy's and the server's 256 KiB body cap.
	textPostMax = 200 << 10
)

// API is the relay's client of the brokered assistant route on the run's proxy.
type API struct {
	Base   string
	Client *http.Client
}

// NewAPI returns a client of the proxy at base. Proxy: nil, as wardyn-toolgate's client: the proxy is a known
// on-segment address, and the default transport would send the call back through whatever HTTP_PROXY names.
// The timeout outlasts the server's 25 s long-poll.
func NewAPI(base string) *API {
	return &API{Base: strings.TrimRight(base, "/"), Client: &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 60 * time.Second}}
}

// Turn is one answer of GET turns.
type Turn struct {
	Seq       int64                    `json:"seq"`
	Kind      string                   `json:"kind"`
	SessionID string                   `json:"session_id"`
	Model     string                   `json:"model,omitempty"`
	Text      string                   `json:"text,omitempty"`
	System    string                   `json:"system,omitempty"`
	Records   []types.AssistantSection `json:"records,omitempty"`
	Tools     []string                 `json:"tools,omitempty"`
}

// errStatus is an answer the caller did not expect.
type errStatus int

func (e errStatus) Error() string { return "status " + strconv.Itoa(int(e)) }

func (a *API) do(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.Base+routePrefix+path, rdr)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.Client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(io.LimitReader(resp.Body, harnessLineMax))
	return resp.StatusCode, out, err
}

// Turn long-polls for the next turn after seq after. A poll that ends with no turn answers nil.
func (a *API) Turn(ctx context.Context, after int64) (*Turn, error) {
	status, body, err := a.do(ctx, http.MethodGet, "turns?after="+strconv.FormatInt(after, 10), nil)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusNoContent:
		return nil, nil
	case http.StatusOK:
		var t Turn
		if err := json.Unmarshal(body, &t); err != nil {
			return nil, err
		}
		return &t, nil
	}
	return nil, errStatus(status)
}

// Post posts one event for session and answers the server's status.
func (a *API) Post(ctx context.Context, session string, ev Event) (int, error) {
	payload := ev.Payload
	if payload == nil {
		payload = map[string]any{}
	}
	body, err := json.Marshal(map[string]any{"session_id": session, "kind": ev.Kind, "payload": payload})
	if err != nil {
		return 0, err
	}
	status, _, err := a.do(ctx, http.MethodPost, "events", body)
	return status, err
}

// errNoTools is the answer of Tools when the server gives no usable tool list.
var errNoTools = errors.New("no usable tool list")

// Tools reads the server's tool list. Anything but a 200 whose body is a non-empty list of well-named tools is
// errNoTools, with the status (0 when there was no answer).
func (a *API) Tools(ctx context.Context) ([]Tool, int, error) {
	status, body, err := a.do(ctx, http.MethodGet, "tools", nil)
	if err != nil {
		return nil, 0, errNoTools
	}
	var tools []Tool
	if status != http.StatusOK || json.Unmarshal(body, &tools) != nil || len(tools) == 0 {
		return nil, status, errNoTools
	}
	for _, t := range tools {
		if !toolName.MatchString(t.Name) {
			return nil, status, errNoTools
		}
	}
	return tools, status, nil
}

// Call posts one tool call for session.
func (a *API) Call(ctx context.Context, name, session string, args json.RawMessage) (int, []byte, error) {
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	body, err := json.Marshal(map[string]any{"session_id": session, "args": args})
	if err != nil {
		return 0, nil, err
	}
	return a.do(ctx, http.MethodPost, "tools/"+name, body)
}

// Relay is `wardyn-assist relay`: it long-polls for turns, runs the harness for each, and posts the harness's
// stream back as events, each tagged with its session id.
type Relay struct {
	API      *API
	Launcher Launcher
	Iso      *Isolation
	// Claude is the harness command.
	Claude string
	// Self is this binary's path, named in each session's MCP config.
	Self string
	// Env is the harness's env (Isolation.Env).
	Env []string
	// Stream selects stream mode; the default is resume mode.
	Stream bool
	// Log takes counts and states only: never turn text, a tool argument or model output.
	Log io.Writer

	IdleAfter time.Duration
	TurnTools int
	TurnTime  time.Duration
	TextEvery time.Duration
	Retry     time.Duration
	NewUUID   func() string
	Now       func() time.Time

	after    int64
	session  string
	model    string
	system   string
	records  []types.AssistantSection
	mcp      string
	harness  string // the harness's session uuid
	fresh    bool   // the harness session has not started: the next turn carries the records
	live     *running
	lastTurn time.Time
	badLines int
}

// running is one harness process tree and its stream.
type running struct {
	proc    Proc
	lines   <-chan []byte
	in      io.WriteCloser // stream mode: the harness's stdin
	session string         // the harness's own session id, from its init line
	parser  *Parser
}

func (r *Relay) defaults() {
	if r.IdleAfter == 0 {
		r.IdleAfter = defaultIdleAfter
	}
	if r.TurnTools == 0 {
		r.TurnTools = defaultTurnTools
	}
	if r.TurnTime == 0 {
		r.TurnTime = defaultTurnTime
	}
	if r.TextEvery == 0 {
		r.TextEvery = defaultTextEvery
	}
	if r.Retry == 0 {
		r.Retry = defaultRetry
	}
	if r.NewUUID == nil {
		r.NewUUID = func() string { return uuid.NewString() }
	}
	if r.Now == nil {
		r.Now = time.Now
	}
	if r.Log == nil {
		r.Log = io.Discard
	}
}

func (r *Relay) logf(format string, args ...any) {
	fmt.Fprintf(r.Log, "wardyn-assist relay: "+format+"\n", args...)
}

// Run serves turns until an end turn, the idle limit, a failure or ctx's end, and answers the exit code.
func (r *Relay) Run(ctx context.Context) int {
	r.defaults()
	r.lastTurn = r.Now()
	defer r.stopHarness()
	fails := 0
	for {
		turn, err := r.API.Turn(ctx, r.after)
		if ctx.Err() != nil {
			return ExitOK
		}
		if err != nil {
			fails++
			r.logf("a poll failed (%d in a row)", fails)
			if fails >= maxPollFails {
				return ExitFailed
			}
			sleep(ctx, r.Retry)
			continue
		}
		fails = 0
		if turn == nil {
			if r.Now().Sub(r.lastTurn) >= r.IdleAfter {
				r.stopHarness()
				if r.session != "" { // with no session there is nothing to end, and the server refuses an empty id
					r.post(ctx, Event{Kind: kindNotice, Payload: map[string]any{"notice": "idle"}})
				}
				r.logf("idle; exiting")
				return ExitOK
			}
			continue
		}
		if turn.Seq > r.after {
			r.after = turn.Seq
		}
		if code, done := r.handle(ctx, turn); done {
			return code
		}
	}
}

// handle serves one turn and reports whether the relay is done, with its exit code.
func (r *Relay) handle(ctx context.Context, t *Turn) (int, bool) {
	switch t.Kind {
	case "end":
		r.logf("end; exiting")
		return ExitOK, true
	case "reset":
		r.logf("reset")
		return r.begin(ctx, t)
	case "user":
		r.lastTurn = r.Now()
		if t.SessionID != r.session {
			if code, done := r.begin(ctx, t); done {
				return code, true
			}
		}
		return r.runTurn(ctx, t.Text)
	}
	r.logf("a turn of an unknown kind was skipped")
	return ExitOK, false
}

// begin starts a session: the first turn, a reset, or a turn for a session the relay has not served. A
// previous session's tree goes first, in order: it is terminated by process group, every process in it has
// exited and is reaped, and its conversation is cleared. Only then is the new session's MCP config written.
func (r *Relay) begin(ctx context.Context, t *Turn) (int, bool) {
	if r.session != "" {
		if err := r.stopHarness(); err != nil {
			r.logf("the previous harness did not exit; exiting")
			return ExitFailed, true
		}
		if err := r.Iso.ClearStore(); err != nil {
			r.logf("could not clear the previous conversation; exiting")
			return ExitFailed, true
		}
	}
	r.session, r.model, r.system, r.records = t.SessionID, t.Model, t.System, t.Records
	r.harness, r.fresh = r.NewUUID(), true
	r.lastTurn = r.Now() // a new session's idle clock starts at its start, not at the old session's last turn
	if _, status, err := r.API.Tools(ctx); err != nil {
		r.post(ctx, Event{Kind: kindError, Payload: map[string]any{"code": "tools_unavailable", "status": status}})
		r.logf("tools unavailable (status %d); exiting", status)
		return ExitToolsUnavailable, true
	}
	if err := r.Iso.WritePrompt(r.system); err != nil {
		r.logf("could not write the system prompt; exiting")
		return ExitFailed, true
	}
	mcp, err := r.Iso.WriteSessionMCP(r.session, r.Self, r.API.Base)
	if err != nil {
		r.logf("could not write the session's MCP config; exiting")
		return ExitFailed, true
	}
	r.mcp = mcp
	r.post(ctx, Event{Kind: kindState, Payload: map[string]any{"state": "ready"}})
	return ExitOK, false
}

// outcome is how a turn ended.
type outcome int

const (
	turnDone    outcome = iota // the harness's result line
	turnEndConv                // an EndConversation tool use
	turnLimit                  // past the tool-call or time cap
	turnExited                 // the harness's stream ended first
	turnStopped                // ctx ended
)

// runTurn runs one person turn. The first turn of a harness session carries the records ahead of the text,
// byte for byte as the server's prompt digest reads them.
func (r *Relay) runTurn(ctx context.Context, text string) (int, bool) {
	if r.fresh {
		text = assistant.RecordBytes(r.records) + text
	}
	if err := r.Iso.CheckAncestors(); err != nil {
		r.post(ctx, Event{Kind: kindError, Payload: map[string]any{"code": "isolation_unsafe"}})
		r.logf("refusing to start the harness: %v; exiting", err)
		return ExitFailed, true
	}
	if r.Stream {
		return r.streamTurn(ctx, text)
	}
	return r.resumeTurn(ctx, text)
}

func (r *Relay) resumeTurn(ctx context.Context, text string) (int, bool) {
	argv := ResumeArgs(r.Claude, r.model, r.system, r.mcp, r.harness, !r.fresh)
	run, err := r.start(ctx, argv, strings.NewReader(text), nil)
	if err != nil {
		r.post(ctx, Event{Kind: kindError, Payload: map[string]any{"code": "harness_start"}})
		r.logf("the harness did not start")
		return ExitOK, false
	}
	out := r.watch(ctx, run)
	// A first harness that ended before its init line may have kept no session: the next turn starts over,
	// fresh, under a new id, with the records again. Any other outcome leaves a session to resume.
	if r.fresh && run.session == "" && out == turnExited {
		r.harness = r.NewUUID()
	} else {
		r.fresh = false
	}
	if err := r.stopHarness(); err != nil {
		r.logf("the harness did not exit; exiting")
		return ExitFailed, true
	}
	r.afterTurn(ctx, out)
	return ExitOK, false
}

// streamTurn writes the turn to the session's long-lived harness, starting it first if need be. A turn that
// ends any way but the result line stops that harness; the next turn resumes its session in a new one.
func (r *Relay) streamTurn(ctx context.Context, text string) (int, bool) {
	if r.live == nil {
		var argv []string
		if r.fresh {
			argv = StreamArgs(r.Claude, r.model, r.system, r.mcp)
		} else {
			argv = append(StreamArgs(r.Claude, r.model, r.system, r.mcp), "--resume", r.harness)
		}
		pr, pw := io.Pipe()
		if _, err := r.start(ctx, argv, pr, pw); err != nil {
			r.post(ctx, Event{Kind: kindError, Payload: map[string]any{"code": "harness_start"}})
			r.logf("the harness did not start")
			return ExitOK, false
		}
	}
	line, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{
		"role": "user", "content": []map[string]any{{"type": "text", "text": text}},
	}})
	if _, err := r.live.in.Write(append(line, '\n')); err != nil {
		r.logf("could not write to the harness")
	}
	r.fresh = false
	out := r.watch(ctx, r.live)
	if out != turnDone {
		// The next turn resumes this harness session in a new process; with no session id seen it starts over.
		if r.live.session != "" {
			r.harness = r.live.session
		} else {
			r.harness, r.fresh = r.NewUUID(), true
		}
		if err := r.stopHarness(); err != nil {
			r.logf("the harness did not exit; exiting")
			return ExitFailed, true
		}
	}
	r.afterTurn(ctx, out)
	return ExitOK, false
}

// afterTurn posts the end of a turn that did not reach the harness's result line: its notice or error, then
// turn_end with no usage, so the server closes the turn (its suggestion record and per-turn digests) as for
// any other.
func (r *Relay) afterTurn(ctx context.Context, out outcome) {
	switch out {
	case turnLimit:
		r.post(ctx, Event{Kind: kindNotice, Payload: map[string]any{"notice": "turn_limit"}})
		r.logf("a turn reached its limit")
	case turnExited:
		r.post(ctx, Event{Kind: kindError, Payload: map[string]any{"code": "harness_exited"}})
		r.logf("the harness stream ended before its result")
	default:
		return
	}
	r.post(ctx, Event{Kind: kindTurnEnd, Payload: map[string]any{}})
}

// start launches a harness tree and begins reading its stream.
func (r *Relay) start(ctx context.Context, argv []string, stdin io.Reader, in io.WriteCloser) (*running, error) {
	proc, err := r.Launcher.Start(ctx, argv, r.Env, r.Iso.Work, stdin)
	if err != nil {
		return nil, err
	}
	lines := make(chan []byte, 64)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(proc.Stdout())
		sc.Buffer(make([]byte, 64<<10), harnessLineMax)
		for sc.Scan() {
			lines <- append([]byte(nil), sc.Bytes()...)
		}
	}()
	r.live = &running{proc: proc, lines: lines, in: in, parser: NewParser()}
	return r.live, nil
}

// stopHarness stops the live tree, if any, and waits until every process in it has exited.
func (r *Relay) stopHarness() error {
	if r.live == nil {
		return nil
	}
	run := r.live
	r.live = nil
	if run.in != nil {
		_ = run.in.Close()
	}
	return run.proc.Stop()
}

// watch reads one turn of the harness stream, posting its events, and answers how the turn ended. Text is
// batched every TextEvery; the turn is capped at TurnTools tool calls and TurnTime.
func (r *Relay) watch(ctx context.Context, run *running) outcome {
	deadline := time.NewTimer(r.TurnTime)
	defer deadline.Stop()
	tick := time.NewTicker(r.TextEvery)
	defer tick.Stop()
	var text strings.Builder
	flush := func() { r.postText(ctx, &text) }
	defer flush()
	calls := 0
	for {
		select {
		case line, ok := <-run.lines:
			if !ok {
				return turnExited
			}
			st, err := run.parser.Line(line)
			if err != nil {
				r.badLines++
				continue
			}
			if st.HarnessSession != "" {
				run.session = st.HarnessSession
			}
			if calls+st.ToolCalls > r.TurnTools {
				return turnLimit
			}
			calls += st.ToolCalls
			for _, ev := range st.Events { // in line order: text is batched, anything else posts after the text before it
				if ev.Kind == kindText {
					s, _ := ev.Payload["text"].(string)
					text.WriteString(s)
					continue
				}
				flush()
				r.post(ctx, ev)
			}
			if st.EndTurn {
				return r.endTurn(ctx, &text, st)
			}
		case <-tick.C:
			flush()
		case <-deadline.C:
			return turnLimit
		case <-ctx.Done():
			return turnStopped
		}
	}
}

// endTurn posts a turn's end: the text so far, an error when the harness says the turn failed, then turn_end
// with the harness's usage numbers.
func (r *Relay) endTurn(ctx context.Context, text *strings.Builder, st Step) outcome {
	r.postText(ctx, text)
	if st.Failed {
		r.post(ctx, Event{Kind: kindError, Payload: map[string]any{"code": "harness_error"}})
	}
	payload := map[string]any{}
	if st.Usage != nil {
		payload["usage"] = st.Usage
	}
	r.post(ctx, Event{Kind: kindTurnEnd, Payload: payload})
	if st.EndConversation {
		return turnEndConv
	}
	return turnDone
}

// postText posts the batched text, if any, as text events of at most textPostMax bytes once JSON-encoded, cut
// on rune boundaries, and empties the batch.
func (r *Relay) postText(ctx context.Context, text *strings.Builder) {
	if text.Len() == 0 {
		return
	}
	for _, piece := range textPieces(text.String(), textPostMax) {
		r.post(ctx, Event{Kind: kindText, Payload: map[string]any{"text": piece}})
	}
	text.Reset()
}

// textPieces cuts s into pieces whose JSON-encoded text is at most limit bytes each, never inside a rune. Each
// rune is counted at its largest encoded size, so the bound holds for any text.
func textPieces(s string, limit int) []string {
	var out []string
	start, size := 0, 0
	for i, c := range s {
		n := utf8.RuneLen(c)
		switch {
		case c == utf8.RuneError && !strings.HasPrefix(s[i:], string(utf8.RuneError)):
			n = 6 // an invalid byte, encoded as \ufffd
		case c < 0x20 || c == '<' || c == '>' || c == '&' || c == 0x2028 || c == 0x2029:
			n = 6 // \u00XX at most
		case c == '"' || c == '\\':
			n = 2
		}
		if size+n > limit && i > start {
			out = append(out, s[start:i])
			start, size = i, 0
		}
		size += n
	}
	return append(out, s[start:])
}

// post posts one event for the current session. A refused post is counted, never retried: the server answers
// 409 for a session that is no longer live, and its next turn says what follows.
func (r *Relay) post(ctx context.Context, ev Event) {
	status, err := r.API.Post(ctx, r.session, ev)
	switch {
	case err != nil:
		r.logf("a %s event got no answer", ev.Kind)
	case status != http.StatusAccepted:
		r.logf("a %s event was refused (status %d)", ev.Kind, status)
	}
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
