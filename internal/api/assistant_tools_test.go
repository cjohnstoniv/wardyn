// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/assistant"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// assistantToolSpy counts the reads a tool makes of the session's target and of the templates, so a refused
// call can be shown to have read nothing.
type assistantToolSpy struct {
	*assistantFake
	smu         sync.Mutex
	targetReads int
}

func (f *assistantToolSpy) ListTemplatesVisible(ctx context.Context, p string, g []string) ([]types.RunTemplate, error) {
	f.smu.Lock()
	f.targetReads++
	f.smu.Unlock()
	return f.assistantFake.ListTemplatesVisible(ctx, p, g)
}

// TemplateGroupAdminGroups answers no group: the template caller of a new template draft asks it.
func (f *assistantToolSpy) TemplateGroupAdminGroups(context.Context, []string) ([]string, error) {
	return nil, nil
}

func (f *assistantToolSpy) reads() int {
	f.smu.Lock()
	defer f.smu.Unlock()
	return f.targetReads
}

// assistantToolCall posts one tool call on the run's token, as the proxy forwards it.
func assistantToolCall(t *testing.T, srv *Server, run uuid.UUID, name string, session uuid.UUID, args string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"session_id":"` + session.String() + `","args":` + args + `}`
	r := assistantRunRequest(http.MethodPost, "/api/v1/internal/assistant/tools/"+name, body, run)
	rc := chi.NewRouteContext()
	rc.URLParams.Add("name", name)
	r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rc))
	w := httptest.NewRecorder()
	srv.handleAssistantToolCall(w, r)
	return w
}

// assistantToolSession seeds a live session with a run for a fresh person, on a site where compose and review
// are on and ready.
func assistantToolSession(t *testing.T, purpose types.AssistantPurpose) (*assistantToolSpy, *Server, types.AssistantSession, uuid.UUID) {
	t.Helper()
	site, bind := assistantTwoProviders()
	site.Assistant.Review = true
	site.Assistant.Providers[0].Review, site.Assistant.Providers[1].Review = true, true
	f := &assistantToolSpy{assistantFake: newAssistantFake(site)}
	srv, _ := assistantSessionServer(t, f.assistantFake)
	srv.cfg.Store = f
	person := "sub-tools-" + uuid.NewString()
	run := f.seedRun(t, person)
	s := assistantLiveSession(person, &run, bind)
	if purpose == types.AssistantReview {
		change := uuid.New()
		f.changes[change] = types.GovernanceChange{ID: change, TargetKind: govKindRestriction, ProposedBy: person}
		s.Purpose, s.TargetKind, s.TargetKey = types.AssistantReview, types.AssistantTargetChange, change.String()
	}
	f.seed(s)
	return f, srv, s, run
}

func assistantToolNamesOf(t *testing.T, w *httptest.ResponseRecorder) []string {
	t.Helper()
	var list []map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("tool list = %d %s: %v", w.Code, w.Body, err)
	}
	var names []string
	for _, tool := range list {
		var n string
		_ = json.Unmarshal(tool["name"], &n)
		names = append(names, n)
		if len(tool["input_schema"]) == 0 {
			t.Errorf("tool %s has no input_schema: the relay reads the schema from that field", n)
		}
	}
	return names
}

// The tool list keys on the run token's run id as the turn poll does: a run with no session is 404. Both lists are
// non-empty, a review session's leaves out draft, and every name has the relay's shape.
func TestAssistantToolListServesInputSchema(t *testing.T) {
	_, srv, _, run := assistantToolSession(t, types.AssistantCompose)
	w := httptest.NewRecorder()
	srv.handleAssistantToolList(w, assistantRunRequest(http.MethodGet, "/api/v1/internal/assistant/tools", "", uuid.New()))
	wantReason(t, w, http.StatusNotFound, reasonAssistantSessionNotFound)

	w = httptest.NewRecorder()
	srv.handleAssistantToolList(w, assistantRunRequest(http.MethodGet, "/api/v1/internal/assistant/tools", "", run))
	compose := assistantToolNamesOf(t, w)
	_, rsrv, _, rrun := assistantToolSession(t, types.AssistantReview)
	w = httptest.NewRecorder()
	rsrv.handleAssistantToolList(w, assistantRunRequest(http.MethodGet, "/api/v1/internal/assistant/tools", "", rrun))
	review := assistantToolNamesOf(t, w)
	if len(compose) == 0 || len(review) == 0 {
		t.Fatalf("compose %v, review %v: neither list may be empty", compose, review)
	}
	if !slices.Contains(compose, types.AssistantToolDraft) || slices.Contains(review, types.AssistantToolDraft) {
		t.Errorf("compose %v, review %v: draft is a compose tool only", compose, review)
	}
	for _, n := range append(compose, review...) {
		if !types.AssistantToolNameValid(n) {
			t.Errorf("tool name %q does not match the relay's pattern", n)
		}
	}
}

// A call keys on the run's session: a run with none is 404; another session's id is 409; an unknown tool is 404;
// draft in a review session is 403. Every call that names the live session leaves a tool_record.
func TestAssistantToolCallRefusals(t *testing.T) {
	f, srv, s, run := assistantToolSession(t, types.AssistantCompose)
	read := `{"kind":"templates"}`
	wantReason(t, assistantToolCall(t, srv, uuid.New(), types.AssistantToolRead, s.ID, read), http.StatusNotFound, reasonAssistantSessionNotFound)
	wantReason(t, assistantToolCall(t, srv, run, types.AssistantToolRead, uuid.New(), read), http.StatusConflict, reasonAssistantSessionNotFound)
	wantReason(t, assistantToolCall(t, srv, run, "nosuchtool", s.ID, `{}`), http.StatusNotFound, reasonAssistantToolUnknown)
	if w := assistantToolCall(t, srv, run, types.AssistantToolRead, s.ID, read); w.Code != http.StatusOK {
		t.Fatalf("read templates = %d %s, want 200", w.Code, w.Body)
	}
	var names []string
	for _, e := range f.eventsOf(s.ID) {
		if e.Kind == types.AssistantEventToolRecord {
			var p struct {
				Name   string `json:"name"`
				OK     bool   `json:"ok"`
				DryRun bool   `json:"dry_run"`
			}
			_ = json.Unmarshal(e.Payload, &p)
			if !p.DryRun {
				t.Errorf("tool_record %s has no dry_run mark", e.Payload)
			}
			names = append(names, p.Name)
		}
	}
	if want := []string{"nosuchtool", types.AssistantToolRead}; !slices.Equal(names, want) {
		t.Errorf("tool records = %v, want %v (none for a call that named no live session)", names, want)
	}

	_, rsrv, rs, rrun := assistantToolSession(t, types.AssistantReview)
	wantReason(t, assistantToolCall(t, rsrv, rrun, types.AssistantToolDraft, rs.ID, `{"target_kind":"change","parts":[{"section":"name","value":"x"}]}`),
		http.StatusForbidden, reasonAssistantToolRefused)
}

// The purpose turned off between two calls: the second answers 409, reads nothing of the target, and ends the
// session as turned_off.
func TestAssistantToolTurnedOffBetweenCalls(t *testing.T) {
	f, srv, s, run := assistantToolSession(t, types.AssistantCompose)
	if w := assistantToolCall(t, srv, run, types.AssistantToolRead, s.ID, `{"kind":"templates"}`); w.Code != http.StatusOK {
		t.Fatalf("first call = %d %s, want 200", w.Code, w.Body)
	}
	before := f.reads()
	f.site.Assistant.Compose = false
	wantReason(t, assistantToolCall(t, srv, run, types.AssistantToolRead, s.ID, `{"kind":"templates"}`), http.StatusConflict, reasonAssistantOff)
	if f.reads() != before {
		t.Errorf("the refused call read the templates %d times", f.reads()-before)
	}
	if got := f.session(s.ID); got.Live() || got.EndReason != types.AssistantEndTurnedOff {
		t.Errorf("session = %s/%s, want ended as turned_off", got.State, got.EndReason)
	}
}

// A never-sent kind is refused by name; run history is absent while the organisation keeps it back.
func TestAssistantToolNeverSentAndRunHistoryOff(t *testing.T) {
	f, srv, s, run := assistantToolSession(t, types.AssistantCompose)
	f.site.Assistant.RunHistoryCounts = false
	wantReason(t, assistantToolCall(t, srv, run, types.AssistantToolRead, s.ID, `{"kind":"run_history"}`), http.StatusForbidden, reasonAssistantToolRefused)
	wantReason(t, assistantToolCall(t, srv, run, types.AssistantToolSimulate, s.ID, `{"target_kind":"template","draft":{}}`), http.StatusForbidden, reasonAssistantToolRefused)
	f.site.Assistant.NeverSentKinds = []string{govKindUnit}
	// A unit kind on the never-sent list also stops the session on its next call only when the target is a unit;
	// here the target is a template, so the unit read alone is refused.
	wantReason(t, assistantToolCall(t, srv, run, types.AssistantToolRead, s.ID, `{"kind":"unit"}`), http.StatusConflict, reasonAssistantKindNeverSent)
}

// A turn_end after two tool calls yields a suggestion naming exactly those two tools.
func TestAssistantToolSuggestionNamesTheToolRecords(t *testing.T) {
	f, srv, s, run := assistantToolSession(t, types.AssistantCompose)
	for _, c := range []struct{ name, args string }{{types.AssistantToolRead, `{"kind":"templates"}`}, {types.AssistantToolRead, `{"kind":"target"}`}} {
		if w := assistantToolCall(t, srv, run, c.name, s.ID, c.args); w.Code != http.StatusOK {
			t.Fatalf("%s = %d %s", c.name, w.Code, w.Body)
		}
	}
	w := httptest.NewRecorder()
	srv.handleAssistantPostEvent(w, assistantRunRequest(http.MethodPost, "/api/v1/internal/assistant/events",
		`{"session_id":"`+s.ID.String()+`","kind":"`+types.AssistantEventTurnEnd+`","payload":{}}`, run))
	if w.Code != http.StatusAccepted {
		t.Fatalf("turn_end = %d %s", w.Code, w.Body)
	}
	for _, e := range f.eventsOf(s.ID) {
		if e.Kind == types.AssistantEventSuggestion {
			var p struct {
				Tools []string `json:"tools"`
			}
			_ = json.Unmarshal(e.Payload, &p)
			if want := []string{types.AssistantToolRead, types.AssistantToolRead}; !slices.Equal(p.Tools, want) {
				t.Errorf("suggestion tools = %v, want %v", p.Tools, want)
			}
			return
		}
	}
	t.Error("no suggestion event after turn_end")
}

// A result over 64 KiB is cut and marked truncated; every string passes the session's letters.
func TestAssistantToolResultCapAndLetters(t *testing.T) {
	letters := assistant.LettersFromMap(map[string]string{"Person A": "pat@corp.example"})
	raw, ref := assistantToolResult(map[string]any{"who": "ask pat@corp.example", "n": 3}, letters)
	if ref != nil || strings.Contains(string(raw), "pat@corp.example") || !strings.Contains(string(raw), "Person A") {
		t.Errorf("lettered result = %s, want the address replaced by its letter", raw)
	}
	raw, ref = assistantToolResult(map[string]any{"big": strings.Repeat("x", 70<<10)}, letters)
	if ref != nil || len(raw) > assistantToolResultMax {
		t.Fatalf("capped result is %d bytes", len(raw))
	}
	var out struct {
		Truncated bool `json:"truncated"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || !out.Truncated {
		t.Errorf("a result over the cap = %.80s, want truncated: true", raw)
	}
}
