// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/assistant"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// assistantRunRequest is a request on the internal routes, carrying run's token claims.
func assistantRunRequest(method, path, body string, run uuid.UUID) *http.Request {
	ctx := context.WithValue(context.Background(), claimsCtxKey{}, &identity.Claims{RunID: run})
	return httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
}

// The internal routes answer a run with no session as an unknown one, refuse a cursor that is not a number, and
// refuse an event a run may not write: records, a reset of its own, a server-only kind, or a replaced session's id.
func TestAssistantInternalRefusals(t *testing.T) {
	f := newAssistantFake(types.SiteConfig{})
	srv, _ := assistantSessionServer(t, f)
	w := httptest.NewRecorder()
	srv.handleAssistantTurns(w, assistantRunRequest(http.MethodGet, "/api/v1/internal/assistant/turns", "", uuid.New()))
	wantReason(t, w, http.StatusNotFound, reasonAssistantSessionNotFound)

	run := f.seedRun(t, "sub-internal")
	s := assistantLiveSession("sub-internal", &run, types.AssistantBinding{})
	f.seed(s)
	w = httptest.NewRecorder()
	srv.handleAssistantTurns(w, assistantRunRequest(http.MethodGet, "/api/v1/internal/assistant/turns?after=abc", "", run))
	wantReason(t, w, http.StatusBadRequest, reasonInvalidRequestBody)

	post := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		srv.handleAssistantPostEvent(w, assistantRunRequest(http.MethodPost, "/api/v1/internal/assistant/events", body, run))
		return w
	}
	for name, body := range map[string]string{
		"records":         `{"session_id":"` + s.ID.String() + `","kind":"state","payload":{"state":"records","records":[]}}`,
		"a reset":         `{"session_id":"` + s.ID.String() + `","kind":"state","payload":{"turn":"reset"}}`,
		"a server kind":   `{"session_id":"` + s.ID.String() + `","kind":"` + types.AssistantEventToolRecord + `","payload":{"name":"read"}}`,
		"records on text": `{"session_id":"` + s.ID.String() + `","kind":"text","payload":{"text":"hi","records":[]}}`,
	} {
		if w := post(body); w.Code != http.StatusBadRequest {
			t.Errorf("%s = %d %s, want 400", name, w.Code, w.Body)
		}
	}
	wantReason(t, post(`{"session_id":"`+uuid.NewString()+`","kind":"text","payload":{"text":"late"}}`), http.StatusConflict, reasonAssistantSessionNotFound)
	if n := len(f.eventsOf(s.ID)); n != 0 {
		t.Errorf("refused posts stored %d events", n)
	}
	if w := post(`{"session_id":"` + s.ID.String() + `","kind":"text","payload":{"text":"hi"}}`); w.Code != http.StatusAccepted {
		t.Errorf("a text event = %d %s, want 202", w.Code, w.Body)
	}
}

