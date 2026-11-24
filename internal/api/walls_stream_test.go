// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// wallsSSEEvent is one server-sent event as a client reads it.
type wallsSSEEvent struct{ ID, Event, Data string }

// wallsSSENext reads the next event from br, skipping comments; false at the end of the stream.
func wallsSSENext(br *bufio.Reader) (wallsSSEEvent, bool) {
	var ev wallsSSEEvent
	seen := false
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return ev, false
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if seen {
				return ev, true
			}
		case strings.HasPrefix(line, ":"):
		default:
			key, value, _ := strings.Cut(line, ": ")
			seen = true
			switch key {
			case "id":
				ev.ID = value
			case "event":
				ev.Event = value
			case "data":
				ev.Data = value
			}
		}
	}
}

// wallsSSEAll is every event in b.
func wallsSSEAll(b []byte) []wallsSSEEvent {
	var out []wallsSSEEvent
	br := bufio.NewReader(bytes.NewReader(b))
	for {
		ev, ok := wallsSSENext(br)
		if !ok {
			return out
		}
		out = append(out, ev)
	}
}

// wallsStreamEnv is a server whose two live streams run through their real handlers, behind a middleware that
// signs the caller in from the X-Test-Sub and X-Test-Role headers (none: the admin token). Its sampler is beaten by
// hand. /view serves wallsServe with whatever view the test set last, so a test can name any caller's view.
type wallsStreamEnv struct {
	t    *testing.T
	s    *Server
	ts   *httptest.Server
	rec  *recRecorder
	mu   sync.Mutex
	view wallsView
}

// newWallsStreamEnv serves st. shape may set the server's timers before the first request.
func newWallsStreamEnv(t *testing.T, st store.Store, shape ...func(*Server)) *wallsStreamEnv {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	e := &wallsStreamEnv{t: t, rec: &recRecorder{}}
	e.s = &Server{cfg: Config{Store: st, Now: wallsTopoNewClock().Now, BaseCtx: ctx, Audit: e.rec}}
	e.s.wallsLive.sampler.manual = true
	e.s.runEvents.beat, e.s.runEvents.hold, e.s.runEvents.write = 20*time.Millisecond, 10*time.Second, 5*time.Second
	for _, f := range shape {
		f(e.s)
	}
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if sub := req.Header.Get("X-Test-Sub"); sub != "" {
				req = req.WithContext(withOIDCRole(withOIDCHuman(req.Context(), sub), req.Header.Get("X-Test-Role")))
			}
			next.ServeHTTP(w, req)
		})
	})
	r.Get("/walls/stream", e.s.handleWallsStream)
	r.Get("/runs/{id}/walls/stream", e.s.handleRunWallsStream)
	r.Get("/view", func(w http.ResponseWriter, req *http.Request) { e.s.wallsServe(w, req, e.currentView(), nil) })
	e.ts = httptest.NewServer(r)
	t.Cleanup(func() {
		cancel()
		e.ts.Close()
	})
	return e
}

func (e *wallsStreamEnv) currentView() wallsView {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.view
}

func (e *wallsStreamEnv) setView(v wallsView) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.view = v
}

// beat makes one beat of the rows now in the store.
func (e *wallsStreamEnv) beat() { e.s.wallsLive.sampler.beatOnce(context.Background(), e.s) }

// get opens path as the headers name, the pairs "name", "value", ... The caller closes the body, and cancel ends the
// stream from the client's side.
func (e *wallsStreamEnv) get(path string, header ...string) (*http.Response, context.CancelFunc) {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.ts.URL+path, nil)
	if err != nil {
		cancel()
		e.t.Fatal(err)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		e.t.Fatalf("GET %s: %v", path, err)
	}
	return resp, cancel
}

// wallsStreamAs is the headers of a caller signed in as sub with role.
func wallsStreamAs(sub, role string) []string {
	return []string{"X-Test-Sub", sub, "X-Test-Role", role}
}

// events opens path and reads its first n events.
func (e *wallsStreamEnv) events(n int, path string, header ...string) []wallsSSEEvent {
	e.t.Helper()
	resp, cancel := e.get(path, header...)
	defer cancel()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		e.t.Fatalf("GET %s: %d, want 200", path, resp.StatusCode)
	}
	br := bufio.NewReader(resp.Body)
	var out []wallsSSEEvent
	for len(out) < n {
		ev, ok := wallsSSENext(br)
		if !ok {
			e.t.Fatalf("GET %s: the stream ended after %d of %d events", path, len(out), n)
		}
		out = append(out, ev)
	}
	return out
}

