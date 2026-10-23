// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
)

// scrubbedWriter is a relayed response with every credential this proxy holds
// replaced, in its header values and its body, by the masking placeholder: a
// destination that echoes a request header then returns the placeholder, not
// the value the proxy added. Masking changes lengths, so Content-Length goes.
// ponytail: exact bytes only, as output masking; a compressed body passes as is.
type scrubbedWriter struct {
	http.ResponseWriter
	m  secretmask.Masker
	mw *secretmask.MaskingWriter
}

// scrubbed wraps w when Config.ScrubResponses is set; done flushes what the
// masking writer holds back, and runs when the relay ends. The masking set is
// read now, after the outbound request registered its credential.
func (p *Proxy) scrubbed(w http.ResponseWriter) (out http.ResponseWriter, done func()) {
	if !p.scrubResponses {
		return w, func() {}
	}
	m := procRegistry.Masker(p.maskKey)
	if len(m.Secrets()) == 0 {
		return w, func() {}
	}
	s := &scrubbedWriter{ResponseWriter: w, m: m, mw: secretmask.NewMaskingWriter(w, m)}
	return s, func() { _ = s.mw.Close() }
}

func (s *scrubbedWriter) WriteHeader(code int) {
	h := s.ResponseWriter.Header()
	for _, vs := range h {
		for i, v := range vs {
			vs[i] = string(s.m.Mask([]byte(v)))
		}
	}
	h.Del("Content-Length")
	s.ResponseWriter.WriteHeader(code)
}

func (s *scrubbedWriter) Write(b []byte) (int, error) { return s.mw.Write(b) }

// Flush forwards what the masking writer has released; it holds back only an
// end of the stream that could begin a credential, until the next write or done.
func (s *scrubbedWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
