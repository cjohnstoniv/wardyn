// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func validMCPSandboxSpec() MCPSandboxSpec {
	return MCPSandboxSpec{
		ID:          MCPSandboxID{RunID: uuid.New(), Ordinal: 1, Class: types.CC2},
		ServerKey:   "files",
		Image:       "example/agent:1",
		Launch:      types.ComponentMCPLaunch{Command: "npx", Args: []string{"-y", "@example/mcp-files@1.4.2"}},
		Env:         map[string]string{"FILES_ROOT": "/work"},
		SecretEnv:   map[string]string{"FILES_TOKEN": "t", "ANTHROPIC_API_KEY": "the server's own"},
		PlatformEnv: map[string]string{"HTTPS_PROXY": "http://10.0.0.2:3128", "NO_PROXY": "localhost", "SSL_CERT_FILE": "/tmp/wardyn/ca-bundle.pem", "WARDYN_MCP_LAUNCH_JSON": "{}"},
		ProxyConfig: []byte(`{}`),
		Labels:      map[string]string{"wardyn.run-id": "x"},
	}
}

// TestMCPSandboxSpecRefusesMounts: the spec has no field that reaches the host, and Validate refuses one set at
// any depth, so a field added later cannot carry a mount, a drive or a device through unseen.
func TestMCPSandboxSpecRefusesMounts(t *testing.T) {
	if err := validMCPSandboxSpec().Validate(); err != nil {
		t.Fatalf("a valid spec: %v", err)
	}
	var walk func(reflect.Type, string)
	seen := map[reflect.Type]bool{}
	walk = func(ty reflect.Type, path string) {
		for ty.Kind() == reflect.Pointer || ty.Kind() == reflect.Slice || ty.Kind() == reflect.Array || ty.Kind() == reflect.Map {
			ty = ty.Elem()
		}
		if ty.Kind() != reflect.Struct || seen[ty] {
			return
		}
		seen[ty] = true
		for i := range ty.NumField() {
			f := ty.Field(i)
			if hostAccessName(f.Name) {
				t.Errorf("%s.%s: an MCP sandbox takes no mount, drive or device field", path, f.Name)
			}
			walk(f.Type, path+"."+f.Name)
		}
	}
	walk(reflect.TypeFor[MCPSandboxSpec](), "MCPSandboxSpec")

	type dev struct{ Path string }
	type planted struct {
		MCPSandboxSpec
		Nested struct{ Devices []dev }
		Mounts []string
		Drive  *string
		Binds  []string
		Volume string
	}
	drive := "d"
	for name, p := range map[string]planted{
		"mount":  {MCPSandboxSpec: validMCPSandboxSpec(), Mounts: []string{"/var/run/docker.sock:/var/run/docker.sock"}},
		"drive":  {MCPSandboxSpec: validMCPSandboxSpec(), Drive: &drive},
		"bind":   {MCPSandboxSpec: validMCPSandboxSpec(), Binds: []string{"/:/host"}},
		"volume": {MCPSandboxSpec: validMCPSandboxSpec(), Volume: "v"},
		"device": {MCPSandboxSpec: validMCPSandboxSpec(), Nested: struct{ Devices []dev }{[]dev{{"/dev/kvm"}}}},
	} {
		if path := hostAccessField(reflect.ValueOf(p), "planted"); path == "" {
			t.Errorf("%s: a set host-access field was not found", name)
		}
	}
	if path := hostAccessField(reflect.ValueOf(planted{MCPSandboxSpec: validMCPSandboxSpec()}), "planted"); path != "" {
		t.Errorf("unset host-access fields refused: %s", path)
	}
}

