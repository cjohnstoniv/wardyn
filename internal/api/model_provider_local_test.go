// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func localModelProvider() types.ModelProvider {
	return types.ModelProvider{
		ID: "laptop-gpu", UID: "uid-lm", Kind: types.ModelProviderLocalModel,
		BaseURL: "http://host.docker.internal:11434",
		Local:   &types.LocalModelSettings{Models: []string{"qwen2.5-coder:7b", "llama3.2:3b"}},
		Harnesses: []types.ProviderHarness{
			{Harness: "claude-code", Model: "qwen2.5-coder:7b"},
			{Harness: "codex-cli", Model: "llama3.2:3b"},
		},
	}
}

// TestValidateModelProviders_LocalModel is the local_model write boundary: one
// case per rule.
func TestValidateModelProviders_LocalModel(t *testing.T) {
	with := func(edit func(*types.ModelProvider)) *types.ModelProviders {
		p := localModelProvider()
		edit(&p)
		return providerBlock(p)
	}
	authAt := func(url string) *types.ModelProviders {
		return with(func(p *types.ModelProvider) { p.BaseURL, p.Auth = url, &types.ProviderAuth{} })
	}
	const cleartext = "use https:// to send a token off this machine"
	for _, tc := range []struct {
		name  string
		block *types.ModelProviders
		want  string
	}{
		{"a local model server on the laptop host", with(func(*types.ModelProvider) {}), ""},
		{"a node address over https with auth", with(func(p *types.ModelProvider) {
			p.BaseURL, p.Auth = "https://10.0.4.7:8000", &types.ProviderAuth{}
		}), ""},
		// A token crosses plain http only to the Docker host or a private address.
		{"auth over http to host.docker.internal", authAt("http://host.docker.internal:11434"), ""},
		{"auth over http to gateway.docker.internal", authAt("http://gateway.docker.internal:11434"), ""},
		{"auth over http to 10/8", authAt("http://10.0.4.7:11434"), ""},
		{"auth over http to 172.16/12", authAt("http://172.31.0.9:11434"), ""},
		{"auth over http to 192.168/16", authAt("http://192.168.1.20:11434"), ""},
		{"auth over http to 100.64/10", authAt("http://100.100.1.2:11434"), ""},
		{"auth over https to a public name", authAt("https://gpu.example.com"), ""},
		{"no auth over http to a public name", with(func(p *types.ModelProvider) { p.BaseURL = "http://gpu.example.com:11434" }), ""},
		{"auth over http to a public name", authAt("http://gpu.example.com:11434"), cleartext},
		{"auth over http to a public literal", authAt("http://203.0.113.9:11434"), cleartext},
		{"auth over http just outside 172.16/12", authAt("http://172.32.0.1:11434"), cleartext},
		{"auth over http just outside 100.64/10", authAt("http://100.128.0.1:11434"), cleartext},
		{"auth over http to a private IPv6 literal", authAt("http://[fd00::7]:11434"), cleartext},
		{"auth over http to a docker.internal lookalike", authAt("http://host.docker.internal.example.com:11434"), cleartext},
		{"bounds set", with(func(p *types.ModelProvider) {
			p.Local.MaxContext, p.Local.MaxOutput, p.Local.KeepAliveSeconds = 8192, 4096, 600
		}), ""},
		{"no base url", with(func(p *types.ModelProvider) { p.BaseURL = "" }), "base_url is required for a local_model"},
		{"localhost", with(func(p *types.ModelProvider) { p.BaseURL = "http://localhost:11434" }), "proxy's own loopback"},
		{"a loopback literal", with(func(p *types.ModelProvider) { p.BaseURL = "http://127.0.0.1:11434" }), "loopback"},
		{"a path", with(func(p *types.ModelProvider) { p.BaseURL = "http://host.docker.internal:11434/ollama" }), "no path"},
		{"another scheme", with(func(p *types.ModelProvider) { p.BaseURL = "ftp://host.docker.internal" }), "https://"},
		{"no allowlist", with(func(p *types.ModelProvider) { p.Local = nil }), "needs local.models"},
		{"an empty allowlist", with(func(p *types.ModelProvider) { p.Local.Models = nil }), "needs local.models"},
		{"a cloud model", with(func(p *types.ModelProvider) { p.Local.Models = append(p.Local.Models, "gpt-oss:120b-cloud") }), "no cloud tag"},
		{"a :cloud tag", with(func(p *types.ModelProvider) { p.Local.Models = append(p.Local.Models, "kimi:cloud") }), "no cloud tag"},
		{"a registry host", with(func(p *types.ModelProvider) { p.Local.Models = append(p.Local.Models, "registry.example.com/ns/m:1") }), "a registry or namespace"},
		{"a namespace", with(func(p *types.ModelProvider) { p.Local.Models = append(p.Local.Models, "hf.co/x") }), "a registry or namespace"},
		{"a duplicate", with(func(p *types.ModelProvider) { p.Local.Models = append(p.Local.Models, "llama3.2:3b") }), "twice"},
		{"too many", with(func(p *types.ModelProvider) {
			for i := range maxLocalModels {
				p.Local.Models = append(p.Local.Models, "m"+strings.Repeat("x", i))
			}
		}), "at most"},
		{"a negative bound", with(func(p *types.ModelProvider) { p.Local.MaxOutput = -1 }), "local.max_output must be between"},
		{"a keep-alive over a day", with(func(p *types.ModelProvider) { p.Local.KeepAliveSeconds = maxLocalKeepAlive + 1 }), "local.keep_alive_seconds"},
		{"a harness model off the allowlist", with(func(p *types.ModelProvider) { p.Harnesses[0].Model = "mistral:7b" }), "must be one of local.models"},
		{"a harness with no model", with(func(p *types.ModelProvider) { p.Harnesses[1].Model = "" }), "must be one of local.models"},
		{"a per-harness path", with(func(p *types.ModelProvider) { p.Harnesses[0].Path = "/v1" }), "apply only to a custom_endpoint"},
		{"local settings on another kind", providerBlock(func() types.ModelProvider {
			p := keyProvider("a", "claude-code")
			p.Local = &types.LocalModelSettings{Models: []string{"m"}}
			return p
		}()), "local applies only to a local_model"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := validateModelProviders(normalizeModelProviders(tc.block), providerWriteEnv{})
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("refused a valid block: %v", err)
			case tc.want != "" && err == nil:
				t.Fatalf("accepted a block it must refuse (want %q)", tc.want)
			case tc.want != "" && !strings.Contains(err.Error(), tc.want):
				t.Fatalf("refusal = %q, want it to carry %q", err, tc.want)
			}
		})
	}
	// No auth stays no auth; auth given gets the default scheme.
	b := normalizeModelProviders(with(func(*types.ModelProvider) {}))
	if b.Providers[0].Auth != nil {
		t.Fatalf("auth = %+v, want none", b.Providers[0].Auth)
	}
	b = normalizeModelProviders(with(func(p *types.ModelProvider) { p.Auth = &types.ProviderAuth{} }))
	if a := b.Providers[0].Auth; a.Header != "Authorization" || a.Format != "Bearer %s" {
		t.Fatalf("auth = %+v, want the default scheme", a)
	}
}

