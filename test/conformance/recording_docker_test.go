// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package conformance_test

import (
	"cmp"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	dockerclient "github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/hoptls"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/docker"
	"github.com/cjohnstoniv/wardyn/internal/runner/orchestrator"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/test/conformance"
)

// TestRecordingDocker builds the docker substrate the way production does
// (Record on, the real wardyn-proxy image, an agent image carrying wardyn-rec)
// and proves a recorded command's cast leaves the sandbox: through the proxy's
// brokered recording route, over the TLS hop, to a stand-in control plane
// (hopfake) that logs what it received. TestConformanceDocker's busybox proxy
// cannot relay, so its substrate declares no recording and never gets here.
func TestRecordingDocker(t *testing.T) {
	if os.Getenv("WARDYN_TEST_DOCKER") != "1" {
		t.Skip("WARDYN_TEST_DOCKER=1 not set; skipping the docker session-recording case")
	}
	cli, err := dockerclient.New(dockerclient.FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	ensureConformanceNetwork(t, "wardyn-internal")

	caBlob, err := hoptls.NewCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ca, err := hoptls.ParseCA(caBlob)
	if err != nil {
		t.Fatal(err)
	}
	host := "wardyn-hopfake-" + uuid.NewString()[:8]
	token := "recording-run-token"
	fake := startHopFake(t, cli, ca, host, uuid.New(), token)

	sub, err := docker.New(docker.Config{ProxyImage: bootEgressProxyImage, Record: true})
	if err != nil {
		t.Fatalf("docker.New: %v", err)
	}
	r := orchestrator.New(sub)

	// Up front, so a substrate that stopped declaring recording fails here
	// instead of CheckRecordingCapability taking its honest-empty branch.
	caps, err := r.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if !caps.SessionRecording {
		t.Fatalf("docker substrate with Record=true declares SessionRecording=false")
	}

	var runID uuid.UUID
	conformance.CheckRecordingCapability(t, r, conformance.RecordingOptions{
		Options: conformance.Options{
			SandboxImage: cmp.Or(os.Getenv("WARDYN_CONFORMANCE_AGENT_IMAGE"), "wardyn/conformance-agent:local"),
			Timeout:      3 * time.Minute,
			ExitArgv:     func(int) []string { return []string{"sh", "-c", "echo wardyn-rec-canary"} },
		},
		MutateSpec: func(spec *runner.SandboxSpec) {
			spec.ProxyConfig = runner.ProxyConfig{
				RunToken:          token,
				ControlPlaneURL:   "https://" + host + ":8443",
				ControlPlaneCAPEM: string(ca.CertPEM),
				Policy:            types.RunPolicySpec{MinConfinementClass: types.CC1},
			}
			runID = spec.RunID
		},
		RecordingProbe: func(t *testing.T, _ string) {
			want := "hopfake: recording run=" + runID.String()
			logs := waitForLog(t, cli, fake, want, 60*time.Second)
			for line := range strings.SplitSeq(logs, "\n") {
				if !strings.HasPrefix(line, want) {
					continue
				}
				if !strings.Contains(line, "canary=true") || !strings.Contains(line, "tls=true") {
					t.Fatalf("the cast arrived without the canary or off TLS: %q\n%s", line, logs)
				}
				t.Logf("control plane saw: %s", line)
				return
			}
			t.Fatalf("no line starts with %q:\n%s", want, logs)
		},
	})
}
