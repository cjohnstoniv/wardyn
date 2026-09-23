// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package conformance_test

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	dockerclient "github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/hoptls"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/docker"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestProxyHopTLSDocker drives the REAL wardyn-proxy image through the docker
// driver against a stand-in control plane (test/conformance/hopfake) that
// serves TLS with a leaf from a Wardyn internal CA (internal/hoptls), exactly
// the shape wardynd's -internal-listen serves:
//
//   - pinned CA, through the driver's own CreateSandbox: the proxy's startup
//     credential resolve arrives over TLS and the proxy stays up;
//   - wrong CA: the proxy refuses to start on x509 and the resolve never lands;
//   - plaintext to a non-loopback host: the proxy refuses at config load.
//
// The two refusals run the same image with the same sealed config
// (runner.BuildProxyConfig, what the driver puts in WARDYN_PROXY_CONFIG_JSON)
// as a bare container: a proxy that dies inside CreateSandbox is rolled back
// with its logs, and the logs are the evidence.
//
// The k8s substrate carries the same sealed config (runner.BuildProxyConfig
// into the per-run Secret); its real-cluster case is test-conformance-k8s.
func TestProxyHopTLSDocker(t *testing.T) {
	if os.Getenv("WARDYN_TEST_DOCKER") != "1" {
		t.Skip("WARDYN_TEST_DOCKER=1 not set; skipping the control-plane TLS hop case")
	}
	cli, err := dockerclient.New(dockerclient.FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	ensureConformanceNetwork(t, "wardyn-internal")

	now := time.Now()
	caBlob, err := hoptls.NewCA(now)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := hoptls.ParseCA(caBlob)
	if err != nil {
		t.Fatal(err)
	}
	host := "wardyn-hopfake-" + uuid.NewString()[:8]
	grant, token := uuid.New(), "hop-tls-run-token"
	fake := startHopFake(t, cli, ca, host, grant, token)
	cpURL := "https://" + host + ":8443"

	sub, err := docker.New(docker.Config{ProxyImage: bootEgressProxyImage})
	if err != nil {
		t.Fatalf("docker.New: %v", err)
	}
	spec := func(url, caPEM string) runner.SandboxSpec {
		return runner.SandboxSpec{
			RunID:            uuid.New(),
			Image:            "busybox:latest",
			ConfinementClass: types.CC1,
			Labels:           map[string]string{"wardyn.conformance": "true"},
			ProxyConfig: runner.ProxyConfig{
				RunToken:          token,
				ControlPlaneURL:   url,
				ControlPlaneCAPEM: caPEM,
				Policy:            types.RunPolicySpec{AllowedDomains: []string{"api.example.com"}, MinConfinementClass: types.CC1},
				Injection:         []runner.InjectionGrant{{GrantID: grant, Rule: egress.InjectionRule{Host: "api.example.com", Header: "X-Api-Key"}}},
			},
		}
	}
	t.Run("pinned CA resolves over TLS", func(t *testing.T) {
		s := spec(cpURL, string(ca.CertPEM))
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		sb, err := sub.CreateSandbox(ctx, s)
		if err != nil {
			t.Fatalf("CreateSandbox: %v", err)
		}
		t.Cleanup(func() { _ = sub.KillSandbox(context.Background(), sb.Ref) })
		proxy := "wardyn-proxy-" + s.RunID.String()
		logs := waitForLog(t, cli, fake, "hopfake: resolve grant="+grant.String(), 60*time.Second)
		if !strings.Contains(logs, "hopfake: resolve grant="+grant.String()+" tls=true") {
			t.Fatalf("the proxy's credential resolve did not arrive over TLS:\n%s", logs)
		}
		time.Sleep(2 * time.Second) // past the startup resolve: a fail-closed proxy has exited by now
		if running, code, plog := proxyState(t, cli, proxy); !running {
			t.Fatalf("proxy exited (code %d) after a pinned-CA resolve:\n%s", code, plog)
		}
		t.Logf("control plane saw:\n%s", logs)
	})

	t.Run("wrong CA fails closed", func(t *testing.T) {
		otherBlob, _ := hoptls.NewCA(now)
		other, _ := hoptls.ParseCA(otherBlob)
		before := strings.Count(containerLogs(t, cli, fake), "hopfake: resolve grant=")
		plog := waitForExit(t, cli, runProxy(t, cli, spec(cpURL, string(other.CertPEM))), 60*time.Second)
		if !strings.Contains(plog, "certificate signed by unknown authority") {
			t.Fatalf("proxy did not fail on the certificate:\n%s", plog)
		}
		if after := strings.Count(containerLogs(t, cli, fake), "hopfake: resolve grant="); after != before {
			t.Fatalf("a resolve reached the control plane through a proxy pinned to the wrong CA")
		}
	})

	t.Run("plaintext to a non-loopback host refuses", func(t *testing.T) {
		plog := waitForExit(t, cli, runProxy(t, cli, spec("http://"+host+":8443", "")), 60*time.Second)
		if !strings.Contains(plog, "plain http:// to a non-loopback host") {
			t.Fatalf("proxy did not refuse the plaintext control plane:\n%s", plog)
		}
	})
}

// startHopFake builds test/conformance/hopfake for the daemon's platform and
// runs it in a busybox container on wardyn-internal under the alias host, with
// a serving cert for host signed by ca.
func startHopFake(t *testing.T, cli *dockerclient.Client, ca *hoptls.CA, host string, grant uuid.UUID, token string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "hopfake")
	build := exec.Command("go", "build", "-o", bin, "./hopfake")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build hopfake: %v\n%s", err, out)
	}
	cert, err := ca.ServingCert(host, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	created, err := cli.ContainerCreate(ctx, dockerclient.ContainerCreateOptions{
		Name: host,
		Config: &container.Config{
			Image: "busybox:latest",
			Cmd:   []string{"/hopfake"},
			Env: []string{
				"HOPFAKE_CERT_PEM=" + string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})),
				"HOPFAKE_KEY_PEM=" + string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})),
				"HOPFAKE_GRANT=" + grant.String(),
				"HOPFAKE_TOKEN=" + token,
			},
		},
		HostConfig: &container.HostConfig{NetworkMode: "wardyn-internal"},
		NetworkingConfig: &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{
			"wardyn-internal": {Aliases: []string{host}},
		}},
	})
	if err != nil {
		t.Fatalf("create hopfake: %v", err)
	}
	t.Cleanup(func() {
		_, _ = cli.ContainerRemove(context.Background(), created.ID, dockerclient.ContainerRemoveOptions{Force: true})
	})
	b, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "hopfake", Mode: 0o755, Size: int64(len(b))}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write(b)
	_ = tw.Close()
	if _, err := cli.CopyToContainer(ctx, created.ID, dockerclient.CopyToContainerOptions{DestinationPath: "/", Content: &buf}); err != nil {
		t.Fatalf("copy hopfake: %v", err)
	}
	if _, err := cli.ContainerStart(ctx, created.ID, dockerclient.ContainerStartOptions{}); err != nil {
		t.Fatalf("start hopfake: %v", err)
	}
	return created.ID
}