// held is how many shared stream slots, organisation stream slots and sampler subscribers who has.
func (e *wallsStreamEnv) held(who runEventStreamer) (shared, org, subs int) {
	e.s.runEvents.mu.Lock()
	shared = e.s.runEvents.streams[who]
	e.s.runEvents.mu.Unlock()
	e.s.wallsLive.mu.Lock()
	org = e.s.wallsLive.orgStreams[who]
	e.s.wallsLive.mu.Unlock()
	sm := &e.s.wallsLive.sampler
	sm.mu.Lock()
	subs = sm.subs
	sm.mu.Unlock()
	return shared, org, subs
}

// wallsStreamReason is the wire reason of a refusal's body.
func wallsStreamReason(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	var body errorBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode the refusal: %v", err)
	}
	return body.Reason
}

// wallsStreamScenario is one beat of three runs: a in unit a, b in unit b, and one with no unit.
type wallsStreamScenario struct {
	unitA, unitB      uuid.UUID
	runA, runB, runNo uuid.UUID
	rowsA, rowsOut    []store.DecisionRow
}

// wallsStreamNonce is the nonce the scenario's events carry.
const wallsStreamNonce = "nonce"

func newWallsStreamScenario(t *testing.T) *wallsStreamScenario {
	t.Helper()
	sc := &wallsStreamScenario{unitA: uuid.New(), unitB: uuid.New(), runA: uuid.New(), runB: uuid.New(), runNo: uuid.New()}
	sc.rowsA = []store.DecisionRow{
		wallsLiveRow(t, sc.runA, "egress.allow", "policy:allowlist", "pypi.org", wallsLiveWeight(3)),
		wallsLiveRow(t, sc.runA, "egress.deny", "policy:denied", "evil.example",
			wallsLiveData("refusal_reason", "blocked by the team rule"), wallsLiveData("layer_name", "team-a"),
			wallsLiveData("matched_rule", "rule-1"), wallsLiveData("by", "policy")),
		wallsLiveRow(t, sc.runA, "egress.hold", "approval:pending", "pypi.org", wallsLiveData("approval_id", "ap-a1")),
		wallsLiveRow(t, sc.runA, "egress.allow", "brokered:mint", "cp.example", wallsLiveWeight(4)),
		wallsLiveRow(t, sc.runA, "egress.allow", "builtin:upstream-proxy", "proxy.example"),
		wallsLiveRow(t, sc.runA, "egress.deny", "builtin:spool-lost", "", wallsLiveData("cause", "7 decisions lost")),
		wallsLiveRow(t, sc.runA, "approval.decide", "", "", wallsLiveData("approval_id", "ap-a1")),
	}
	sc.rowsOut = []store.DecisionRow{
		wallsLiveRow(t, sc.runB, "egress.allow", "policy:allowlist", "other.example"),
		wallsLiveRow(t, sc.runB, "egress.deny", "policy:denied", "secret-b.example", wallsLiveData("refusal_reason", "b's reason")),
		wallsLiveRow(t, sc.runB, "approval.decide", "", "", wallsLiveData("approval_id", "ap-b1")),
		wallsLiveRow(t, sc.runNo, "egress.allow", "policy:allowlist", "none.example"),
		wallsLiveRow(t, sc.runNo, "credential.mint", "", ""),
	}
	return sc
}

// beat folds rows of the scenario into beat number n.
func (sc *wallsStreamScenario) beat(n uint64, rows ...[]store.DecisionRow) wallsBeat {
	var all []store.DecisionRow
	for _, r := range rows {
		all = append(all, r...)
	}
	meta := map[uuid.UUID]wallsRunMeta{sc.runA: {UnitID: &sc.unitA}, sc.runB: {UnitID: &sc.unitB}, sc.runNo: {}}
	b := wallsFoldBeat(all, meta)
	b.N, b.T = n, wallsLiveEpoch
	return b
}

// wallsStreamKeys adds every object key in v, at any depth, to into.
func wallsStreamKeys(v any, into map[string]bool) {
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			into[k] = true
			wallsStreamKeys(child, into)
		}
	case []any:
		for _, child := range x {
			wallsStreamKeys(child, into)
		}
	}
}

// wallsStreamDecode decodes the JSON of v as generic values.
func wallsStreamDecode(t *testing.T, v any) (decoded any, keys map[string]bool) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	keys = map[string]bool{}
	wallsStreamKeys(decoded, keys)
	return decoded, keys
}

// wallsStreamIdentityKeys are the keys no frame or notice may carry, in either view: the door, and who owns a run
// or what workload it is, and the contact state.
var wallsStreamIdentityKeys = []string{"egress", "owner", "run_owner", "workload", "run_workload", "sponsor", "contact_lost", "contact_age"}

