// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"archive/tar"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/mount"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

func trustedCAOf(t *testing.T, cfg []byte) string {
	t.Helper()
	var c struct {
		TrustedCAPEM string `json:"trusted_ca_pem"`
	}
	if err := json.Unmarshal(cfg, &c); err != nil {
		t.Fatalf("decode proxy config: %v", err)
	}
	return c.TrustedCAPEM
}

// A runner's own bundle reaches its proxy on create AND on revive (a revive rebuilds from the
// organisation's stored config, which carries no CA for a runner), and the sandbox's CA trust after
// whatever the organisation staged, without touching the caller's env map.
func TestRunnerTrustedCA_ProxyOnCreateAndReviveAndSandbox(t *testing.T) {
	const bundle = "runner-ca-pem"
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	d := newWithClient(f, Config{ProxyImage: "wardyn-proxy:dev", InternalNetwork: "wardyn-internal", TrustedCAPEM: bundle})
	ctx := context.Background()
	spec := testSpec()
	spec.Env = map[string]string{"WARDYN_MITM_CA_PEM": "run-ca-pem\norg-ca-pem"}
	sb, err := d.CreateSandbox(ctx, spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	name := proxyContainerName(spec.RunID)
	if got := trustedCAOf(t, f.stdinOf(name)); got != bundle {
		t.Errorf("created proxy trusted_ca_pem = %q, want the runner's bundle %q", got, bundle)
	}
	env := f.containers[sb.Ref].cfg.Env
	for _, want := range []string{"WARDYN_MITM_CA_PEM=run-ca-pem\norg-ca-pem\n" + bundle, "SSL_CERT_FILE=/tmp/wardyn/ca-bundle.pem", "AWS_CA_BUNDLE=/tmp/wardyn/ca-bundle.pem"} {
		if !slices.Contains(env, want) {
			t.Errorf("sandbox env lacks %q: %v", want, env)
		}
	}
	if spec.Env["WARDYN_MITM_CA_PEM"] != "run-ca-pem\norg-ca-pem" || len(spec.Env) != 1 {
		t.Errorf("the caller's env map changed: %v", spec.Env)
	}

	if err := d.StopProxy(ctx, sb.Ref); err != nil {
		t.Fatalf("StopProxy: %v", err)
	}
	stored := renderedTestConfig(t, "fresh")
	if trustedCAOf(t, stored) != "" {
		t.Fatal("the stored config already carries a CA; the revive leg proves nothing")
	}
	if err := d.ReplaceProxy(ctx, sb.Ref, stored); err != nil {
		t.Fatalf("ReplaceProxy: %v", err)
	}
	if got := trustedCAOf(t, f.stdinOf(name)); got != bundle {
		t.Errorf("revived proxy trusted_ca_pem = %q, want the runner's bundle %q", got, bundle)
	}
}

// The runner's bundle file is mounted read-only into every sandbox. runnerd refuses a bundle over
// 64 KiB, so with nothing staged it always fits WARDYN_MITM_CA_PEM. When it and the PEM the
// organisation staged do not fit one environment string, it is left out and rides the mount alone;
// the trust variables still name the file even an older image builds from the staged PEM.
func TestRunnerTrustedCA_LargeBundleRidesTheMountOnly(t *testing.T) {
	trustVars := map[string]string{}
	runner.SetSandboxCATrustVars(trustVars, false)
	for _, tc := range []struct {
		name, staged string
		bundle       int
		appended     bool
	}{
		{name: "a 64 KiB bundle with nothing staged", bundle: runner.MaxEnvTrustedCAPEM, appended: true},
		{name: "10 KiB staged plus a 60 KiB bundle", staged: strings.Repeat("s", 10<<10), bundle: 60 << 10},
		{name: "70 KiB staged plus a 60 KiB bundle", staged: strings.Repeat("s", 70<<10), bundle: 60 << 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			big := strings.Repeat("x", tc.bundle)
			f := newFakeDocker()
			f.images["busybox:latest"] = true
			d := newWithClient(f, Config{ProxyImage: "wardyn-proxy:dev", InternalNetwork: "wardyn-internal", TrustedCAPEM: big, TrustedCAFile: "/state/trusted-ca.pem"})
			spec := testSpec()
			if tc.staged != "" {
				spec.Env = map[string]string{"WARDYN_MITM_CA_PEM": tc.staged}
			}
			sb, err := d.CreateSandbox(context.Background(), spec)
			if err != nil {
				t.Fatalf("CreateSandbox: %v", err)
			}
			agent := f.containers[sb.Ref]
			if !slices.ContainsFunc(agent.host.Mounts, func(m mount.Mount) bool {
				return m.Type == mount.TypeBind && m.Source == "/state/trusted-ca.pem" && m.Target == runner.TrustedCAMountPath && m.ReadOnly
			}) {
				t.Errorf("sandbox mounts = %+v; want the bundle file read-only at %s", agent.host.Mounts, runner.TrustedCAMountPath)
			}
			env := map[string]string{}
			for _, kv := range agent.cfg.Env {
				k, v, _ := strings.Cut(kv, "=")
				env[k] = v
			}
			if pem := env["WARDYN_MITM_CA_PEM"]; tc.appended != strings.Contains(pem, big) || !tc.appended && pem != tc.staged {
				t.Errorf("WARDYN_MITM_CA_PEM has %d bytes (staged %d, bundle %d); want the bundle appended: %v", len(pem), len(tc.staged), len(big), tc.appended)
			}
			for k, v := range trustVars {
				if env[k] != v {
					t.Errorf("%s = %q; want %q", k, env[k], v)
				}
			}
			if got := trustedCAOf(t, f.stdinOf(proxyContainerName(spec.RunID))); got != big {
				t.Errorf("proxy trusted_ca_pem has %d bytes, want the whole %d-byte bundle (stdin has no size bound)", len(got), len(big))
			}
		})
	}
}