// runProxy starts the proxy image as a bare container on wardyn-internal with
// the sealed config the driver would hand it, and returns its name.
func runProxy(t *testing.T, cli *dockerclient.Client, s runner.SandboxSpec) string {
	t.Helper()
	cfg, err := runner.BuildProxyConfig(s.RunID, s.ProxyConfig, runner.ProxyListenPort)
	if err != nil {
		t.Fatal(err)
	}
	name := "wardyn-hoptls-proxy-" + s.RunID.String()[:8]
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	created, err := cli.ContainerCreate(ctx, dockerclient.ContainerCreateOptions{
		Name:       name,
		Config:     &container.Config{Image: bootEgressProxyImage, Env: []string{"WARDYN_PROXY_CONFIG_JSON=" + string(cfg)}},
		HostConfig: &container.HostConfig{NetworkMode: "wardyn-internal"},
	})
	if err != nil {
		t.Fatalf("create proxy: %v", err)
	}
	t.Cleanup(func() {
		_, _ = cli.ContainerRemove(context.Background(), created.ID, dockerclient.ContainerRemoveOptions{Force: true})
	})
	if _, err := cli.ContainerStart(ctx, created.ID, dockerclient.ContainerStartOptions{}); err != nil {
		t.Fatalf("start proxy: %v", err)
	}
	return name
}

func containerLogs(t *testing.T, cli *dockerclient.Client, ref string) string {
	t.Helper()
	rc, err := cli.ContainerLogs(context.Background(), ref, dockerclient.ContainerLogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		return "logs: " + err.Error()
	}
	defer rc.Close()
	var out bytes.Buffer
	_, _ = stdcopy.StdCopy(&out, &out, rc)
	return out.String()
}

func waitForLog(t *testing.T, cli *dockerclient.Client, ref, want string, within time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		logs := containerLogs(t, cli, ref)
		if strings.Contains(logs, want) {
			return logs
		}
		if time.Now().After(deadline) {
			t.Fatalf("%q never appeared in %s's logs:\n%s", want, ref, logs)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func proxyState(t *testing.T, cli *dockerclient.Client, name string) (running bool, code int, logs string) {
	t.Helper()
	res, err := cli.ContainerInspect(context.Background(), name, dockerclient.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("inspect %s: %v", name, err)
	}
	st := res.Container.State
	return st.Running, st.ExitCode, containerLogs(t, cli, name)
}

// waitForExit waits for a proxy that must refuse to start, and returns its logs.
func waitForExit(t *testing.T, cli *dockerclient.Client, name string, within time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		running, code, logs := proxyState(t, cli, name)
		if !running {
			if code == 0 {
				t.Fatalf("%s exited 0; a refusal must exit non-zero:\n%s", name, logs)
			}
			return logs
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s is still running; it must fail closed:\n%s", name, logs)
		}
		time.Sleep(500 * time.Millisecond)
	}
}
