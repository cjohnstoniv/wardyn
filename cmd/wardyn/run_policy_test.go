// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// policyViewServer answers the three reads `run policy` makes.
func policyViewServer(t *testing.T, view sdk.RunPolicyView, createdBy, principal string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/policy"):
			_ = json.NewEncoder(w).Encode(view)
		case r.URL.Path == "/api/v1/me":
			_ = json.NewEncoder(w).Encode(map[string]string{"principal": principal})
		default:
			_ = json.NewEncoder(w).Encode(types.AgentRun{ID: view.RunID, CreatedBy: createdBy})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func execRunPolicy(t *testing.T, srv *httptest.Server, args ...string) (stdout string, err error) {
	t.Helper()
	cmd := runPolicyCmd(func() *sdk.Client { return &sdk.Client{BaseURL: srv.URL} })
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SilenceUsage = true // the root sets it once flags parse (main.go)
	cmd.SetArgs(args)
	err = cmd.Execute()
	return out.String(), err
}

func recordedView() sdk.RunPolicyView {
	id := uuid.New()
	at := time.Now().UTC()
	return sdk.RunPolicyView{
		RunID: uuid.New(), State: sdk.RunPolicyViewRecorded, Complete: true,
		Source: sdk.RunPolicySource{Kind: "stored", PolicyID: &id, Name: "ci"},
		Spec: &types.RunPolicySpec{
			AllowedDomains:      []string{"api.anthropic.com", "registry.npmjs.org"},
			DeniedDomains:       []string{"corp.example"},
			MinConfinementClass: types.CC2,
			FirstUseApproval:    types.FirstUseAlwaysDeny,
			WorkspaceMounts:     []types.WorkspaceMount{{Source: "<redacted>", Target: "/home/agent/work/shared"}},
			EligibleGrants: []types.GrantSpec{{
				Kind: types.GrantAPIKey, Scope: json.RawMessage(`{"header":"Authorization","host":"api.anthropic.com"}`), TTLSeconds: 300,
			}},
		},
		Changes: []sdk.RunPolicyChange{
			{Cause: "workspace", Field: "allowed_domains", Added: []string{"registry.npmjs.org"}},
			{Cause: "profile", Field: "denied_domains", Added: []string{"corp.example"}, Profile: "walled"},
			{Cause: "restart", Field: "denied_domains", Added: []string{"paste.example"}, At: &at},
		},
	}
}

func TestRunPolicy_YAMLRoundTripsAsAPolicy(t *testing.T) {
	view := recordedView()
	view.Spec.LLMInspection = &types.LLMInspectionSpec{Mode: "alert", DetectSecrets: true, WorkspaceSecretNames: []string{"prod-db"}}
	view.Spec.Resources = &types.ResourceLimits{DiskMiB: 4096}
	out, err := execRunPolicy(t, policyViewServer(t, view, "alice", "alice"), view.RunID.String())
	if err != nil {
		t.Fatal(err)
	}
	asJSON, err := policyToJSON([]byte(out))
	if err != nil {
		t.Fatalf("the output is not a YAML document: %v\n%s", err, out)
	}
	got, err := decodeSpecStrict(asJSON)
	if err != nil {
		t.Fatalf("the output does not strict-decode as a policy: %v\n%s", err, out)
	}
	want, _ := json.Marshal(view.Spec)
	gotJSON, _ := json.Marshal(got)
	if !bytes.Equal(want, gotJSON) {
		t.Errorf("round trip changed the policy:\n want %s\n got  %s\nyaml:\n%s", want, gotJSON, out)
	}
	if i := strings.Index(out, "allowed_domains:"); i < 0 || strings.Index(out, "min_confinement_class:") < i {
		t.Errorf("the spec's key order was lost:\n%s", out)
	}
}

func TestRunPolicy_HeaderLines(t *testing.T) {
	view := recordedView()
	restartedOn := view.Changes[2].At.Format("Jan 2, 2006")
	view.Redacted = true
	view.Changes = append(view.Changes, sdk.RunPolicyChange{
		Cause: "limits", Field: "allowed_domains", Removed: []string{"pastebin.com"}, Detail: []string{"Removed pastebin.com."},
	})
	t.Run("the owner", func(t *testing.T) {
		out, err := execRunPolicy(t, policyViewServer(t, view, "alice", "alice"), view.RunID.String())
		if err != nil {
			t.Fatal(err)
		}
		head, _, _ := strings.Cut(out, "allowed_domains:")
		for _, want := range []string{
			`# Started from the saved policy "ci".`,
			"# Changed when the run started\n",
			"# Added for the workspace: registry.npmjs.org\n",
			"# Limited by the walled governance profile: corp.example\n",
			"# Blocked when the run was restarted on " + restartedOn + ": paste.example\n",
			"# Narrowed to fit your limits: pastebin.com\n",
			"# Values shown as <redacted> are hidden from you. Fill them in before using this as a policy.\n",
		} {
			if !strings.Contains(head, want) {
				t.Errorf("header lacks %q:\n%s", want, head)
			}
		}
		if strings.Contains(head, "started before Wardyn recorded") {
			t.Errorf("the older-run line appeared for a complete run:\n%s", head)
		}
		for _, line := range strings.Split(strings.TrimRight(head, "\n"), "\n") {
			if !strings.HasPrefix(line, "#") {
				t.Errorf("header line %q is not a YAML comment", line)
			}
		}
	})
	t.Run("someone else reading it names the person", func(t *testing.T) {
		out, err := execRunPolicy(t, policyViewServer(t, view, "dana@acme.example", "admin"), view.RunID.String())
		if err != nil || !strings.Contains(out, "# Narrowed to fit the limits set for dana@acme.example: pastebin.com\n") {
			t.Errorf("err %v, output:\n%s", err, out)
		}
	})
	t.Run("no hidden-values line for an admin, and an older run says so", func(t *testing.T) {
		v := recordedView()
		v.Complete = false
		out, _ := execRunPolicy(t, policyViewServer(t, v, "alice", "alice"), v.RunID.String())
		if strings.Contains(out, "<redacted> are hidden") || !strings.Contains(out, "# This run started before Wardyn recorded each change, so some changes may not be listed.\n") {
			t.Errorf("output:\n%s", out)
		}
	})
	t.Run("every source line", func(t *testing.T) {
		for src, want := range map[sdk.RunPolicySource]string{
			{Kind: "stored", Name: "ci", Deleted: true}:           `Started from the saved policy "ci", which has since been deleted.`,
			{Kind: "stored", Deleted: true}:                       "Started from a saved policy that has since been deleted.",
			{Kind: "inline"}:                                      "Started from a policy written for this run.",
			{Kind: "default"}:                                     "Started from your organization's default policy.",
			{Kind: "profile", Name: "walled"}:                     "Started from the walled governance profile.",
			{Kind: "profile"}:                                     "Started from your organization's default policy.",
			{Kind: "unknown"}:                                     "Wardyn set this policy for this run.",
			{Kind: "inline", Preset: "nightly", PresetVersion: 3}: "Started from a policy written for this run.\n# Launched from the preset \"nightly\", version 3.",
		} {
			if got := strings.Join(runPolicyHeaderLines(sdk.RunPolicyView{Source: src, Complete: true}, ""), "\n# "); got != want {
				t.Errorf("source %+v = %q, want %q", src, got, want)
			}
		}
	})
}

func TestRunPolicy_JSONPrintsTheWholeResponse(t *testing.T) {
	view := recordedView()
	out, err := execRunPolicy(t, policyViewServer(t, view, "alice", "alice"), view.RunID.String(), "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got sdk.RunPolicyView
	if err := json.Unmarshal([]byte(out), &got); err != nil || got.RunID != view.RunID || len(got.Changes) != 3 || got.Spec == nil {
		t.Errorf("--json output = %q (err %v), want the whole response", out, err)
	}
}

func TestRunPolicy_NoPolicyExitsNonZeroWithTheSentence(t *testing.T) {
	for state, want := range map[string]string{
		sdk.RunPolicyViewNotYet: "Wardyn records this run's policy when its sandbox is set up. This run hasn't reached that step.",
		sdk.RunPolicyViewNever:  "This run stopped before its sandbox was set up, so no policy was applied to it.",
	} {
		view := sdk.RunPolicyView{RunID: uuid.New(), State: state, Changes: []sdk.RunPolicyChange{}}
		out, err := execRunPolicy(t, policyViewServer(t, view, "alice", "alice"), view.RunID.String())
		if err == nil || err.Error() != want || out != "" {
			t.Errorf("%s: err %v, stdout %q, want the sentence as the error and nothing on stdout", state, err, out)
		}
		if exitCodeFor(err) != 1 {
			t.Errorf("%s: exit code %d, want 1", state, exitCodeFor(err))
		}
	}
}

func TestRunPolicy_HelpIsThePacketsWording(t *testing.T) {
	cmd := runPolicyCmd(func() *sdk.Client { return &sdk.Client{} })
	if cmd.Short != "Show the policy a run got when it started, as YAML" {
		t.Errorf("short = %q", cmd.Short)
	}
	if want := `Shows where a run's policy came from, what Wardyn changed when the run started, and the policy itself as YAML. If you're an admin, you can reuse the YAML as is with "wardyn run --policy-file". Anyone else sees hidden values as <redacted> and fills them in first.`; cmd.Long != want {
		t.Errorf("long = %q", cmd.Long)
	}
	if f := cmd.Flags().Lookup("json"); f == nil || f.Usage != "print the full response as JSON" {
		t.Errorf("--json flag = %+v", f)
	}
	if cmd.Flags().Lookup("output") != nil || cmd.Flags().ShorthandLookup("o") != nil {
		t.Error("an -o/--output flag exists; --json is the only output flag in this CLI")
	}
}

func TestRunPolicy_IsUnderRunAndRejectsABadID(t *testing.T) {
	if err := execCmd(t, "run", "policy", "not-a-uuid"); err == nil || !strings.Contains(err.Error(), "invalid run id") {
		t.Errorf("err = %v, want an invalid run id error", err)
	}
	cs := newCmdServer(t, http.StatusOK, recordedView())
	if err := execCmd(t, "--url", cs.URL, "run", "policy", uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if got := cs.last(); got.method != http.MethodGet || !strings.HasSuffix(got.path, "/policy") {
		t.Errorf("request = %s %s, want GET .../policy", got.method, got.path)
	}
}

func TestRunPolicy_LimitsLineNeverClaimsYourWhenTheRunCannotBeRead(t *testing.T) {
	view := recordedView()
	view.Changes = []sdk.RunPolicyChange{{Cause: "limits", Field: "allowed_domains", Removed: []string{"pastebin.com"}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/policy") {
			_ = json.NewEncoder(w).Encode(view)
			return
		}
		http.Error(w, `{"error":"boom"}`, http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	out, err := execRunPolicy(t, srv, view.RunID.String())
	if err == nil || out != "" {
		t.Errorf("err %v, stdout %q, want a failure and no policy printed", err, out)
	}
}
