// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A destination that echoes the request's headers hands the injected value
// back. With ScrubResponses the sandbox gets the placeholder in the body and in
// a header, across a body written in pieces; without it, the bytes as today.
func TestScrubResponsesReplacesAnEchoedInjection(t *testing.T) {
	value := "Bearer scrub-" + uuid.NewString()
	registerHeaderCredentialFor(uuid.Nil, value)
	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("Authorization")
		w.Header().Set("X-Echo", got)
		w.Header().Set("Content-Length", strconv.Itoa(len(got)))
		for i := 0; i < len(got); i += 7 { // split across writes, and so across the masker's window
			_, _ = w.Write([]byte(got[i:min(i+7, len(got))]))
			w.(http.Flusher).Flush()
		}
	}))
	t.Cleanup(echo.Close)
	spec := types.RunPolicySpec{AllowedDomains: []string{"api.test"}}
	for _, scrub := range []bool{false, true} {
		p, _ := newTestProxy(t, spec, upstreamAddr(echo), nil, staticInj(map[string]injectedHeader{
			"api.test": {name: "Authorization", value: value},
		}))
		p.scrubResponses = scrub
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, mustProxyReq(t, http.MethodGet, "http://api.test/v1"))
		body, hdr := rec.Body.String(), rec.Header().Get("X-Echo")
		leaked := strings.Contains(body, "scrub-") || strings.Contains(hdr, "scrub-")
		if scrub && (leaked || !strings.Contains(body, "<secret-hidden>") || rec.Header().Get("Content-Length") != "") {
			t.Errorf("scrubbed: body %q, X-Echo %q, Content-Length %q; want the placeholder and no length", body, hdr, rec.Header().Get("Content-Length"))
		}
		if !scrub && (body != value || hdr != value) {
			t.Errorf("unscrubbed: body %q, X-Echo %q, want the echo verbatim", body, hdr)
		}
	}
}