// The bundle's mount sits off every managed-file walk, so a run that delivers a component secret
// still starts with it and keeps the mount.
func TestRunnerTrustedCA_MountBesideAComponentSecret(t *testing.T) {
	f := newManagedFake()
	f.runDir = &tar.Header{Name: "run/", Typeflag: tar.TypeDir, Mode: 0o755}
	d := newWithClient(f, Config{ProxyImage: "wardyn-proxy:dev", TrustedCAPEM: "runner-ca-pem", TrustedCAFile: "/state/trusted-ca.pem"})
	spec := managedSecretSpec()
	sb, err := d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateSandbox with a component secret and the CA mount: %v", err)
	}
	if len(f.copies) != 1 || !slices.Contains(f.startedNames, agentContainerName(spec.RunID)) {
		t.Fatalf("%d copies, started %v; want the secret delivered and the agent started", len(f.copies), f.startedNames)
	}
	if !slices.ContainsFunc(f.containers[sb.Ref].host.Mounts, func(m mount.Mount) bool {
		return m.Source == "/state/trusted-ca.pem" && m.Target == runner.TrustedCAMountPath && m.ReadOnly
	}) {
		t.Errorf("sandbox mounts = %+v; want the bundle file kept at %s", f.containers[sb.Ref].host.Mounts, runner.TrustedCAMountPath)
	}
}

// install_mitm_ca appends the mounted bundle after the PEM, and runs on the mounted file alone.
func TestAgentRunLib_InstallMITMCAAppendsTheMountedBundle(t *testing.T) {
	lib, err := filepath.Abs(agentRunLibPath)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	corp := filepath.Join(dir, "trusted-ca.pem")
	if err := os.WriteFile(corp, []byte("corp-ca-pem\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ pem, want string }{{"run-ca-pem", "run-ca-pem\ncorp-ca-pem\n"}, {"", "corp-ca-pem\n"}} {
		out := filepath.Join(t.TempDir(), "ca")
		cmd := exec.Command("bash", "-c", `set -euo pipefail; source "$1"; install_mitm_ca`, "lib-test", lib)
		cmd.Env = append(os.Environ(), "WARDYN_MITM_CA_PEM="+tc.pem, "WARDYN_MITM_CA_DIR="+out, "WARDYN_TRUSTED_CA_PATH="+corp)
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("install_mitm_ca: %v\n%s", err, b)
		}
		got, err := os.ReadFile(filepath.Join(out, "mitm-ca.pem"))
		if err != nil || string(got) != tc.want {
			t.Errorf("WARDYN_MITM_CA_PEM=%q: mitm-ca.pem = %q, %v; want %q", tc.pem, got, err, tc.want)
		}
		if bundle, err := os.ReadFile(filepath.Join(out, "ca-bundle.pem")); err != nil || !strings.HasSuffix(string(bundle), tc.want) {
			t.Errorf("WARDYN_MITM_CA_PEM=%q: ca-bundle.pem does not end with the CA file: %v", tc.pem, err)
		}
	}
}
