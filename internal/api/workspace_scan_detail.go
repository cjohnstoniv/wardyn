// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"os"
	"path/filepath"
	"strings"
)

// runningInContainer reports whether wardynd itself is inside a container
// (docker's /.dockerenv, podman's /run/.containerenv). Used ONLY to choose the
// WORDING of a local-directory scan failure — never for a security decision:
// a sealed daemon that cannot see the operator's directory should say that,
// not claim the directory doesn't exist.
func runningInContainer() bool {
	for _, p := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// localDirScanFailureDetail is the 422 body for a local_dir source that failed
// its pre-scan stat — shown VERBATIM as the failure headline in the wizard and
// the scan toast, so it carries the fix, not just the fact.
//
// The compose deployment made the bare message a lie in the operator's eyes:
// wardynd runs sealed there and sees ONLY what WARDYN_WORKSPACES_ROOT mounts
// in (nothing, by default — deliberately, see docker-compose.yaml), so "not
// found on this host" fired for directories that plainly exist on the host.
// Three shapes, most specific first: outside the configured root, no root
// configured at all (containerized), and the plain host-mode miss (a typo).
func localDirScanFailureDetail(path string, notDir bool, workspacesRoot string, containerized bool) string {
	if notDir {
		return "onboarded source is not a directory: " + path
	}
	base := "local directory not found on this host: " + path
	switch {
	case workspacesRoot != "" && !pathWithinRoot(workspacesRoot, path):
		return base + ". wardynd only sees directories under its configured workspaces root (" + workspacesRoot +
			") — move the project under it, or point WARDYN_WORKSPACES_ROOT at a directory that contains it and re-run `make setup`."
	case workspacesRoot == "" && containerized:
		return base + ". wardynd is running inside a container with no WARDYN_WORKSPACES_ROOT set, so NO host directory is visible to it. " +
			"Set WARDYN_WORKSPACES_ROOT to the directory that contains your projects (not your whole home directory) and re-run `make setup`."
	default:
		return base
	}
}

// pathWithinRoot reports whether path is root itself or inside it, on cleaned
// absolute paths — a plain prefix check would call /home/x-evil inside /home/x.
func pathWithinRoot(root, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}
