// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/contentscan"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The gateway recipes run against the real gateways. LiteLLM, Portkey's open-source gateway and Presidio
// run as the operator's own unmodified containers, from the images pinned below, and nothing of them is
// copied or bundled. Wardyn's proxy runs in this process, with the canned model endpoint of
// test/gatewayfake as the provider behind each gateway, so no provider key and no real model is involved.
//
// The images are third-party code, so the test runs only in the hosted nightly job
// (docker-tagged-live): it needs WARDYN_TEST_DOCKER=1 and GITHUB_ACTIONS=true. Pull paths: LiteLLM and
// Presidio come from ghcr.io, Portkey from Docker Hub, which the base-image mirror (deploy/base-images.txt)
// does not carry.
const (
	// LiteLLM v1.83.10-stable.
	recipeLiteLLMImage = "ghcr.io/berriai/litellm@sha256:6c82d338a60e7b273ae46bf1d8db814d2856ae010f96c44eeadde574d3893f76"
	// Portkey gateway 1.15.2. TRUSTED_CUSTOM_HOSTS exists from 1.14.0 and the gateway refuses a private
	// custom host without it.
	recipePortkeyImage = "docker.io/portkeyai/gateway@sha256:97f094d9c8a764cbfaa2a7138c0017b247ca923bb06db1b4c13b7f8a33b5200d"
	// Presidio analyzer 2.2.363.
	recipePresidioImage = "ghcr.io/data-privacy-stack/presidio-analyzer@sha256:330c78c144eca0c554e4cf12ab7c05a21872dca63fe6f93d67f08741de30b62f"

	// recipeStubAlias is the name the gateway containers reach the host's stub by (docker's host-gateway),
	// and the one hostname Portkey is told to trust as a custom host.
	recipeStubAlias = "gatewayfake.test"
	// recipeGatewayHost is the gateway's name in the proxy's config. The proxy dials the TLS front for
	// it, so the name resolves to nothing real.
	recipeGatewayHost = "llm-gateway.corp.internal"

	recipeLiteLLMKey  = "sk-wardyn-recipe-test-master"
	recipePortkeyKey  = "BROKERED-PORTKEY-RECIPE-KEY"
	recipeSandboxMark = "SANDBOX-SMUGGLED"
)

// recipeRequire skips the test anywhere but the hosted job with a live Docker daemon.
func recipeRequire(t *testing.T) {
	t.Helper()
	if os.Getenv("WARDYN_TEST_DOCKER") != "1" {
		t.Skip("WARDYN_TEST_DOCKER=1 not set; skipping the gateway recipes")
	}
	if os.Getenv("GITHUB_ACTIONS") != "true" {
		t.Skip("third-party gateway images run only in the hosted job; skipping the gateway recipes")
	}
}

func recipeDocker(t *testing.T, timeout time.Duration, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// recipePull pulls image and logs how long it took.
func recipePull(t *testing.T, image string) {
	t.Helper()
	start := time.Now()
	recipeDocker(t, 10*time.Minute, "pull", "--quiet", image)
	t.Logf("pulled %s in %s", image, time.Since(start).Round(time.Second))
}

// recipeRun starts image detached, with the stub's alias pointing at the host, one container port
// published on loopback, and returns the container id and the host port. The container is removed, and
// its log printed if the test failed.
func recipeRun(t *testing.T, image, containerPort string, env map[string]string, mounts []string, args ...string) (string, string) {
	t.Helper()
	cmd := []string{"run", "-d", "--add-host", recipeStubAlias + ":host-gateway", "-p", "127.0.0.1::" + containerPort}
	for k, v := range env {
		cmd = append(cmd, "-e", k+"="+v)
	}
	for _, m := range mounts {
		cmd = append(cmd, "-v", m)
	}
	cmd = append(cmd, image)
	cmd = append(cmd, args...)
	id := recipeDocker(t, 2*time.Minute, cmd...)
	t.Cleanup(func() {
		if t.Failed() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			out, _ := exec.CommandContext(ctx, "docker", "logs", "--tail", "60", id).CombinedOutput()
			t.Logf("container %s log tail:\n%s", id[:12], out)
		}
		_ = exec.Command("docker", "rm", "-f", id).Run()
	})
	mapped := recipeDocker(t, time.Minute, "port", id, containerPort+"/tcp")
	line, _, _ := strings.Cut(mapped, "\n")
	i := strings.LastIndex(line, ":")
	if i < 0 {
		t.Fatalf("docker port printed %q", mapped)
	}
	return id, line[i+1:]
}

