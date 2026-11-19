// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// proxyDNSRows reads the proxy container's log and returns the DNS rows in its mirrored decision stream.
func proxyDNSRows(ctx context.Context, t *testing.T, name string) []egress.DNSQuery {
	t.Helper()
	cli, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer func() { _ = cli.Close() }()
	rc, err := cli.ContainerLogs(ctx, name, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		t.Fatalf("proxy logs: %v", err)
	}
	defer func() { _ = rc.Close() }()
	var stdout, stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, &stderr, rc); err != nil && err != io.EOF {
		t.Fatalf("proxy logs: %v", err)
	}
	var rows []egress.DNSQuery
	for _, line := range strings.Split(stdout.String(), "\n") {
		var dl egress.DecisionLog
		if strings.HasPrefix(line, "{") && json.Unmarshal([]byte(line), &dl) == nil && dl.DNS != nil {
			rows = append(rows, *dl.DNS)
		}
	}
	return rows
}

// TestL0_DNSQueryIsRecorded: on a real daemon with the real proxy image, the agent's /etc/resolv.conf is exactly
// its run proxy, a lookup there is answered REFUSED by the proxy's listener (the lookup fails), and the proxy
// records it as a DNS row naming the queried host with via "proxy" and outcome "refused". The image is
// wardyn/wardyn-proxy:local (WARDYN_TEST_PROXY_IMAGE overrides), which test-conformance-docker already needs.
func TestL0_DNSQueryIsRecorded(t *testing.T) {
	if os.Getenv("WARDYN_TEST_DOCKER") != "1" {
		t.Skip("set WARDYN_TEST_DOCKER=1 to run the docker L0 network tests")
	}
	img := os.Getenv("WARDYN_TEST_PROXY_IMAGE")
	if img == "" {
		img = "wardyn/wardyn-proxy:local"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	d, err := New(Config{ProxyImage: img})
	if err != nil {
		t.Fatalf("docker.New: %v", err)
	}
	ensureNetwork(t, d, d.cfg.InternalNetwork)
	runID := uuid.New()
	sb, err := d.CreateSandbox(ctx, runner.SandboxSpec{
		RunID:            runID,
		Image:            "busybox:latest",
		ConfinementClass: types.CC1,
		ProxyConfig:      runner.ProxyConfig{RunToken: "dns-audit-test", ControlPlaneURL: "http://127.0.0.1:9", Policy: types.RunPolicySpec{AllowedDomains: []string{"example.com"}}},
		Resources:        runner.Resources{CPUMillis: 500, MemoryMiB: 128},
	})
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	t.Cleanup(func() { _ = d.KillSandbox(context.Background(), sb.Ref) })
	waitForLog(ctx, t, proxyContainerName(runID), "listening")

	const probe = "wardyn-l0-dns-audit-probe.example.com"
	proxyIP := strings.TrimSpace(execInSandbox(ctx, t, d, sb.Ref, []string{"sh", "-c", "awk '$2==\"wardyn-proxy\"{print $1}' /etc/hosts"}))
	out := execInSandbox(ctx, t, d, sb.Ref, []string{"sh", "-c", "cat /etc/resolv.conf; echo ---; nslookup -type=a " + probe + " 2>&1; echo RC=$?"})
	t.Logf("resolver probe:\n%s", out)
	resolv, lookup, ok := strings.Cut(out, "---\n")
	if !ok || proxyIP == "" {
		t.Fatalf("the probe printed no separator or the agent pins no proxy address (%q):\n%s", proxyIP, out)
	}
	if strings.TrimSpace(resolv) != "nameserver "+proxyIP {
		t.Errorf("/etc/resolv.conf = %q, want exactly %q", strings.TrimSpace(resolv), "nameserver "+proxyIP)
	}
	if !strings.Contains(lookup, proxyIP) || !strings.Contains(lookup, "RC=") || strings.Contains(lookup, "RC=0") {
		t.Errorf("the lookup should name the proxy %s as its server and fail:\n%s", proxyIP, lookup)
	}

	want := egress.DNSQuery{Name: probe, Type: "A", Outcome: "refused", Via: "proxy"}
	var rows []egress.DNSQuery
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
		rows = proxyDNSRows(ctx, t, proxyContainerName(runID))
		for _, r := range rows {
			if r == want {
				return
			}
		}
	}
	t.Fatalf("no DNS row %+v in the proxy's decision log; rows seen: %+v", want, rows)
}
