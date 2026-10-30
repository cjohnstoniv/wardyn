// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"strings"
	"testing"
)

// TestAWSSigV4_RoleKeyIDMaskedInTheLog: once the proxy holds the role
// credentials, the access key id is masked in the decision log like the
// secret key and the session token, even when a request path carries it.
func TestAWSSigV4_RoleKeyIDMaskedInTheLog(t *testing.T) {
	h := newSigV4Harness(t)
	h.mustReSign(t, h.send(t, sandboxRequest(t, "GET", sigv4S3, "/bucket/key", "", nil, "", "s3", "us-east-1", ""), sigv4S3), "s3")
	if rec := h.send(t, sandboxRequest(t, "GET", sigv4S3, "/bucket/"+sigv4RoleKey, "", nil, "", "s3", "us-east-1", ""), sigv4S3); rec.Code != 200 {
		t.Fatalf("status %d body %s, want 200", rec.Code, rec.Body)
	}
	log := h.log()
	if !strings.Contains(log, `"`+ruleSourceAWSSigV4+`"`) {
		t.Fatalf("no %s allow row: %s", ruleSourceAWSSigV4, log)
	}
	if strings.Contains(log, sigv4RoleKey) {
		t.Fatalf("the decision log carries the role access key id: %s", log)
	}
}