// Past 5000 events a turn's digests and tool names are read from the turn alone: the tools are the server's
// tool_record names since the turn_end before it, never the relay's tool_call events or an older turn's records,
// the output digest is the turn's text and proposal, and every read is bounded.
func TestAssistantTurnDigestsPastFiveThousandEvents(t *testing.T) {
	f := newAssistantFake(types.SiteConfig{})
	srv, _ := assistantSessionServer(t, f)
	s := assistantLiveSession("sub-digests", nil, types.AssistantBinding{})
	records := []types.AssistantSection{{Label: assistant.LabelUnit, Text: "unit a"}}
	recJSON, _ := json.Marshal(map[string]any{"state": "records", "records": records})
	ev := func(seq int, kind, payload string) types.AssistantEvent {
		return types.AssistantEvent{Seq: int64(seq), Kind: kind, Payload: json.RawMessage(payload)}
	}
	evs := []types.AssistantEvent{{Seq: 1, Kind: types.AssistantEventState, Payload: recJSON}}
	seq := 1
	add := func(kind, payload string) {
		seq++
		evs = append(evs, ev(seq, kind, payload))
	}
	for i := 0; i < 5000; i++ {
		switch i {
		case 100:
			add(types.AssistantEventToolRecord, `{"name":"old"}`)
		case 200:
			add(types.AssistantEventUserMessage, `{"turn":"earlier"}`)
		default:
			add(types.AssistantEventText, `{"text":"z"}`)
		}
	}
	add(types.AssistantEventTurnEnd, `{}`)
	add(types.AssistantEventUserMessage, `{"turn":"please"}`)
	var text strings.Builder
	for i := 0; i < 3*assistantEventsPage; i++ {
		part := fmt.Sprintf("p%d ", i)
		text.WriteString(part)
		add(types.AssistantEventText, `{"text":"`+part+`"}`)
		if i == 10 {
			add(types.AssistantEventToolRecord, `{"name":"read"}`)
			add(types.AssistantEventToolCall, `{"name":"draft"}`)
		}
	}
	add(types.AssistantEventToolRecord, `{"name":"validate"}`)
	proposal := `{"proposal":{"name":"x"}}`
	add(types.AssistantEventProposal, proposal)
	add(types.AssistantEventTurnEnd, `{}`)
	turnEnd := int64(seq)
	add(types.AssistantEventText, `{"text":"after"}`)
	f.seed(s, evs...)

	prompt, output, tools, err := srv.assistantTurnDigests(context.Background(), f, s.ID, turnEnd)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(tools, []string{"read", "validate"}) {
		t.Errorf("tools = %v, want read and validate only", tools)
	}
	if want := assistant.OutputDigest(text.String(), []byte(proposal)); output != want {
		t.Errorf("output digest = %s, want %s: the turn's text and proposal", output, want)
	}
	if want := assistant.PromptDigest(assistant.RecordBytes(records) + "please"); prompt != want {
		t.Errorf("prompt digest = %s, want %s: the records and the last person turn", prompt, want)
	}
	assistantFindsBounded(t, f)
}

// A fresh session's first turn carries the records the create stored, which are the preview's, byte for byte,
// with the system prompt, the session's model and the purpose's tools; a later turn carries none of them.
func TestAssistantFreshFirstTurnCarriesThePreviewsRecords(t *testing.T) {
	site, bind := assistantTwoProviders()
	f := newAssistantFake(site)
	f.templates = []types.RunTemplate{{ID: uuid.New(), Name: "deploy", Description: "ships the app"}}
	srv, _ := assistantSessionServer(t, f)
	person := "sub-fresh-" + uuid.NewString()
	cookie := ssoSession(t, person, person+"@corp.example", oidc.RoleUser)
	w := doSSO(t, srv, http.MethodPost, "/api/v1/assistant/preview", cookie, `{"purpose":"compose","target":{"kind":"template"}}`)
	var preview types.AssistantPreview
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &preview) != nil || len(preview.Sections) == 0 {
		t.Fatalf("preview = %d %s", w.Code, w.Body)
	}
	run := f.seedRun(t, person)
	s := assistantLiveSession(person, &run, bind)
	stored, _ := json.Marshal(assistantRecordsEvent(&assistantPrep{digest: preview.Digest, payload: assistant.Payload{Sections: preview.Sections}}, "", s.ID))
	f.seed(s, types.AssistantEvent{Seq: 1, Kind: types.AssistantEventState, Payload: stored},
		types.AssistantEvent{Seq: 2, Kind: types.AssistantEventUserMessage, Payload: json.RawMessage(`{"turn":"hi"}`)},
		types.AssistantEvent{Seq: 3, Kind: types.AssistantEventUserMessage, Payload: json.RawMessage(`{"turn":"again"}`)})
	turnAfter := func(after int64) types.AssistantTurn {
		rec := httptest.NewRecorder()
		srv.handleAssistantTurns(rec, assistantRunRequest(http.MethodGet, fmt.Sprintf("/api/v1/internal/assistant/turns?after=%d", after), "", run))
		var turn types.AssistantTurn
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &turn) != nil {
			t.Fatalf("turns after %d = %d %s", after, rec.Code, rec.Body)
		}
		return turn
	}
	first := turnAfter(0)
	got, _ := json.Marshal(first.Records)
	want, _ := json.Marshal(preview.Sections)
	if first.Kind != types.AssistantTurnUser || first.Text != "hi" || string(got) != string(want) {
		t.Fatalf("first turn = %s %q records %s, want the person's turn with the preview's %s", first.Kind, first.Text, got, want)
	}
	if first.System == "" || first.Model != bind.Model || !slices.Equal(first.Tools, assistantToolNames(types.AssistantCompose)) {
		t.Errorf("first turn system %d bytes, model %q, tools %v", len(first.System), first.Model, first.Tools)
	}
	if later := turnAfter(2); later.Text != "again" || later.Records != nil || later.System != "" || later.Tools != nil {
		t.Errorf("a later turn = %+v, want the text alone", later)
	}
}