// recipeWait polls url until it answers 200.
func recipeWait(t *testing.T, url string, limit time.Duration) {
	t.Helper()
	client := &http.Client{Timeout: 3 * time.Second}
	deadline := time.Now().Add(limit)
	var last error
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
			err = fmt.Errorf("status %d", resp.StatusCode)
		}
		last = err
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("%s did not answer 200 within %s: %v", url, limit, last)
}

var (
	recipeStubOnce sync.Once
	recipeStubBin  string
	recipeStubErr  error
)

// recipeStubBinary builds test/gatewayfake once.
func recipeStubBinary(t *testing.T) string {
	t.Helper()
	recipeStubOnce.Do(func() {
		dir, err := os.MkdirTemp("", "gatewayfake-bin")
		if err != nil {
			recipeStubErr = err
			return
		}
		recipeStubBin = filepath.Join(dir, "gatewayfake")
		if out, err := exec.Command("go", "build", "-o", recipeStubBin, "../../../test/gatewayfake").CombinedOutput(); err != nil {
			recipeStubErr = fmt.Errorf("go build gatewayfake: %v\n%s", err, out)
		}
	})
	if recipeStubErr != nil {
		t.Fatal(recipeStubErr)
	}
	return recipeStubBin
}

// recipeStub is a running test/gatewayfake listening on every interface, so a container reaches it.
type recipeStub struct{ port string }

func recipeStartStub(t *testing.T) recipeStub {
	t.Helper()
	bin := recipeStubBinary(t)
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	port := fmt.Sprint(ln.Addr().(*net.TCPAddr).Port)
	_ = ln.Close()
	cmd := exec.Command(bin, "-addr", "0.0.0.0:"+port)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	recipeWait(t, "http://127.0.0.1:"+port+"/healthz", 30*time.Second)
	return recipeStub{port: port}
}

// recipeSeen is the slice of the stub's /_seen the assertions read.
type recipeSeen struct {
	ModelCalls int `json:"model_calls"`
	Requests   []struct {
		Path    string              `json:"path"`
		Headers map[string][]string `json:"headers"`
	} `json:"requests"`
}

func (s recipeStub) seen(t *testing.T) recipeSeen {
	t.Helper()
	resp, err := http.Get("http://127.0.0.1:" + s.port + "/_seen")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out recipeSeen
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// recipeFront is the TLS listener the proxy dials for the gateway host: the gateways serve plain http
// here, and the proxy's base URL is https only. It relays to the gateway container, flushing as it goes
// so a stream stays a stream, and keeps the headers of the request it relays: the request as the gateway
// received it from Wardyn.
type recipeFront struct {
	srv  *httptest.Server
	mu   sync.Mutex
	last http.Header
	n    int
}

func recipeStartFront(t *testing.T, gatewayAddr string) *recipeFront {
	t.Helper()
	f := &recipeFront{}
	rp := &httputil.ReverseProxy{
		FlushInterval: -1,
		Director: func(r *http.Request) {
			f.mu.Lock()
			f.last, f.n = r.Header.Clone(), f.n+1
			f.mu.Unlock()
			r.URL.Scheme, r.URL.Host, r.Host = "http", gatewayAddr, gatewayAddr
		},
	}
	f.srv = httptest.NewTLSServer(rp)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *recipeFront) wire() (http.Header, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last.Clone(), f.n
}

func (f *recipeFront) addr() string { return f.srv.Listener.Addr().String() }

// recipeProxy is the proxy of one run whose Anthropic and OpenAI routes go to the gateway host: the
// credential is injected, extra holds the admin's headers for the gateway host, and the allowed models
// list is set so the rerouting-key check is armed.
func recipeProxy(t *testing.T, f *recipeFront, credential string, extra map[string]string) (*Proxy, *bytes.Buffer) {
	t.Helper()
	inj := staticInj(map[string]injectedHeader{recipeGatewayHost: {name: "Authorization", value: "Bearer " + credential}})
	if len(extra) > 0 {
		// The same rules the admin's save and the proxy's config load apply, so a recipe that passes here
		// is a provider an admin can save.
		for name, value := range extra {
			if err := ReservedExtraHeader(name, "Authorization"); err != nil {
				t.Fatalf("extra header %s: %v", name, err)
			}
			if err := ExtraHeaderValueRefusal(value); err != nil {
				t.Fatalf("extra header %s value: %v", name, err)
			}
		}
		inj.setLLMHeaders(map[string]map[string]string{recipeGatewayHost: extra})
	}
	buf := &bytes.Buffer{}
	p := newProxy(Options{
		RunID:           uuid.New(),
		Policy:          CompilePolicy(types.RunPolicySpec{}),
		Sink:            &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 64)},
		Resolver:        fakeResolver{m: map[string][]net.IP{recipeGatewayHost: ips("10.40.1.5")}},
		Dial:            redirectDial(f.addr()),
		Injector:        inj,
		LLMUpstreams:    map[string]string{anthropicHost: "https://" + recipeGatewayHost + "/v1", openaiHost: "https://" + recipeGatewayHost + "/v1"},
		TLSClientConfig: testInsecureTLSConfig,
	})
	p.allowedModels = modelSet([]string{"claude-sonnet-5", "gpt-5"})
	return p, buf
}

