// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func supportArgs(srvURL string, args ...string) []string {
	return append([]string{"run", "support"}, append(args, "--url", srvURL, "--token", "t")...)
}

// TestRunSupportRequestAcceptDeclineEndSendTheRoutesAndBodies: each verb posts to its own route with the
// body the server reads, and request and accept name no field the other owns.
func TestRunSupportRequestAcceptDeclineEndSendTheRoutesAndBodies(t *testing.T) {
	run, grant := uuid.New(), uuid.New()
	base := "/api/v1/runs/" + run.String() + "/support"
	for _, tc := range []struct {
		name   string
		args   []string
		path   string
		method string
		body   map[string]any
	}{
		{"request", []string{"request", run.String(), "--doors", "terminal, commands", "--reason", "stuck build"}, base, http.MethodPost,
			map[string]any{"doors": []any{"terminal", "commands"}, "reason": "stuck build"}},
		{"accept", []string{"accept", run.String(), grant.String(), "--doors", "terminal", "--minutes", "30"}, base + "/" + grant.String() + "/accept", http.MethodPost,
			map[string]any{"doors": []any{"terminal"}, "minutes": float64(30)}},
		{"decline", []string{"decline", run.String(), grant.String()}, base + "/" + grant.String() + "/decline", http.MethodPost, nil},
		{"end", []string{"end", run.String(), grant.String()}, base + "/" + grant.String() + "/end", http.MethodPost, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newCmdServer(t, http.StatusOK, map[string]any{"grant": grant, "helper": "sam", "state": "requested", "doors_asked": []string{"terminal"}})
			if err := execCmd(t, supportArgs(srv.URL, tc.args...)...); err != nil {
				t.Fatal(err)
			}
			r := srv.last()
			if r.method != tc.method || r.path != tc.path {
				t.Fatalf("sent %s %s, want %s %s", r.method, r.path, tc.method, tc.path)
			}
			if tc.body == nil {
				if len(r.body) != 0 {
					t.Errorf("body = %s, want none", r.body)
				}
				return
			}
			var got map[string]any
			if err := json.Unmarshal(r.body, &got); err != nil {
				t.Fatal(err)
			}
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(tc.body)
			if string(gotJSON) != string(wantJSON) {
				t.Errorf("body = %s, want %s", gotJSON, wantJSON)
			}
		})
	}
}

// TestRunSupportRefusesBadInputBeforeAnyRequest: a bad door, minutes or reason is a usage error that names
// its flag, and nothing is sent.
func TestRunSupportRefusesBadInputBeforeAnyRequest(t *testing.T) {
	run, grant := uuid.New().String(), uuid.New().String()
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"no doors":        {[]string{"request", run, "--reason", "x"}, "--doors is required"},
		"unknown door":    {[]string{"request", run, "--doors", "shell", "--reason", "x"}, `"shell" is not a door`},
		"repeated door":   {[]string{"request", run, "--doors", "files,files", "--reason", "x"}, "named twice"},
		"no reason":       {[]string{"request", run, "--doors", "files"}, "--reason must be 1 to 500"},
		"reason too long": {[]string{"request", run, "--doors", "files", "--reason", strings.Repeat("é", 501)}, "--reason must be 1 to 500"},
		"bad minutes":     {[]string{"accept", run, grant, "--doors", "files", "--minutes", "45"}, "--minutes must be 15, 30 or 60"},
		"no minutes":      {[]string{"accept", run, grant, "--doors", "files"}, "--minutes must be 15, 30 or 60"},
		"accept no door":  {[]string{"accept", run, grant, "--minutes", "15"}, "--doors is required"},
		"bad run id":      {[]string{"end", "nope", grant}, "invalid run id"},
	} {
		t.Run(name, func(t *testing.T) {
			srv := newCmdServer(t, http.StatusOK, map[string]any{})
			err := execCmd(t, supportArgs(srv.URL, tc.args...)...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
			if len(srv.reqs) != 0 {
				t.Errorf("sent %d requests, want none", len(srv.reqs))
			}
		})
	}
	// 500 characters is the most, counted in characters and not bytes.
	srv := newCmdServer(t, http.StatusOK, map[string]any{"grant": uuid.New(), "state": "requested"})
	if err := execCmd(t, supportArgs(srv.URL, "request", run, "--doors", "files", "--reason", strings.Repeat("é", 500))...); err != nil {
		t.Errorf("a 500-character reason: %v", err)
	}
}

// TestRunSupportStatusShowsTheOwnerWhatTheyAnswer: for a requested grant, status prints the helper, their
// reason, the doors asked and the consent words with the run's recording answer filled in.
func TestRunSupportStatusShowsTheOwnerWhatTheyAnswer(t *testing.T) {
	run := uuid.New()
	srv := newCmdServer(t, http.StatusOK, map[string]any{
		"open": map[string]any{"grant": uuid.New(), "helper": "Sam Rivera", "reason": "build stalls", "state": "requested",
			"doors_asked": []string{"terminal", "desktop"}, "requested_at": time.Now().Add(-5 * time.Minute), "recorded_doors": []string{"terminal"}},
		"ended": nil, "doors_offered": []string{"terminal"}, "recorded_doors": []string{"terminal"},
	})
	out, err := execCmdCapture(t, supportArgs(srv.URL, "status", run.String())...)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Sam Rivera", "build stalls", "terminal, desktop", "Terminal: recorded on this run.", "No credential is paused: the helper can use every connection this run has, as you.",
		"Terminal and Commands each run anything the agent can", "Desktop: not recorded on this run."} {
		if !strings.Contains(out, want) {
			t.Errorf("status output lacks %q:\n%s", want, out)
		}
	}
}

// TestRunSupportStatusEscapesOtherPeoplesText: the helper's name and reason are text another person wrote, so
// status prints them through termLine: a line break cannot forge a row and an escape sequence cannot reach the
// terminal.
func TestRunSupportStatusEscapesOtherPeoplesText(t *testing.T) {
	run := uuid.New()
	srv := newCmdServer(t, http.StatusOK, map[string]any{
		"open": map[string]any{"grant": uuid.New(), "helper": "sam\x1b]0;owned\x07", "reason": "stuck\n  doors:      desktop\x1b[2J",
			"state": "active", "doors_asked": []string{"terminal"}, "doors": []string{"terminal"}, "requested_at": time.Now().Add(-5 * time.Minute)},
	})
	out, err := execCmdCapture(t, supportArgs(srv.URL, "status", run.String())...)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(out, "\x1b\x07") {
		t.Errorf("status passed a control character through:\n%q", out)
	}
	if strings.Contains(out, "\n  doors:      desktop") {
		t.Errorf("the reason's line break forged a row:\n%s", out)
	}
	for _, want := range []string{`helper:     sam\x1b]0;owned\x07`, `reason:     stuck\n  doors:      desktop\x1b[2J`} {
		if !strings.Contains(out, want) {
			t.Errorf("status output lacks %q:\n%s", want, out)
		}
	}
}
