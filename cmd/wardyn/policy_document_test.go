// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestPolicyToJSON_RejectsAdditionalDocuments(t *testing.T) {
	for _, extra := range []string{
		"---\nfirst_use_approval: always_deny\n",
		"---\n",
		"---\ninvalid: [unterminated\n",
	} {
		t.Run(extra, func(t *testing.T) {
			if out, err := policyToJSON([]byte("allowed_domains: [example.com]\n" + extra)); err == nil {
				t.Fatalf("silently accepted only the first policy document: %s", out)
			}
		})
	}
}

func TestPolicyCommands_RejectAdditionalDocumentBeforeRequest(t *testing.T) {
	file := filepath.Join(t.TempDir(), "policy.yaml")
	writeFile(t, file, "allowed_domains: [example.com]\n---\nfirst_use_approval: always_deny\n")
	for _, args := range [][]string{
		{"policy", "create", "-f", file, "--name", "test"},
		{"policy", "update", uuid.NewString(), "-f", file, "--name", "test"},
		{"policy", "render", "-f", file},
		{"run", "--agent", "claude-code", "--policy-file", file},
	} {
		t.Run(strings.Join(args[:2], "_"), func(t *testing.T) {
			// An unusable transport makes an accidental API attempt fail with
			// a different error; no real server or external request is needed.
			args = append(args, "--url", ":invalid", "--token", "test")
			err := execCmd(t, args...)
			if err == nil || !strings.Contains(err.Error(), "one document") {
				t.Fatalf("want a local document-count error, got %v", err)
			}
		})
	}
}

func TestPolicyToJSON_SingleDocumentBoundaries(t *testing.T) {
	for _, raw := range []string{
		"allowed_domains: [example.com]\n",
		"---\nallowed_domains: [example.com]\n...\n# trailing comment\n",
		"{\"allowed_domains\":[\"example.com\"]}\n",
	} {
		got, err := policyToJSON([]byte(raw))
		if err != nil || string(got) != `{"allowed_domains":["example.com"]}` {
			t.Errorf("single-document conversion = %s, %v", got, err)
		}
	}
}
