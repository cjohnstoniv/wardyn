// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package conformance_test

import (
	"bytes"
	"cmp"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/network"
	dockerclient "github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/hoptls"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/docker"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// runCACN names the stand-in per-run interception CA, so a sandbox curl can tell a certificate the
// proxy minted from one the corporate CA signed.
const runCACN = "Wardyn conformance run CA"

// TestRunnerTrustedCADocker drives the REAL wardyn-proxy image through the docker driver the way a
// laptop runner does (docker.Config.TrustedCAPEM, from --trusted-ca-file), against two TLS upstreams
// whose certificates a stand-in corporate CA signed, with the run's own interception CA live at the
// same time:
//
//   - an intercepted host (MITMHosts): the sandbox trusts the run's CA, and the PROXY must trust the
//     corporate CA when it dials the upstream;
//   - a tunneled host: the SANDBOX's own curl must trust the corporate CA.
//
// With the bundle both succeed; without it both fail.
func TestRunnerTrustedCADocker(t *testing.T) {
	if os.Getenv("WARDYN_TEST_DOCKER") != "1" {
		t.Skip("WARDYN_TEST_DOCKER=1 not set; skipping the runner corporate-CA case")
	}
	cli, err := dockerclient.New(dockerclient.FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	ensureConformanceNetwork(t, "wardyn-internal")

	now := time.Now()
	cp, corp := mustHopCA(t, now), mustHopCA(t, now)
	id := uuid.NewString()[:8]
	cpHost := "wardyn-cafake-cp-" + id
	mitmHost, tunnelHost := "mitm-"+id+".corp.test", "tunnel-"+id+".corp.test"
	startHopFake(t, cli, cp, cpHost, uuid.New(), "ca-run-token")
	// The corporate upstreams sit on their own private network, a stand-in for the corporate network
	// the runner reaches. The proxy joins it only once running: a network it held at start is one of
	// its own subnets, which internal_hosts never lifts.
	corpNet := "wardyn-corpca-" + id
	if _, err := cli.NetworkCreate(context.Background(), corpNet, dockerclient.NetworkCreateOptions{Driver: "bridge"}); err != nil {
		t.Fatalf("create %s: %v", corpNet, err)
	}
	t.Cleanup(func() { _, _ = cli.NetworkRemove(context.Background(), corpNet, dockerclient.NetworkRemoveOptions{}) })
	for _, host := range []string{mitmHost, tunnelHost} {
		fake := startHopFake(t, cli, corp, host, uuid.New(), "ca-run-token")
		if _, err := cli.NetworkConnect(context.Background(), corpNet, dockerclient.NetworkConnectOptions{
			Container: fake, EndpointConfig: &network.EndpointSettings{Aliases: []string{host}}}); err != nil {
			t.Fatalf("connect %s: %v", host, err)
		}
		if _, err := cli.NetworkDisconnect(context.Background(), "wardyn-internal", dockerclient.NetworkDisconnectOptions{Container: fake}); err != nil {
			t.Fatalf("disconnect %s: %v", host, err)
		}
	}
	runCert, runKey := runCAPEM(t, now)
	bundle, _, err := runner.TrustedCABundle(corp.CertPEM)
	if err != nil {
		t.Fatal(err)
	}
	proxyImage := cmp.Or(os.Getenv("WARDYN_TEST_PROXY_IMAGE"), bootEgressProxyImage)
	spec := func() runner.SandboxSpec {
		proxyURL := "http://wardyn-proxy:" + strconv.Itoa(runner.ProxyListenPort)
		// What dispatch stages for an intercepted run: proxy env and the run's own CA.
		env := map[string]string{"HTTPS_PROXY": proxyURL, "https_proxy": proxyURL, "WARDYN_MITM_CA_PEM": runCert}
		runner.SetSandboxCATrustVars(env, false)
		return runner.SandboxSpec{
			RunID:            uuid.New(),
			Image:            "wardyn/agent-claude-code:local",
			ConfinementClass: types.CC1,
			Labels:           map[string]string{"wardyn.conformance": "true"},
			Env:              env,
			ProxyConfig: runner.ProxyConfig{
				RunToken:          "ca-run-token",
				ControlPlaneURL:   "https://" + cpHost + ":8443",
				ControlPlaneCAPEM: string(cp.CertPEM),
				MITMCACertPEM:     runCert,
				MITMCAKeyPEM:      runKey,
				MITMHosts:         []string{mitmHost + ":8443"},
				InternalHosts:     []types.InternalHost{{HostSuffix: "corp.test"}},
				Policy:            types.RunPolicySpec{AllowedDomains: []string{mitmHost, tunnelHost}, MinConfinementClass: types.CC1},
			},
		}
	}
	// start runs a sandbox the way runnerd composes the driver: the bundle for the proxy, and the
	// same bundle as a file the sandbox mounts.
	start := func(t *testing.T, trusted string) string {
		t.Helper()
		var file string
		if trusted != "" {
			file = filepath.Join(t.TempDir(), "trusted-ca.pem")
			if err := os.WriteFile(file, []byte(trusted), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		sub, err := docker.New(docker.Config{ProxyImage: proxyImage, TrustedCAPEM: trusted, TrustedCAFile: file})
		if err != nil {
			t.Fatalf("docker.New: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		s := spec()
		sb, err := sub.CreateSandbox(ctx, s)
		if err != nil {
			t.Fatalf("CreateSandbox: %v", err)
		}
		t.Cleanup(func() { _ = sub.KillSandbox(context.Background(), sb.Ref) })
		if _, err := cli.NetworkConnect(ctx, corpNet, dockerclient.NetworkConnectOptions{
			Container: "wardyn-proxy-" + s.RunID.String(), EndpointConfig: &network.EndpointSettings{}}); err != nil {
			t.Fatalf("connect the proxy to %s: %v", corpNet, err)
		}
		// The idle main process assembles the CA bundle the curl below reads.
		for deadline := time.Now().Add(30 * time.Second); ; {
			if code, _ := sandboxExec(t, cli, sb.Ref, "sh", "-c", "test -s /tmp/wardyn/ca-bundle.pem"); code == 0 {
				return sb.Ref
			}
			if time.Now().After(deadline) {
				t.Fatal("the sandbox never assembled /tmp/wardyn/ca-bundle.pem")
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
	curl := func(t *testing.T, ref, host string) (int, string) {
		t.Helper()
		return sandboxExec(t, cli, ref, "curl", "-v", "-sS", "-f", "--max-time", "20", "-o", "/dev/null", "-w", "status=%{http_code}\n", "https://"+host+":8443/")
	}

	t.Run("with the runner's bundle", func(t *testing.T) {
		ref := start(t, bundle)
		code, out := curl(t, ref, mitmHost)
		if code != 0 || !strings.Contains(out, "status=202") || !strings.Contains(out, runCACN) {
			t.Fatalf("intercepted host: exit %d; want 202 through the proxy's interception (issuer %q), so the proxy trusted the corporate CA:\n%s", code, runCACN, out)
		}
		code, out = curl(t, ref, tunnelHost)
		if code != 0 || !strings.Contains(out, "status=202") || strings.Contains(out, runCACN) {
			t.Fatalf("tunneled host: exit %d; want 202 on the upstream's own certificate, so the sandbox trusted the corporate CA:\n%s", code, out)
		}
	})

	// Over 128 KiB no environment string can carry it: the sandbox trusts it through the mounted file
	// alone, and the proxy through its stdin config.
	t.Run("a bundle over 128 KiB", func(t *testing.T) {
		large := bundle
		for i := 0; len(large) <= 128<<10; i++ {
			c, _ := runCAPEM(t, now.Add(time.Duration(i)))
			large += c
		}
		ref := start(t, large)
		if code, out := sandboxExec(t, cli, ref, "sh", "-c", `[ -z "$WARDYN_MITM_CA_PEM" ] || printf %s "$WARDYN_MITM_CA_PEM" | grep -c BEGIN`); code != 0 || strings.TrimSpace(out) != "1" {
			t.Fatalf("WARDYN_MITM_CA_PEM should hold only the run's CA (the bundle rides the file): exit %d %q", code, out)
		}
		code, out := curl(t, ref, mitmHost)
		if code != 0 || !strings.Contains(out, "status=202") || !strings.Contains(out, runCACN) {
			t.Fatalf("intercepted host with a %d-byte bundle: exit %d:\n%s", len(large), code, out)
		}
		if code, out = curl(t, ref, tunnelHost); code != 0 || !strings.Contains(out, "status=202") {
			t.Fatalf("tunneled host with a %d-byte bundle: exit %d:\n%s", len(large), code, out)
		}
	})

	t.Run("without it", func(t *testing.T) {
		ref := start(t, "")
		code, out := curl(t, ref, mitmHost)
		// The proxy reached the upstream and refused its certificate: not a policy or guard denial.
		if code == 0 || strings.Contains(out, "X-Wardyn-Egress: denied") || !strings.Contains(out, "< HTTP/1.1 502") {
			t.Fatalf("intercepted host: exit %d; want the proxy's 502 for an upstream certificate it does not trust:\n%s", code, out)
		}
		// TODO(lane B, 0.9.3): once B's refusal body is on main, also assert the proxy answered the
		// shared "tls_untrusted" reason here (the lead finishes this at integration).
		if code, out = curl(t, ref, tunnelHost); code != 60 {
			t.Fatalf("tunneled host: exit %d; want curl's 60 (certificate not trusted) with no corporate CA in the sandbox:\n%s", code, out)
		}
	})
}

func mustHopCA(t *testing.T, now time.Time) *hoptls.CA {
	t.Helper()
	blob, err := hoptls.NewCA(now)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := hoptls.ParseCA(blob)
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

// runCAPEM is a stand-in per-run interception CA, in the form dispatch mints it.
func runCAPEM(t *testing.T, now time.Time) (certPEM, keyPEM string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(now.UnixNano()), Subject: pkix.Name{CommonName: runCACN}, NotBefore: now.Add(-time.Hour),
		NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
}

// sandboxExec runs argv in the sandbox with its own environment and returns the exit code and output.
func sandboxExec(t *testing.T, cli *dockerclient.Client, ref string, argv ...string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	ex, err := cli.ExecCreate(ctx, ref, dockerclient.ExecCreateOptions{Cmd: argv, AttachStdout: true, AttachStderr: true})
	if err != nil {
		t.Fatalf("ExecCreate: %v", err)
	}
	att, err := cli.ExecAttach(ctx, ex.ID, dockerclient.ExecAttachOptions{})
	if err != nil {
		t.Fatalf("ExecAttach: %v", err)
	}
	var out bytes.Buffer
	_, _ = stdcopy.StdCopy(&out, &out, att.Reader)
	att.Close()
	for {
		insp, err := cli.ExecInspect(ctx, ex.ID, dockerclient.ExecInspectOptions{})
		if err != nil {
			t.Fatalf("ExecInspect: %v", err)
		}
		if !insp.Running {
			return insp.ExitCode, out.String()
		}
		time.Sleep(100 * time.Millisecond)
	}
}
