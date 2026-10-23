// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/contentscan"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// stallBody serves left bytes of a JSON-ish body, then parks until release
// closes or ctx ends, and fails as a client that hung up does.
type stallBody struct {
	left    int
	served  *atomic.Int64
	parked  *atomic.Int64
	release <-chan struct{}
	ctx     context.Context
	once    sync.Once
}

func (s *stallBody) Read(b []byte) (int, error) {
	if s.left == 0 {
		s.once.Do(func() { s.parked.Add(1) })
		select {
		case <-s.release:
		case <-s.ctx.Done():
		}
		return 0, errors.New("client gone")
	}
	n := min(len(b), s.left)
	for i := range b[:n] {
		b[i] = 'a'
	}
	s.left -= n
	s.served.Add(int64(n))
	return n, nil
}

// waitFor polls cond for up to d.
func waitFor(d time.Duration, cond func() bool) {
	for end := time.Now().Add(d); !cond() && time.Now().Before(end); {
		time.Sleep(5 * time.Millisecond)
	}
}

// scannerCases runs the local model gate with the scanner off and on.
var scannerCases = []struct {
	name string
	eng  func(t *testing.T) *contentscan.Engine
}{
	{"scanner off", func(*testing.T) *contentscan.Engine { return nil }},
	{"scanner on", func(t *testing.T) *contentscan.Engine { return scanEngine(t, "alert", scanTestSecret) }},
}

// Stalled bodies on the local model route hold no more than the retained
// budget between them, whether or not the scanner runs.
func TestLocalModelGate_StalledBodiesStayWithinTheScanBudget(t *testing.T) {
	const n, serve, declared, budget = 8, 900 << 10, 1 << 20, 2 << 20
	for _, sc := range scannerCases {
		t.Run(sc.name, func(t *testing.T) {
			p, _ := localModelProxy(t, "127.0.0.1:1", nil, types.LocalModelSettings{})
			p.scanner = sc.eng(t)
			shrinkRetainedBudget(t, p, budget)
			var served, parked atomic.Int64
			release := make(chan struct{})
			var wg sync.WaitGroup
			for range n {
				body := &stallBody{left: serve, served: &served, parked: &parked, release: release, ctx: context.Background()}
				req := mustLocalReq(t, http.MethodPost, llmOpenAIPrefix+"v1/chat/completions", body)
				req.ContentLength = declared
				wg.Add(1)
				go func() { defer wg.Done(); p.ServeHTTP(httptest.NewRecorder(), req) }()
			}
			waitFor(time.Second, func() bool { return parked.Load() == n })
			held := served.Load()
			close(release)
			wg.Wait()
			if held > budget {
				t.Fatalf("%d stalled bodies held %d bytes, over the %d-byte budget", n, held, budget)
			}
			if !retainedBudgetEmpty(p) {
				t.Fatal("bytes still charged after every request returned")
			}
		})
	}
}

// blockingUpstream records the body it reads, then holds its answer until
// release closes, so a test can look at the budget while the request is held.
func blockingUpstream(t *testing.T) (addr string, arrived <-chan string, release chan struct{}) {
	got, rel := make(chan string, 1), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- string(b)
		<-rel
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(srv.Close)
	return upstreamAddr(srv), got, rel
}

// A request the gate lets through stays charged, at the size it forwards,
// until its answer is relayed, is charged once although the scanner reads it
// too, and reaches the server as the gate left it.
func TestLocalModelGate_ChargedOnceUntilTheAnswer(t *testing.T) {
	for _, bc := range []struct{ name, body, want string }{
		{"unchanged", `{"model":"qwen2.5:3b", "messages":[{"role":"user","content":"keep  these   bytes"}]}`,
			`{"model":"qwen2.5:3b", "messages":[{"role":"user","content":"keep  these   bytes"}]}`},
		{"bounded copy grows", `{"model":"qwen2.5:3b","max_tokens":-1,"messages":[]}`,
			`{"model":"qwen2.5:3b","max_tokens":4096,"messages":[]}`},
	} {
		for _, sc := range scannerCases {
			t.Run(bc.name+"/"+sc.name, func(t *testing.T) {
				addr, arrived, release := blockingUpstream(t)
				p, buf := localModelProxy(t, addr, nil, types.LocalModelSettings{MaxOutput: 4096})
				p.scanner = sc.eng(t)
				shrinkRetainedBudget(t, p, len(bc.want)) // a second charge would have to wait
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				req := mustLocalReq(t, http.MethodPost, llmOpenAIPrefix+"v1/chat/completions", strings.NewReader(bc.body)).WithContext(ctx)
				rec := httptest.NewRecorder()
				done := make(chan struct{})
				go func() { defer close(done); p.ServeHTTP(rec, req) }()
				var got string
				select {
				case got = <-arrived:
				case <-done:
					t.Fatalf("never reached the server: status %d body %q", rec.Code, rec.Body.String())
				}
				charged := p.scan.retained.TryAcquire(1)
				close(release)
				<-done
				if charged {
					t.Fatalf("budget had room while the %d forwarded bytes were held", len(bc.want))
				}
				if rec.Code != http.StatusOK || got != bc.want {
					t.Fatalf("status %d, server got %q, want 200 and %q", rec.Code, got, bc.want)
				}
				if d := lastDecision(t, buf); d.Decision != egress.Allow {
					t.Fatalf("decision = %+v, want allow", d)
				}
				if !retainedBudgetEmpty(p) {
					t.Fatal("bytes still charged after the answer")
				}
			})
		}
	}
}

