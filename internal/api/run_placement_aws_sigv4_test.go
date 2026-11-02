// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

const awsSigV4Path = "ProxyConfig.AWSSigV4"

// resignedLocalRun is a bedrock_sso run on the person's own runner with WARDYN_AWS_SIGV4_AT_PROXY
// on, composed the way dispatch composes it (the provider arm, splitSecretEnv, the re-signer's grant
// through createDispatchGrant).
func resignedLocalRun(t *testing.T) (*componentFixture, awsSSOBlob, types.AgentRun, runner.SandboxSpec, llmTransport) {
	t.Helper()
	f, blob := bedrockSSOLocalFixture(t, true)
	f.srv.cfg.AWSSigV4AtProxy = true
	ctx := context.Background()
	run := types.AgentRun{ID: uuid.New(), Agent: "claude-code", CreatedBy: awsSSOTestOwner, Placement: types.PlacementLocal}
	policy := &types.RunPolicySpec{}
	env := map[string]string{"WARDYN_TASK_MODE": "agent"}
	llm := f.srv.providerBedrockTransport(ctx, run, policy, env, chosenProvider{provider: awsSSOTestProvider(), owner: awsSSOTestOwner}, blob)
	if !llm.bedrockReady || len(llm.bedrock.sigv4Hosts) != 2 {
		t.Fatalf("transport ready=%v sigv4 hosts %v, want a re-signed lane on two hosts", llm.bedrockReady, llm.bedrock.sigv4Hosts)
	}
	spec := runner.SandboxSpec{RunID: run.ID, ProxyConfig: runner.ProxyConfig{Policy: *policy}}
	spec.SecretEnv = splitSecretEnv(env, llm.secretEnvKeys)
	spec.Env = env
	injections, mitm, sigv4, ok := f.srv.authorBedrockSSOInjection(ctx, run, llm, llm.providerAWSScope(), nil)
	if !ok || sigv4 == nil {
		t.Fatalf("re-signer grant not authored for a local run: ok=%v %+v", ok, sigv4)
	}
	spec.ProxyConfig.Injection, spec.ProxyConfig.MITMHosts, spec.ProxyConfig.AWSSigV4 = injections, mitm, sigv4
	return f, blob, run, spec, llm
}

// With the switch on, a bedrock_sso run on the person's own runner is re-signed at that runner's
// proxy: the dispatch-time classification admits it with the config unchanged, a revive classifies
// it the same way, and nothing the sandbox or the proxy config holds is a credential.
func TestLocalBedrockSSOResignedOnAPersonsRunner(t *testing.T) {
	f, blob, run, spec, llm := resignedLocalRun(t)
	ctx := context.Background()
	if g := f.st.grants; len(g) != 1 || g[0].ID != spec.ProxyConfig.AWSSigV4.GrantID || g[0].Delivery != types.GrantDeliveryOwn || !g[0].Spec.OwnerOnly {
		t.Fatalf("re-signer grant is not the one own grant: %+v", g)
	}
	p, err := f.srv.localResolvedPlan(ctx, run, false, f.st.siteConfig, spec, nil, llm)
	if err != nil {
		t.Fatal(err)
	}
	if o := p.Origins[awsSigV4Path]; o.Class != placement.ClassOwn || !o.Stored || !o.OwnNamespace || !o.OwnerOnly {
		t.Fatalf("re-signer origin = %+v, want own, stored, in the owner's namespace", o)
	}
	out, ref := placement.LocalEligibility(p)
	if ref != nil || out.ProxyConfig.AWSSigV4 != spec.ProxyConfig.AWSSigV4 {
		t.Fatalf("a re-signed run on the person's own runner: refusal %v, config %+v", ref, out.ProxyConfig.AWSSigV4)
	}
	if llm.bedrockAudit.signing == nil {
		t.Fatal("no aws_signing on the run.bedrock.configure row")
	}
	audit := map[string]any{}
	awsSigningAudit(audit, *llm.bedrockAudit.signing)
	if audit["aws_signing"] != "proxy" {
		t.Errorf("run.bedrock.configure aws_signing = %v, want proxy", audit)
	}

	// The negatives: no role or SSO credential in the sandbox, no route through the portal.
	// No AWS key, secret or token variable at all: the access key pair and the session token among them.
	for _, env := range []map[string]string{spec.Env, spec.SecretEnv} {
		for k := range env {
			if strings.HasPrefix(k, "AWS_") && (strings.Contains(k, "KEY") || strings.Contains(k, "SECRET") || strings.Contains(k, "TOKEN")) {
				t.Errorf("%s reaches the sandbox", k)
			}
		}
	}
	if len(spec.ProxyConfig.Injection) != 0 {
		t.Errorf("a re-signed lane carries injection rules: %+v", spec.ProxyConfig.Injection)
	}
	for _, h := range spec.ProxyConfig.MITMHosts {
		for _, signed := range spec.ProxyConfig.AWSSigV4.Hosts {
			if strings.HasPrefix(h, signed) || strings.Contains(h, "portal.sso.") {
				t.Errorf("MITM entry %q for a re-signed lane", h)
			}
		}
	}
	files := decodeSSOFiles(t, spec.Env[awsSSOConfigEnvVar])
	if !strings.Contains(files[".aws/"+awsSigV4CredentialFile], proxy.AWSSigV4PlaceholderAccessKeyID) {
		t.Errorf("staged ~/.aws = %q, want the placeholder pair", files)
	}
	raw, err := runner.BuildProxyConfig(run.ID, spec.ProxyConfig, runner.ProxyListenPort)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{blob.AccessToken, blob.RefreshToken, blob.ClientID, blob.ClientSecret} {
		for path, body := range files {
			if strings.Contains(body, secret) {
				t.Errorf("staged %s carries session material %q", path, secret)
			}
		}
		if strings.Contains(string(raw), secret) {
			t.Errorf("the proxy config carries session material %q", secret)
		}
	}

	// Revive rebuilds the config from what the proxy was handed and classifies it the same way.
	stored := localStoredSpec(runner.ProxyConfigOf(run.ID, spec.ProxyConfig, runner.ProxyListenPort))
	rp, err := f.srv.localResolvedPlan(ctx, run, false, f.st.siteConfig, stored, nil, llmTransport{})
	if err != nil {
		t.Fatal(err)
	}
	if out, ref := placement.LocalEligibility(rp); ref != nil || out.ProxyConfig.AWSSigV4 == nil || out.ProxyConfig.AWSSigV4.GrantID != spec.ProxyConfig.AWSSigV4.GrantID {
		t.Fatalf("revive of a re-signed run: refusal %v, config %+v", ref, out.ProxyConfig.AWSSigV4)
	}
}

