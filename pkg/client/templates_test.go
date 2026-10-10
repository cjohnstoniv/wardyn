// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/pkg/client"
)

func TestTemplateStubsReturnUnavailableWithoutARequest(t *testing.T) {
	c := client.New("http://127.0.0.1:1", "token") // nothing listens: a request would fail differently
	ctx := context.Background()
	if _, err := c.ListTemplates(ctx); !errors.Is(err, client.ErrTemplatesUnavailable) {
		t.Errorf("ListTemplates: %v", err)
	}
	if _, err := c.GetTemplate(ctx, uuid.New(), 0); !errors.Is(err, client.ErrTemplatesUnavailable) {
		t.Errorf("GetTemplate: %v", err)
	}
	if _, err := c.SaveTemplate(ctx, uuid.Nil, client.TemplateSaveRequest{}); !errors.Is(err, client.ErrTemplatesUnavailable) {
		t.Errorf("SaveTemplate: %v", err)
	}
	if _, err := c.ImportTemplate(ctx, client.TemplateImportRequest{Format: client.TemplateFormatYAML}); !errors.Is(err, client.ErrTemplatesUnavailable) {
		t.Errorf("ImportTemplate: %v", err)
	}
}

// TestTemplateIntentKeepsPresence is the SDK half of the presence contract: an
// intent distinguishes an absent field from one that is explicitly empty,
// false or zero, through JSON, Set, Unset and the request it materialises.
func TestTemplateIntentKeepsPresence(t *testing.T) {
	var doc client.TemplateDocument
	src := `{"api_version":"wardyn/v1","kind":"RunTemplate","coverage":"partial","intent":{
	  "interactive": false, "workspaces": [], "resources": {"cpu_millis": 0},
	  "inline_policy": {"allowed_domains": [], "auto_stop_after_sec": 0}}}`
	if err := json.Unmarshal([]byte(src), &doc); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"interactive", "workspaces", "resources", "inline_policy"} {
		if !doc.Intent.Has(name) {
			t.Errorf("%s was lost", name)
		}
	}
	if doc.Intent.Has("agent") || doc.Intent.Has("drive") {
		t.Error("an absent field reads as present")
	}
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"interactive":false`, `"workspaces":[]`, `"cpu_millis":0`, `"allowed_domains":[]`, `"auto_stop_after_sec":0`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("encoded document lost %s: %s", want, out)
		}
	}
	var again client.TemplateDocument
	if err := json.Unmarshal(out, &again); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(again.Intent.Names(), ","), "inline_policy,interactive,resources,workspaces"; got != want {
		t.Errorf("names = %s, want %s", got, want)
	}

	req, err := doc.Intent.Request()
	if err != nil {
		t.Fatal(err)
	}
	if req.Workspaces == nil || len(req.Workspaces) != 0 || req.Resources == nil || req.InlinePolicy == nil || req.InlinePolicy.AllowedDomains == nil {
		t.Errorf("explicit empties did not materialise as present: %+v", req)
	}

	var intent client.TemplateIntent
	if err := intent.Set("task", "write the tests"); err != nil {
		t.Fatal(err)
	}
	if !intent.Has("task") {
		t.Fatal("Set did not specify the field")
	}
	intent.Unset("task")
	if intent.Has("task") {
		t.Fatal("Unset kept the field")
	}
	if b, _ := json.Marshal(intent); string(b) != "{}" {
		t.Errorf("an empty intent encodes as %s", b)
	}
	if b, _ := json.Marshal(client.TemplateIntent{}); string(b) != "{}" {
		t.Errorf("a zero intent encodes as %s", b)
	}
}

func TestTemplateIntentRefusesWhatTheRequestDoesNotKnow(t *testing.T) {
	var intent client.TemplateIntent
	if err := json.Unmarshal([]byte(`{"agnet":"claude"}`), &intent); err != nil {
		t.Fatal(err)
	}
	if _, err := intent.Request(); err == nil {
		t.Fatal("an unknown field materialised silently")
	}
	var pool client.TemplateIntent
	if err := json.Unmarshal([]byte(`{"pool_id":"p1","agent":"claude"}`), &pool); err != nil {
		t.Fatal(err)
	}
	if req, err := pool.Request(); err != nil || req.Agent != "claude" {
		t.Errorf("pool_id must not break materialisation: %+v %v", req, err)
	}
}
