// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package envbuild

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestComposeEnvbuildBuildNetworkIsolatedFromControlPlane is the regression for
// W10-S1-2: deploy/compose/docker-compose.yaml used to default
// WARDYN_ENVBUILD_BUILD_NETWORK to the SAME `wardyn-internal` bridge that
// postgres and wardynd's admin API sit on. The envbuild build container runs
// attacker-controlled code (devcontainer RUN/feature/onCreate steps —
// Builder's own package doc), so that made it an in-network peer able to dial
// postgres/the admin API by compose service name. This parses the real
// compose file (no `docker` build tag needed — it never touches a daemon) and
// asserts the build-network default resolves to a DIFFERENT top-level network
// than the one postgres/wardynd are members of.
func TestComposeEnvbuildBuildNetworkIsolatedFromControlPlane(t *testing.T) {
	path := filepath.Join("..", "..", "deploy", "compose", "docker-compose.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	var doc struct {
		Services map[string]struct {
			Environment map[string]string `yaml:"environment"`
			Networks    []string          `yaml:"networks"`
		} `yaml:"services"`
		Networks map[string]struct {
			Name string `yaml:"name"`
		} `yaml:"networks"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	postgres, ok := doc.Services["postgres"]
	if !ok {
		t.Fatal("compose file has no `postgres` service (test needs updating, or the file is broken)")
	}
	for _, n := range postgres.Networks {
		if n == "wardyn-envbuild" {
			t.Fatalf("postgres is a member of wardyn-envbuild — the untrusted build container's network must not carry the control-plane database")
		}
	}

	wardynd, ok := doc.Services["wardynd"]
	if !ok {
		t.Fatal("compose file has no `wardynd` service (test needs updating, or the file is broken)")
	}
	buildNetExpr, ok := wardynd.Environment["WARDYN_ENVBUILD_BUILD_NETWORK"]
	if !ok {
		t.Fatal("wardynd has no WARDYN_ENVBUILD_BUILD_NETWORK env entry (test needs updating, or the file regressed)")
	}

	internalNet, ok := doc.Networks["wardyn"]
	if !ok {
		t.Fatal("compose file has no top-level `wardyn` network (test needs updating, or the file is broken)")
	}
	envbuildNet, ok := doc.Networks["wardyn-envbuild"]
	if !ok {
		t.Fatal("compose file has no dedicated `wardyn-envbuild` network — the build container still shares a network with postgres/wardynd")
	}

	if strings.Contains(buildNetExpr, internalNet.Name) {
		t.Fatalf("WARDYN_ENVBUILD_BUILD_NETWORK default (%q) resolves onto the control-plane network (%q) — postgres/wardynd's admin API would be a reachable peer of the untrusted build container", buildNetExpr, internalNet.Name)
	}
	if !strings.Contains(buildNetExpr, envbuildNet.Name) {
		t.Fatalf("WARDYN_ENVBUILD_BUILD_NETWORK default (%q) does not reference the dedicated build network (%q)", buildNetExpr, envbuildNet.Name)
	}
}
