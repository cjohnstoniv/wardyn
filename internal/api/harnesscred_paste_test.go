// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// `aws` is a captureViaHelper row with no tokenPrefix, so the paste door's only
// guard let {"token":…} straight through to Secrets.Put on the RESERVED SSO
// secret: the structured blob wardyn-aws-sso captured was overwritten, Bedrock
// fell through to the ~/.aws mount or static keys with nothing refusing
// anywhere, and the arbitrary pasted string joined the process-global mask
// corpus for the life of the daemon.
func TestHarnessPaste_RefusedForAHelperCapturedProvider(t *testing.T) {
	secrets := &memSecrets{m: map[string][]byte{}}
	_, srv := harnessCredSrv(t, secrets)

	// A captured SSO session already in the store, exactly as the helper wrote it.
	blob, err := json.Marshal(awsSSOBlob{AccessToken: "sso-access-token-1234567890", Region: "us-east-1"})
	if err != nil {
		t.Fatalf("marshal blob: %v", err)
	}
	name := harnessCredSecretName(awsSSOProvider)
	if perr := secrets.Put(context.Background(), name, blob); perr != nil {
		t.Fatalf("seed captured blob: %v", perr)
	}

	w := do(t, srv, http.MethodPut, "/api/v1/setup/harness-credential/aws", adminToken,
		`{"token":"anything-at-all"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "containerized login") {
		t.Errorf("body = %s, want the refusal to name the containerized login the operator should use instead", w.Body.String())
	}
	got, gerr := secrets.Get(context.Background(), name)
	if gerr != nil {
		t.Fatalf("read back the captured blob: %v", gerr)
	}
	if string(got) != string(blob) {
		t.Errorf("stored blob = %s, want the captured session intact", got)
	}
}

// TestHarnessPaste_EmptyAndOverlongTokens is the shape half: neither may
// reach the store or MaskRegistry.AddGlobal.
func TestHarnessPaste_EmptyAndOverlongTokens(t *testing.T) {
	for name, body := range map[string]string{
		"empty":     `{"token":"   "}`,
		"over-long": `{"token":"sk-ant-oat01-` + strings.Repeat("x", maxHarnessPasteTokenLen) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			secrets := &memSecrets{m: map[string][]byte{}}
			_, srv := harnessCredSrv(t, secrets)
			w := do(t, srv, http.MethodPut, "/api/v1/setup/harness-credential/anthropic", adminToken, body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("code = %d, want 400; body=%s", w.Code, w.Body.String())
			}
			if _, err := secrets.Get(context.Background(), harnessCredSecretName("anthropic")); err == nil {
				t.Error("a refused paste still wrote the credential")
			}
		})
	}
}

// TestHarnessPaste_AnthropicUnchanged is the negative control: the provider
// the paste door exists for is untouched.
func TestHarnessPaste_AnthropicUnchanged(t *testing.T) {
	secrets := &memSecrets{m: map[string][]byte{}}
	_, srv := harnessCredSrv(t, secrets)
	w := do(t, srv, http.MethodPut, "/api/v1/setup/harness-credential/anthropic", adminToken,
		`{"token":"sk-ant-oat01-real-looking-token"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if _, err := secrets.Get(context.Background(), harnessCredSecretName("anthropic")); err != nil {
		t.Errorf("the pasted anthropic credential was not stored: %v", err)
	}
}