// A turn_end writes one assistant.suggestion.create row: the person as actor, the session as target, the purpose,
// binding, target and digests, the server's tool names, and no prompt or output text.
func TestAssistantSuggestionRowFields(t *testing.T) {
	site, bind := assistantTwoProviders()
	f := newAssistantFake(site)
	srv, h := assistantSessionServer(t, f)
	person := "sub-suggest-" + uuid.NewString()
	run := f.seedRun(t, person)
	s := assistantLiveSession(person, &run, bind)
	f.seed(s, types.AssistantEvent{Seq: 1, Kind: types.AssistantEventState, Payload: json.RawMessage(`{"state":"records","records":[]}`)},
		types.AssistantEvent{Seq: 2, Kind: types.AssistantEventUserMessage, Payload: json.RawMessage(`{"turn":"secret prompt words"}`)},
		types.AssistantEvent{Seq: 3, Kind: types.AssistantEventToolRecord, Payload: json.RawMessage(`{"name":"validate"}`)},
		types.AssistantEvent{Seq: 4, Kind: types.AssistantEventText, Payload: json.RawMessage(`{"text":"secret output words"}`)})
	rec := httptest.NewRecorder()
	srv.handleAssistantPostEvent(rec, assistantRunRequest(http.MethodPost, "/api/v1/internal/assistant/events",
		`{"session_id":"`+s.ID.String()+`","kind":"turn_end","payload":{}}`, run))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("turn_end = %d %s", rec.Code, rec.Body)
	}
	rows := auditActions(h, assistantSuggestionAction)
	if len(rows) != 1 {
		t.Fatalf("%d suggestion rows, want 1", len(rows))
	}
	row := rows[0]
	if row.ActorType != types.ActorHuman || row.Actor != person || row.Target != s.ID.String() || row.RunID == nil || *row.RunID != run {
		t.Errorf("row actor %s/%s target %s run %v", row.ActorType, row.Actor, row.Target, row.RunID)
	}
	var data map[string]any
	if err := json.Unmarshal(row.Data, &data); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"purpose", "provider_id", "model", "prompt_digest", "output_digest", "tools", "target_kind", "target_key"} {
		if _, ok := data[k]; !ok {
			t.Errorf("row data has no %s: %s", k, row.Data)
		}
	}
	if data["provider_id"] != bind.ProviderID || data["model"] != bind.Model || fmt.Sprint(data["tools"]) != "[validate]" {
		t.Errorf("row data = %s", row.Data)
	}
	if strings.Contains(string(row.Data), "secret") {
		t.Errorf("the row carries text: %s", row.Data)
	}
}

// Past store.AssistantSessionEventsMax events the run's post is 429 and writes nothing.
func TestAssistantRunEventsAreCapped(t *testing.T) {
	f := newAssistantFake(types.SiteConfig{})
	srv, _ := assistantSessionServer(t, f)
	run := f.seedRun(t, "sub-full")
	s := assistantLiveSession("sub-full", &run, types.AssistantBinding{})
	evs := make([]types.AssistantEvent, store.AssistantSessionEventsMax)
	for i := range evs {
		evs[i] = types.AssistantEvent{Seq: int64(i + 1), Kind: types.AssistantEventText, Payload: json.RawMessage(`{}`)}
	}
	f.seed(s, evs...)
	rec := httptest.NewRecorder()
	srv.handleAssistantPostEvent(rec, assistantRunRequest(http.MethodPost, "/api/v1/internal/assistant/events",
		`{"session_id":"`+s.ID.String()+`","kind":"text","payload":{"text":"one more"}}`, run))
	wantReason(t, rec, http.StatusTooManyRequests, reasonAssistantSessionEventsFull)
	if n := len(f.eventsOf(s.ID)); n != store.AssistantSessionEventsMax {
		t.Errorf("%d events after a refused post, want %d", n, store.AssistantSessionEventsMax)
	}
}
