// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/pkg/client"
)

func previewDoc(t *testing.T, raw string) client.WorkloadDocument {
	t.Helper()
	doc, err := client.ParseWorkload([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func changeLines(changes []client.WorkloadChange) string {
	var out []string
	for _, c := range changes {
		line := c.Op + " " + c.Path
		if c.From != nil {
			line += " from=" + string(c.From)
		}
		if c.To != nil {
			line += " to=" + string(c.To)
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func TestDiffWorkloadDocuments(t *testing.T) {
	const head = `{"api_version":"wardyn/v1","kind":"Workload","name":"web",`
	stored := previewDoc(t, head+`"description":"Web","run_in":"platform","restart_key":"1",`+
		`"intent":{"agent":"claude-code","task":"serve","inline_policy":{"allowed_domains":["a.example","b.example"]},"env":{"a/b":"1","c~d":"2"}}}`)

	cases := []struct {
		name     string
		proposed string
		want     string
	}{
		{"identical", head + `"description":"Web","run_in":"platform","restart_key":"1",` +
			`"intent":{"agent":"claude-code","task":"serve","inline_policy":{"allowed_domains":["a.example","b.example"]},"env":{"a/b":"1","c~d":"2"}}}`, ""},
		{"description only", head + `"description":"Web 2","run_in":"platform","restart_key":"1",` +
			`"intent":{"agent":"claude-code","task":"serve","inline_policy":{"allowed_domains":["a.example","b.example"]},"env":{"a/b":"1","c~d":"2"}}}`,
			`change /description from="Web" to="Web 2"`},
		{"header fields added and removed", head + `"description":"Web","intent":{"agent":"claude-code","task":"serve","inline_policy":{"allowed_domains":["a.example","b.example"]},"env":{"a/b":"1","c~d":"2"}},"restart_key":"2"}`,
			"change /restart_key from=\"1\" to=\"2\"\nremove /run_in from=\"platform\""},
		{"nested object recurses and an array is one change", head + `"description":"Web","run_in":"platform","restart_key":"1",` +
			`"intent":{"agent":"claude-code","task":"serve!","inline_policy":{"allowed_domains":["a.example"]},"env":{"a/b":"1","c~d":"2"}}}`,
			"change /intent/inline_policy/allowed_domains from=[\"a.example\",\"b.example\"] to=[\"a.example\"]\nchange /intent/task from=\"serve\" to=\"serve!\""},
		{"intent field added and removed", head + `"description":"Web","run_in":"platform","restart_key":"1",` +
			`"intent":{"agent":"claude-code","task":"serve","interactive":true,"inline_policy":{"allowed_domains":["a.example","b.example"]}}}`,
			"remove /intent/env from={\"a/b\":\"1\",\"c~d\":\"2\"}\nadd /intent/interactive to=true"},
		{"keys with a slash and a tilde", head + `"description":"Web","run_in":"platform","restart_key":"1",` +
			`"intent":{"agent":"claude-code","task":"serve","inline_policy":{"allowed_domains":["a.example","b.example"]},"env":{"a/b":"9","c~d":"2"}}}`,
			`change /intent/env/a~1b from="1" to="9"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := client.DiffWorkloadDocuments(&stored, previewDoc(t, c.proposed))
			if got == nil {
				t.Fatal("changes = nil, want a non-nil slice")
			}
			if lines := changeLines(got); lines != c.want {
				t.Fatalf("changes =\n%s\nwant\n%s", lines, c.want)
			}
			if raw, err := json.Marshal(got); err != nil || (len(got) == 0 && string(raw) != "[]") {
				t.Fatalf("changes marshal to %s (%v), want [] when empty", raw, err)
			}
		})
	}

	t.Run("nothing stored adds every field", func(t *testing.T) {
		proposed := previewDoc(t, head+`"description":"Web","intent":{"agent":"claude-code","task":"serve"}}`)
		want := "add /description to=\"Web\"\nadd /intent/agent to=\"claude-code\"\nadd /intent/task to=\"serve\""
		if got := changeLines(client.DiffWorkloadDocuments(nil, proposed)); got != want {
			t.Fatalf("changes =\n%s\nwant\n%s", got, want)
		}
	})

	t.Run("the same inputs give the same order", func(t *testing.T) {
		proposed := previewDoc(t, head+`"description":"x","run_in":"y","restart_key":"z","intent":{"agent":"codex","task":"q"}}`)
		first := changeLines(client.DiffWorkloadDocuments(&stored, proposed))
		for range 20 {
			if again := changeLines(client.DiffWorkloadDocuments(&stored, proposed)); again != first {
				t.Fatalf("order moved:\n%s\nvs\n%s", first, again)
			}
		}
	})
}
