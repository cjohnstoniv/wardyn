// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// registerRaw runs register against a server that keeps the request body as sent.
func registerRaw(t *testing.T, args ...string) (map[string]json.RawMessage, bool, error) {
	t.Helper()
	var org string
	var raw map[string]json.RawMessage
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		body, _ := io.ReadAll(r.Body)
		var sent types.RunnerRegisterRequest
		if err := json.Unmarshal(body, &sent); err != nil || json.Unmarshal(body, &raw) != nil {
			t.Error("undecodable request body")
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(types.RunnerRegistration{RunnerID: uuid.New(), Fingerprint: runnerwire.Fingerprint(sent.PublicKey),
			State: types.RunnerUnclaimed, OrgURLSHA256: federation.OrgURLSHA256(org), Limits: sent.Limits})
	}))
	defer srv.Close()
	org = srv.URL
	root := t.TempDir()
	tokenFile := filepath.Join(root, "token")
	if err := os.WriteFile(tokenFile, []byte("wdr_"+strings.Repeat("b", 64)), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := newRegisterCommand()
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs(append([]string{"--org", org, "--token-file", tokenFile, "--state-dir", filepath.Join(root, "identity")}, args...))
	err := cmd.Execute()
	return raw, called, err
}

// With no --label the request has no labels key at all, so it registers against an organisation that
// predates labels.
func TestRunnerRegister_NoLabelsSendsNoField(t *testing.T) {
	raw, called, err := registerRaw(t)
	if err != nil || !called {
		t.Fatalf("register: called=%v err=%v", called, err)
	}
	if _, has := raw["labels"]; has {
		t.Errorf("the request carries labels: %s", raw["labels"])
	}
}

func TestRunnerRegister_SendsLabelFlags(t *testing.T) {
	raw, _, err := registerRaw(t, "--label", "cluster=eu-1", "--label=region=west")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.Unmarshal(raw["labels"], &got); err != nil || !maps.Equal(got, map[string]string{"cluster": "eu-1", "region": "west"}) {
		t.Errorf("labels sent = %v, %v", got, err)
	}
}

// A malformed, repeated or out-of-syntax label fails before anything is sent, so the token is not spent.
func TestRunnerRegister_BadLabelRefusedBeforeSending(t *testing.T) {
	for _, tc := range []struct{ name, flag, want string }{
		{"no equals", "cluster", "want key=value"},
		{"repeated key", "cluster=a --label cluster=b", "each key once"},
		{"bad value", "cluster=https://x", "labels: label cluster"},
		{"bad key", "-x=v", "labels: label key"},
	} {
		args := []string{}
		for _, f := range strings.Split(tc.flag, " --label ") {
			args = append(args, "--label", f)
		}
		_, called, err := registerRaw(t, args...)
		if err == nil || called || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: called=%v err=%v, want a refusal containing %q before any request", tc.name, called, err, tc.want)
		}
	}
}
