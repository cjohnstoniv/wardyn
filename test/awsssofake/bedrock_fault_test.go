// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package awsssofake

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func postModel(t *testing.T, s *Server, fault string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, s.URL()+"/model/us.anthropic.claude-haiku-4-5/converse", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if fault != "" {
		req.Header.Set(bedrockFaultHeader, fault)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

// The fault modes answer in bedrock-runtime's own REST-JSON error shape: the
// class on x-amzn-ErrorType (with AWS's namespace suffix), {"message": ...} in
// the body — and they never count as an answered model call.
func TestBedrockDataPlaneFault_FakeShapes(t *testing.T) {
	s := New()
	defer s.Close()

	resp, body := postModel(t, s, "deny")
	if resp.StatusCode != http.StatusForbidden ||
		!strings.HasPrefix(resp.Header.Get("x-amzn-ErrorType"), "AccessDeniedException:") ||
		!strings.Contains(body, "explicit deny in a service control policy") {
		t.Fatalf("deny = %d %q %s", resp.StatusCode, resp.Header.Get("x-amzn-ErrorType"), body)
	}

	for i := 0; i < 2; i++ {
		resp, body = postModel(t, s, "throttle:2")
		if resp.StatusCode != http.StatusTooManyRequests ||
			!strings.HasPrefix(resp.Header.Get("x-amzn-ErrorType"), "ThrottlingException:") ||
			body != bedrockThrottleBody {
			t.Fatalf("throttle #%d = %d %q %s", i+1, resp.StatusCode, resp.Header.Get("x-amzn-ErrorType"), body)
		}
	}
	if s.BedrockCalls() != 0 {
		t.Fatalf("a refused call counted as answered: BedrockCalls = %d", s.BedrockCalls())
	}
	// The third call through a throttle:2 fault is served.
	resp, body = postModel(t, s, "throttle:2")
	if resp.StatusCode != http.StatusOK || body != bedrockStubBody || s.BedrockCalls() != 1 {
		t.Fatalf("after the throttle budget = %d %s (calls %d), want the stub's 200", resp.StatusCode, body, s.BedrockCalls())
	}
}