// TestProviderDispatch_LocalModel: a run on a local model sends the proxy its
// upstream and its allowlist; with no auth set nothing is read, authored,
// injected or allowlisted, and with auth set the owner's own token is.
func TestProviderDispatch_LocalModel(t *testing.T) {
	for _, tc := range []struct {
		name   string
		agent  string
		auth   bool
		grants int
	}{
		{"claude-code, no auth", "claude-code", false, 0},
		{"codex-cli, no auth", "codex-cli", false, 0},
		{"claude-code, auth set", "claude-code", true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := localModelProvider()
			if tc.auth {
				p.Auth = &types.ProviderAuth{Header: "Authorization", Format: "Bearer %s"}
			}
			st := &bearerGuardStore{
				run:  types.AgentRun{ID: uuid.New(), Agent: tc.agent, CreatedBy: mpOwner, ModelProviderID: p.ID},
				site: types.SiteConfig{ModelProviders: providerBlock(p)},
			}
			h, _ := bearerGuardHarness(t, st)
			if tc.auth {
				h = mpHarness(t, st, p)
			}
			policy := types.RunPolicySpec{}
			plan, env, grants, ok := mpDispatch(t, h, st, &policy, nil)
			if !ok || len(grants) != tc.grants {
				t.Fatalf("dispatch ok=%v grants=%d, want %d", ok, len(grants), tc.grants)
			}
			if tc.auth {
				if sc := grantScope(t, grants[0]); sc["host"] != "host.docker.internal" || sc["header"] != "Authorization" {
					t.Errorf("grant scope = %v, want the owner's token on the model server", sc)
				}
			}
			// The injector needs its host allowlisted exactly; with no grant
			// there is nothing to allow.
			if slices.Contains(policy.AllowedDomains, "host.docker.internal") != tc.auth {
				t.Errorf("allowlist = %v, want the model server only when a grant rides to it", policy.AllowedDomains)
			}
			vendor := "api.anthropic.com"
			if tc.agent == "codex-cli" {
				vendor = "api.openai.com"
			}
			if plan.llmUpstreams[vendor] != p.BaseURL {
				t.Errorf("upstreams = %v, want %s -> %s", plan.llmUpstreams, vendor, p.BaseURL)
			}
			lm, found := plan.localModels[p.BaseURL]
			if !found || !slices.Equal(lm.Models, p.Local.Models) || lm.MaxContext != types.DefaultLocalMaxContext ||
				lm.MaxOutput != types.DefaultLocalMaxOutput || lm.KeepAliveSeconds != types.DefaultLocalKeepAliveSeconds {
				t.Errorf("local models = %+v, want the allowlist with the default bounds", plan.localModels)
			}
			switch tc.agent {
			case "claude-code":
				for _, k := range []string{"ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL"} {
					if env[k] != "qwen2.5-coder:7b" {
						t.Errorf("%s = %q, want the allowlisted model", k, env[k])
					}
				}
			case "codex-cli":
				if env["WARDYN_CODEX_MODEL"] != "llama3.2:3b" || env["WARDYN_CODEX_BASE_URL"] != "http://wardyn-proxy:3128/wardyn/llm/openai/v1" {
					t.Errorf("codex env = %v, want the allowlisted model on the brokered /v1 route", env)
				}
			}
		})
	}
}