// TestWallsStreamFullProjection: the full view carries weighted counters under counters, every sample field, the
// fidelity total, and neither the control plane's host, the start row's host nor an identity key.
func TestWallsStreamFullProjection(t *testing.T) {
	sc := newWallsStreamScenario(t)
	b := sc.beat(1, sc.rowsA, sc.rowsOut)
	frame := wallsProject(b, wallsView{Kind: wallsViewFull})
	m, keys := wallsStreamDecode(t, frame)
	if len(frame.Runs) != 3 || frame.Runs[0].RunID != sc.runA {
		t.Fatalf("%d runs, first %v; want 3, the busiest (a) first", len(frame.Runs), frame.Runs[0].RunID)
	}
	if c, ok := frame.Runs[0].Counters.(*WallsCounters); !ok || *c != (WallsCounters{Allowed: 3, Refused: 1, Held: 1, Fidelity: 7, Control: 4, Config: 1}) {
		t.Errorf("run a counters %+v, want the weighted full counters", frame.Runs[0].Counters)
	}
	if frame.Fidelity == nil || frame.Fidelity.NotRecorded != 7 {
		t.Errorf("fidelity %+v, want 7 not recorded", frame.Fidelity)
	}
	for _, want := range []string{"counters", "refusal_reason", "policy_contact", "matched_rule", "window_ms", "t"} {
		if !keys[want] {
			t.Errorf("the full frame has no %q key", want)
		}
	}
	for _, banned := range wallsStreamIdentityKeys {
		if keys[banned] {
			t.Errorf("the full frame carries a %q key", banned)
		}
	}
	raw, _ := json.Marshal(m)
	for _, host := range []string{"cp.example", "proxy.example"} {
		if strings.Contains(string(raw), host) {
			t.Errorf("the full frame names %s, a host no frame lists", host)
		}
	}
	if hosts := frame.Runs[0].Hosts; len(hosts) != 2 || hosts[0] != (WallsFrameHost{Host: "pypi.org", Port: 443, Count: 4}) {
		t.Errorf("run a hosts %+v, want pypi.org (weight 4) then evil.example", hosts)
	}
	refused := frame.Runs[0].Samples[0]
	if refused.Class != "refused" || refused.RefusalReason != "blocked by the team rule" || refused.PolicyContact != "team-a" || refused.MatchedRule != "rule-1" {
		t.Errorf("first sample %+v, want the refused row with every field", refused)
	}
	notices := wallsNotices(b, wallsView{Kind: wallsViewFull})
	if len(notices) != 3 || notices[0] != (WallsNotice{Action: "decide", RunID: sc.runA, ApprovalID: "ap-a1"}) {
		t.Errorf("notices %+v, want the three in beat order", notices)
	}
	_, nkeys := wallsStreamDecode(t, notices)
	for _, banned := range wallsStreamIdentityKeys {
		if nkeys[banned] {
			t.Errorf("a notice carries a %q key", banned)
		}
	}
}

// TestWallsStreamUnitProjection: a unit admin's frame names their own runs only, with one count per row, a class on
// each sample, and none of the full view's reasons, contacts, rules, fidelity or identity.
func TestWallsStreamUnitProjection(t *testing.T) {
	sc := newWallsStreamScenario(t)
	b := sc.beat(1, sc.rowsA, sc.rowsOut)
	view := wallsView{Kind: wallsViewUnit, Units: map[uuid.UUID]bool{sc.unitA: true}}
	frame := wallsProject(b, view)
	m, keys := wallsStreamDecode(t, frame)
	if len(frame.Runs) != 1 || frame.Runs[0].RunID != sc.runA {
		t.Fatalf("runs %+v, want run a alone", frame.Runs)
	}
	if c, ok := frame.Runs[0].Counters.(*WallsUnitCounters); !ok || *c != (WallsUnitCounters{Allowed: 1, Refused: 1, Held: 1, Fidelity: 1}) {
		t.Errorf("run a counters %+v, want one per row, less the control plane's and the start row", frame.Runs[0].Counters)
	}
	for _, banned := range append([]string{"refusal_reason", "policy_contact", "matched_rule", "rule_source", "by", "reason", "not_recorded"}, wallsStreamIdentityKeys...) {
		if keys[banned] {
			t.Errorf("the unit frame carries a %q key", banned)
		}
	}
	if _, ok := m.(map[string]any)["fidelity"]; ok {
		t.Error("the unit frame has a top-level fidelity")
	}
	if frame.Overflow != nil || frame.Fidelity != nil {
		t.Errorf("overflow %+v, fidelity %+v; want neither", frame.Overflow, frame.Fidelity)
	}
	var classes []string
	for _, s := range frame.Runs[0].Samples {
		classes = append(classes, s.Class)
		if s.RefusalReason != "" || s.PolicyContact != "" || s.MatchedRule != "" {
			t.Errorf("sample %+v carries a full-view field", s)
		}
	}
	if len(classes) != 4 || classes[1] != "refused" || frame.Runs[0].Samples[1].Host != "evil.example" {
		t.Errorf("sample classes %v, want 4 with the refused one second, on evil.example", classes)
	}
	if frame.Runs[0].Samples[2].ApprovalID != "ap-a1" {
		t.Errorf("the held sample's approval id %q, want ap-a1", frame.Runs[0].Samples[2].ApprovalID)
	}
	raw, _ := json.Marshal(m)
	for _, leak := range []string{"cp.example", "proxy.example", "blocked by the team rule", "team-a", "rule-1", "secret-b.example", "other.example", "none.example", sc.runB.String(), sc.runNo.String()} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("the unit frame holds %q", leak)
		}
	}
	notices := wallsNotices(b, view)
	if len(notices) != 1 || notices[0].RunID != sc.runA || notices[0].ApprovalID != "ap-a1" {
		t.Errorf("notices %+v, want run a's one", notices)
	}
	if nm := wallsNotices(b, wallsView{Kind: wallsViewUnit, Units: map[uuid.UUID]bool{}}); len(nm) != 0 {
		t.Errorf("an empty scope is sent %d notices", len(nm))
	}
}

