// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// registryClients are module paths that can read an image registry. The control plane never does:
// a prewrapped image is checked by the kubelet's own pull in a check pod, so no registry request or
// pull credential passes through wardynd. github.com/distribution/reference only parses image names
// and stays allowed.
var registryClients = []string{
	"github.com/google/go-containerregistry",
	"github.com/distribution/distribution",
	"github.com/docker/distribution",
	"github.com/containerd/containerd/remotes",
	"github.com/containerd/containerd/v2/core/remotes",
	"github.com/containers/image",
	"github.com/regclient/regclient",
	"oras.land/oras-go",
}

// The API links no registry client under any build it ships with, so a registry read cannot come
// back into it unnoticed.
func TestAPILinksNoRegistryClient(t *testing.T) {
	root := repoRoot(t)
	for _, tags := range []string{"", "docker", "k8s", "docker,k8s"} {
		cmd := exec.Command("go", "list", "-deps", "-buildvcs=false", "-tags="+tags, "./internal/api/...")
		var stderr strings.Builder
		cmd.Dir, cmd.Env, cmd.Stderr = root, append(os.Environ(), "PWD="+root), &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("go list -deps -tags=%q ./internal/api/...: %v\n%s", tags, err, stderr.String())
		}
		for _, pkg := range strings.Fields(string(out)) {
			for _, client := range registryClients {
				if pkg == client || strings.HasPrefix(pkg, client+"/") {
					t.Errorf("tags=%q: internal/api links %s, a registry client", tags, pkg)
				}
			}
		}
	}
}