// Every refusal after the read gives the charge back.
func TestLocalModelGate_RefusalReleasesItsCharge(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		eng        func(t *testing.T) *contentscan.Engine
		status     int
	}{
		{"other model", `{"model":"llama3.2:3b"}`, nil, http.StatusForbidden},
		{"not json", `model=qwen2.5:3b`, nil, http.StatusBadRequest},
		{"second model key", `{"model":"qwen2.5:3b","Model":"llama3.2:3b"}`, nil, http.StatusBadRequest},
		{"scanner blocks", `{"model":"qwen2.5:3b","messages":[{"role":"user","content":"` + scanTestSecret + `"}]}`,
			func(t *testing.T) *contentscan.Engine { return scanEngine(t, "block", scanTestSecret) }, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := captureUpstream(t, false, `{}`)
			p, _ := localModelProxy(t, upstreamAddr(up.srv), nil, types.LocalModelSettings{})
			if tc.eng != nil {
				p.scanner = tc.eng(t)
			}
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, mustLocalReq(t, http.MethodPost, llmOpenAIPrefix+"v1/chat/completions", strings.NewReader(tc.body)))
			if rec.Code != tc.status || up.reached {
				t.Fatalf("status %d reached %v, want %d and refused", rec.Code, up.reached, tc.status)
			}
			if !retainedBudgetEmpty(p) {
				t.Fatal("a refused request left bytes charged")
			}
		})
	}
}

// A body that finds no room in the budget is refused, closed, with the scan
// path's 502; a client that hangs up mid-body gives its charge back.
func TestLocalModelGate_BudgetWaitFailsClosedAndHangUpReleases(t *testing.T) {
	t.Run("no room", func(t *testing.T) {
		up := captureUpstream(t, false, `{}`)
		p, buf := localModelProxy(t, upstreamAddr(up.srv), nil, types.LocalModelSettings{})
		shrinkRetainedBudget(t, p, 64)
		if !p.scan.retained.TryAcquire(64) { // another request holds the whole budget
			t.Fatal("budget not empty at start")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, mustLocalReq(t, http.MethodPost, llmOpenAIPrefix+"v1/chat/completions",
			strings.NewReader(`{"model":"qwen2.5:3b"}`)).WithContext(ctx))
		p.scan.retained.Release(64)
		if rec.Code != http.StatusBadGateway || up.reached || !strings.Contains(rec.Body.String(), reasonNotInspected) {
			t.Fatalf("status %d reached %v body %q, want a %s 502 before the server", rec.Code, up.reached, rec.Body.String(), reasonNotInspected)
		}
		if d := lastDecision(t, buf); d.Decision != egress.Deny {
			t.Fatalf("decision = %+v, want deny", d)
		}
		if !retainedBudgetEmpty(p) {
			t.Fatal("the refused wait left bytes charged")
		}
	})
	t.Run("hang up mid-body", func(t *testing.T) {
		p, _ := localModelProxy(t, "127.0.0.1:1", nil, types.LocalModelSettings{})
		shrinkRetainedBudget(t, p, 1<<20)
		ctx, cancel := context.WithCancel(context.Background())
		var served, parked atomic.Int64
		body := &stallBody{left: 1 << 10, served: &served, parked: &parked, release: make(chan struct{}), ctx: ctx}
		req := mustLocalReq(t, http.MethodPost, llmOpenAIPrefix+"v1/chat/completions", body).WithContext(ctx)
		req.ContentLength = 1 << 20
		done := make(chan struct{})
		go func() { defer close(done); p.ServeHTTP(httptest.NewRecorder(), req) }()
		waitFor(2*time.Second, func() bool { return parked.Load() == 1 })
		charged := !retainedBudgetEmpty(p)
		cancel()
		<-done
		if !charged {
			t.Fatal("a body mid-read was not charged to the budget")
		}
		if !retainedBudgetEmpty(p) {
			t.Fatal("a client that hung up left bytes charged")
		}
	})
}

// The AWS door holds its body charged; inspecting that body after it charges
// nothing again, so under a budget the size of the body the request neither
// waits on its own bytes nor leaves any charged.
func TestAWSSigV4_InspectedBodyChargedOnce(t *testing.T) {
	h := newSigV4Harness(t)
	h.p.scanner = forwardScanEngine(t, "alert")
	body := `{"messages":[{"role":"user","content":[{"text":"hi"}]}]}`
	shrinkRetainedBudget(t, h.p, len(body))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req := sandboxRequest(t, "POST", sigv4Bedrock, "/model/m/converse", body,
		map[string]string{"Content-Type": "application/json"}, "", "bedrock", "us-east-1", "").WithContext(ctx)
	a := h.mustReSign(t, h.send(t, req, sigv4Bedrock), "bedrock")
	if string(a.body) != body {
		t.Fatalf("AWS saw %q, want the sandbox's body", a.body)
	}
	if !retainedBudgetEmpty(h.p) {
		t.Fatal("bytes still charged after the request")
	}
}
