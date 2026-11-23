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
	"github.com/cjohnstoniv/wardyn/internal/identity"
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
