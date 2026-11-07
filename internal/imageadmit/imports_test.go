// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package imageadmit_test

import (
	"os/exec"
	"strings"
	"testing"
)

// The package stays light: it never links the API (which links it), and no registry client library (the list in
// cmd/wardynd/registry_read_guard_test.go). go-containerregistry's pkg/name is the one exception: sigstore-go
// links it through sigstore's signature/payload, and it only parses image names, as distribution/reference does.
func TestImportsStayLight(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "-buildvcs=false", ".").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	banned := []string{
		"github.com/cjohnstoniv/wardyn/internal/api", "github.com/cjohnstoniv/wardyn/internal/store",
		"github.com/google/go-containerregistry", "github.com/distribution/distribution", "github.com/docker/distribution",
		"github.com/containerd/containerd/remotes", "github.com/containerd/containerd/v2/core/remotes",
		"github.com/containers/image", "github.com/regclient/regclient", "oras.land/oras-go",
	}
	for _, pkg := range strings.Fields(string(out)) {
		if pkg == "github.com/google/go-containerregistry/pkg/name" {
			continue
		}
		for _, b := range banned {
			if pkg == b || strings.HasPrefix(pkg, b+"/") {
				t.Errorf("internal/imageadmit links %s", pkg)
			}
		}
	}
}
