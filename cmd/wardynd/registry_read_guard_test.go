// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// registryClients are module paths that can read an image registry. The control plane links none of
// them: a prewrapped image is checked by the kubelet's own pull in a check pod, and image admission's
// registry reads go through internal/imageadmit's own client, only while admission is on.
// github.com/distribution/reference only parses image names and stays allowed.
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

// registryNameParsers are packages under a registryClients path that only parse image names, like
// github.com/distribution/reference: no transport, no client. Each is allowed as that exact package, never
// its subtree. go-containerregistry's pkg/name imports nothing beyond the standard library and go-digest; it
// reaches internal/api through sigstore-go, both its signature payload (which parses the signed image's name)
// and its trusted root parser (image admission's check of a trusted root file at write).
var registryNameParsers = map[string]bool{
	"github.com/google/go-containerregistry/pkg/name": true,
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
			if registryNameParsers[pkg] {
				continue
			}
			for _, client := range registryClients {
				if pkg == client || strings.HasPrefix(pkg, client+"/") {
					t.Errorf("tags=%q: internal/api links %s, a registry client", tags, pkg)
				}
			}
		}
	}
}