// TestWallsStreamOutOfScopeRowsChangeNoByte: the bytes a unit admin is sent, ids included, are the same with and
// without the rows of runs outside their units, on both streams.
func TestWallsStreamOutOfScopeRowsChangeNoByte(t *testing.T) {
	sc := newWallsStreamScenario(t)
	alone := sc.beat(5, sc.rowsA)
	crowded := sc.beat(5, sc.rowsOut[:2], sc.rowsA, sc.rowsOut[2:])
	units := map[uuid.UUID]bool{sc.unitA: true}
	for name, view := range map[string]wallsView{
		"the organisation's stream": {Kind: wallsViewUnit, Units: units},
		"the run's stream":          {Kind: wallsViewUnit, Run: sc.runA, Units: units},
	} {
		want, got := wallsEvents(wallsStreamNonce, alone, view), wallsEvents(wallsStreamNonce, crowded, view)
		if !bytes.Equal(want, got) {
			t.Errorf("%s: out-of-scope rows changed the bytes:\n%s\nwant\n%s", name, got, want)
		}
		if !strings.HasPrefix(string(got), "id: nonce.5\nevent: frame\ndata: {") {
			t.Errorf("%s: the events begin %q", name, got[:min(len(got), 60)])
		}
	}
}

// TestWallsStreamCapsApplyAfterProjection: out-of-scope runs never push a unit admin's runs into overflow, while a
// full view of the same beat caps at 500 runs and counts the rest.
func TestWallsStreamCapsApplyAfterProjection(t *testing.T) {
	unitA, unitB := uuid.New(), uuid.New()
	meta := map[uuid.UUID]wallsRunMeta{}
	var rows []store.DecisionRow
	for i := range wallsFrameRuns + 103 {
		run, unit := uuid.New(), unitB
		if i%200 == 0 {
			unit = unitA
		}
		meta[run] = wallsRunMeta{UnitID: &unit}
		rows = append(rows, wallsLiveRow(t, run, "egress.allow", "policy:allowlist", "a.example"))
	}
	b := wallsFoldBeat(rows, meta)
	unit := wallsProject(b, wallsView{Kind: wallsViewUnit, Units: map[uuid.UUID]bool{unitA: true}})
	if len(unit.Runs) != 4 || unit.Overflow != nil {
		t.Errorf("a unit admin with 4 runs in scope among %d: %d runs, overflow %+v; want 4 and none", len(rows), len(unit.Runs), unit.Overflow)
	}
	full := wallsProject(b, wallsView{Kind: wallsViewFull})
	if len(full.Runs) != wallsFrameRuns || full.Overflow == nil || full.Overflow.Runs != 103 {
		t.Fatalf("a full view: %d runs, overflow %+v; want %d and 103 more", len(full.Runs), full.Overflow, wallsFrameRuns)
	}
	if c, ok := full.Overflow.Counters.(*WallsCounters); !ok || c.Allowed != 103 {
		t.Errorf("overflow counters %+v, want 103 allowed in the full type", full.Overflow.Counters)
	}
	if empty := wallsProject(wallsBeat{}, wallsView{Kind: wallsViewFull}); empty.Runs == nil || len(empty.Runs) != 0 {
		t.Errorf("an empty frame's runs are %#v, want an empty list", empty.Runs)
	}
}

