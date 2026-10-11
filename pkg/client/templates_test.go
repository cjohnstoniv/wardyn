// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// TestTemplateMethodsCallTheTemplateRoutes proves each typed method makes its
// real call, and that the 501 the server answers until the store lands comes
// back as an APIError carrying the reason.
func TestTemplateMethodsCallTheTemplateRoutes(t *testing.T) {
	type call struct{ method, path, body string }
	var got call
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = call{r.Method, r.URL.Path, string(b)}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotImplemented)
		_, _ = w.Write([]byte(`{"error":"Templates are not available on this server yet.","reason":"templates_unavailable"}`))
	}))
	defer srv.Close()
	c := client.New(srv.URL, "token")
	ctx := context.Background()
	id := uuid.New()
	rev := 3
	grant := client.TemplateGroupAdmin{Group: "eng", Person: "gina"}
	for _, tc := range []struct {
		name string
		do   func() error
		want call
	}{
		{"list", func() error { _, err := c.ListTemplates(ctx); return err }, call{"GET", "/api/v1/templates", ""}},
		{"get", func() error { _, err := c.GetTemplate(ctx, id, 0); return err }, call{"GET", "/api/v1/templates/" + id.String(), ""}},
		{"get a revision", func() error { _, err := c.GetTemplate(ctx, id, 2); return err }, call{"GET", "/api/v1/templates/" + id.String() + "/revisions/2", ""}},
		{"create", func() error {
			_, err := c.SaveTemplate(ctx, uuid.Nil, client.TemplateSaveRequest{Scope: client.TemplateScopeOrg, Name: "n"})
			return err
		},
			call{"POST", "/api/v1/templates", ""}},
		{"update", func() error {
			_, err := c.SaveTemplate(ctx, id, client.TemplateSaveRequest{Scope: client.TemplateScopePerson, Name: "n", ExpectedRevision: &rev})
			return err
		}, call{"PUT", "/api/v1/templates/" + id.String(), ""}},
		{"delete", func() error { return c.DeleteTemplate(ctx, id) }, call{"DELETE", "/api/v1/templates/" + id.String(), ""}},
		{"copy", func() error {
			_, err := c.CopyTemplate(ctx, id, client.TemplateCopyRequest{Scope: client.TemplateScopeGroup, GroupID: "eng"})
			return err
		}, call{"POST", "/api/v1/templates/" + id.String() + "/copy", `{"scope":"group","group_id":"eng"}`}},
		{"import", func() error {
			_, err := c.ImportTemplate(ctx, client.TemplateImportRequest{Format: client.TemplateFormatYAML, Source: "a: 1"})
			return err
		}, call{"POST", "/api/v1/templates/import", `{"format":"yaml","source":"a: 1"}`}},
		{"list group admins", func() error { _, err := c.ListTemplateGroupAdmins(ctx); return err }, call{"GET", "/api/v1/admin/template-group-admins", ""}},
		{"grant", func() error { _, err := c.GrantTemplateGroupAdmin(ctx, grant); return err },
			call{"PUT", "/api/v1/admin/template-group-admins", `{"group_id":"eng","person":"gina"}`}},
		{"revoke", func() error { return c.RevokeTemplateGroupAdmin(ctx, grant) },
			call{"DELETE", "/api/v1/admin/template-group-admins", `{"group_id":"eng","person":"gina"}`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.do()
			var apiErr *client.APIError
			if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotImplemented || apiErr.Reason != "templates_unavailable" {
				t.Fatalf("error = %v, want the server's 501 templates_unavailable", err)
			}
			if got.method != tc.want.method || got.path != tc.want.path {
				t.Errorf("request = %s %s, want %s %s", got.method, got.path, tc.want.method, tc.want.path)
			}
			if tc.want.body != "" && !strings.Contains(strings.TrimSpace(got.body), tc.want.body) {
				t.Errorf("body = %s, want it to hold %s", got.body, tc.want.body)
			}
		})
	}
}

// TestTemplateIntentBytesAreCanonical: key order and spacing in the source never
// change what is stored, so a revision hash or a diff is stable.
func TestTemplateIntentBytesAreCanonical(t *testing.T) {
	var a, b client.TemplateIntent
	if err := json.Unmarshal([]byte(`{"inline_policy":{"b":[1, 2],"a":"<x>"},"agent":"x"}`), &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte("{\"agent\":\"x\",\"inline_policy\":{\"a\":\"<x>\",\"b\":[1,2]}}"), &b); err != nil {
		t.Fatal(err)
	}
	if string(a.Raw("inline_policy")) != string(b.Raw("inline_policy")) || string(a.Raw("inline_policy")) != `{"a":"<x>","b":[1,2]}` {
		t.Errorf("a = %s, b = %s", a.Raw("inline_policy"), b.Raw("inline_policy"))
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
	// pool_id was an invented working name: it is no exception to the strict read.
	var pool client.TemplateIntent
	if err := json.Unmarshal([]byte(`{"pool_id":"p1","agent":"claude"}`), &pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Request(); err == nil {
		t.Error("pool_id materialised: the only pool carrier is the request's own runner_pool_id")
	}
}
