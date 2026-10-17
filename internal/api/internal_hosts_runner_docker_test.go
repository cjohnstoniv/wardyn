// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package api

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/hoptls"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/docker"
	"github.com/cjohnstoniv/wardyn/internal/testfloor"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestInternalHostsOnRunnerDocker is the live tier of the remote-runner pin:
// the spec the organisation's local-placement dispatch produced
// (dispatchInternalHostsRun) goes over the runner wire (a loopback runner
// session served by runnertest) to the real Docker driver, which starts the
// real wardyn-proxy image with it. The proxy's hosts map both test names to a
// CGNAT address that answers (the gateway of a 100.64.x.0/24 bridge, where a
// host-network httpd listens), and a stand-in control plane collects the
// decision rows the proxy posts. Asserted from those rows:
//   - the declared name is allowed and attributed to the declaration itself
//     (site-config:internal-host, no approval);
//   - the undeclared name in the same range is denied builtin:private-ip;
//   - after the site drops the declaration, the revive path
//     (refreshDeploymentConfig, currentInternalHosts) replaces the proxy over
//     the wire and the declared name is denied again.
func TestInternalHostsOnRunnerDocker(t *testing.T) {
	testfloor.Mark(t, "internal-hosts-docker")
	if os.Getenv("WARDYN_TEST_DOCKER") != "1" {
		t.Skip("WARDYN_TEST_DOCKER=1 not set; skipping the live internal_hosts runner pin")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	tag := uuid.NewString()[:8]

	cpNet := "wardyn-ih-cp-" + tag
	dockerCLI(t, "network", "create", cpNet)
	t.Cleanup(func() { _ = exec.Command("docker", "network", "rm", cpNet).Run() })
	// The CGNAT answer is a host address (a bridge gateway), not a container:
	// the proxy reaches it from its own network, and it is in none of the
	// proxy's interface subnets, which the lift excludes.
	octet := strconv.Itoa(1 + rand.IntN(254))
	cgnatNet, cgnatIP := "wardyn-ih-cgnat-"+tag, "100.64."+octet+".1"
	dockerCLI(t, "network", "create", "--subnet", "100.64."+octet+".0/24", "--gateway", cgnatIP, cgnatNet)
	t.Cleanup(func() { _ = exec.Command("docker", "network", "rm", cgnatNet).Run() })
	port := strconv.Itoa(20000 + rand.IntN(10000))
	httpd := "wardyn-ih-httpd-" + tag
	dockerCLI(t, "run", "-d", "--name", httpd, "--network", "host", "busybox:latest",
		"sh", "-c", "mkdir -p /w && echo internal > /w/index.html && exec httpd -f -p "+port+" -h /w")
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", httpd).Run() })

	caBlob, err := hoptls.NewCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ca, err := hoptls.ParseCA(caBlob)
	if err != nil {
		t.Fatal(err)
	}
	cpHost := "wardyn-ih-cp-fake-" + tag
	startDecisionCollector(t, ca, cpHost, cpNet)

	sub, err := docker.New(docker.Config{ProxyImage: "wardyn/wardyn-proxy:local", InternalNetwork: cpNet,
		TestProxyExtraHosts: []string{internalHostDeclared + ":" + cgnatIP, internalHostUndeclared + ":" + cgnatIP}})
	if err != nil {
		t.Fatal(err)
	}
	sc := internalHostsSite()
	sc.Runners = &types.RunnerSettings{Enabled: true}
	runnerID := uuid.New()
	srv, run, spec := dispatchInternalHostsRun(t, &sc, runnerID, "busybox:latest", "https://"+cpHost+":8443", string(ca.CertPEM))
	attachRunner(t, srv, runnerID, sub)
	ep := srv.runnerEndpoint(runnerID)
	sb, err := ep.sub.CreateSandbox(ctx, spec)
	if err != nil {
		t.Fatalf("CreateSandbox on the runner: %v", err)
	}
	t.Cleanup(func() { _ = ep.sub.KillSandbox(context.Background(), sb.Ref) })
	agent, ok := strings.CutPrefix(sb.Ref, placement.RunnerRefPrefix(runnerID.String()))
	if !ok {
		t.Fatalf("sandbox ref %q is not this runner's", sb.Ref)
	}

	// fetch asks the agent to GET host through its proxy and reports whether
	// the page came back.
	fetch := func(host string) bool {
		return exec.Command("docker", "exec", "-e", "http_proxy=http://wardyn-proxy:"+strconv.Itoa(runner.ProxyListenPort), agent,
			"wget", "-q", "-T", "10", "-O", "/dev/null", "http://"+host+":"+port+"/").Run() == nil
	}
	rows := func() string { return dockerCLI(t, "logs", cpHost) }
	const rowPrefix = "hopfake: decision "

	if !fetch(internalHostDeclared) {
		t.Errorf("the agent did not reach %s through its runner's proxy", internalHostDeclared)
	}
	if d := waitForDecision(t, rows, rowPrefix, internalHostDeclared, 0); d.Decision != egress.Allow || d.RuleSource != "site-config:internal-host" || d.ApprovalID != nil {
		t.Errorf("declared: decision %s %s (approval %v), want allow site-config:internal-host by the declaration, not an approval", d.Decision, d.RuleSource, d.ApprovalID)
	}
	if fetch(internalHostUndeclared) {
		t.Errorf("the agent reached %s, which no internal_hosts entry declares", internalHostUndeclared)
	}
	if d := waitForDecision(t, rows, rowPrefix, internalHostUndeclared, 0); d.Decision != egress.Deny || d.RuleSource != "builtin:private-ip" {
		t.Errorf("undeclared: decision %s %s, want deny builtin:private-ip", d.Decision, d.RuleSource)
	}

	// The organisation removes the declaration; the run is revived.
	sc.InternalHosts = nil
	srv.cfg.Store = reviveTrustDispatchStore{srv.cfg.Store.(siteConfigDispatchStore)}
	raw, err := srv.loadRunProxyConfig(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := srv.loadRenderedProxyConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.refreshDeploymentConfig(ctx, run, cfg); err != nil {
		t.Fatalf("revive re-check: %v", err)
	}
	if len(cfg.InternalHosts) != 0 {
		t.Fatalf("revived config keeps internal_hosts %+v the site no longer declares", cfg.InternalHosts)
	}
	cfgJSON, rerr := encodeRevivedConfig(cfg)
	if rerr != nil {
		t.Fatalf("encode revived config: %+v", rerr)
	}
	if err := ep.sub.ReplaceProxy(ctx, sb.Ref, cfgJSON); err != nil {
		t.Fatalf("ReplaceProxy on the runner: %v", err)
	}
	if fetch(internalHostDeclared) {
		t.Errorf("the agent still reached %s after its declaration was removed", internalHostDeclared)
	}
	if d := waitForDecision(t, rows, rowPrefix, internalHostDeclared, 1); d.Decision != egress.Deny || d.RuleSource != "builtin:private-ip" {
		t.Errorf("after revive: decision %s %s, want deny builtin:private-ip", d.Decision, d.RuleSource)
	}
}

// startDecisionCollector runs test/conformance/hopfake as the stand-in control
// plane on network under the alias host, serving TLS with a leaf from ca; it
// logs every decision row the proxy posts.
func startDecisionCollector(t *testing.T, ca *hoptls.CA, host, network string) {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "hopfake")
	build := exec.Command("go", "build", "-o", bin, "github.com/cjohnstoniv/wardyn/test/conformance/hopfake")
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
	dockerCLI(t, "create", "--name", host, "--network", network, "--network-alias", host,
		"-e", "HOPFAKE_CERT_PEM="+string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})),
		"-e", "HOPFAKE_KEY_PEM="+string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})),
		"-e", "HOPFAKE_GRANT="+uuid.NewString(), "-e", "HOPFAKE_TOKEN=run-token",
		"busybox:latest", "/hopfake")
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", host).Run() })
	dockerCLI(t, "cp", bin, host+":/hopfake")
	dockerCLI(t, "start", host)
}

func dockerCLI(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", args[0], err, out)
	}
	return string(out)
}

// waitForDecision waits for the (skip+1)th decision row for host among the
// lines logs() returns, each a JSON row after prefix, and returns it.
func waitForDecision(t *testing.T, logs func() string, prefix, host string, skip int) egress.DecisionLog {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		seen, text := 0, logs()
		for _, line := range strings.Split(text, "\n") {
			row, ok := strings.CutPrefix(line, prefix)
			var d egress.DecisionLog
			if !ok || json.Unmarshal([]byte(row), &d) != nil || d.Request.Host != host {
				continue
			}
			if seen == skip {
				return d
			}
			seen++
		}
		if time.Now().After(deadline) {
			t.Fatalf("no decision row %d for %s:\n%s", skip+1, host, text)
		}
		time.Sleep(500 * time.Millisecond)
	}
}
