// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

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

// flushTap is a response writer that reports what the client has been sent at each flush.
type flushTap struct {
	*httptest.ResponseRecorder
	flushed chan string
}

func (f *flushTap) Flush() {
	f.ResponseRecorder.Flush()
	f.flushed <- f.Body.String()
}

// With scrubbing on and a registered value longer than one streamed frame, each frame reaches the
// client at its flush. Only an end that could begin the value waits, and the value is still
// masked when it arrives split across frames.
func TestScrubResponsesStreamsFramesPastALongSecret(t *testing.T) {
	key := uuid.New()
	secret := "long-session-" + strings.Repeat("t0k3n", 100)
	procMaskFor(key, []byte(secret))
	t.Cleanup(func() { procRegistry.Evict(key) })
	p := &Proxy{scrubResponses: true, maskKey: key}
	body, upstream := io.Pipe()
	w := &flushTap{ResponseRecorder: httptest.NewRecorder(), flushed: make(chan string, 8)}
	done := make(chan struct{})
	go func() {
		p.relayStreaming(w, &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: body})
		close(done)
	}()
	const frame = "data: {\"delta\":\"hi\"}\n\n"
	for _, step := range []struct{ write, sent string }{
		{frame, frame},
		{"data: " + secret[:9], frame + "data: "}, // "long-sess" could begin the value: held, the rest sent
		{secret[9:] + "\n\n", frame + "data: <secret-hidden>\n\n"},
	} {
		if _, err := upstream.Write([]byte(step.write)); err != nil {
			t.Fatal(err)
		}
		select {
		case got := <-w.flushed:
			if got != step.sent {
				t.Fatalf("after writing %.20q the client has %q, want %q", step.write, got, step.sent)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no flush after writing %.20q", step.write)
		}
	}
	_ = upstream.Close()
	<-done
	if strings.Contains(w.Body.String(), "t0k3n") {
		t.Fatalf("the value reached the client: %q", w.Body.String())
	}
}