// TestWallsStreamReplay: a resume inside the ring replays what was missed, projected under the scope held now; a
// beat with nothing for the stream's key replays as an empty frame; and a foreign nonce, a number the ring lost, a
// number past the head and a malformed id each get one gap event with the head's id.
func TestWallsStreamReplay(t *testing.T) {
	st := &wallsLiveStore{}
	unitA, unitB := uuid.New(), uuid.New()
	runA, runB := st.addRun("sub-a", &unitA), st.addRun("sub-b", &unitB)
	e := newWallsStreamEnv(t, st)
	row := func(run uuid.UUID) store.DecisionRow {
		return wallsLiveRow(t, run, "egress.allow", "policy:allowlist", "a.example")
	}
	for _, run := range []uuid.UUID{runA, runB, runA} {
		st.add(row(run))
		e.beat()
	}
	nonce := e.s.wallsLive.sampler.nonce()
	frameRuns := func(ev wallsSSEEvent) (ids []uuid.UUID) {
		var f struct {
			Runs []struct {
				RunID uuid.UUID `json:"run_id"`
			} `json:"runs"`
		}
		if ev.Event != "frame" || json.Unmarshal([]byte(ev.Data), &f) != nil {
			t.Fatalf("event %+v is not a frame", ev)
		}
		for _, r := range f.Runs {
			ids = append(ids, r.RunID)
		}
		return ids
	}
	last := func(id string) []string { return []string{"Last-Event-ID", id} }

	e.setView(wallsView{Kind: wallsViewUnit, Units: map[uuid.UUID]bool{unitA: true}})
	got := e.events(2, "/view", last(nonce+".1")...)
	if got[0].ID != nonce+".2" || got[1].ID != nonce+".3" || len(frameRuns(got[0])) != 0 || len(frameRuns(got[1])) != 1 || frameRuns(got[1])[0] != runA {
		t.Errorf("a resume at 1 in unit a's view: %+v; want beat 2 empty and beat 3 with run a", got)
	}
	e.setView(wallsView{Kind: wallsViewUnit, Units: map[uuid.UUID]bool{unitB: true}})
	got = e.events(2, "/view", last(nonce+".1")...)
	if r := frameRuns(got[0]); len(r) != 1 || r[0] != runB || len(frameRuns(got[1])) != 0 {
		t.Errorf("the same resume after the scope changed: %+v; want beat 2 with run b and beat 3 empty", got)
	}
	e.setView(wallsView{Kind: wallsViewFull, Run: uuid.New()})
	for _, ev := range e.events(3, "/view", last(nonce+".0")...) {
		if ev.Event != "frame" || !strings.Contains(ev.Data, `"runs":[]`) {
			t.Errorf("a beat with nothing for the key replayed as %+v, want an empty frame", ev)
		}
	}

	e.setView(wallsView{Kind: wallsViewFull})
	gapped := func(what, id string) {
		t.Helper()
		head := e.s.wallsLive.sampler.headN()
		ev := e.events(1, "/view", last(id)...)[0]
		if ev.Event != "gap" || ev.Data != "{}" || ev.ID != nonce+"."+strconv.FormatUint(head, 10) {
			t.Errorf("%s: %+v, want one gap event with data {} and id %s.%d", what, ev, nonce, head)
		}
	}
	gapped("a foreign nonce", "other.1")
	gapped("a number past the head", nonce+".9999")
	gapped("a malformed id", "garbage")
	gapped("a malformed number", nonce+".x")
	for range wallsRingBeats {
		e.beat()
	}
	gapped("a number the ring lost", nonce+".1")
}

