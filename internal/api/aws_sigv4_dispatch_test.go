// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// With WARDYN_AWS_SIGV4_AT_PROXY on, each AWS sign-in lane dispatches a placeholder key pair
// and hands its session grant to the proxy's re-signer: no ~/.aws, no route to the sign-in
// hosts, no injection rule or MITM entry on the portal. The config loads in this tree's proxy.
func TestAWSSigV4On_LanesDispatchThePlaceholder(t *testing.T) {
	on := func(c *Config) { c.AWSSigV4AtProxy = true }
	for _, tc := range []struct {
		name, portal, region string
		dispatch             func(*testing.T, func(*Config)) runner.SandboxSpec
		account, role        string
		hosts                []string
	}{
		{"bedrock_sso", "portal.sso.us-east-1.amazonaws.com", "", dispatchBedrockSSORun,
			createRenewalBlob().AccountID, createRenewalBlob().RoleName,
			[]string{"bedrock-runtime.us-east-1.amazonaws.com", "bedrock.us-east-1.amazonaws.com"}},
		{"aws_cli", "portal.sso." + cliAWSRow().SSORegion + ".amazonaws.com", cliAWSRow().Region, dispatchAWSCLIRun,
			cliAWSRow().AccountID, cliAWSRow().RoleName, []string{"sts.eu-west-2.amazonaws.com"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := tc.dispatch(t, on)
			pc := spec.ProxyConfig
			g := pc.AWSSigV4
			if g == nil {
				t.Fatal("no aws_sigv4 grant with the switch on")
			}
			if g.GrantID == uuid.Nil || g.PortalHost != tc.portal || g.AccountID != tc.account || g.RoleName != tc.role || !slices.Equal(g.Hosts, tc.hosts) {
				t.Fatalf("aws_sigv4 = %+v, want portal %s account %s role %s hosts %v", *g, tc.portal, tc.account, tc.role, tc.hosts)
			}
			for _, in := range pc.Injection {
				if in.GrantID == g.GrantID || strings.HasPrefix(in.Rule.Host, "portal.sso.") {
					t.Fatalf("the session grant is also an injection rule on %s: the sandbox could redeem it", in.Rule.Host)
				}
			}
			for _, h := range append(slices.Clone(pc.MITMHosts), pc.Policy.AllowedDomains...) {
				if strings.Contains(h, "portal.sso.") || strings.Contains(h, "oidc.") {
					t.Fatalf("a sign-in host is reachable or terminated: %q", h)
				}
			}
			if spec.Env[envAWSProfile] != awsSSOProfileName {
				t.Fatalf("AWS_PROFILE = %q, want %q", spec.Env[envAWSProfile], awsSSOProfileName)
			}
			files := decodeSSOFiles(t, spec.Env[awsSSOConfigEnvVar])
			if len(files) != 1 || files[".aws/config"] != awsSigV4ConfigFileContents(tc.region) {
				t.Fatalf("staged ~/.aws = %q, want only the placeholder profile", files)
			}
			cfg := files[".aws/config"]
			if !strings.Contains(cfg, "credential_process = echo '{") || !strings.Contains(cfg, `"AccessKeyId":"`+proxy.AWSSigV4PlaceholderAccessKeyID+`"`) ||
				strings.Contains(cfg, "sso_") || (tc.region != "" && !strings.Contains(cfg, "region = "+tc.region+"\n")) {
				t.Fatalf("~/.aws/config does not hold just the placeholder profile:\n%s", cfg)
			}
			pc.ControlPlaneURL = "http://127.0.0.1:8080" // the fixture's in-cluster name is refused as plain http
			raw, err := runner.BuildProxyConfig(spec.RunID, pc, runner.ProxyListenPort)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := proxy.LoadConfigBytes(raw); err != nil {
				t.Fatalf("the dispatched config does not load in this proxy: %v", err)
			}
		})
	}
}

// The switch applies only where it can: off, with the session in the sandbox, against a test
// endpoint, or with a host the proxy cannot re-sign, the lane keeps its old path.
func TestAWSSigV4Hosts(t *testing.T) {
	on := Config{AWSSigV4AtProxy: true, AWSSSOProxyInject: true}
	for _, tc := range []struct {
		name  string
		cfg   Config
		hosts []string
		want  []string
	}{
		{"on", on, []string{"bedrock-runtime.us-east-1.amazonaws.com", "Bedrock.us-east-1.amazonaws.com:443", "bedrock-runtime.us-east-1.amazonaws.com."},
			[]string{"bedrock-runtime.us-east-1.amazonaws.com", "bedrock.us-east-1.amazonaws.com"}},
		{"switch off", Config{AWSSSOProxyInject: true}, []string{"sts.amazonaws.com"}, nil},
		{"session in the sandbox", Config{AWSSigV4AtProxy: true}, []string{"sts.amazonaws.com"}, nil},
		{"test endpoint", Config{AWSSigV4AtProxy: true, AWSSSOProxyInject: true, AWSSSOEndpointOverride: "http://fake:8090"}, []string{"sts.amazonaws.com"}, nil},
		{"wildcard", on, []string{"sts.amazonaws.com", "*.s3.us-east-1.amazonaws.com"}, nil},
		{"another port", on, []string{"s3.us-east-1.amazonaws.com:8443"}, nil},
		{"private endpoint", on, []string{"vpce-1.bedrock-runtime.us-east-1.vpce.amazonaws.com"}, nil},
		{"no hosts", on, nil, nil},
	} {
		s := &Server{cfg: tc.cfg}
		got, ok := s.awsSigV4Hosts(tc.hosts)
		if ok != (tc.want != nil) || !slices.Equal(got, tc.want) {
			t.Errorf("%s: awsSigV4Hosts = %v %v, want %v", tc.name, got, ok, tc.want)
		}
	}
}

func TestResolveAWSSigV4AtProxy(t *testing.T) {
	for raw, want := range map[string]bool{"": false, "off": false, "nonsense": false, "on": true, " ON ": true, "true": true, "1": true, "0": false} {
		if got := ResolveAWSSigV4AtProxy(raw); got != want {
			t.Errorf("ResolveAWSSigV4AtProxy(%q) = %v, want %v", raw, got, want)
		}
	}
	if AWSSigV4AtProxyFlagDefault != "off" || ResolveAWSSigV4AtProxy(AWSSigV4AtProxyFlagDefault) {
		t.Fatal("the switch's default must be off")
	}
}

// The profile's credential_process, run by a shell as the AWS SDKs and the CLI run it, prints
// the placeholder pair in the process-credential shape.
func TestAWSSigV4ConfigFile_ProcessPrintsThePlaceholder(t *testing.T) {
	cfg := awsSigV4ConfigFileContents("eu-west-2")
	_, rest, ok := strings.Cut(cfg, "credential_process = ")
	if !ok {
		t.Fatalf("no credential_process:\n%s", cfg)
	}
	cmd, _, _ := strings.Cut(rest, "\n")
	out, err := exec.Command("sh", "-c", cmd).Output()
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Version         int
		AccessKeyID     string `json:"AccessKeyId"`
		SecretAccessKey string
	}
	if err := json.Unmarshal(out, &got); err != nil || got.Version != 1 ||
		got.AccessKeyID != proxy.AWSSigV4PlaceholderAccessKeyID || got.SecretAccessKey != proxy.AWSSigV4PlaceholderSecretAccessKey {
		t.Fatalf("credential_process printed %q (%v)", out, err)
	}
}