const (
	recipeMessagesBody  = `{"model":"claude-sonnet-5","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hello"}]}`
	recipeResponsesBody = `{"model":"gpt-5","stream":true,"input":"hello"}`
)

// recipeSandboxHeaders are the headers Claude Code sends, plus the credentials and gateway routing
// headers a sandbox must not be able to use.
func recipeSandboxHeaders() http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Anthropic-Version", "2023-06-01")
	h.Set("Anthropic-Beta", "claude-code-20250219,interleaved-thinking-2025-05-14")
	h.Set("Authorization", "Bearer "+recipeSandboxMark)
	h.Set("X-Api-Key", recipeSandboxMark+"-KEY")
	h.Set("X-Portkey-Custom-Host", "http://evil.test:9/v1")
	h.Set("X-Portkey-Config", "pc-"+recipeSandboxMark)
	h.Set("X-Portkey-Api-Key", recipeSandboxMark+"-PK")
	return h
}

func recipeSend(t *testing.T, p *Proxy, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := mustLocalReq(t, http.MethodPost, path, strings.NewReader(body))
	for k, v := range recipeSandboxHeaders() {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

// recipeNoSandboxValue fails when any header holds a value the sandbox sent.
func recipeNoSandboxValue(t *testing.T, where string, h http.Header) {
	t.Helper()
	for k, vs := range h {
		for _, v := range vs {
			if strings.Contains(v, recipeSandboxMark) || strings.Contains(v, "evil.test") {
				t.Errorf("%s carries a sandbox value in %s", where, k)
			}
		}
	}
}

// recipeUsage checks the call left one usage row of the vendor's dialect with counted, non-zero figures.
func recipeUsage(t *testing.T, buf *bytes.Buffer, vendor string, wantRows int) {
	t.Helper()
	rows := usageRows(t, buf)
	if len(rows) != wantRows {
		t.Fatalf("%d usage rows, want %d:\n%s", len(rows), wantRows, buf)
	}
	u := rows[wantRows-1].Usage
	if u == nil || u.Vendor != vendor || u.State != "counted" || u.Input <= 0 || u.Output <= 0 {
		t.Fatalf("usage = %+v, want a counted %s call with input and output tokens", u, vendor)
	}
}

// recipeRefusesFallbacks sends a body with a gateway fallbacks list to the Messages route: the proxy
// refuses it, and neither the gateway nor the stub sees a request.
func recipeRefusesFallbacks(t *testing.T, p *Proxy, f *recipeFront, stub recipeStub) {
	t.Helper()
	_, wireBefore := f.wire()
	callsBefore := stub.seen(t).ModelCalls
	body := `{"model":"claude-sonnet-5","max_tokens":16,"fallbacks":["gpt-5"],"messages":[{"role":"user","content":"hello"}]}`
	rec := recipeSend(t, p, llmAnthropicPrefix+"messages", body)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a fallbacks body got status %d, want 403: %s", rec.Code, rec.Body)
	}
	if _, wireAfter := f.wire(); wireAfter != wireBefore || stub.seen(t).ModelCalls != callsBefore {
		t.Fatal("a refused fallbacks body still reached the gateway")
	}
}

// TestGatewayRecipes_Live is the six claims the gateway recipes make, against the real gateways.
func TestGatewayRecipes_Live(t *testing.T) {
	recipeRequire(t)
	if out, err := exec.Command("df", "-h", "/").CombinedOutput(); err == nil {
		t.Logf("disk before the pulls:\n%s", out)
	}
	for _, image := range []string{recipeLiteLLMImage, recipePortkeyImage, recipePresidioImage} {
		recipePull(t, image)
	}
	if out, err := exec.Command("df", "-h", "/").CombinedOutput(); err == nil {
		t.Logf("disk after the pulls:\n%s", out)
	}

	t.Run("litellm", func(t *testing.T) {
		start := time.Now()
		stub := recipeStartStub(t)
		upstream := "http://" + recipeStubAlias + ":" + stub.port
		cfg := fmt.Sprintf(`model_list:
  - model_name: claude-sonnet-5
    litellm_params:
      model: anthropic/claude-sonnet-5
      api_base: %[1]s
      api_key: fake-anthropic-upstream-key
  - model_name: gpt-5
    litellm_params:
      model: openai/gpt-5
      api_base: %[1]s/v1
      api_key: fake-openai-upstream-key
`, upstream)
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(cfg), 0o644); err != nil {
			t.Fatal(err)
		}
		_, port := recipeRun(t, recipeLiteLLMImage, "4000",
			map[string]string{"LITELLM_MASTER_KEY": recipeLiteLLMKey, "LITELLM_LOCAL_MODEL_COST_MAP": "True"},
			// Not /app: that is the image's working directory, which holds its relative entrypoint.
			[]string{dir + ":/etc/litellm:ro"}, "--config", "/etc/litellm/config.yaml", "--port", "4000")
		recipeWait(t, "http://127.0.0.1:"+port+"/health/liveliness", 4*time.Minute)
		t.Logf("litellm ready after %s", time.Since(start).Round(time.Second))

		front := recipeStartFront(t, "127.0.0.1:"+port)
		p, buf := recipeProxy(t, front, recipeLiteLLMKey, nil)

		// 1 and 3: the credential is the brokered one (LiteLLM answers 401 to any other), and a streamed
		// Messages call is metered.
		rec := recipeSend(t, p, llmAnthropicPrefix+"messages?beta=true", recipeMessagesBody)
		if rec.Code != http.StatusOK {
			t.Fatalf("messages: status %d: %s", rec.Code, rec.Body)
		}
		wire, _ := front.wire()
		if got := wire.Get("Authorization"); got != "Bearer "+recipeLiteLLMKey {
			t.Errorf("the gateway received an Authorization that is not the brokered credential")
		}
		recipeNoSandboxValue(t, "the request LiteLLM received", wire)
		if got := wire.Get("Anthropic-Beta"); !strings.Contains(got, "claude-code-20250219") {
			t.Errorf("anthropic-beta = %q, want the sandbox's value forwarded", got)
		}
		recipeUsage(t, buf, "anthropic", 1)

		// 5: a Responses stream through LiteLLM is metered.
		rec = recipeSend(t, p, llmOpenAIPrefix+"responses", recipeResponsesBody)
		if rec.Code != http.StatusOK {
			t.Fatalf("responses: status %d: %s", rec.Code, rec.Body)
		}
		if !strings.Contains(rec.Body.String(), "response.completed") {
			t.Errorf("the Responses stream lacks response.completed: %s", rec.Body)
		}
		recipeUsage(t, buf, "openai", 2)

		// 4: a fallbacks list is refused.
		recipeRefusesFallbacks(t, p, front, stub)
	})

	t.Run("portkey", func(t *testing.T) {
		start := time.Now()
		stub := recipeStartStub(t)
		_, port := recipeRun(t, recipePortkeyImage, "8787", map[string]string{"TRUSTED_CUSTOM_HOSTS": recipeStubAlias}, nil)
		recipeWait(t, "http://127.0.0.1:"+port+"/", time.Minute)
		t.Logf("portkey ready after %s", time.Since(start).Round(time.Second))

		front := recipeStartFront(t, "127.0.0.1:"+port)
		extra := map[string]string{
			"x-portkey-provider":    "anthropic",
			"x-portkey-custom-host": "http://" + recipeStubAlias + ":" + stub.port + "/v1",
		}
		p, buf := recipeProxy(t, front, recipePortkeyKey, extra)

		// 1, 2 and 3: the admin's routing headers reach Portkey and the sandbox's are gone, the
		// credential is the brokered one, and the streamed call is metered.
		rec := recipeSend(t, p, llmAnthropicPrefix+"messages?beta=true", recipeMessagesBody)
		if rec.Code != http.StatusOK {
			t.Fatalf("messages: status %d: %s", rec.Code, rec.Body)
		}
		wire, _ := front.wire()
		if got := wire.Values("X-Portkey-Provider"); len(got) != 1 || got[0] != "anthropic" {
			t.Errorf("x-portkey-provider = %q, want exactly the configured value", got)
		}
		if got := wire.Values("X-Portkey-Custom-Host"); len(got) != 1 || got[0] != extra["x-portkey-custom-host"] {
			t.Errorf("x-portkey-custom-host = %q, want exactly the configured value", got)
		}
		if got := wire.Get("Authorization"); got != "Bearer "+recipePortkeyKey {
			t.Errorf("Portkey received an Authorization that is not the brokered credential")
		}
		recipeNoSandboxValue(t, "the request Portkey received", wire)
		recipeUsage(t, buf, "anthropic", 1)

		// What reached the provider behind Portkey: one call, the brokered key, the beta header, and
		// nothing the sandbox sent.
		seen := stub.seen(t)
		if seen.ModelCalls != 1 || len(seen.Requests) != 1 {
			t.Fatalf("the stub saw %d model calls, want 1: %+v", seen.ModelCalls, seen.Requests)
		}
		got := seen.Requests[0].Headers
		if v := got["x-api-key"]; len(v) != 1 || v[0] != recipePortkeyKey {
			t.Errorf("the provider's x-api-key is not the brokered credential")
		}
		if v := got["anthropic-beta"]; len(v) != 1 || !strings.Contains(v[0], "claude-code-20250219") {
			t.Errorf("anthropic-beta at the provider = %q, want the sandbox's value forwarded", v)
		}
		recipeNoSandboxValue(t, "the request at the provider", http.Header(got))

		// 4: a fallbacks list is refused.
		recipeRefusesFallbacks(t, p, front, stub)
	})

	t.Run("presidio", func(t *testing.T) {
		start := time.Now()
		_, port := recipeRun(t, recipePresidioImage, "3000", nil, nil)
		base := "http://127.0.0.1:" + port
		recipeWait(t, base+"/health", 3*time.Minute)
		// The first analysis loads the language model; the detector gives each call three seconds.
		var warm error
		for deadline := time.Now().Add(3 * time.Minute); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
			resp, err := http.Post(base+"/analyze", "application/json", strings.NewReader(`{"text":"warm up","language":"en"}`))
			if warm = err; err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					break
				}
				warm = fmt.Errorf("status %d", resp.StatusCode)
			}
		}
		if warm != nil {
			t.Fatalf("presidio never analysed text: %v", warm)
		}
		t.Logf("presidio ready after %s", time.Since(start).Round(time.Second))

		eng, err := contentscan.NewEngine(types.LLMInspectionSpec{
			Mode: "block", DetectorSidecarURL: base + "/analyze", DetectorSidecarFormat: types.DetectorSidecarFormatPresidio,
			BlockMinSeverity: "medium", OnScannerError: "block",
		}, nil)
		if err != nil {
			t.Fatal(err)
		}

		// 6: an email is flagged as pii and blocks at medium.
		res, _, err := eng.ScanRequest(contentscan.ChannelGeneric, []byte("please email alice.smith@example.com about the invoice"))
		if err != nil {
			t.Fatal(err)
		}
		flagged := false
		for _, fd := range res.Findings {
			flagged = flagged || (fd.Category == contentscan.CategoryPII && fd.Detector == "sidecar:EMAIL_ADDRESS")
		}
		if !flagged || res.Skipped || !eng.ShouldBlock(res) {
			t.Errorf("email result = %+v, want an EMAIL_ADDRESS pii finding that blocks, scanned fully", res)
		}

		// 6: a code-like prompt (dates, a name, a URL, an id, an address and port, a licence-like word) is
		// not blocked at medium.
		code := "2025-11-07 Alice Smith https://example.com/a/b 123e4567-e89b-12d3-a456-426614174000 10.0.0.5:8080 license AB12345 ok"
		res, _, err = eng.ScanRequest(contentscan.ChannelGeneric, []byte(code))
		if err != nil {
			t.Fatal(err)
		}
		if res.Skipped || eng.ShouldBlock(res) {
			t.Errorf("code-like prompt result = %+v, want it scanned fully and not blocked", res)
		}
	})
}
