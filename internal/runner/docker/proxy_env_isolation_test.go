// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"strings"
	"testing"
)

// `wardyn-runnerd setup` copies the shell's proxy variables into the runner service only. They are the
// runner's own route to the organisation: a run's container and its proxy sidecar build their environment
// from the run, never from the daemon's, so neither may carry them.
func TestCreateSandbox_NeitherContainerCarriesTheServicesProxyVariables(t *testing.T) {
	const planted = "http://svc-user:planted-pw@corp-proxy.example:3128"
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy"} {
		t.Setenv(k, planted)
	}
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	if _, err := newTestDriver(f).CreateSandbox(context.Background(), testSpec()); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if len(f.containers) < 2 {
		t.Fatalf("want the sandbox and its proxy sidecar, got %d containers", len(f.containers))
	}
	for ref, c := range f.containers {
		for _, e := range c.cfg.Env {
			if strings.Contains(e, "corp-proxy.example") || strings.Contains(e, "planted-pw") {
				k, _, _ := strings.Cut(e, "=")
				t.Errorf("container %s carries the service's proxy setting %s", ref, k)
			}
		}
	}
}
