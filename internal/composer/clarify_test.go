// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestClarificationJSONSchema_IsStrictPortableSubset(t *testing.T) {
	schema := ClarificationJSONSchema()
	forbidden := map[string]bool{
		"oneOf": true, "anyOf": true, "allOf": true, "pattern": true, "format": true,
		"minLength": true, "maxLength": true, "minItems": true, "maxItems": true,
		"minimum": true, "maximum": true, "patternProperties": true,
	}
	var walk func(node any, path string)
	walk = func(node any, path string) {
		m, ok := node.(map[string]any)
		if !ok {
			return
		}
		for k := range m {
			if forbidden[k] {
				t.Errorf("forbidden schema keyword %q at %s", k, path)
			}
		}
		if m["type"] == "object" {
			if ap, ok := m["additionalProperties"].(bool); !ok || ap {
				t.Errorf("object at %s must set additionalProperties:false", path)
			}
			props, _ := m["properties"].(map[string]any)
			req, _ := m["required"].([]string)
			if len(props) != len(req) {
				t.Errorf("object at %s: %d properties but %d required", path, len(props), len(req))
			}
			for name, sub := range props {
				walk(sub, path+"."+name)
			}
		}
		if m["type"] == "array" {
			walk(m["items"], path+"[]")
		}
	}
	walk(schema, "$")
}

func TestParseClarification_MapsAndDefaults(t *testing.T) {
	raw := []byte(`{
		"ready": false,
		"questions": [
			{"id":"gh","question":"Push access?","why":"scope the token","options":["read","write"],"multi":false},
			{"id":"","question":"Which hosts?","why":"egress","options":[],"multi":true}
		],
		"assumptions": ["targets acme/widgets"],
		"notes": "  a note  "
	}`)
	cl, err := ParseClarification(raw)
	if err != nil {
		t.Fatalf("ParseClarification: %v", err)
	}
	if cl.Ready {
		t.Error("ready should be false")
	}
	if len(cl.Questions) != 2 {
		t.Fatalf("got %d questions, want 2", len(cl.Questions))
	}
	if cl.Questions[0].ID != "gh" || len(cl.Questions[0].Options) != 2 || cl.Questions[0].Multi {
		t.Errorf("q0 mapped wrong: %+v", cl.Questions[0])
	}
	if cl.Questions[1].ID == "" {
		t.Error("a blank id should be auto-filled")
	}
	if cl.Questions[1].Multi != true || len(cl.Questions[1].Options) != 0 {
		t.Errorf("q1 (free-text, multi) mapped wrong: %+v", cl.Questions[1])
	}
	if cl.Notes != "a note" {
		t.Errorf("notes = %q, want trimmed", cl.Notes)
	}
}

func TestParseClarification_EnrichmentFieldsRoundTrip(t *testing.T) {
	raw := []byte(`{
		"ready": false,
		"questions": [
			{"id":"gh","question":"Push access?","why":"scope the token","options":["read","write"],"multi":false,
			 "help":"  Controls whether the agent can only read or also write your repo.  ",
			 "risk":"  Write access lets it push commits and open PRs.  ",
			 "examples":["  read: browse and open PRs from a fork  ","","  write: push directly  ","   "],
			 "misconceptions":["  Read-only still lets it clone privately  ",""]}
		],
		"assumptions": [],
		"notes": ""
	}`)
	cl, err := ParseClarification(raw)
	if err != nil {
		t.Fatalf("ParseClarification: %v", err)
	}
	if len(cl.Questions) != 1 {
		t.Fatalf("got %d questions, want 1", len(cl.Questions))
	}
	q := cl.Questions[0]
	if q.Help != "Controls whether the agent can only read or also write your repo." {
		t.Errorf("help = %q, want trimmed", q.Help)
	}
	if q.Risk != "Write access lets it push commits and open PRs." {
		t.Errorf("risk = %q, want trimmed", q.Risk)
	}
	wantExamples := []string{"read: browse and open PRs from a fork", "write: push directly"}
	if !reflect.DeepEqual(q.Examples, wantExamples) {
		t.Errorf("examples = %#v, want %#v (trimmed, blanks dropped)", q.Examples, wantExamples)
	}
	wantMisconceptions := []string{"Read-only still lets it clone privately"}
	if !reflect.DeepEqual(q.Misconceptions, wantMisconceptions) {
		t.Errorf("misconceptions = %#v, want %#v (trimmed, blanks dropped)", q.Misconceptions, wantMisconceptions)
	}
}

func TestParseClarification_EnrichmentFieldsOmittedStayEmpty(t *testing.T) {
	raw := []byte(`{
		"ready": false,
		"questions": [
			{"id":"q","question":"Which hosts?","why":"egress","options":[],"multi":false}
		],
		"assumptions": [],
		"notes": ""
	}`)
	cl, err := ParseClarification(raw)
	if err != nil {
		t.Fatalf("ParseClarification: %v", err)
	}
	if len(cl.Questions) != 1 {
		t.Fatalf("got %d questions, want 1", len(cl.Questions))
	}
	q := cl.Questions[0]
	if q.Help != "" || q.Risk != "" || len(q.Examples) != 0 || len(q.Misconceptions) != 0 {
		t.Errorf("enrichment fields should stay empty when omitted: %+v", q)
	}
}

func TestParseClarification_NotReadyNoQuestionsBecomesReady(t *testing.T) {
	cl, err := ParseClarification([]byte(`{"ready":false,"questions":[],"assumptions":[],"notes":""}`))
	if err != nil {
		t.Fatalf("ParseClarification: %v", err)
	}
	if !cl.Ready {
		t.Error("not-ready with zero questions must be coerced to ready (no stall)")
	}
}