// TestWallsStreamSendsEachBeat: a stream that sent no Last-Event-ID is sent the beats made after it opened, each as
// a frame with its id, and a notice after it with no id.
func TestWallsStreamSendsEachBeat(t *testing.T) {
	st := &wallsLiveStore{}
	run := st.addRun("sub-a", nil)
	e := newWallsStreamEnv(t, st)
	resp, cancel := e.get("/runs/"+run.String()+"/walls/stream", wallsStreamAs("sub-walls-admin", oidc.RoleAdmin)...)
	defer cancel()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("%d %s, want an event stream", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	st.add(wallsLiveRow(t, run, "egress.allow", "policy:allowlist", "a.example"), wallsLiveRow(t, run, "approval.decide", "", "", wallsLiveData("approval_id", "ap-1")))
	e.beat()
	br := bufio.NewReader(resp.Body)
	frame, ok1 := wallsSSENext(br)
	notice, ok2 := wallsSSENext(br)
	nonce := e.s.wallsLive.sampler.nonce()
	if !ok1 || !ok2 || frame.Event != "frame" || frame.ID != nonce+".1" || notice.Event != "notice" || notice.ID != "" ||
		!strings.Contains(notice.Data, `"action":"decide"`) || !strings.Contains(notice.Data, `"approval_id":"ap-1"`) {
		t.Errorf("events %+v then %+v, want frame %s.1 then a notice with no id", frame, notice, nonce)
	}
}

// TestWallsStreamOpenLimits: the 7th open in a burst is 429 with Retry-After; a principal at 32 shared streams gets
// 422 event_stream_cap and no audit row; a third organisation stream gets 422 and frees its shared slot.
func TestWallsStreamOpenLimits(t *testing.T) {
	st := &wallsLiveStore{}
	run := st.addRun("sub-walls-owner", nil)
	e := newWallsStreamEnv(t, st)
	runPath := "/runs/" + run.String() + "/walls/stream"

	rate := wallsStreamAs("sub-walls-rate", oidc.RoleAdmin)
	for i := range wallsOpenBurst {
		resp, cancel := e.get(runPath, rate...)
		defer cancel()
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("open %d: %d, want 200", i+1, resp.StatusCode)
		}
	}
	resp, cancel := e.get(runPath, rate...)
	defer cancel()
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "5" {
		t.Errorf("the open after the burst: %d, Retry-After %q; want 429 and 5", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	if reason := wallsStreamReason(t, resp); reason != reasonWallsRateLimited {
		t.Errorf("the burst's refusal reason %q, want %s", reason, reasonWallsRateLimited)
	}

	capped := runEventStreamer{types.ActorHuman, "sub-walls-cap"}
	for range maxRunEventStreams {
		if _, ok := e.s.runEvents.openStream(capped); !ok {
			t.Fatal("a slot below the cap was refused")
		}
	}
	rows := func() (n int) {
		for _, ev := range e.rec.snapshot() {
			if ev.Action == authz.AuditAction {
				n++
			}
		}
		return n
	}
	before := rows()
	resp, cancel = e.get(runPath, wallsStreamAs("sub-walls-cap", oidc.RoleAdmin)...)
	defer cancel()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("a principal at %d streams: %d, want 422", maxRunEventStreams, resp.StatusCode)
	}
	if reason := wallsStreamReason(t, resp); reason != string(authz.ReasonEventStreamCap) {
		t.Errorf("the cap's reason %q, want %s", reason, authz.ReasonEventStreamCap)
	}
	if after := rows(); after != before {
		t.Errorf("%d audit rows written for the cap's refusal, want none", after-before)
	}

	org := wallsStreamAs("sub-walls-org", oidc.RoleAdmin)
	who := runEventStreamer{types.ActorHuman, "sub-walls-org"}
	for i := range wallsOrgStreamsMax {
		resp, cancel := e.get("/walls/stream", org...)
		defer cancel()
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("organisation stream %d: %d, want 200", i+1, resp.StatusCode)
		}
	}
	resp, cancel = e.get("/walls/stream", org...)
	defer cancel()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("a third organisation stream: %d, want 422", resp.StatusCode)
	}
	if reason := wallsStreamReason(t, resp); reason != string(authz.ReasonEventStreamCap) {
		t.Errorf("the organisation cap's reason %q, want %s", reason, authz.ReasonEventStreamCap)
	}
	waitFor(t, "the refused stream's shared slot to be released", func() bool {
		shared, orgN, _ := e.held(who)
		return shared == wallsOrgStreamsMax && orgN == wallsOrgStreamsMax
	})
}

// TestWallsStreamEndsOnDisconnect: when the client goes away the handler returns, and the shared slot, the
// organisation slot and the sampler's subscriber are all released.
func TestWallsStreamEndsOnDisconnect(t *testing.T) {
	e := newWallsStreamEnv(t, &wallsLiveStore{})
	who := runEventStreamer{types.ActorHuman, "sub-walls-admin"}
	resp, cancel := e.get("/walls/stream", wallsStreamAs("sub-walls-admin", oidc.RoleAdmin)...)
	defer resp.Body.Close()
	if shared, org, subs := e.held(who); resp.StatusCode != http.StatusOK || shared != 1 || org != 1 || subs != 1 {
		t.Fatalf("an open stream: %d, %d shared, %d organisation, %d subscribers; want 200 and one of each", resp.StatusCode, shared, org, subs)
	}
	start := time.Now()
	cancel()
	waitFor(t, "every slot to be released", func() bool {
		shared, org, subs := e.held(who)
		return shared == 0 && org == 0 && subs == 0
	})
	if d := time.Since(start); d > time.Second {
		t.Errorf("the handler took %v to return after the disconnect, want under 1s", d)
	}
	e.s.runEvents.mu.Lock()
	defer e.s.runEvents.mu.Unlock()
	e.s.wallsLive.mu.Lock()
	defer e.s.wallsLive.mu.Unlock()
	if len(e.s.runEvents.streams) != 0 || len(e.s.wallsLive.orgStreams) != 0 {
		t.Errorf("slot tables %v and %v are not empty", e.s.runEvents.streams, e.s.wallsLive.orgStreams)
	}
}

