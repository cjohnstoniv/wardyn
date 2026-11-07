// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
)

// A call to a gateway goes out only with its body signed, so the gateway cannot change it: an
// unsigned payload mode, or a declared hash that is not the body's, is refused before anything
// is minted or sent.
func TestAWSSigV4Gateway_RefusesAnUnsignedOrWrongPayload(t *testing.T) {
	const body = `{"messages":[]}`
	other := sha256.Sum256([]byte("another body"))
	for name, declared := range map[string]string{
		"UNSIGNED-PAYLOAD":                   "UNSIGNED-PAYLOAD",
		"STREAMING-UNSIGNED-PAYLOAD-TRAILER": "STREAMING-UNSIGNED-PAYLOAD-TRAILER",
		"another body's hash":                hex.EncodeToString(other[:]),
	} {
		t.Run(name, func(t *testing.T) {
			g := newSGWHarness(t)
			req := sandboxRequest(t, http.MethodPost, sgwHost, sgwPrefix+"/model/anthropic.claude-x:0/converse", body,
				map[string]string{"Content-Type": "application/json", "X-Amz-Content-Sha256": declared}, "", "bedrock", "us-east-1", declared)
			rec := g.send(t, req, sgwHost)
			if rec.Code != http.StatusForbidden || rec.Header().Get("X-Wardyn-Reason") != awsSigV4Payload {
				t.Fatalf("gateway call with payload %q: status %d reason %q, want 403 %s", declared, rec.Code, rec.Header().Get("X-Wardyn-Reason"), awsSigV4Payload)
			}
			if want := "unsigned payload"; name != "another body's hash" && !strings.Contains(rec.Body.String(), want) {
				t.Fatalf("refusal %q does not say it declares an %s", rec.Body, want)
			}
			if n := len(g.arrivals()); n != 0 {
				t.Fatalf("the gateway saw %d requests whose body was not signed, want none", n)
			}
			if n := g.mints.Load(); n != 0 {
				t.Fatalf("%d mints for a refused request, want none", n)
			}
		})
	}
}

// The declared hash of the body itself still goes to the gateway, signed over that hash.
func TestAWSSigV4Gateway_DeclaredBodyHashSent(t *testing.T) {
	const body = `{"messages":[]}`
	sum := sha256.Sum256([]byte(body))
	declared := hex.EncodeToString(sum[:])
	g := newSGWHarness(t)
	rec := g.send(t, sandboxRequest(t, http.MethodPost, sgwHost, sgwPrefix+"/model/anthropic.claude-x:0/converse", body,
		map[string]string{"Content-Type": "application/json", "X-Amz-Content-Sha256": declared}, "", "bedrock", "us-east-1", declared), sgwHost)
	if rec.Code != http.StatusOK || len(g.arrivals()) != 1 {
		t.Fatalf("status %d arrivals %d, want 200 and one", rec.Code, len(g.arrivals()))
	}
	if got := g.arrivals()[0].header.Get("X-Amz-Content-Sha256"); got != declared {
		t.Fatalf("the gateway got x-amz-content-sha256 %q, want the body's hash", got)
	}
}

// A direct AWS host keeps taking both unsigned payload modes, as before.
func TestAWSSigV4_DirectRouteStillTakesUnsignedPayloads(t *testing.T) {
	for _, mode := range []string{"UNSIGNED-PAYLOAD", "STREAMING-UNSIGNED-PAYLOAD-TRAILER"} {
		t.Run(mode, func(t *testing.T) {
			h := newSigV4Harness(t)
			rec := h.send(t, sandboxRequest(t, http.MethodPost, sigv4Bedrock, "/model/anthropic.claude-x:0/converse", `{}`,
				map[string]string{"Content-Type": "application/json", "X-Amz-Content-Sha256": mode}, "", "bedrock", "us-east-1", mode), sigv4Bedrock)
			a := h.mustReSign(t, rec, "bedrock")
			if got := a.header.Get("X-Amz-Content-Sha256"); got != mode {
				t.Fatalf("AWS got x-amz-content-sha256 %q, want %q unchanged", got, mode)
			}
		})
	}
}