func TestParseClarification_CapsQuestionCount(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"ready":false,"questions":[`)
	for i := 0; i < MaxClarifyQuestions+5; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"id":"q","question":"x","why":"y","options":[],"multi":false}`)
	}
	b.WriteString(`],"assumptions":[],"notes":""}`)
	cl, err := ParseClarification([]byte(b.String()))
	if err != nil {
		t.Fatalf("ParseClarification: %v", err)
	}
	if len(cl.Questions) != MaxClarifyQuestions {
		t.Errorf("got %d questions, want capped at %d", len(cl.Questions), MaxClarifyQuestions)
	}
}

func TestParseClarification_FailsClosed(t *testing.T) {
	if _, err := ParseClarification([]byte("not json")); err == nil {
		t.Fatal("expected an error on malformed JSON")
	}
}

func TestClarifyWithRetry_RetriesThenSucceeds(t *testing.T) {
	attempts := 0
	cl, err := ClarifyWithRetry(context.Background(), 3, func(_ context.Context, _ int) ([]byte, error) {
		attempts++
		if attempts < 2 {
			return []byte("garbage"), nil
		}
		return []byte(`{"ready":true,"questions":[],"assumptions":[],"notes":""}`), nil
	})
	if err != nil {
		t.Fatalf("ClarifyWithRetry: %v", err)
	}
	if !cl.Ready || attempts != 2 {
		t.Errorf("ready=%v attempts=%d, want ready after 2 attempts", cl.Ready, attempts)
	}
}

func TestClarifyWithRetry_TransportErrorNotRetried(t *testing.T) {
	attempts := 0
	_, err := ClarifyWithRetry(context.Background(), 3, func(_ context.Context, _ int) ([]byte, error) {
		attempts++
		return nil, errors.New("boom")
	})
	if err == nil || attempts != 1 {
		t.Errorf("transport error must abort immediately: err=%v attempts=%d", err, attempts)
	}
}

func TestFakeComposer_ClarifyInterviewThenReady(t *testing.T) {
	f := &FakeComposer{
		ClarifyEnabled: true,
		ClarifyResult:  Clarification{Questions: []Question{{ID: "q", Question: "?"}}},
	}
	// Round 0 (no transcript): asks.
	cl, _ := f.Clarify(context.Background(), ComposeRequest{})
	if cl.Ready || len(cl.Questions) != 1 {
		t.Errorf("round 0 should ask: %+v", cl)
	}
	// After an answer (transcript present): ready.
	cl, _ = f.Clarify(context.Background(), ComposeRequest{Transcript: []QA{{Question: "?", Answer: "a"}}})
	if !cl.Ready {
		t.Error("with a transcript the fake should be ready")
	}
	// Disabled fake is always ready.
	plain := &FakeComposer{}
	cl, _ = plain.Clarify(context.Background(), ComposeRequest{})
	if !cl.Ready {
		t.Error("a non-interview fake must be ready")
	}
}

// proposeOnly implements Composer but NOT Clarifier, to prove the registry
// degrades a non-interview backend to Ready:true.
type proposeOnly struct{}

func (proposeOnly) Propose(context.Context, ComposeRequest) (Proposal, error) {
	return Proposal{}, nil
}

func TestRegistry_ClarifyNonClarifierIsReady(t *testing.T) {
	reg, err := NewRegistry("p", []RegistryEntry{{
		Info:     BackendInfo{Name: "p", Provider: "x", Model: "m"},
		Composer: proposeOnly{},
	}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	cl, err := reg.Clarify(context.Background(), "", ComposeRequest{})
	if err != nil {
		t.Fatalf("Clarify: %v", err)
	}
	if !cl.Ready || len(cl.Questions) != 0 {
		t.Errorf("a non-Clarifier backend must degrade to ready with no questions: %+v", cl)
	}
}

func TestRegistry_ClarifyUnknownBackend(t *testing.T) {
	reg, err := NewRegistry("p", []RegistryEntry{{
		Info:     BackendInfo{Name: "p", Provider: "x", Model: "m"},
		Composer: &FakeComposer{},
	}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if _, err := reg.Clarify(context.Background(), "nope", ComposeRequest{}); !errors.Is(err, ErrUnknownBackend) {
		t.Errorf("want ErrUnknownBackend, got %v", err)
	}
}

func TestValidateRequest_TranscriptCaps(t *testing.T) {
	base := ComposeRequest{Prompt: "do a thing", Workspace: Workspace{Kind: WorkspaceEphemeral}}

	tooMany := base
	for i := 0; i < MaxTranscriptQAs+1; i++ {
		tooMany.Transcript = append(tooMany.Transcript, QA{Question: "q", Answer: "a"})
	}
	if err := ValidateRequest(tooMany); !errors.Is(err, ErrInputTooLarge) {
		t.Errorf("over-count transcript must be ErrInputTooLarge, got %v", err)
	}

	tooBig := base
	tooBig.Transcript = []QA{{Question: "q", Answer: strings.Repeat("x", MaxTranscriptBytes+1)}}
	if err := ValidateRequest(tooBig); !errors.Is(err, ErrInputTooLarge) {
		t.Errorf("over-size transcript must be ErrInputTooLarge, got %v", err)
	}

	ok := base
	ok.Transcript = []QA{{Question: "q", Answer: "a"}}
	if err := ValidateRequest(ok); err != nil {
		t.Errorf("a small transcript must validate, got %v", err)
	}
}