// The re-signer's provenance is the grant dispatch wrote for it, matched on every field: a config
// naming another grant, portal, account, role or set of hosts keeps no origin and is refused; one
// listing the same hosts in another order is the same set. A snapshot of another person's session or
// a shared one gets no grant row at all.
func TestLocalAWSSigV4ProvenanceIsBoundToItsGrant(t *testing.T) {
	f, _, run, spec, llm := resignedLocalRun(t)
	ctx := context.Background()
	good := *spec.ProxyConfig.AWSSigV4
	const other = "bedrock-agent-runtime.us-east-1.amazonaws.com"
	judge := func(c proxy.AWSSigV4Config) (bool, *placement.Refusal) {
		t.Helper()
		spec.ProxyConfig.AWSSigV4 = &c
		p, err := f.srv.localResolvedPlan(ctx, run, false, f.st.siteConfig, spec, nil, llm)
		if err != nil {
			t.Fatal(err)
		}
		_, stamped := p.Origins[awsSigV4Path]
		_, ref := placement.LocalEligibility(p)
		return stamped, ref
	}
	reordered := good
	reordered.Hosts = []string{good.Hosts[1], good.Hosts[0]}
	if stamped, ref := judge(reordered); !stamped || ref != nil {
		t.Fatalf("the same hosts in another order: stamped=%v refusal %v, want admitted", stamped, ref)
	}
	for name, mutate := range map[string]func(*proxy.AWSSigV4Config){
		"a grant not the run's": func(c *proxy.AWSSigV4Config) { c.GrantID = uuid.New() },
		"another portal":        func(c *proxy.AWSSigV4Config) { c.PortalHost = "portal.sso.eu-west-1.amazonaws.com" },
		"another account":       func(c *proxy.AWSSigV4Config) { c.AccountID = "210987654321" },
		"another role":          func(c *proxy.AWSSigV4Config) { c.RoleName = "SomeoneElsesRole" },
		"a host dropped":        func(c *proxy.AWSSigV4Config) { c.Hosts = c.Hosts[:1] },
		"a host added":          func(c *proxy.AWSSigV4Config) { c.Hosts = append(c.Hosts, other) },
		"a host swapped":        func(c *proxy.AWSSigV4Config) { c.Hosts = []string{c.Hosts[0], other} },
		"no hosts":              func(c *proxy.AWSSigV4Config) { c.Hosts = nil },
	} {
		c := good
		c.Hosts = slices.Clone(good.Hosts)
		mutate(&c)
		if stamped, ref := judge(c); stamped || ref == nil || ref.Reason != placement.ReasonPlacementCredential ||
			ref.Field != awsSigV4Path || ref.Detail != "credential provenance is unknown" {
			t.Errorf("%s: stamped=%v refusal %v, want no origin and the unknown-provenance refusal", name, stamped, ref)
		}
	}

	stored := f.st.grants[0]
	for name, field := range map[string][2]string{
		"another person's session": {"owner_subject", "mallory@example.com"},
		"a shared session":         {"credential_source", string(types.CredentialSourceShared)},
	} {
		var scope map[string]any
		if err := json.Unmarshal(stored.Spec.Scope, &scope); err != nil {
			t.Fatal(err)
		}
		scope["snapshot"].(map[string]any)[field[0]] = field[1]
		raw, err := json.Marshal(scope)
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.srv.createDispatchGrant(ctx, run, types.CredentialGrant{ID: uuid.New(), RunID: run.ID,
			Spec: types.GrantSpec{Kind: types.GrantAPIKey, Scope: raw, TTLSeconds: stored.Spec.TTLSeconds}}, false)
		var ref *placement.Refusal
		if !errors.As(err, &ref) || ref.Reason != placement.ReasonPlacementCredential || ref.Detail != "credential is not available in its owner's namespace" {
			t.Errorf("%s: createDispatchGrant = %v, want the owner's-namespace refusal", name, err)
		}
		if n := len(f.st.grants); n != 1 {
			t.Errorf("%s: %d grant rows, want only the run owner's own", name, n)
		}
	}
}
