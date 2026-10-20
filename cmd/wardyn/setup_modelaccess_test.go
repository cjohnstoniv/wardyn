// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// `wardyn setup` has no model-access subcommand, so an unmet model-access row must name the
// console path, never "below" or a bare button label.
func TestSetupStatusNamesTheConsoleForUnmetModelAccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ready":true,"tier":"org","checks":[` +
			`{"id":"llm_provider","label":"LLM access","status":"warn","detail":"d","fix":"Connect your own credential on a provider's row below (Settings → Model providers)."},` +
			`{"id":"llm_provider:bedrock-prod","label":"LLM access: Bedrock","status":"warn","detail":"d","fix":"Sign in to AWS"},` +
			`{"id":"runner","label":"Sandbox runner","status":"info","detail":"d","fix":"Run ` + "`wardyn setup wall`" + `."}]}`))
	}))
	defer srv.Close()
	t.Setenv("WARDYN_URL", srv.URL)
	t.Setenv("WARDYN_TOKEN", "tok")
	root := rootCmd()
	out := &strings.Builder{}
	root.SetArgs([]string{"setup", "status"})
	root.SetOut(out)
	root.SetErr(&strings.Builder{})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	console := "use the console: Your account → Your model connections (" + srv.URL + "/account)"
	for _, want := range []string{
		"→ " + console + "\n",
		"→ " + console + " — Sign in to AWS\n",
		"→ Run `wardyn setup wall`.\n", // a non-model row keeps the server's text
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "row below") {
		t.Errorf("the server's \"below\" wording leaked into the CLI:\n%s", out.String())
	}
}