// TestMCPSandboxSpecValidate: a variable name a component may not set, a platform variable outside the closed
// list and an empty proxy config are refused; a model provider's key name stays the server's own secret.
func TestMCPSandboxSpecValidate(t *testing.T) {
	for name, edit := range map[string]func(*MCPSandboxSpec){
		"WARDYN_ env":           func(s *MCPSandboxSpec) { s.Env["WARDYN_RUN_TOKEN"] = "x" },
		"reserved secret env":   func(s *MCPSandboxSpec) { s.SecretEnv["LD_PRELOAD"] = "x" },
		"reserved prefix":       func(s *MCPSandboxSpec) { s.Env["CLAUDE_CODE_X"] = "x" },
		"proxy name in own env": func(s *MCPSandboxSpec) { s.Env["HTTPS_PROXY"] = "x" },
		"platform outside list": func(s *MCPSandboxSpec) { s.PlatformEnv["WARDYN_RUN_TOKEN"] = "x" },
		"platform docker host":  func(s *MCPSandboxSpec) { s.PlatformEnv["DOCKER_HOST"] = "unix:///var/run/docker.sock" },
		"no proxy config":       func(s *MCPSandboxSpec) { s.ProxyConfig = nil },
	} {
		s := validMCPSandboxSpec()
		edit(&s)
		if err := s.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	s := validMCPSandboxSpec()
	for k := range sandboxCATrustVars() {
		s.PlatformEnv[k] = "/tmp/wardyn/ca-bundle.pem"
	}
	for _, k := range []string{"HTTP_PROXY", "http_proxy", "https_proxy", "no_proxy", "WARDYN_MITM_CA_PEM"} {
		s.PlatformEnv[k] = "x"
	}
	if err := s.Validate(); err != nil {
		t.Errorf("the closed platform list: %v", err)
	}
}

// cutFake records each call; its agent methods answer agentErr, and StopMCPProxies mcpErr.
type cutFake struct {
	calls            []string
	agentErr, mcpErr error
}

func (f *cutFake) StopProxy(context.Context, string) error {
	f.calls = append(f.calls, "StopProxy")
	return f.agentErr
}
func (f *cutFake) StopProxyAt(context.Context, string, int) error {
	f.calls = append(f.calls, "StopProxyAt")
	return f.agentErr
}
func (f *cutFake) EndSandbox(context.Context, string) error {
	f.calls = append(f.calls, "EndSandbox")
	return f.agentErr
}
func (f *cutFake) EndSandboxAt(context.Context, string, int) error {
	f.calls = append(f.calls, "EndSandboxAt")
	return f.agentErr
}
func (f *cutFake) FenceProxy(context.Context, string) error {
	f.calls = append(f.calls, "FenceProxy")
	return f.agentErr
}
func (f *cutFake) StopLostProxyAt(context.Context, string, int) error {
	f.calls = append(f.calls, "StopLostProxyAt")
	return f.agentErr
}
func (f *cutFake) StopMCPProxies(_ context.Context, ref string) error {
	f.calls = append(f.calls, "StopMCPProxies:"+ref)
	return f.mcpErr
}

// agentOnly is a receiver that cannot stop MCP proxies.
type agentOnly struct{ calls int }

func (a *agentOnly) StopProxy(context.Context, string) error { a.calls++; return nil }

// cutHelpers is every helper as one call shape.
func cutHelpers() map[string]func(context.Context, *cutFake) error {
	return map[string]func(context.Context, *cutFake) error{
		"CutEgress":       func(ctx context.Context, f *cutFake) error { return CutEgress(ctx, f, "ref-1") },
		"CutEgressAt":     func(ctx context.Context, f *cutFake) error { return CutEgressAt(ctx, f, "ref-1", 3) },
		"FenceEgress":     func(ctx context.Context, f *cutFake) error { return FenceEgress(ctx, f, "ref-1") },
		"EndSandboxCut":   func(ctx context.Context, f *cutFake) error { return EndSandboxCut(ctx, f, "ref-1") },
		"EndSandboxCutAt": func(ctx context.Context, f *cutFake) error { return EndSandboxCutAt(ctx, f, "ref-1", 3) },
		"CutLostEgressAt": func(ctx context.Context, f *cutFake) error { return CutLostEgressAt(ctx, f, "ref-1", 3) },
	}
}

// TestCutEgressHelpersStopMCPProxies: each helper calls the agent's method, then stops the run's MCP proxies,
// also after a failed agent call, and answers the agent's error unchanged. A receiver that cannot stop MCP
// proxies is called as before.
func TestCutEgressHelpersStopMCPProxies(t *testing.T) {
	agentErr := errors.Join(ErrEndUnsupported, errors.New("x"))
	for name, call := range cutHelpers() {
		for _, want := range []error{nil, agentErr} {
			f := &cutFake{agentErr: want}
			if got := call(context.Background(), f); got != want {
				t.Errorf("%s: answer %v, want %v unchanged", name, got, want)
			}
			if len(f.calls) != 2 || f.calls[1] != "StopMCPProxies:ref-1" {
				t.Errorf("%s (agent err %v): calls %v", name, want, f.calls)
			}
		}
	}
	a := &agentOnly{}
	if err := CutEgress(context.Background(), a, "ref-1"); err != nil || a.calls != 1 {
		t.Errorf("a receiver without MCP proxies: %v, %d calls", err, a.calls)
	}
}

// TestCutEgressAtLeavesNewerMCPProxies: when a newer revive owns the run, the At helpers answer
// ErrProxyGenerationNewer and leave its MCP proxies running.
func TestCutEgressAtLeavesNewerMCPProxies(t *testing.T) {
	for name, call := range map[string]func(*cutFake) error{
		"CutEgressAt":     func(f *cutFake) error { return CutEgressAt(context.Background(), f, "ref-1", 2) },
		"EndSandboxCutAt": func(f *cutFake) error { return EndSandboxCutAt(context.Background(), f, "ref-1", 2) },
		"CutLostEgressAt": func(f *cutFake) error { return CutLostEgressAt(context.Background(), f, "ref-1", 2) },
	} {
		f := &cutFake{agentErr: ErrProxyGenerationNewer}
		if err := call(f); !errors.Is(err, ErrProxyGenerationNewer) {
			t.Errorf("%s: %v", name, err)
		}
		for _, c := range f.calls {
			if strings.HasPrefix(c, "StopMCPProxies") {
				t.Errorf("%s: %v stopped a newer revive's MCP proxies", name, f.calls)
			}
		}
	}
}

// TestCutHelperLogsMCPStopFailure: an MCP stop that fails is logged with the ref and never joined into the
// answer, so the helper answers nil when the agent's call did.
func TestCutHelperLogsMCPStopFailure(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	for name, call := range cutHelpers() {
		buf.Reset()
		f := &cutFake{mcpErr: errors.New("mcp proxy would not stop")}
		if err := call(context.Background(), f); err != nil {
			t.Errorf("%s: answer %v, want nil", name, err)
		}
		if out := buf.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "ref=ref-1") || !strings.Contains(out, "mcp proxy would not stop") {
			t.Errorf("%s: log %q", name, out)
		}
	}
}

// TestCapabilitiesMCPSandboxKindsFailClosed: absent kinds read as none, and the field is left out of the JSON.
func TestCapabilitiesMCPSandboxKindsFailClosed(t *testing.T) {
	var c Capabilities
	if len(c.MCPSandboxKinds) != 0 {
		t.Fatal("zero Capabilities advertises an MCP sandbox kind")
	}
	f, ok := reflect.TypeFor[Capabilities]().FieldByName("MCPSandboxKinds")
	if !ok || f.Tag.Get("json") != "mcp_sandbox_kinds,omitempty" {
		t.Errorf("tag %q", f.Tag.Get("json"))
	}
}
