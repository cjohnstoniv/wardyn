// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/pkg/client"
)

const workloadNative = `{"api_version":"wardyn/v1","kind":"Workload","name":"triage-bot","description":"Triage","run_in":"platform","restart_key":"1",` +
	`"intent":{"experience":"background","inline_policy":{"allowed_domains":["a.example"]},"workload":{"command":"./serve","kind":"command"}}}`

const workloadCR = `{"apiVersion":"wardyn.dev/v1alpha1","kind":"WardynWorkload",` +
	`"metadata":{"name":"triage-bot","namespace":"team-a","uid":"x","labels":{"a":"b"},"creationTimestamp":"0001-01-01T00:00:00Z"},` +
	`"spec":{"description":"Triage","run_in":"platform","restart_key":"1",` +
	`"intent":{"experience":"background","inline_policy":{"allowed_domains":["a.example"]},"workload":{"command":"./serve","kind":"command"}}},` +
	`"status":{"phase":"Running","runID":"5f0c"}}`

func TestWorkloadDocument_BothHeadersRoundTrip(t *testing.T) {
	native, err := client.ParseWorkload([]byte(workloadNative))
	if err != nil {
		t.Fatal(err)
	}
	cr, err := client.ParseWorkload([]byte(workloadCR))
	if err != nil {
		t.Fatal(err)
	}
	if cr.Namespace != "team-a" || native.Namespace != "" {
		t.Fatalf("namespaces %q, %q", native.Namespace, cr.Namespace)
	}
	// The two headers carry one workload: the same request, the same name.
	a, _ := json.Marshal(native.Request())
	b, _ := json.Marshal(cr.Request())
	if string(a) != string(b) || native.Name != cr.Name {
		t.Fatalf("native %s / cr %s", a, b)
	}
	// native -> CR -> native
	crBytes, err := json.Marshal(native.CR("team-a"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(crBytes), `"apiVersion":"wardyn.dev/v1alpha1"`) || !strings.Contains(string(crBytes), `"namespace":"team-a"`) {
		t.Fatalf("cr = %s", crBytes)
	}
	back, err := client.ParseWorkload(crBytes)
	if err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(back)
	if err != nil || string(out) != workloadNative {
		t.Fatalf("round trip:\n got %s, %v\nwant %s", out, err, workloadNative)
	}
}

func TestParseWorkload_Refuses(t *testing.T) {
	big := `{"api_version":"wardyn/v1","kind":"Workload","name":"w","intent":{"task":"` + strings.Repeat("a", client.MaxWorkloadSpecBytes) + `"}}`
	for name, doc := range map[string]string{
		"not json":          `name: x`,
		"wrong kind":        `{"api_version":"wardyn/v1","kind":"Run","name":"w","intent":{"task":"t"}}`,
		"wrong cr kind":     `{"apiVersion":"wardyn.dev/v1alpha1","kind":"Workload","metadata":{"name":"w"},"spec":{"intent":{"task":"t"}}}`,
		"wrong cr version":  `{"apiVersion":"wardyn.dev/v1","kind":"WardynWorkload","metadata":{"name":"w"},"spec":{"intent":{"task":"t"}}}`,
		"unknown field":     `{"api_version":"wardyn/v1","kind":"Workload","name":"w","intent":{"task":"t"},"owner":"x"}`,
		"unknown spec key":  `{"apiVersion":"wardyn.dev/v1alpha1","kind":"WardynWorkload","metadata":{"name":"w"},"spec":{"intent":{"task":"t"},"owner":"x"}}`,
		"uppercase name":    `{"api_version":"wardyn/v1","kind":"Workload","name":"Triage","intent":{"task":"t"}}`,
		"digit-led name":    `{"api_version":"wardyn/v1","kind":"Workload","name":"1w","intent":{"task":"t"}}`,
		"long name":         `{"api_version":"wardyn/v1","kind":"Workload","name":"a` + strings.Repeat("b", 63) + `","intent":{"task":"t"}}`,
		"hyphen-led name":   `{"api_version":"wardyn/v1","kind":"Workload","name":"-w","intent":{"task":"t"}}`,
		"hyphen-ended name": `{"api_version":"wardyn/v1","kind":"Workload","name":"w-","intent":{"task":"t"}}`,
		"long description":  `{"api_version":"wardyn/v1","kind":"Workload","name":"w","description":"` + strings.Repeat("d", 201) + `","intent":{"task":"t"}}`,
		"control in desc":   `{"api_version":"wardyn/v1","kind":"Workload","name":"w","description":"a\nb","intent":{"task":"t"}}`,
		"long restart key":  `{"api_version":"wardyn/v1","kind":"Workload","name":"w","restart_key":"` + strings.Repeat("k", 65) + `","intent":{"task":"t"}}`,
		"no intent":         `{"api_version":"wardyn/v1","kind":"Workload","name":"w"}`,
		"spec over 256 KiB": big,
	} {
		if _, err := client.ParseWorkload([]byte(doc)); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}

// The name grammar's boundaries, as the runner wire holds them (internal/runnerwire): one character,
// an inner hyphen and 63 characters parse.
func TestParseWorkload_NameBoundaries(t *testing.T) {
	for _, name := range []string{"w", "w-1", "a-b-c", "a" + strings.Repeat("b", 62)} {
		if _, err := client.ParseWorkload([]byte(`{"api_version":"wardyn/v1","kind":"Workload","name":"` + name + `","intent":{"task":"t"}}`)); err != nil {
			t.Errorf("%q: %v", name, err)
		}
	}
}

func TestRunDocumentRequest_MovesPolicyToOverlay(t *testing.T) {
	var intent client.TemplateIntent
	if err := json.Unmarshal([]byte(`{"experience":"background","task":"serve","inline_policy":{"allowed_domains":["a.example"]}}`), &intent); err != nil {
		t.Fatal(err)
	}
	req, err := client.RunDocumentRequest(intent)
	if err != nil {
		t.Fatal(err)
	}
	if string(req.PolicyOverlay) != `{"allowed_domains":["a.example"]}` || req.InlinePolicy != nil || req.Experience != "background" || req.ModeContract != client.RunModeContractV09 {
		t.Fatalf("req = %+v", req)
	}
	if !intent.Has("inline_policy") {
		t.Fatal("the caller's intent lost inline_policy")
	}
	var plain client.TemplateIntent
	if err := json.Unmarshal([]byte(`{"task":"t"}`), &plain); err != nil {
		t.Fatal(err)
	}
	if req, err = client.RunDocumentRequest(plain); err != nil || req.ModeContract != "" || len(req.PolicyOverlay) != 0 {
		t.Fatalf("plain = %+v, %v", req, err)
	}
	var unknown client.TemplateIntent
	_ = json.Unmarshal([]byte(`{"nope":1}`), &unknown)
	if _, err := client.RunDocumentRequest(unknown); err == nil {
		t.Fatal("an unknown field was dropped")
	}
}

func TestWorkloadMethods_RequestShape(t *testing.T) {
	id := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	run := uuid.New()
	applied := `{"workload":{"name":"triage-bot","owner":"alice","source":"api","generation":2,"current_generation":2,"run_id":"` + run.String() + `",` +
		`"intent":{"task":"t"},"created_at":"0001-01-01T00:00:00Z","updated_at":"0001-01-01T00:00:00Z"},"outcome":"updated","launch":{"state":"launched","run_id":"` + run.String() + `"}}`
	doc, err := client.ParseWorkload([]byte(workloadNative))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name       string
		status     int
		answer     string
		call       func(c *client.Client) error
		wantMethod string
		wantPath   string
		wantQuery  string
		wantBody   string
	}{
		{"ApplyWorkload", 200, applied, func(c *client.Client) error {
			got, err := c.ApplyWorkload(context.Background(), doc.Name, doc.Request())
			if err == nil && (got.Outcome != client.WorkloadUpdated || got.Launch.State != client.WorkloadLaunched || got.Launch.RunID == nil || *got.Launch.RunID != run) {
				t.Errorf("ApplyWorkload = %+v", got)
			}
			return err
		}, "PUT", "/api/v1/workloads/triage-bot", "", `{"description":"Triage","run_in":"platform","restart_key":"1","intent":{"experience":"background","inline_policy":{"allowed_domains":["a.example"]},"workload":{"command":"./serve","kind":"command"}}}`},
		{"ListWorkloads", 200, `[{"name":"w","owner":"service:bot","source":"k8s","namespace":"team-a","generation":1,"current_generation":0,"last_refusal":{"generation":1,"reason":"x","at":"0001-01-01T00:00:00Z"}}]`, func(c *client.Client) error {
			got, err := c.ListWorkloads(context.Background(), "service:bot", client.ListOpts{Limit: 5})
			if err == nil && (len(got) != 1 || got[0].Source != client.WorkloadSourceK8s || got[0].LastRefusal == nil) {
				t.Errorf("ListWorkloads = %+v", got)
			}
			return err
		}, "GET", "/api/v1/workloads", "owner=service%3Abot&limit=5", ""},
		{"GetWorkload", 200, `{"name":"a/b","owner":"alice","source":"api"}`, func(c *client.Client) error {
			_, err := c.GetWorkload(context.Background(), "a/b", "")
			return err
		}, "GET", "/api/v1/workloads/a%2Fb", "", ""},
		{"WorkloadHistory", 200, `[{"action":"launch","generation":4,"run_id":"` + id.String() + `"}]`, func(c *client.Client) error {
			got, err := c.WorkloadHistory(context.Background(), "w", "alice", client.ListOpts{Limit: 5})
			if err == nil && (len(got) != 1 || got[0].Action != "launch" || got[0].Generation != 4 || got[0].RunID == nil || *got[0].RunID != id) {
				t.Errorf("WorkloadHistory = %+v", got)
			}
			return err
		}, "GET", "/api/v1/workloads/w/history", "owner=alice&limit=5", ""},
		{"DeleteWorkload", 204, ``, func(c *client.Client) error {
			return c.DeleteWorkload(context.Background(), "w", "alice")
		}, "DELETE", "/api/v1/workloads/w", "owner=alice", ""},
		{"RestartWorkload", 200, applied, func(c *client.Client) error {
			_, err := c.RestartWorkload(context.Background(), "w", "")
			return err
		}, "POST", "/api/v1/workloads/w/restart", "", ""},
		{"CreateWorkloadBinding", 201, `{"id":"` + id.String() + `","pool_id":"` + id.String() + `","namespace":"team-a","service":"bot","sponsor":"alice"}`, func(c *client.Client) error {
			got, err := c.CreateWorkloadBinding(context.Background(), client.CreateWorkloadBindingRequest{Pool: "prod", Namespace: "team-a", Service: "bot"})
			if err == nil && (got.ID != id || got.Sponsor != "alice") {
				t.Errorf("CreateWorkloadBinding = %+v", got)
			}
			return err
		}, "POST", "/api/v1/workload-bindings", "", `{"pool":"prod","namespace":"team-a","service":"bot"}`},
		{"ListWorkloadBindings", 200, `[]`, func(c *client.Client) error {
			_, err := c.ListWorkloadBindings(context.Background())
			return err
		}, "GET", "/api/v1/workload-bindings", "", ""},
		{"DeleteWorkloadBinding", 204, ``, func(c *client.Client) error {
			return c.DeleteWorkloadBinding(context.Background(), id)
		}, "DELETE", "/api/v1/workload-bindings/" + id.String(), "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotMethod, gotPath, gotQuery, gotBody string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotMethod, gotPath, gotQuery = r.Method, r.URL.EscapedPath(), r.URL.RawQuery
				b, _ := io.ReadAll(r.Body)
				gotBody = string(b)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.answer))
			}))
			defer srv.Close()
			if err := tc.call(client.New(srv.URL, "tok")); err != nil {
				t.Fatal(err)
			}
			if gotMethod != tc.wantMethod || gotPath != tc.wantPath || gotQuery != tc.wantQuery || gotBody != tc.wantBody {
				t.Fatalf("sent %s %s ?%s %s\nwant %s %s ?%s %s", gotMethod, gotPath, gotQuery, gotBody, tc.wantMethod, tc.wantPath, tc.wantQuery, tc.wantBody)
			}
		})
	}
}