// TestWallsStreamEndsAtTheHold: a super admin with no unit grant keeps their stream across many refreshes, to the
// hold, and no sooner.
func TestWallsStreamEndsAtTheHold(t *testing.T) {
	const hold = 300 * time.Millisecond
	e := newWallsStreamEnv(t, &wallsLiveStore{}, func(s *Server) {
		s.runEvents.beat, s.runEvents.hold = 10*time.Millisecond, hold
	})
	start := time.Now()
	resp, cancel := e.get("/walls/stream", wallsStreamAs("sub-walls-admin", oidc.RoleAdmin)...)
	defer cancel()
	defer resp.Body.Close()
	if _, ok := wallsSSENext(bufio.NewReader(resp.Body)); ok {
		t.Error("an event arrived with no beat made")
	}
	if d := time.Since(start); d < hold-50*time.Millisecond || d > 5*time.Second {
		t.Errorf("the stream ended after %v, want about the hold of %v", d, hold)
	}
}

// TestWallsStreamStaleBeatIsMarked: a beat whose read failed is sent as a stale frame with no runs.
func TestWallsStreamStaleBeatIsMarked(t *testing.T) {
	st := &wallsLiveStore{}
	e := newWallsStreamEnv(t, st)
	st.setReadErr(context.DeadlineExceeded)
	e.beat()
	nonce := e.s.wallsLive.sampler.nonce()
	ev := e.events(1, "/walls/stream", append(wallsStreamAs("sub-walls-admin", oidc.RoleAdmin), "Last-Event-ID", nonce+".0")...)[0]
	if ev.Event != "frame" || !strings.Contains(ev.Data, `"stale":true`) || !strings.Contains(ev.Data, `"runs":[]`) {
		t.Errorf("a failed beat was sent as %+v, want a stale frame with no runs", ev)
	}
}

// wallsGateStore is a store that holds runs and can read no decision rows.
type wallsGateStore struct {
	store.Store
	run types.AgentRun
}

func (s wallsGateStore) GetRun(_ context.Context, id uuid.UUID) (types.AgentRun, error) {
	if id == s.run.ID {
		return s.run, nil
	}
	return types.AgentRun{}, store.ErrNotFound
}

// TestWallsStreamUnavailableWithoutAReader: a store that cannot read decision rows answers 501 past the gate, and a
// caller the gate refuses still gets the missing run's 404.
func TestWallsStreamUnavailableWithoutAReader(t *testing.T) {
	run := types.AgentRun{ID: uuid.New(), CreatedBy: "sub-walls-owner", State: types.RunRunning}
	e := newWallsStreamEnv(t, wallsGateStore{run: run})
	runPath := "/runs/" + run.ID.String() + "/walls/stream"
	for what, resp := range map[string]func() *http.Response{
		"the owner": func() *http.Response {
			r, _ := e.get(runPath, wallsStreamAs("sub-walls-owner", oidc.RoleUser)...)
			return r
		},
		"an admin": func() *http.Response {
			r, _ := e.get("/walls/stream", wallsStreamAs("sub-walls-admin", oidc.RoleAdmin)...)
			return r
		},
	} {
		r := resp()
		if r.StatusCode != http.StatusNotImplemented {
			t.Errorf("%s: %d, want 501", what, r.StatusCode)
		}
		if reason := wallsStreamReason(t, r); reason != reasonWallsViewUnavailable {
			t.Errorf("%s: reason %q, want %s", what, reason, reasonWallsViewUnavailable)
		}
	}
	other := wallsStreamAs("sub-walls-other", oidc.RoleUser)
	foreign, _ := e.get(runPath, other...)
	missing, _ := e.get("/runs/"+uuid.New().String()+"/walls/stream", other...)
	defer foreign.Body.Close()
	defer missing.Body.Close()
	var a, b bytes.Buffer
	_, _ = a.ReadFrom(foreign.Body)
	_, _ = b.ReadFrom(missing.Body)
	if foreign.StatusCode != http.StatusNotFound || missing.StatusCode != http.StatusNotFound || a.String() != b.String() {
		t.Errorf("a non-owner: %d %q, a missing run: %d %q; want the same 404", foreign.StatusCode, a.String(), missing.StatusCode, b.String())
	}
}

