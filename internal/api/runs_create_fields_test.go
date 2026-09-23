// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestCreateRun_TextFieldsAreCappedAndControlCharFree.
//
// title and description are rune-capped AND control-char-checked, and repo,
// devcontainer_repo, task and agent are capped too — the 1 MiB body limit is
// not a field bound. A NUL in a title would reach Postgres, which rejects it,
// so the caller would get a 500 instead of a 400 naming the field; a 1 MiB
// repo would land in the run row, in every list payload and in the
// hash-chained audit row.
func TestCreateRun_TextFieldsAreCappedAndControlCharFree(t *testing.T) {
	for name, tc := range map[string]struct {
		body  string
		field string
	}{
		"a NUL in the title": {
			body:  `{"agent":"claude-code","title":"bad` + "\\u0000" + `title"}`,
			field: "title",
		},
		"an over-long repo": {
			body:  `{"agent":"claude-code","repo":"` + strings.Repeat("r", maxRunRepoLen+1) + `"}`,
			field: "repo",
		},
		"an over-long task": {
			body:  `{"agent":"claude-code","task":"` + strings.Repeat("t", maxRunTaskLen+1) + `"}`,
			field: "task",
		},
		"an over-long agent": {
			body:  `{"agent":"` + strings.Repeat("a", maxRunAgentLen+1) + `"}`,
			field: "agent",
		},
		"a newline forged into the repo locator": {
			body:  `{"agent":"claude-code","repo":"org/app\nother/app"}`,
			field: "repo",
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", strings.NewReader(tc.body))
			w := httptest.NewRecorder()
			if _, _, _, _, ok := h.srv.decodeAndValidateCreateRun(w, r); ok {
				t.Fatalf("%s was accepted", name)
			}
			if w.Code != http.StatusBadRequest {
				t.Fatalf("code = %d, want 400: %s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tc.field) {
				t.Errorf("body = %s, want the field %q named so the caller knows what to fix", w.Body.String(), tc.field)
			}
		})
	}
}

// TestCreateRun_MultilineTaskStillAccepted is B1-F6's negative control: a task
// and a description are prose a human pasted, so newlines and tabs are content
// there, not a forgery attempt.
func TestCreateRun_MultilineTaskStillAccepted(t *testing.T) {
	h := newHarness(t)
	body := `{"agent":"claude-code","task":"do this\nthen this\n\tindented","description":"line 1\nline 2"}`
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", strings.NewReader(body))
	w := httptest.NewRecorder()
	if _, _, _, _, ok := h.srv.decodeAndValidateCreateRun(w, r); !ok {
		t.Fatalf("a multi-line task was rejected (code %d): %s", w.Code, w.Body.String())
	}
}

// TestInlineSecretRefs_NamesTheGrantKind is B1-F7. `needed` was filled from all
// three arms (api_key, git_pat, ssh_key) but every refusal said "api_key", so a
// git_pat or ssh_key grant naming a missing secret sent the author looking at
// the wrong grant. Reachable from four doors.
func TestInlineSecretRefs_NamesTheGrantKind(t *testing.T) {
	h := newHarness(t)
	cfg := baseTestConfig(h, nil)
	// One known name, so a spec naming any other reaches the "unknown secret" arm.
	cfg.Secrets = &memSecrets{m: map[string][]byte{"known": []byte("v")}}
	srv := New(cfg)

	for _, tc := range []struct {
		kind  types.GrantKind
		grant types.GrantSpec
		want  string
	}{
		{types.GrantGitPAT, gitPATGrant("dev.azure.com", "missing-pat"), "git_pat"},
		{types.GrantSSHKey, types.GrantSpec{Kind: types.GrantSSHKey, Scope: mustJSON(map[string]any{
			"host": "github.com", "key_secret_ref": "missing-key",
		})}, "ssh_key"},
		{types.GrantAPIKey, apiKeyGrant("api.anthropic.com", "missing-key"), "api_key"},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			code, err := srv.validateInlineSecretRefs(context.Background(), "",
				types.RunPolicySpec{EligibleGrants: []types.GrantSpec{tc.grant}})
			if err == nil {
				t.Fatalf("code = %d, want a refusal for a missing secret", code)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("message = %q, want it to name the %s grant that actually references the secret", err, tc.want)
			}
		})
	}
}

// TestInlineSecretRefs_NoStoreNamesTheGrantKind is the second message on the
// same path: "an api_key grant requires a secret store" was equally wrong for a
// git_pat-only spec.
func TestInlineSecretRefs_NoStoreNamesTheGrantKind(t *testing.T) {
	h := newHarness(t)
	srv := New(baseTestConfig(h, nil)) // no Secrets

	_, err := srv.validateInlineSecretRefs(context.Background(), "",
		types.RunPolicySpec{EligibleGrants: []types.GrantSpec{gitPATGrant("dev.azure.com", "ado-pat")}})
	if err == nil {
		t.Fatal("a grant needing a secret store was accepted with no store configured")
	}
	if want := fmt.Sprintf(inlineSecretStoreMissingRefusal, types.GrantGitPAT); err.Error() != want {
		t.Errorf("message = %q, want %q — the sentence must name the grant kind that needs the store", err, want)
	}
}
