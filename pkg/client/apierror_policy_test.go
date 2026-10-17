// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import "testing"

func TestNewAPIErrorParsesPolicy(t *testing.T) {
	body := `{"error":"no","reason":"governance_profile","policy":{"source":"profile","name":"walled","owner":"Platform team","email":"platform@example.com","request_url":"https://help.example.com/request","request_text":"File a ticket."}}`
	e := NewAPIError(403, []byte(body))
	want := PolicyRef{Source: "profile", Name: "walled", Owner: "Platform team", Email: "platform@example.com",
		RequestURL: "https://help.example.com/request", RequestText: "File a ticket."}
	if e.Reason != "governance_profile" || e.Policy == nil || *e.Policy != want {
		t.Errorf("Reason = %q, Policy = %+v, want %+v", e.Reason, e.Policy, want)
	}
	if got := e.Error(); got != "API error 403: no" {
		t.Errorf("Error() = %q, want the sentence alone: the policy is for the caller to project", got)
	}
	for _, raw := range []string{`{"error":"no","reason":"x"}`, `not json`, `{"policy":null}`} {
		if e := NewAPIError(403, []byte(raw)); e.Policy != nil {
			t.Errorf("%s: Policy = %+v, want nil", raw, e.Policy)
		}
	}
}