// TestWallsStreamPayloadsCarryNoMountsKey: no live stream wire type, and no type one reaches, has a json key named
// mounts or a field holding a policy spec, a unit row, an overlay, a restriction or the boundary, and every field
// of a wire type names its json key.
func TestWallsStreamPayloadsCarryNoMountsKey(t *testing.T) {
	banned := map[reflect.Type]bool{
		reflect.TypeFor[types.RunPolicySpec](): true, reflect.TypeFor[types.GovernanceUnit](): true,
		reflect.TypeFor[types.CeilingOverlay](): true, reflect.TypeFor[types.Restriction](): true,
		reflect.TypeFor[types.Boundary](): true,
	}
	own := reflect.TypeFor[WallsFrame]().PkgPath()
	module := own[:strings.Index(own, "/internal/")+1]
	seen := map[reflect.Type]bool{}
	var walk func(at string, rt reflect.Type)
	walk = func(at string, rt reflect.Type) {
		for rt.Kind() == reflect.Pointer || rt.Kind() == reflect.Slice || rt.Kind() == reflect.Array || rt.Kind() == reflect.Map {
			rt = rt.Elem()
		}
		if banned[rt] {
			t.Errorf("%s holds %s, which no Walls view payload may carry", at, rt)
		}
		if rt.Kind() != reflect.Struct || seen[rt] || !strings.HasPrefix(rt.PkgPath(), module) {
			return
		}
		seen[rt] = true
		for i := range rt.NumField() {
			f := rt.Field(i)
			key, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if key == "" {
				t.Errorf("%s.%s names no json key", at, f.Name)
			}
			if key == "mounts" {
				t.Errorf("%s.%s is the json key mounts", at, f.Name)
			}
			walk(at+"."+f.Name, f.Type)
		}
	}
	for _, rt := range []reflect.Type{
		reflect.TypeFor[WallsFrame](), reflect.TypeFor[WallsFrameRun](), reflect.TypeFor[WallsFrameHost](),
		reflect.TypeFor[WallsFrameSample](), reflect.TypeFor[WallsFrameOverflow](), reflect.TypeFor[WallsFrameFidelity](),
		reflect.TypeFor[WallsNotice](),
	} {
		walk(rt.Name(), rt)
	}
	if len(seen) < 7 {
		t.Errorf("the walk reached %d types, want the 7 wire types", len(seen))
	}
}

// TestWallsStreamRefreshUsesAFreshMemo: a grant revoked after the stream opened is gone on the next refresh. The
// request the stream opened on carries a populated memo that keeps answering with the old grants; the fresh one
// reads the store again.
func TestWallsStreamRefreshUsesAFreshMemo(t *testing.T) {
	const sub = "sub-walls-unit"
	unit := uuid.New()
	st := &wallsTopoUnitStore{wallsTopoStore: &wallsTopoStore{},
		grants: []types.GovernanceUnitAdmin{{UnitID: unit, SubjectType: types.CapabilitySubjectUser, Subject: sub}}}
	s := wallsTopoServer(st, wallsTopoNewClock())
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r = r.WithContext(withOIDCRole(withOIDCHuman(r.Context(), sub), oidc.RoleUser))
	opened := r.WithContext(context.WithValue(r.Context(), unitScopeMemoKey{}, &unitScopeMemo{
		in: map[unitScopeKey]bool{}, grants: map[string][]types.GovernanceUnitAdmin{},
	}))
	if g, err := s.callerUnitGrants(opened.Context()); err != nil || len(g) != 1 {
		t.Fatalf("grants at open: %v, %v; want one", g, err)
	}
	st.grants = nil
	if g, _ := s.callerUnitGrants(opened.Context()); len(g) != 1 {
		t.Fatalf("the open request's memo answered %d grants after the revoke; the premise is that it stays stale", len(g))
	}
	if g, err := s.callerUnitGrants(wallsFreshScope(opened).Context()); err != nil || len(g) != 0 {
		t.Errorf("grants through a fresh memo after the revoke: %v, %v; want none", g, err)
	}
	if _, ok := s.wallsCallerUnits(wallsFreshScope(opened)); ok {
		t.Error("the caller's units after the revoke are a scope, want none")
	}
}
