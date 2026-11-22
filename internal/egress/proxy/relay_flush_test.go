package proxy

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/recording"
)

// relay flushes each chunk to the client as it arrives, scrubbing on or off.
func TestRelayFlushesEachChunk(t *testing.T) {
	for _, scrub := range []bool{false, true} {
		name := "scrub off"
		if scrub {
			name = "scrub on"
		}
		t.Run(name, func(t *testing.T) {
			p := &Proxy{}
			if scrub {
				key := uuid.New()
				procMaskFor(key, []byte("long-session-"+strings.Repeat("t0k3n", 100)))
				t.Cleanup(func() { procRegistry.Evict(key) })
				p = &Proxy{scrubResponses: true, maskKey: key}
			}
			body, upstream := io.Pipe()
			w := &flushTap{ResponseRecorder: httptest.NewRecorder(), flushed: make(chan string, 8)}
			done := make(chan struct{})
			go func() {
				p.relay(w, &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: body})
				close(done)
			}()
			const one, two = "data: one\n\n", "data: two\n\n"
			for _, step := range []struct{ write, sent string }{
				{one, one},
				{two, one + two},
			} {
				if _, err := upstream.Write([]byte(step.write)); err != nil {
					t.Fatal(err)
				}
				select {
				case got := <-w.flushed:
					if got != step.sent {
						t.Fatalf("after writing %q the client has %q, want %q", step.write, got, step.sent)
					}
				case <-time.After(5 * time.Second):
					t.Fatalf("no flush after writing %q", step.write)
				}
			}
			_ = upstream.Close()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("relay did not return after the body closed")
			}
		})
	}
}

// holdingUpstream is a TLS event-stream server that sends one event, then holds the rest back until
// release is closed. The returned func releases it, and is safe to call twice.
func holdingUpstream(t *testing.T) (*httptest.Server, func()) {
	t.Helper()
	release := make(chan struct{})
	var once sync.Once
	rel := func() { once.Do(func() { close(release) }) }
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: one\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
			_, _ = io.WriteString(w, "event: two\n\n")
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(up.Close)
	t.Cleanup(rel)
	return up, rel
}

// firstEvent reads a response's status and its first len(want) body bytes on a goroutine, and fails
// unless they arrive within 5 s. The wait is generous for a loaded runner; the test's point is that
// the bytes arrive while the upstream still holds the rest. It logs the measured latency.
func firstEvent(t *testing.T, start time.Time, read func() (*http.Response, error), want string) *http.Response {
	t.Helper()
	type result struct {
		resp *http.Response
		got  string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		resp, err := read()
		if err != nil {
			ch <- result{err: err}
			return
		}
		buf := make([]byte, len(want))
		_, err = io.ReadFull(resp.Body, buf)
		ch <- result{resp: resp, got: string(buf), err: err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("reading the first event: %v", r.err)
		}
		if r.resp.StatusCode != http.StatusOK || r.got != want {
			t.Fatalf("status %d, first bytes %q; want 200 and %q", r.resp.StatusCode, r.got, want)
		}
		t.Logf("first event after %v", time.Since(start))
		return r.resp
	case <-time.After(5 * time.Second):
		t.Fatalf("the first event did not arrive in 5 s while the upstream held the rest")
		return nil
	}
}

// A streamed answer on the brokered model route reaches the agent as each event is written.
func TestLLMRoute_StreamedAnswerReachesTheAgentPerEvent(t *testing.T) {
	up, release := holdingUpstream(t)
	p, _ := newLocalRouteProxy(t, "http://wardynd.test:8080", "RUNTOK", upstreamAddr(up), anthropicInjector(), testInsecureTLSConfig)
	front := httptest.NewServer(p)
	defer front.Close()

	start := time.Now()
	resp := firstEvent(t, start, func() (*http.Response, error) {
		return http.Post(front.URL+llmAnthropicPrefix+"v1/messages", "application/json", strings.NewReader("{}"))
	}, "event: one\n\n")
	defer func() { _ = resp.Body.Close() }()
	release()
	rest, err := io.ReadAll(resp.Body)
	if err != nil || string(rest) != "event: two\n\n" {
		t.Fatalf("rest of the body = %q, err %v; want the second event", rest, err)
	}
}

// The same, on a terminated (MITM) Anthropic connection opened through CONNECT.
func TestMITMLLM_StreamedAnswerReachesTheAgentPerEvent(t *testing.T) {
	up, release := holdingUpstream(t)
	p, _, caPEM := mitmProxy(t, "alert", up)
	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()

	tlsConn := agentMITMConn(t, proxySrv.URL, caPEM)
	defer func() { _ = tlsConn.Close() }()
	start := time.Now()
	_, err := io.WriteString(tlsConn, "POST /v1/messages HTTP/1.1\r\nHost: "+anthropicHost+"\r\nContent-Type: application/json\r\nContent-Length: 2\r\n\r\n{}")
	if err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(tlsConn)
	resp := firstEvent(t, start, func() (*http.Response, error) {
		return http.ReadResponse(br, &http.Request{Method: http.MethodPost})
	}, "event: one\n\n")
	release()
	rest, err := io.ReadAll(resp.Body)
	if err != nil || string(rest) != "event: two\n\n" {
		t.Fatalf("rest of the body = %q, err %v; want the second event", rest, err)
	}
}

// Of the sandbox's headers, a recording upload carries only the exact plain-log format value.
func TestBrokeredRecordingCarriesOnlyThePlainLogFormat(t *testing.T) {
	type seen struct{ path, format, auth string }
	var mu sync.Mutex
	var calls []seen
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		calls = append(calls, seen{r.URL.Path, r.Header.Get(recording.FormatHeader), r.Header.Get("Authorization")})
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer cp.Close()
	p, _ := newLocalRouteProxy(t, "http://wardynd.test:8080", "RUNTOK", upstreamAddr(cp), nil, nil)

	for _, tc := range []struct {
		name, route, suffix, header, want string
	}{
		{"plain log", routeRecordings, "", recording.FormatLog, recording.FormatLog},
		{"plain log part", routeRecordings, "/parts/2", recording.FormatLog, recording.FormatLog},
		{"cast (no header)", routeRecordings, "", "", ""},
		{"another value", routeRecordings, "", "cast", ""},
		{"scan result", routeScanResults, "", recording.FormatLog, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := uuid.NewString()
			body := "{}"
			if tc.route == routeRecordings {
				body = "a plain log line\n"
			}
			r := mustLocalReq(t, http.MethodPut, tc.route+id+tc.suffix, strings.NewReader(body))
			if tc.header != "" {
				r.Header.Set(recording.FormatHeader, tc.header)
			}
			w := httptest.NewRecorder()
			p.ServeHTTP(w, r)
			if w.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want 204; body %q", w.Code, w.Body.String())
			}
			mu.Lock()
			defer mu.Unlock()
			if len(calls) == 0 {
				t.Fatal("the control plane saw no call")
			}
			got := calls[len(calls)-1]
			if !strings.Contains(got.path, id) {
				t.Fatalf("control plane saw path %q, want one naming %s", got.path, id)
			}
			if got.format != tc.want {
				t.Errorf("control plane saw %s = %q, want %q", recording.FormatHeader, got.format, tc.want)
			}
			if got.auth != "Bearer RUNTOK" {
				t.Errorf("control plane saw Authorization %q, want the run token", got.auth)
			}
		})
	}
}
