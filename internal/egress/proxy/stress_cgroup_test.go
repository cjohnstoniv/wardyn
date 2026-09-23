// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// The pre-release memory stress (scripts/stress-proxy-cgroup.sh). Both tests
// skip unless that script sets their variable: the first records a push on the
// host, where git is, and the second runs the inspection load inside a
// container with the sidecar's 256 MiB cgroup cap, where the script reads the
// OOM verdict off the container.

import (
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	// stressPushMiB is the highest max_inspect_pack_mib the control plane
	// admits (internal/api's maxPushRulesInspectPackMiB).
	stressPushMiB = 64
	// stressLLMBodies concurrent in-cap bodies: one extracting and three
	// retained is the arithmetic maxRetainedScanBytes exists to bound.
	stressLLMBodies = 4
	stressGateway   = "llm-gateway.stress.test"
)

// The pushed branch lives under this run's namespace, and the push is recorded
// in a different process from the one that sends it, so the id is fixed.
var stressRunID = uuid.MustParse("5f7e55e5-0715-4000-8000-000000000715")

// TestStressRecordMaxInspectPush writes a push body just under stressPushMiB:
// four blobs of incompressible bytes, each under gitpack's per-object ceiling.
func TestStressRecordMaxInspectPush(t *testing.T) {
	out := os.Getenv("WARDYN_STRESS_RECORD_PUSH")
	if out == "" {
		t.Skip("scripts/stress-proxy-cgroup.sh sets WARDYN_STRESS_RECORD_PUSH")
	}
	rng := rand.NewChaCha8([32]byte{7, 1, 5})
	files := map[string]string{}
	for i := range 4 {
		b := make([]byte, (stressPushMiB-2)<<20/4)
		_, _ = rng.Read(b)
		files[fmt.Sprintf("blob%d.bin", i)] = string(b)
	}
	body := recordedPush(t, BranchNSPrefix(stressRunID)+"stress", files)
	if n := len(body); n > stressPushMiB<<20 || n < (stressPushMiB-4)<<20 {
		t.Fatalf("recorded push is %d bytes, want just under %d MiB", n, stressPushMiB)
	}
	if err := os.WriteFile(out, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestStressInspectionUnderProxyCgroup sends stressLLMBodies in-cap LLM bodies
// and one push at the inspection ceiling through one proxy at once. Every one
// must be inspected and forwarded; the script separately requires that the
// process was not OOM-killed.
func TestStressInspectionUnderProxyCgroup(t *testing.T) {
	pushPath := os.Getenv("WARDYN_STRESS_PUSH_BODY")
	if pushPath == "" {
		t.Skip("scripts/stress-proxy-cgroup.sh sets WARDYN_STRESS_PUSH_BODY")
	}
	exp := "2099-01-01T00:00:00Z"
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/internal/credentials/mint" {
			_, _ = io.WriteString(w, `{"kind":"github_token","token":"gh-inst-token","jti":"j","expires_at":"`+exp+`"}`)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(up.Close)

	sink := &decisionSink{out: io.Discard, ch: make(chan egress.DecisionLog, 64)}
	p := newProxy(Options{
		RunID: stressRunID,
		Policy: CompilePolicy(types.RunPolicySpec{PushRules: &types.PushRulesSpec{
			DenyPaths: []string{".github/workflows/**"}, MaxInspectPackMiB: stressPushMiB,
		}}),
		Sink:            sink,
		Scanner:         forwardScanEngine(t, "alert"),
		Resolver:        publicResolver{},
		Dial:            redirectDial(upstreamAddr(up)),
		Injector:        staticInj(map[string]injectedHeader{stressGateway: {name: "X-Api-Key", value: "k"}}),
		LLMUpstreams:    map[string]string{anthropicHost: "https://" + stressGateway + "/v1"},
		ControlPlaneURL: "https://wardynd.test:8080",
		RunToken:        newTokenSource("RUNTOK"),
		TLSClientConfig: testInsecureTLSConfig,
		GitGrants:       map[string]uuid.UUID{"octocat/hello-world": uuid.New()},
	})

	push, err := os.Open(pushPath)
	if err != nil {
		t.Fatal(err)
	}
	defer push.Close()

	reqs := []*http.Request{mustLocalReq(t, http.MethodPost, "/wardyn/gh/octocat/hello-world/git-receive-pack", push)}
	for range stressLLMBodies {
		body, n := stressLLMBody()
		req := mustLocalReq(t, http.MethodPost, llmAnthropicPrefix+"messages", body)
		req.ContentLength = n
		reqs = append(reqs, req)
	}
	recs := make([]*httptest.ResponseRecorder, len(reqs))
	var wg sync.WaitGroup
	for i, req := range reqs {
		recs[i] = httptest.NewRecorder()
		wg.Go(func() { p.ServeHTTP(recs[i], req) })
	}
	wg.Wait()

	for i, rec := range recs {
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status %d, want 200: %.200s", reqs[i].URL.Path, rec.Code, rec.Body)
		}
	}
	// Scanned means buffered and extracted, which is the memory this measures;
	// the detectors themselves stop at contentscan's per-request scan budget.
	scanned := 0
	for len(sink.ch) > 0 {
		if d := <-sink.ch; d.Scan != nil && d.Scan.Scanned {
			scanned++
		}
	}
	if scanned != stressLLMBodies {
		t.Errorf("%d LLM bodies were inspected, want %d: a body let through uninspected is not load held", scanned, stressLLMBodies)
	}
	if status, err := os.ReadFile("/proc/self/status"); err == nil {
		for line := range strings.Lines(string(status)) {
			if strings.HasPrefix(line, "VmHWM:") {
				t.Logf("peak resident set: %s", strings.TrimSpace(strings.TrimPrefix(line, "VmHWM:")))
			}
		}
	}
}

// stressLLMBody streams an Anthropic messages body just under maxLLMScanBody,
// made of many small text blocks (the extractor's worst case per byte), without
// the sender holding the bytes: its memory is the proxy's to account for.
func stressLLMBody() (io.Reader, int64) {
	const (
		prefix = `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[`
		last   = `{"type":"text","text":"end"}]}]}`
	)
	block := []byte(`{"type":"text","text":"` + strings.Repeat("w", 1000) + `"},`)
	blocks := (int64(maxLLMScanBody) - int64(len(prefix)+len(last))) / int64(len(block))
	size := int64(len(prefix)) + blocks*int64(len(block)) + int64(len(last))
	return io.MultiReader(strings.NewReader(prefix),
		io.LimitReader(&repeatReader{unit: block}, blocks*int64(len(block))),
		strings.NewReader(last)), size
}

// repeatReader yields unit over and over.
type repeatReader struct {
	unit []byte
	off  int
}

func (r *repeatReader) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		c := copy(p[n:], r.unit[r.off:])
		n += c
		r.off = (r.off + c) % len(r.unit)
	}
	return n, nil
}
