// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestProxyConfigIsNeverInTheContainer_RealDocker is #1176 on a real daemon,
// with the real wardyn-proxy image (WARDYN_TEST_PROXY_IMAGE, a build under a
// tag of its own). The proxy runs on the config it was handed on stdin, and
// neither the running nor the stopped container's config (Env, Cmd,
// Entrypoint) holds the run token, the MITM CA key or the upstream-proxy
// credential. A stopped proxy started again gets no config and exits
// non-zero, as does one started with no config at all. At the run's end the
// proxy container is removed.
func TestProxyConfigIsNeverInTheContainer_RealDocker(t *testing.T) {
	if os.Getenv("WARDYN_TEST_DOCKER") != "1" {
		t.Skip("set WARDYN_TEST_DOCKER=1 to run the real-Docker proxy config test")
	}
	img := os.Getenv("WARDYN_TEST_PROXY_IMAGE")
	if img == "" {
		t.Skip("set WARDYN_TEST_PROXY_IMAGE to a wardyn-proxy build of this tree")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	cpNet := "wardyn-test-cp-" + uuid.NewString()[:8]
	d, err := New(Config{ProxyImage: img, InternalNetwork: cpNet})
	if err != nil {
		t.Fatalf("docker.New: %v", err)
	}
	ensureNetwork(t, d, cpNet)
	t.Cleanup(func() { _, _ = d.cli.NetworkRemove(context.Background(), cpNet, client.NetworkRemoveOptions{}) })

	runID := uuid.New()
	token := "run-token-" + uuid.NewString()
	caCert, caKey := testCAPEM(t)
	upstreamPass := "upstream-pass-" + uuid.NewString()[:8]
	secrets := []string{token, "PRIVATE KEY", strings.TrimSpace(string(caKey)), upstreamPass}
	sb, err := d.CreateSandbox(ctx, runner.SandboxSpec{
		RunID:            runID,
		Image:            "busybox:latest",
		ConfinementClass: types.CC1,
		ProxyConfig: runner.ProxyConfig{
			RunToken:         token,
			ControlPlaneURL:  "http://127.0.0.1:9",
			Policy:           types.RunPolicySpec{AllowedDomains: []string{"example.com"}},
			MITMCACertPEM:    string(caCert),
			MITMCAKeyPEM:     string(caKey),
			UpstreamProxyURL: "http://proxyuser:" + upstreamPass + "@127.0.0.1:3129",
		},
		Resources: runner.Resources{CPUMillis: 500, MemoryMiB: 128},
	})
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	t.Cleanup(func() { _ = d.KillSandbox(context.Background(), sb.Ref) })
	name := proxyContainerName(runID)

	waitForLog(ctx, t, name, "listening")
	assertNoSecretsAtRest(ctx, t, d, name, "running", secrets)

	timeout := 10
	if _, err := d.cli.ContainerStop(ctx, name, client.ContainerStopOptions{Timeout: &timeout}); err != nil {
		t.Fatalf("stop proxy: %v", err)
	}
	assertNoSecretsAtRest(ctx, t, d, name, "stopped", secrets)

	// Started again by hand, it has no config to read, and must not run.
	if _, err := d.cli.ContainerStart(ctx, name, client.ContainerStartOptions{}); err != nil {
		t.Fatalf("restart proxy: %v", err)
	}
	if code := waitExit(ctx, t, d, name); code == 0 {
		t.Errorf("a restarted proxy with no config exited 0; want non-zero")
	}

	if err := d.EndSandbox(ctx, sb.Ref); err != nil {
		t.Fatalf("EndSandbox: %v", err)
	}
	if _, err := d.cli.ContainerInspect(ctx, name, client.ContainerInspectOptions{}); !isNotFound(err) {
		t.Errorf("proxy container after the run's end: inspect err = %v; want it removed", err)
	}

	// No config at all: the proxy refuses to run.
	bare := "wardyn-test-proxy-noconfig-" + uuid.NewString()[:8]
	resp, err := d.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config:     &container.Config{Image: img, Env: []string{proxyConfigStdinEnv + "=1"}},
		HostConfig: &container.HostConfig{NetworkMode: "none"},
		Name:       bare,
	})
	if err != nil {
		t.Fatalf("create config-less proxy: %v", err)
	}
	t.Cleanup(func() {
		_, _ = d.cli.ContainerRemove(context.Background(), resp.ID, client.ContainerRemoveOptions{Force: true})
	})
	if _, err := d.cli.ContainerStart(ctx, resp.ID, client.ContainerStartOptions{}); err != nil {
		t.Fatalf("start config-less proxy: %v", err)
	}
	if code := waitExit(ctx, t, d, resp.ID); code == 0 {
		t.Errorf("a proxy with no config exited 0; want non-zero")
	}
}

// assertNoSecretsAtRest fails if the container's config names any secret.
func assertNoSecretsAtRest(ctx context.Context, t *testing.T, d *Driver, name, state string, secrets []string) {
	t.Helper()
	res, err := d.cli.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("inspect %s proxy: %v", state, err)
	}
	c := res.Container.Config
	if c == nil {
		t.Fatalf("%s proxy has no config", state)
	}
	fields := map[string][]string{"Env": c.Env, "Cmd": c.Cmd, "Entrypoint": c.Entrypoint}
	for field, vals := range fields {
		joined := strings.Join(vals, "\n")
		for _, s := range secrets {
			if strings.Contains(joined, s) {
				t.Errorf("%s proxy's Config.%s holds a secret (%.20q...)", state, field, s)
			}
		}
	}
	if res.Container.HostConfig != nil && res.Container.HostConfig.RestartPolicy.Name != container.RestartPolicyDisabled {
		t.Errorf("%s proxy restart policy = %q; want %q", state, res.Container.HostConfig.RestartPolicy.Name, container.RestartPolicyDisabled)
	}
}

// waitForLog waits for want in the container's logs, read through a client of
// its own (the driver's dockerAPI has no use for logs).
func waitForLog(ctx context.Context, t *testing.T, name, want string) {
	t.Helper()
	cli, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer func() { _ = cli.Close() }()
	var last string
	for range 60 {
		rc, err := cli.ContainerLogs(ctx, name, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true})
		if err == nil {
			b, _ := io.ReadAll(rc)
			_ = rc.Close()
			last = string(b)
			if strings.Contains(last, want) {
				return
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for %q in %s logs: %v; last logs:\n%s", want, name, ctx.Err(), last)
		case <-time.After(time.Second):
		}
	}
	t.Fatalf("%q never appeared in %s logs:\n%s", want, name, last)
}

// waitExit waits for the container to stop and returns its exit code.
func waitExit(ctx context.Context, t *testing.T, d *Driver, id string) int {
	t.Helper()
	w := d.cli.ContainerWait(ctx, id, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case r := <-w.Result:
		return int(r.StatusCode)
	case err := <-w.Error:
		t.Fatalf("wait for %s: %v", id, err)
	case <-ctx.Done():
		t.Fatalf("wait for %s: %v", id, ctx.Err())
	}
	return 0
}

// testCAPEM mints a throwaway ECDSA CA, as a run's MITM CA is.
func testCAPEM(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test run CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kder})
}
