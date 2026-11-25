// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/contentscan"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// scanwGateway is the LLM gateway the test proxies forward Anthropic requests to.
const scanwGateway = "llm-gateway.scan.test"

// scanwProxy is a proxy whose Anthropic route inspects every body with eng and forwards it to a gateway that
// answers 200.
func scanwProxy(tb testing.TB, eng *contentscan.Engine) *Proxy {
	tb.Helper()
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	tb.Cleanup(up.Close)
	return newProxy(Options{
		RunID:           uuid.New(),
		Policy:          CompilePolicy(types.RunPolicySpec{}),
		Sink:            &decisionSink{out: io.Discard, ch: make(chan queuedDecision, 64)},
		Scanner:         eng,
		Resolver:        publicResolver{},
		Dial:            redirectDial(upstreamAddr(up)),
		Injector:        staticInj(map[string]injectedHeader{scanwGateway: {name: "X-Api-Key", value: "k"}}),
		LLMUpstreams:    map[string]string{anthropicHost: "https://" + scanwGateway + "/v1"},
		TLSClientConfig: testInsecureTLSConfig,
	})
}

// scanwBody is an Anthropic messages body of size bytes: one earlier message pads it, and the last one, the
// text inspection reads, is one short span.
func scanwBody(size int) string {
	const prefix, last = `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"`, `"},{"role":"user","content":"hello"}]}`
	return prefix + strings.Repeat("w", size-len(prefix)-len(last)) + last
}

// scanwBlocks is an Anthropic messages body of about size bytes whose last message is many small text blocks,
// the extractor's worst case per byte.
func scanwBlocks(size int) string {
	const prefix, last = `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[`, `{"type":"text","text":"end"}]}]}`
	block := `{"type":"text","text":"` + strings.Repeat("w", 1000) + `"},`
	return prefix + strings.Repeat(block, (size-len(prefix)-len(last))/len(block)) + last
}

// scanwSend sends each body to p's Anthropic route at once and fails unless every one is answered 200.
func scanwSend(tb testing.TB, p *Proxy, bodies ...string) {
	var wg sync.WaitGroup
	for _, body := range bodies {
		wg.Go(func() {
			req := httptest.NewRequest(http.MethodPost, llmAnthropicPrefix+"messages", strings.NewReader(body))
			req.URL.Host, req.URL.Scheme, req.RequestURI = "", "", llmAnthropicPrefix+"messages"
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				tb.Errorf("status %d: %.200s", rec.Code, rec.Body)
			}
		})
	}
	wg.Wait()
}

// scanwSidecar is a detector sidecar that holds each call until want calls are in at once, or 1.5 s (inside the
// engine's 2 s sidecar timeout) have passed, and reports the most it saw in at once.
func scanwSidecar(t *testing.T, want int) (*contentscan.Engine, func() int) {
	t.Helper()
	var mu sync.Mutex
	in, most := 0, 0
	all := make(chan struct{})
	sc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		in++
		most = max(most, in)
		if in == want {
			close(all)
		}
		mu.Unlock()
		select {
		case <-all:
		case <-time.After(1500 * time.Millisecond):
		}
		mu.Lock()
		in--
		mu.Unlock()
		_, _ = io.WriteString(w, `{"findings":[]}`)
	}))
	t.Cleanup(sc.Close)
	eng, err := contentscan.NewEngine(types.LLMInspectionSpec{Mode: "alert", DetectorSidecarURL: sc.URL}, nil)
	if err != nil || eng == nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return eng, func() int { mu.Lock(); defer mu.Unlock(); return most }
}

// TestLLMScansOfSmallBodiesRunSideBySide: four 1 MiB request bodies sent at once are inspected side by side,
// not one after another, and each is forwarded.
func TestLLMScansOfSmallBodiesRunSideBySide(t *testing.T) {
	eng, most := scanwSidecar(t, 4)
	p := scanwProxy(t, eng)
	body := scanwBody(1 << 20)
	scanwSend(t, p, body, body, body, body)
	if n := most(); n != 4 {
		t.Fatalf("%d of 4 inspections of 1 MiB bodies ran at once, want all 4", n)
	}
}

// TestLLMScanOfALargestBodyRunsAlone: the memory bound holds. With the largest inspected body set to 4 MiB, a
// body just under it and a 1 MiB body sent at once are inspected one after the other: together they are more
// than one largest body.
func TestLLMScanOfALargestBodyRunsAlone(t *testing.T) {
	saved := maxLLMScanBody
	maxLLMScanBody = 4 << 20
	t.Cleanup(func() { maxLLMScanBody = saved })
	eng, most := scanwSidecar(t, 2)
	p := scanwProxy(t, eng)
	scanwSend(t, p, scanwBody(4<<20-4<<10), scanwBody(1<<20))
	if n := most(); n != 1 {
		t.Fatalf("%d inspections ran at once, want 1: two bodies larger together than the largest one", n)
	}
}

// BenchmarkLLMScanConcurrency is the wall time of one 1 MiB Anthropic body through the inspected route, and of
// four sent at once, with every built-in detector on.
func BenchmarkLLMScanConcurrency(b *testing.B) {
	eng, err := contentscan.NewEngine(types.LLMInspectionSpec{Mode: "alert", DetectSecrets: true, DetectSecretPatterns: true,
		DetectEntropy: true, DetectPII: true}, [][]byte{[]byte(scanTestSecret)})
	if err != nil || eng == nil {
		b.Fatalf("NewEngine: %v", err)
	}
	p := scanwProxy(b, eng)
	body := scanwBlocks(1 << 20)
	for _, n := range []int{1, 4} {
		b.Run(map[int]string{1: "one", 4: "four"}[n], func(b *testing.B) {
			bodies := make([]string, n)
			for i := range bodies {
				bodies[i] = body
			}
			b.ResetTimer()
			for range b.N {
				scanwSend(b, p, bodies...)
			}
		})
	}
}
